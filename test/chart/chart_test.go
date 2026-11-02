package chart

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/")

const (
	chartDir    = "../../charts/hvmc"
	toolsMod    = "../../tools/go.mod"
	release     = "hvmc"
	namespace   = "hvmc"
	kubeVersion = "1.37.0"
)

// cases are the values files the chart is rendered with. Every case has
// testdata/<name>.golden.yaml (manifests without the CRDs) and
// testdata/<name>.notes.txt.
//
// The CRDs are left out of the golden files: they are copies of
// config/crd/bases (hack/chart-crds.sh, checked by CI's generated-code
// check), and would make up most of every golden file. checkCRDs covers what
// the chart adds to them.
var cases = []struct {
	name   string
	values string // relative to this directory; "" for the chart defaults
	// keepCRDs, when set, checks that both CRDs render and whether they
	// carry helm.sh/resource-policy: keep.
	keepCRDs *bool
}{
	{name: "default", keepCRDs: new(false)},
	// Ingress with host, class, annotations and TLS; every kind of VM.
	{name: "ingress", values: chartDir + "/ci/ci-values.yaml"},
	{name: "ingress-off", values: "testdata/ingress-off.values.yaml"},
	{name: "loadbalancer", values: "testdata/loadbalancer.values.yaml"},
	{name: "crds-keep", values: "testdata/crds-keep.values.yaml", keepCRDs: new(true)},
	{name: "hostconfig-existing", values: "testdata/hostconfig-existing.values.yaml"},
	// ServiceMonitor on, NetworkPolicies off, scheduling and image overrides.
	{name: "options", values: "testdata/options.values.yaml"},
}

func TestGolden(t *testing.T) {
	// Not a line-ending issue (.gitattributes keeps LF everywhere): the test
	// builds the pinned Helm, a large binary, only to check output that does
	// not depend on the platform, and the chart is linux/amd64 only.
	if runtime.GOOS == "windows" {
		t.Skip("the Linux job runs the chart tests; building Helm on Windows would only repeat them")
	}
	// The chart itself is read in-process by chartWithNotesManifest.
	track(t, toolsMod)
	h := newHelm(t)
	notesChart := chartWithNotesManifest(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := []string{"--namespace", namespace, "--kube-version", kubeVersion}
			if tc.values != "" {
				args = append(args, "--values", tc.values)
				track(t, tc.values)
			}
			manifests, crds := splitCRDs(h.run(t, append([]string{"template", release, chartDir}, args...)...))
			compare(t, "testdata/"+tc.name+".golden.yaml", manifests)
			if tc.keepCRDs != nil {
				checkCRDs(t, crds, *tc.keepCRDs)
			}

			out := h.run(t, append([]string{
				"template", release, notesChart, "--show-only", "templates/" + notesManifest,
			}, args...)...)
			_, value, ok := strings.Cut(string(out), "\n  notes: ")
			var notes string
			if !ok || json.Unmarshal([]byte(strings.TrimSpace(value)), &notes) != nil {
				t.Fatalf("no notes in rendered %s:\n%s", notesManifest, out)
			}
			compare(t, "testdata/"+tc.name+".notes.txt", []byte(notes+"\n"))
		})
	}
}

var (
	crdKind         = regexp.MustCompile(`(?m)^kind: CustomResourceDefinition$`)
	crdName         = regexp.MustCompile(`(?m)^  name: (\S+)$`)
	keepAnnotation  = regexp.MustCompile(`(?m)^    helm\.sh/resource-policy: keep$`)
	wantCRDNames    = []string{"hostconfigs.hvmc.fax.fyi", "memoryleases.hvmc.fax.fyi"}
	documentDivider = "---\n"
)

// splitCRDs splits helm template output into the manifests without the
// CustomResourceDefinitions and the CRD documents.
func splitCRDs(out []byte) (manifests []byte, crds []string) {
	var keep []string
	for doc := range strings.SplitSeq(string(out), documentDivider) {
		if crdKind.MatchString(doc) {
			crds = append(crds, doc)
			continue
		}
		keep = append(keep, doc)
	}
	return []byte(strings.Join(keep, documentDivider)), crds
}

// checkCRDs checks that both CRDs rendered, with the keep resource policy
// exactly when crds.keep is set.
func checkCRDs(t *testing.T, crds []string, keep bool) {
	t.Helper()
	var names []string
	for _, doc := range crds {
		m := crdName.FindStringSubmatch(doc)
		if m == nil {
			t.Fatalf("CRD without metadata.name:\n%s", doc)
		}
		names = append(names, m[1])
		if got := keepAnnotation.MatchString(doc); got != keep {
			t.Errorf("CRD %s: helm.sh/resource-policy: keep present = %t, want %t", m[1], got, keep)
		}
	}
	slices.Sort(names)
	if !slices.Equal(names, wantCRDNames) {
		t.Errorf("rendered CRDs = %v, want %v", names, wantCRDNames)
	}
}

// helm runs the Helm CLI pinned in tools/go.mod through `go tool`, isolated
// from the user's Helm configuration and kubeconfig.
type helm struct {
	modfile string
	env     []string
}

func newHelm(t *testing.T) *helm {
	t.Helper()
	modfile, err := filepath.Abs(toolsMod)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	return &helm{
		modfile: modfile,
		env: append(os.Environ(),
			// Same build cache entry as the Makefile's `go tool helm`.
			"CGO_ENABLED=0",
			"HELM_CACHE_HOME="+filepath.Join(home, "cache"),
			"HELM_CONFIG_HOME="+filepath.Join(home, "config"),
			"HELM_DATA_HOME="+filepath.Join(home, "data"),
			"KUBECONFIG="+filepath.Join(home, "kubeconfig"),
		),
	}
}

func (h *helm) run(t *testing.T, args ...string) []byte {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", append([]string{"tool", "-modfile=" + h.modfile, "helm"}, args...)...)
	cmd.Env = h.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.Bytes()
}

const notesManifest = "golden-notes.yaml"

// chartWithNotesManifest copies the chart and adds a manifest carrying the
// rendered NOTES.txt. Helm renders NOTES.txt only on install, and a
// client-only install dry run still contacts the cluster.
func chartWithNotesManifest(t *testing.T) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "hvmc")
	if err := os.CopyFS(dst, os.DirFS(chartDir)); err != nil {
		t.Fatal(err)
	}
	notes, err := os.ReadFile(filepath.Join(chartDir, "templates", "NOTES.txt"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"_golden-notes.tpl": `{{- define "golden.notes" -}}` + string(notes) + `{{- end }}`,
		notesManifest: `apiVersion: v1
kind: ConfigMap
metadata:
  name: golden-notes
data:
  notes: {{ include "golden.notes" . | toJson }}
`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dst, "templates", name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

// track reads files that only the helm subprocess uses, so that go test's
// result cache notices when they change.
func track(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.ReadFile(p); err != nil {
			t.Fatal(err)
		}
	}
}

func compare(t *testing.T, golden string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(golden, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run `make helm-golden` to create it)", err)
	}
	if diff := cmp.Diff(string(want), string(got)); diff != "" {
		t.Errorf("%s differs (-want +got; run `make helm-golden` if the change is intended):\n%s", golden, diff)
	}
}
