package policy

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"sigs.k8s.io/yaml"
)

// exampleFile is the agent.yaml of DESIGN §4.3.
const exampleFile = `role: host
listen: 0.0.0.0:9443
health: 0.0.0.0:9090
allowedClients: ["hvmc-coordinator"]      # certificate CN
vms:
  - name: linux-node-1
    fixed: true                           # read-only
  - name: windows-node-1
    min: 6Gi
    max: 24Gi
  - name: docker-host-1
    min: 4Gi
    max: 32Gi
`

const exampleCanonical = `vms:
- max: 32Gi
  min: 4Gi
  name: docker-host-1
- fixed: true
  name: linux-node-1
- max: 24Gi
  min: 6Gi
  name: windows-node-1
`

func TestCanonicalForm(t *testing.T) {
	// VMs sorted by name, keys alphabetically within an entry.
	if diff := cmp.Diff(exampleCanonical, string(example().Canonical())); diff != "" {
		t.Fatalf("Canonical() mismatch (-want +got):\n%s", diff)
	}
	if got := string((Policy{}).Canonical()); got != "vms: []\n" {
		t.Fatalf("empty Canonical() = %q", got)
	}
	if got, want := (Policy{}).Digest(), (Policy{VMs: []VM{}}).Digest(); got != want {
		t.Fatalf("nil and empty VMs differ: %s, %s", got, want)
	}
}

func TestCanonicalQuantitySpelling(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{8 * gi, "8Gi"},
		{1536 * mi, "1536Mi"},
		{2 * mi, "2Mi"},
		{1000000000, `"1000000000"`},
		{3*gi + 1, `"3221225473"`},
		{Unbounded, `"9223372036854775807"`},
	}
	for _, tt := range tests {
		p := Policy{VMs: []VM{{Name: "a", Min: tt.bytes, Max: tt.bytes}}}
		want := "vms:\n- max: " + tt.want + "\n  min: " + tt.want + "\n  name: a\n"
		if got := string(p.Canonical()); got != want {
			t.Errorf("Canonical(%d) = %q, want %q", tt.bytes, got, want)
		}
	}
}

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func TestDigestStable(t *testing.T) {
	want := example().Digest()
	if !digestRE.MatchString(want) {
		t.Fatalf("Digest() = %q, want sha256:<64 hex>", want)
	}
	variants := map[string]string{
		"design example with agent settings and comments": exampleFile,
		"canonical form": exampleCanonical,
		"reordered VMs and keys": `
vms:
  - max: 24Gi
    name: windows-node-1
    min: 6Gi
  - fixed: true
    name: linux-node-1
  - {min: 4Gi, name: docker-host-1, max: 32Gi}
`,
		"other quantity spellings": `vms:
- name: windows-node-1
  min: 6144Mi
  max: "25165824Ki"
- name: docker-host-1
  min: 4294967296
  max: 0.03125Ti
- name: linux-node-1
  fixed: true
  min: 0
`,
		"flow style, explicit fixed false, other settings after vms": `{"vms": [{"name": "docker-host-1", "fixed": false, "min": "4Gi", "max": "32Gi"}, {"name": "linux-node-1", "fixed": true}, {"name": "windows-node-1", "min": "6Gi", "max": "24Gi"}], "role": "host"}`,
	}
	for name, doc := range variants {
		t.Run(name, func(t *testing.T) {
			p, err := Parse([]byte(doc))
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			if got := p.Digest(); got != want {
				t.Fatalf("Digest() = %s, want %s\ncanonical:\n%s", got, want, p.Canonical())
			}
		})
	}
	// The proto form, in any order, has the same digest.
	m := example().Proto()
	m.Vms[0], m.Vms[2] = m.Vms[2], m.Vms[0]
	p, err := FromProto(m)
	if err != nil || p.Digest() != want {
		t.Fatalf("FromProto(reordered).Digest() = %s, %v; want %s", p.Digest(), err, want)
	}
}

func TestDigestDistinguishesContent(t *testing.T) {
	base := example()
	mutations := map[string]func(*Policy){
		"min":         func(p *Policy) { p.VMs[1].Min += 2 * mi },
		"max":         func(p *Policy) { p.VMs[1].Max -= 2 * mi },
		"fixed":       func(p *Policy) { p.VMs[1] = VM{Name: p.VMs[1].Name, Fixed: true} },
		"name":        func(p *Policy) { p.VMs[1].Name = "windows-node-2" },
		"removed VM":  func(p *Policy) { p.VMs = p.VMs[:2] },
		"added VM":    func(p *Policy) { p.VMs = append(p.VMs, VM{Name: "z", Fixed: true}) },
		"swap bounds": func(p *Policy) { p.VMs[2].Min, p.VMs[2].Max = 32*gi, 4*gi },
	}
	seen := map[string]string{base.Digest(): "base"}
	for name, mutate := range mutations {
		p := Policy{VMs: append([]VM(nil), base.VMs...)}
		mutate(&p)
		d := p.Digest()
		if prev, ok := seen[d]; ok {
			t.Errorf("%s: digest equals that of %s", name, prev)
		}
		seen[d] = name
	}
}

func TestParseYAMLRoundTrip(t *testing.T) {
	for _, p := range []Policy{
		{},
		example(),
		{VMs: []VM{{Name: "odd", Min: 3*gi + 1, Max: Unbounded}, {Name: "with space", Min: 1000, Max: 1000000000}}},
	} {
		got, err := Parse(p.Canonical())
		if err != nil {
			t.Fatalf("Parse(Canonical()) = %v\n%s", err, p.Canonical())
		}
		if diff := cmp.Diff(Policy{VMs: p.sorted()}, got, cmpopts.EquateEmpty()); diff != "" {
			t.Fatalf("round trip mismatch (-want +got):\n%s", diff)
		}
		if string(got.Canonical()) != string(p.Canonical()) {
			t.Fatalf("Canonical not idempotent:\n%s\n%s", p.Canonical(), got.Canonical())
		}
	}
}

func TestParseErrors(t *testing.T) {
	tests := map[string]struct {
		doc  string
		want string
	}{
		"not YAML":               {doc: "vms: [", want: "invalid policy"},
		"duplicate key":          {doc: "vms: []\nvms: []\n", want: `"vms" already set`},
		"not a mapping":          {doc: "- name: a\n", want: "document must be a mapping"},
		"empty document":         {doc: "", want: `missing key "vms"`},
		"missing vms":            {doc: "role: host\nvm: []\n", want: `missing key "vms"`},
		"vms not a list":         {doc: "vms: {name: a}\n", want: "vms: json: cannot unmarshal"},
		"unknown entry field":    {doc: "vms:\n- name: a\n  fixed: true\n  mx: 1Gi\n", want: `unknown field "mx"`},
		"bad quantity":           {doc: "vms:\n- name: a\n  min: 1Gb\n  max: 2Gi\n", want: "quantities must match"},
		"fractional bytes":       {doc: "vms:\n- name: a\n  min: 1.5\n  max: 2Gi\n", want: `vms[0] "a": 1500m is not a whole number of bytes`},
		"milli bytes":            {doc: "vms:\n- name: a\n  min: 1Gi\n  max: 100m\n", want: "100m is not a whole number of bytes"},
		"writable without max":   {doc: "vms:\n- name: a\n  min: 1Gi\n", want: "writable VMs need min and max"},
		"writable without min":   {doc: "vms:\n- name: a\n  max: 1Gi\n", want: "writable VMs need min and max"},
		"writable without both":  {doc: "vms:\n- name: a\n", want: "writable VMs need min and max"},
		"fixed with bounds":      {doc: "vms:\n- name: a\n  fixed: true\n  max: 1Gi\n", want: "fixed VMs must not set min or max"},
		"max below min":          {doc: "vms:\n- name: a\n  min: 2Gi\n  max: 1Gi\n", want: "max 1Gi is below min 2Gi"},
		"zero min":               {doc: "vms:\n- name: a\n  min: 0\n  max: 1Gi\n", want: "min must be positive"},
		"duplicate VM":           {doc: "vms:\n- {name: a, fixed: true}\n- {name: a, fixed: true}\n", want: "duplicate name"},
		"fixed as string":        {doc: "vms:\n- name: a\n  fixed: \"yes\"\n", want: "cannot unmarshal string"},
		"several errors at once": {doc: "vms:\n- name: a\n- name: b\n  min: 0.5\n  max: 1\n", want: "vms[0] \"a\": writable VMs need min and max (or fixed: true)\nvms[1] \"b\": 500m"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			p, err := Parse([]byte(tt.doc))
			if !errors.Is(err, ErrInvalidPolicy) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse() = %+v, %v; want ErrInvalidPolicy containing %q", p, err, tt.want)
			}
		})
	}
}

func TestParseEmptyPolicies(t *testing.T) {
	for _, doc := range []string{"vms: []\n", "vms:\n", "vms: null\nrole: host\n"} {
		p, err := Parse([]byte(doc))
		if err != nil || len(p.VMs) != 0 {
			t.Errorf("Parse(%q) = %+v, %v; want empty policy", doc, p, err)
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	writeFile(t, path, exampleFile)
	p, err := Load(path)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if p.Digest() != example().Digest() {
		t.Fatalf("Load() = %+v, want the DESIGN example", p)
	}

	if _, err := Load(filepath.Join(dir, "missing.yaml")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load(missing) = %v, want fs.ErrNotExist", err)
	}
	bad := filepath.Join(dir, "bad.yaml")
	writeFile(t, bad, "role: host\n")
	if _, err := Load(bad); !errors.Is(err, ErrInvalidPolicy) || !strings.Contains(err.Error(), bad) {
		t.Fatalf("Load(bad) = %v, want ErrInvalidPolicy naming the file", err)
	}
}

func TestSaveNewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.pending.yaml")
	if err := Save(path, example()); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	if got := readFile(t, path); got != string(example().Canonical()) {
		t.Fatalf("new file =\n%s\nwant the canonical form", got)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
		}
	}
	p, err := Load(path)
	if err != nil || p.Digest() != example().Digest() {
		t.Fatalf("Load(Save(p)) = %+v, %v", p, err)
	}
	assertNoTempFiles(t, filepath.Dir(path))
}

func TestSaveKeepsAgentSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	writeFile(t, path, exampleFile)
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	proposed := Policy{VMs: []VM{{Name: "linux-node-1", Fixed: true}, {Name: "windows-node-1", Min: 8 * gi, Max: 32 * gi}}}
	if err := Save(path, proposed); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	var got map[string]any
	if err := yaml.Unmarshal([]byte(readFile(t, path)), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"role":           "host",
		"listen":         "0.0.0.0:9443",
		"health":         "0.0.0.0:9090",
		"allowedClients": []any{"hvmc-coordinator"},
		"vms": []any{
			map[string]any{"name": "linux-node-1", "fixed": true},
			map[string]any{"name": "windows-node-1", "min": "8Gi", "max": "32Gi"},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("saved file mismatch (-want +got):\n%s", diff)
	}
	p, err := Load(path)
	if err != nil || p.Digest() != proposed.Digest() {
		t.Fatalf("Load(Save(p)) = %+v, %v", p, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(path); st.Mode().Perm() != 0o640 {
			t.Fatalf("mode = %v, want 0640 kept", st.Mode().Perm())
		}
	}
	assertNoTempFiles(t, filepath.Dir(path))
}

func TestSaveEmptyExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.yaml")
	writeFile(t, path, "")
	if err := Save(path, example()); err != nil {
		t.Fatalf("Save() = %v", err)
	}
	if got := readFile(t, path); got != string(example().Canonical()) {
		t.Fatalf("file =\n%s\nwant the canonical form", got)
	}
}

func TestSaveErrors(t *testing.T) {
	dir := t.TempDir()
	if err := Save(filepath.Join(dir, "x.yaml"), Policy{VMs: []VM{{Name: ""}}}); !errors.Is(err, ErrInvalidPolicy) {
		t.Errorf("Save(invalid) = %v, want ErrInvalidPolicy", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.yaml")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Save(invalid) wrote a file: %v", err)
	}

	list := filepath.Join(dir, "list.yaml")
	writeFile(t, list, "- a\n")
	if err := Save(list, example()); err == nil || !strings.Contains(err.Error(), "not a YAML mapping") {
		t.Errorf("Save(over a list) = %v, want not a YAML mapping", err)
	}
	if got := readFile(t, list); got != "- a\n" {
		t.Errorf("Save(over a list) changed the file: %q", got)
	}

	if err := Save(dir, example()); err == nil {
		t.Errorf("Save(directory) = nil, want error")
	}
	if err := Save(filepath.Join(dir, "missing", "agent.yaml"), example()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Save(missing dir) = %v, want fs.ErrNotExist", err)
	}
	assertNoTempFiles(t, dir)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}
