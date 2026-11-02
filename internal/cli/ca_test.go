package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matfax/hvmc/internal/pki"
)

func runCA(t *testing.T, f readerFactory, args ...string) (string, error) {
	t.Helper()
	cmd := newCACommand(f)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestCAExport(t *testing.T) {
	ca, err := pki.GenerateCA()
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "hvmc", Name: pki.CABundleConfigMapName},
		Data:       map[string]string{pki.CABundleKey: string(ca.CertPEM())},
	}
	var gotKubeconfig, gotNamespace string
	factory := func(kubeconfig, namespace string) (client.Reader, string, error) {
		gotKubeconfig, gotNamespace = kubeconfig, namespace
		return fake.NewClientBuilder().WithObjects(cm).Build(), "hvmc", nil
	}

	out, err := runCA(t, factory, "export", "--kubeconfig", "/tmp/kc", "-n", "hvmc")
	if err != nil {
		t.Fatalf("ca export: %v\n%s", err, out)
	}
	if gotKubeconfig != "/tmp/kc" || gotNamespace != "hvmc" {
		t.Errorf("factory got kubeconfig %q namespace %q", gotKubeconfig, gotNamespace)
	}
	cert, err := pki.ParseCertificatePEM([]byte(out))
	if err != nil || !cert.Equal(ca.Certificate()) {
		t.Fatalf("output does not start with the CA certificate: %v\n%s", err, out)
	}
	if want := "SHA-256 fingerprint: " + ca.Fingerprint() + "\n"; !strings.HasSuffix(out, want) {
		t.Fatalf("output does not end with %q:\n%s", want, out)
	}
}

func TestCAExportErrors(t *testing.T) {
	broken := func(string, string) (client.Reader, string, error) { return nil, "", errors.New("no kubeconfig") }
	if _, err := runCA(t, broken, "export"); err == nil || !strings.Contains(err.Error(), "no kubeconfig") {
		t.Fatalf("error = %v, want kubeconfig error", err)
	}
	empty := func(string, string) (client.Reader, string, error) {
		return fake.NewClientBuilder().Build(), "hvmc", nil
	}
	if _, err := runCA(t, empty, "export", "--secret", "operator-ca"); err == nil || !strings.Contains(err.Error(), "operator-ca") {
		t.Fatalf("error = %v, want missing Secret operator-ca", err)
	}
	if _, err := runCA(t, empty, "export", "extra"); err == nil {
		t.Fatal("positional argument accepted")
	}
}

func TestKubeReaderMissingKubeconfig(t *testing.T) {
	t.Setenv("KUBECONFIG", "")
	t.Setenv("HOME", t.TempDir())
	if _, _, err := kubeReader(t.TempDir()+"/absent", ""); err == nil {
		t.Fatal("kubeReader succeeded without a kubeconfig")
	}
}
