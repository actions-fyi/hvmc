package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matfax/hvmc/api/v1alpha1"
	"github.com/matfax/hvmc/internal/pki"
)

func runToken(t *testing.T, f clientFactory, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newTokenCommand(env{now: func() time.Time { return testNow }}, f)
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

func tokenCluster(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	hc := &v1alpha1.HostConfig{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.HostConfigName, UID: "hc-uid"},
		Spec: v1alpha1.HostConfigSpec{
			Agents: []v1alpha1.AgentSpec{{Name: "host", DNSName: "hyperv-01.example.com"}},
		},
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(hc).Build()
}

func TestTokenCreate(t *testing.T) {
	c := tokenCluster(t)
	var gotKubeconfig, gotNamespace string
	factory := func(kubeconfig, namespace string) (client.Client, string, error) {
		gotKubeconfig, gotNamespace = kubeconfig, namespace
		return c, "hvmc", nil
	}
	out, errOut, err := runToken(t, factory, "create", "--agent", "host", "--kubeconfig", "/tmp/kc", "-n", "hvmc")
	if err != nil {
		t.Fatalf("token create: %v\n%s", err, errOut)
	}
	if gotKubeconfig != "/tmp/kc" || gotNamespace != "hvmc" {
		t.Errorf("factory got kubeconfig %q namespace %q", gotKubeconfig, gotNamespace)
	}
	var s corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "hvmc", Name: "hvmc-bootstrap-host"}, &s); err != nil {
		t.Fatal(err)
	}
	if tok := string(s.Data[pki.BootstrapTokenKey]); out != tok+"\n" || tok == "" {
		t.Fatalf("stdout %q, stored token %q", out, tok)
	}
	for _, want := range []string{"hvmc/hvmc-bootstrap-host", "2026-03-05T05:06:07Z", "kubectl -n hvmc get secret hvmc-bootstrap-host"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	if strings.Contains(errOut, strings.TrimSpace(out)) {
		t.Error("token printed to stderr")
	}

	// A second run replaces the token.
	again, _, err := runToken(t, factory, "create", "--agent", "host")
	if err != nil || again == out {
		t.Fatalf("second token create: %q, %v", again, err)
	}
}

func TestTokenCreateErrors(t *testing.T) {
	c := tokenCluster(t)
	factory := func(string, string) (client.Client, string, error) { return c, "hvmc", nil }
	if _, _, err := runToken(t, factory, "create"); err == nil || !strings.Contains(err.Error(), "--agent is required") {
		t.Fatalf("missing --agent: %v", err)
	}
	if _, _, err := runToken(t, factory, "create", "--agent", "absent"); !errors.Is(err, pki.ErrUnknownAgent) {
		t.Fatalf("unknown agent: %v", err)
	}
	broken := func(string, string) (client.Client, string, error) { return nil, "", errors.New("no kubeconfig") }
	if _, _, err := runToken(t, broken, "create", "--agent", "host"); err == nil || !strings.Contains(err.Error(), "no kubeconfig") {
		t.Fatalf("kubeconfig error: %v", err)
	}
	if _, _, err := runToken(t, factory, "create", "extra"); err == nil {
		t.Fatal("positional argument accepted")
	}
}
