// envtest runs in the Linux CI job only (DESIGN §12); controller-runtime's
// envtest process package does not build for Windows.

//go:build !windows

package pki_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/matfax/hvmc/api/v1alpha1"
	"github.com/matfax/hvmc/internal/pki"
)

// k8sClient talks to the envtest API server started by TestMain; nil when
// KUBEBUILDER_ASSETS is not set.
var k8sClient client.Client

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		return m.Run() // envtest tests skip or fail in requireEnvtest
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest: %v\n", err)
		return 1
	}
	defer func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
		}
	}()
	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "scheme: %v\n", err)
		return 1
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "scheme: %v\n", err)
		return 1
	}
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "client: %v\n", err)
		return 1
	}
	return m.Run()
}

// requireEnvtest skips the test when no envtest binaries are available,
// except in CI (CI=true), where envtest is mandatory; `make test` provides
// the binaries.
func requireEnvtest(t *testing.T) {
	t.Helper()
	if k8sClient != nil {
		return
	}
	msg := "KUBEBUILDER_ASSETS is not set; run `make test` (or `make envtest`) to download the envtest binaries"
	if os.Getenv("CI") == "true" {
		t.Fatal(msg)
	}
	t.Skip(msg)
}

// setup returns a fresh namespace and HostConfig `default` (created if
// absent).
func setup(t *testing.T) (context.Context, string, *v1alpha1.HostConfig) {
	t.Helper()
	requireEnvtest(t)
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "pki-"}}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	return ctx, ns.Name, ensureHostConfig(t)
}

func ensureHostConfig(t *testing.T) *v1alpha1.HostConfig {
	t.Helper()
	hc := &v1alpha1.HostConfig{ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.HostConfigName}}
	err := k8sClient.Create(context.Background(), hc)
	if apierrors.IsAlreadyExists(err) {
		err = k8sClient.Get(context.Background(), types.NamespacedName{Name: hc.Name}, hc)
	}
	if err != nil {
		t.Fatal(err)
	}
	return hc
}

func requireOwnedByHostConfig(t *testing.T, obj metav1.Object, hc *v1alpha1.HostConfig) {
	t.Helper()
	refs := obj.GetOwnerReferences()
	if len(refs) != 1 {
		t.Fatalf("%s: owner references = %+v, want exactly HostConfig default", obj.GetName(), refs)
	}
	r := refs[0]
	if r.APIVersion != "hvmc.fax.fyi/v1alpha1" || r.Kind != "HostConfig" || r.Name != "default" || r.UID != hc.UID {
		t.Fatalf("%s: owner reference = %+v, want HostConfig default uid %s", obj.GetName(), r, hc.UID)
	}
	if r.BlockOwnerDeletion != nil && *r.BlockOwnerDeletion {
		t.Errorf("%s: owner reference blocks owner deletion", obj.GetName())
	}
}

func getSecret(t *testing.T, ns, name string) *corev1.Secret {
	t.Helper()
	var s corev1.Secret
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &s); err != nil {
		t.Fatal(err)
	}
	return &s
}

func TestEnvCACreatedOnceAndOwned(t *testing.T) {
	ctx, ns, hc := setup(t)
	cfg := pki.StoreConfig{Namespace: ns}

	// Two replicas start at once: both end up with the same CA.
	const replicas = 4
	cas := make([]*pki.CA, replicas)
	errs := make([]error, replicas)
	var wg sync.WaitGroup
	for i := range replicas {
		wg.Go(func() { cas[i], errs[i] = pki.LoadOrCreateCA(ctx, k8sClient, cfg) })
	}
	wg.Wait()
	for i := range replicas {
		if errs[i] != nil {
			t.Fatalf("replica %d: %v", i, errs[i])
		}
		if cas[i].Fingerprint() != cas[0].Fingerprint() {
			t.Fatalf("replica %d loaded a different CA", i)
		}
	}

	s := getSecret(t, ns, pki.CASecretName)
	requireOwnedByHostConfig(t, s, hc)
	if s.Type != corev1.SecretTypeTLS {
		t.Errorf("Secret type = %s, want %s", s.Type, corev1.SecretTypeTLS)
	}
	stored, err := pki.ParseCertificatePEM(s.Data[corev1.TLSCertKey])
	if err != nil || pki.Fingerprint(stored) != cas[0].Fingerprint() {
		t.Fatalf("stored certificate differs from the loaded CA: %v", err)
	}

	again, err := pki.LoadOrCreateCA(ctx, k8sClient, cfg)
	if err != nil || again.Fingerprint() != cas[0].Fingerprint() {
		t.Fatalf("restart did not load the stored CA: %v", err)
	}
}

func TestEnvExistingCASecret(t *testing.T) {
	ctx, ns, _ := setup(t)
	operatorCA := newTestCA(t, pki.SystemClock)
	keyPEM, err := operatorCA.KeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "operator-ca"},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{corev1.TLSCertKey: operatorCA.CertPEM(), corev1.TLSPrivateKeyKey: keyPEM},
	}
	if err := k8sClient.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}

	ca, err := pki.LoadOrCreateCA(ctx, k8sClient, pki.StoreConfig{Namespace: ns, ExistingSecret: "operator-ca"})
	if err != nil {
		t.Fatal(err)
	}
	if ca.Fingerprint() != operatorCA.Fingerprint() {
		t.Fatal("did not load the operator's CA")
	}
	s := getSecret(t, ns, "operator-ca")
	if len(s.OwnerReferences) != 0 || s.ResourceVersion != secret.ResourceVersion {
		t.Fatalf("operator Secret was modified: owners %+v, resourceVersion %s → %s", s.OwnerReferences, secret.ResourceVersion, s.ResourceVersion)
	}
	assertNotFound(t, ns, pki.CASecretName, &corev1.Secret{})

	_, err = pki.LoadOrCreateCA(ctx, k8sClient, pki.StoreConfig{Namespace: ns, ExistingSecret: "absent"})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("missing existing Secret: error %v, want NotFound", err)
	}
	assertNotFound(t, ns, "absent", &corev1.Secret{})
	assertNotFound(t, ns, pki.CASecretName, &corev1.Secret{})
}

func TestEnvCorruptCASecretIsNotReplaced(t *testing.T) {
	ctx, ns, _ := setup(t)
	bad := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: pki.CASecretName},
		Data:       map[string][]byte{corev1.TLSCertKey: []byte("garbage")},
	}
	if err := k8sClient.Create(ctx, bad); err != nil {
		t.Fatal(err)
	}
	if _, err := pki.LoadOrCreateCA(ctx, k8sClient, pki.StoreConfig{Namespace: ns}); err == nil {
		t.Fatal("loaded a corrupt CA Secret")
	}
	if s := getSecret(t, ns, pki.CASecretName); string(s.Data[corev1.TLSCertKey]) != "garbage" {
		t.Fatal("corrupt CA Secret was overwritten")
	}
}

func TestEnvCARequiresHostConfig(t *testing.T) {
	ctx, ns, hc := setup(t)
	if err := k8sClient.Delete(ctx, hc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ensureHostConfig(t) })
	_, err := pki.LoadOrCreateCA(ctx, k8sClient, pki.StoreConfig{Namespace: ns})
	if err == nil || !strings.Contains(err.Error(), "HostConfig") {
		t.Fatalf("error = %v, want missing HostConfig", err)
	}
	assertNotFound(t, ns, pki.CASecretName, &corev1.Secret{})
	if err := pki.PublishCABundle(ctx, k8sClient, ns, []byte("x")); err == nil {
		t.Fatal("published the CA bundle without HostConfig")
	}
}

func assertNotFound(t *testing.T, ns, name string, obj client.Object) {
	t.Helper()
	err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, obj)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("%s/%s: error %v, want NotFound", ns, name, err)
	}
}

func getBundle(t *testing.T, ns string) *corev1.ConfigMap {
	t.Helper()
	var cm corev1.ConfigMap
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: pki.CABundleConfigMapName}, &cm); err != nil {
		t.Fatal(err)
	}
	return &cm
}

func TestEnvPublishCABundle(t *testing.T) {
	ctx, ns, hc := setup(t)
	first := newTestCA(t, pki.SystemClock)

	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Go(func() { errs[i] = pki.PublishCABundle(ctx, k8sClient, ns, first.CertPEM()) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("publisher %d: %v", i, err)
		}
	}
	cm := getBundle(t, ns)
	requireOwnedByHostConfig(t, cm, hc)
	if cm.Data[pki.CABundleKey] != string(first.CertPEM()) {
		t.Fatal("ConfigMap does not hold the CA certificate")
	}

	// Unchanged content does not write.
	if err := pki.PublishCABundle(ctx, k8sClient, ns, first.CertPEM()); err != nil {
		t.Fatal(err)
	}
	if again := getBundle(t, ns); again.ResourceVersion != cm.ResourceVersion {
		t.Error("republishing the same CA updated the ConfigMap")
	}

	second := newTestCA(t, pki.SystemClock)
	if err := pki.PublishCABundle(ctx, k8sClient, ns, second.CertPEM()); err != nil {
		t.Fatal(err)
	}
	cm = getBundle(t, ns)
	requireOwnedByHostConfig(t, cm, hc)
	if cm.Data[pki.CABundleKey] != string(second.CertPEM()) {
		t.Fatal("ConfigMap was not updated to the new CA")
	}
}

func webhookConfig(name string) *admissionregistrationv1.MutatingWebhookConfiguration {
	none := admissionregistrationv1.SideEffectClassNone
	hook := func(n string) admissionregistrationv1.MutatingWebhook {
		return admissionregistrationv1.MutatingWebhook{
			Name: n,
			ClientConfig: admissionregistrationv1.WebhookClientConfig{
				Service: &admissionregistrationv1.ServiceReference{Namespace: "hvmc", Name: pki.WebhookServiceName},
			},
			AdmissionReviewVersions: []string{"v1"},
			SideEffects:             &none,
		}
	}
	return &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Webhooks:   []admissionregistrationv1.MutatingWebhook{hook("pods.hvmc.fax.fyi"), hook("other.hvmc.fax.fyi")},
	}
}

func TestEnvInjectWebhookCABundle(t *testing.T) {
	ctx, _, _ := setup(t)
	name := "hvmc-test-" + strings.ToLower(t.Name())
	cfg := webhookConfig(name)
	if err := k8sClient.Create(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), cfg) })
	ca := newTestCA(t, pki.SystemClock)

	if err := pki.InjectWebhookCABundle(ctx, k8sClient, name, ca.CertPEM()); err != nil {
		t.Fatal(err)
	}
	var got admissionregistrationv1.MutatingWebhookConfiguration
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &got); err != nil {
		t.Fatal(err)
	}
	for _, w := range got.Webhooks {
		if string(w.ClientConfig.CABundle) != string(ca.CertPEM()) {
			t.Errorf("webhook %s: caBundle not injected", w.Name)
		}
		if w.ClientConfig.Service == nil || w.ClientConfig.Service.Name != pki.WebhookServiceName {
			t.Errorf("webhook %s: clientConfig.service lost", w.Name)
		}
	}

	if err := pki.InjectWebhookCABundle(ctx, k8sClient, name, ca.CertPEM()); err != nil {
		t.Fatal(err)
	}
	var again admissionregistrationv1.MutatingWebhookConfiguration
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, &again); err != nil {
		t.Fatal(err)
	}
	if again.ResourceVersion != got.ResourceVersion {
		t.Error("injecting the same caBundle patched the object")
	}

	err := pki.InjectWebhookCABundle(ctx, k8sClient, "absent", ca.CertPEM())
	if !apierrors.IsNotFound(err) {
		t.Fatalf("absent configuration: error %v, want NotFound", err)
	}
}

func TestEnvReadCACertificate(t *testing.T) {
	ctx, ns, _ := setup(t)
	if _, err := pki.ReadCACertificate(ctx, k8sClient, ns, ""); !apierrors.IsNotFound(err) {
		t.Fatalf("nothing published: error %v, want NotFound", err)
	}

	ca, err := pki.LoadOrCreateCA(ctx, k8sClient, pki.StoreConfig{Namespace: ns})
	if err != nil {
		t.Fatal(err)
	}
	fromSecret, err := pki.ReadCACertificate(ctx, k8sClient, ns, "")
	if err != nil || pki.Fingerprint(fromSecret) != ca.Fingerprint() {
		t.Fatalf("Secret fallback: %v", err)
	}

	// Once published, the ConfigMap is the source (no Secret access needed).
	other := newTestCA(t, pki.SystemClock)
	if err := pki.PublishCABundle(ctx, k8sClient, ns, other.CertPEM()); err != nil {
		t.Fatal(err)
	}
	fromCM, err := pki.ReadCACertificate(ctx, k8sClient, ns, "")
	if err != nil || pki.Fingerprint(fromCM) != other.Fingerprint() {
		t.Fatalf("ConfigMap source: %v", err)
	}
}
