package pki_test

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matfax/hvmc/api/v1alpha1"
	"github.com/matfax/hvmc/internal/pki"
)

const testNS = "hvmc"

// The agents of enrollHostConfig.
const (
	hostAgent  = "host"
	guestAgent = "docker-host-1"
	nodeAgent  = "windows-node-1"
	nodeName   = "windows-node-1"
	nodeIP     = "192.0.2.10"
)

var allAgents = []string{hostAgent, guestAgent, nodeAgent}

// enrollHostConfig declares a host agent, a facade-only VM with a guest
// agent DNS name and a node-mapped VM.
func enrollHostConfig() *v1alpha1.HostConfig {
	return &v1alpha1.HostConfig{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.HostConfigName},
		Spec: v1alpha1.HostConfigSpec{
			Agents: []v1alpha1.AgentSpec{{Name: hostAgent, Role: v1alpha1.AgentRoleHost, DNSName: "hyperv-01.example.com", Port: 9443}},
			VMs: []v1alpha1.VMSpec{
				{
					Name: guestAgent, GuestOS: v1alpha1.GuestOSWindows, Mode: v1alpha1.VMModeStatic,
					Base:       resource.MustParse("3Gi"),
					GuestAgent: &v1alpha1.GuestAgentSpec{DNSName: "docker-host-1.example.com", Port: 9444},
				},
				{
					Name: nodeAgent, Node: nodeName, GuestOS: v1alpha1.GuestOSWindows, Mode: v1alpha1.VMModeStatic,
					Base: resource.MustParse("4Gi"),
				},
			},
		},
	}
}

func testScheme(t *testing.T) *k8sruntime.Scheme {
	t.Helper()
	s := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// fakeCluster returns a fake client holding hc (with a UID) and the node
// of nodeAgent.
func fakeCluster(t *testing.T, hc *v1alpha1.HostConfig) client.Client {
	t.Helper()
	objs := []client.Object{node(nodeName, addr(corev1.NodeInternalIP, nodeIP))}
	if hc != nil {
		hc.UID = "hostconfig-uid"
		objs = append(objs, hc)
	}
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
}

func bootstrapSecret(t *testing.T, c client.Client, agent string) *corev1.Secret {
	t.Helper()
	var s corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: pki.BootstrapSecretName(agent)}, &s); err != nil {
		t.Fatalf("get bootstrap Secret of %s: %v", agent, err)
	}
	return &s
}

func token(t *testing.T, c client.Client, agent string) string {
	t.Helper()
	return string(bootstrapSecret(t, c, agent).Data[pki.BootstrapTokenKey])
}

func reconcile(t *testing.T, b *pki.BootstrapTokens, hc *v1alpha1.HostConfig) time.Time {
	t.Helper()
	next, err := b.Reconcile(context.Background(), hc)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return next
}

func TestNewBootstrapToken(t *testing.T) {
	seen := map[string]bool{}
	for range 16 {
		tok, err := pki.NewBootstrapToken()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil || len(raw) < 32 {
			t.Fatalf("token %q: %d bytes, %v; want ≥ 256 bits", tok, len(raw), err)
		}
		if seen[tok] {
			t.Fatalf("token %q repeated", tok)
		}
		seen[tok] = true
	}
}

func TestReconcileCreatesTokens(t *testing.T) {
	hc := enrollHostConfig()
	c := fakeCluster(t, hc)
	clk := newFakeClock()
	b := &pki.BootstrapTokens{Client: c, Namespace: testNS, Clock: clk}

	next := reconcile(t, b, hc)
	if want := t0.Add(pki.BootstrapTokenValidity); !next.Equal(want) {
		t.Errorf("next expiry = %v, want %v", next, want)
	}
	tokens := map[string]bool{}
	for _, a := range allAgents {
		s := bootstrapSecret(t, c, a)
		tok := string(s.Data[pki.BootstrapTokenKey])
		if tok == "" || tokens[tok] {
			t.Fatalf("%s: token %q empty or shared", a, tok)
		}
		tokens[tok] = true
		if s.Labels[pki.BootstrapAgentLabel] != a {
			t.Errorf("%s: labels %v", a, s.Labels)
		}
		if got := s.Annotations[pki.BootstrapExpiresAnnotation]; got != "2026-11-03T12:00:00Z" {
			t.Errorf("%s: expiry %q", a, got)
		}
		if refs := s.OwnerReferences; len(refs) != 1 || refs[0].Kind != "HostConfig" || refs[0].UID != hc.UID {
			t.Errorf("%s: owner references %+v", a, refs)
		}
	}

	// Unchanged before expiry.
	before := token(t, c, hostAgent)
	clk.Advance(pki.BootstrapTokenValidity - time.Second)
	reconcile(t, b, hc)
	if token(t, c, hostAgent) != before {
		t.Fatal("token replaced before expiry")
	}
}

func TestReconcileReplacesExpiredToken(t *testing.T) {
	hc := enrollHostConfig()
	c := fakeCluster(t, hc)
	clk := newFakeClock()
	b := &pki.BootstrapTokens{Client: c, Namespace: testNS, Clock: clk}
	reconcile(t, b, hc)
	old := token(t, c, guestAgent)

	clk.Advance(pki.BootstrapTokenValidity)
	next := reconcile(t, b, hc)
	if got := token(t, c, guestAgent); got == old || got == "" {
		t.Fatalf("expired token not replaced: %q", got)
	}
	if want := t0.Add(2 * pki.BootstrapTokenValidity); !next.Equal(want) {
		t.Errorf("next expiry = %v, want %v", next, want)
	}
	// An unreadable expiry counts as expired.
	s := bootstrapSecret(t, c, guestAgent)
	s.Annotations[pki.BootstrapExpiresAnnotation] = "soon"
	if err := c.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	old = token(t, c, guestAgent)
	reconcile(t, b, hc)
	if token(t, c, guestAgent) == old {
		t.Fatal("token with unreadable expiry not replaced")
	}
}

func TestReconcileKeepsEnrolledAndPrunes(t *testing.T) {
	hc := enrollHostConfig()
	c := fakeCluster(t, hc)
	clk := newFakeClock()
	b := &pki.BootstrapTokens{Client: c, Namespace: testNS, Clock: clk}
	reconcile(t, b, hc)

	s := bootstrapSecret(t, c, hostAgent)
	delete(s.Data, pki.BootstrapTokenKey)
	s.Annotations[pki.BootstrapEnrolledAnnotation] = t0.Format(time.RFC3339)
	s.OwnerReferences = nil
	if err := c.Update(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	clk.Advance(3 * pki.BootstrapTokenValidity)
	reconcile(t, b, hc)
	s = bootstrapSecret(t, c, hostAgent)
	if _, ok := s.Data[pki.BootstrapTokenKey]; ok {
		t.Fatal("enrolled agent got a new token")
	}
	if len(s.OwnerReferences) != 1 {
		t.Fatalf("owner reference not restored: %+v", s.OwnerReferences)
	}

	// Removing a VM deletes its Secret; a nil HostConfig deletes all.
	hc.Spec.VMs = hc.Spec.VMs[:1]
	reconcile(t, b, hc)
	assertNoSecret(t, c, nodeAgent)
	bootstrapSecret(t, c, guestAgent)
	if next := reconcile(t, b, nil); !next.IsZero() {
		t.Errorf("next = %v with no HostConfig", next)
	}
	for _, a := range allAgents {
		assertNoSecret(t, c, a)
	}
}

func assertNoSecret(t *testing.T, c client.Client, agent string) {
	t.Helper()
	var s corev1.Secret
	err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: pki.BootstrapSecretName(agent)}, &s)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("bootstrap Secret of %s: err = %v, want NotFound", agent, err)
	}
}

func TestReissue(t *testing.T) {
	hc := enrollHostConfig()
	c := fakeCluster(t, hc)
	clk := newFakeClock()
	b := &pki.BootstrapTokens{Client: c, Namespace: testNS, Clock: clk}
	ctx := context.Background()

	// Without a Secret, Reissue creates it.
	tok, exp, err := b.Reissue(ctx, nodeAgent)
	if err != nil {
		t.Fatal(err)
	}
	if tok == "" || tok != token(t, c, nodeAgent) || !exp.Equal(t0.Add(pki.BootstrapTokenValidity)) {
		t.Fatalf("Reissue = %q, %v", tok, exp)
	}
	if refs := bootstrapSecret(t, c, nodeAgent).OwnerReferences; len(refs) != 1 || refs[0].UID != hc.UID {
		t.Fatalf("owner references %+v", refs)
	}

	// For an enrolled agent, Reissue clears the mark.
	s := bootstrapSecret(t, c, nodeAgent)
	delete(s.Data, pki.BootstrapTokenKey)
	s.Annotations[pki.BootstrapEnrolledAnnotation] = t0.Format(time.RFC3339)
	if err := c.Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Hour)
	tok2, _, err := b.Reissue(ctx, nodeAgent)
	if err != nil {
		t.Fatal(err)
	}
	s = bootstrapSecret(t, c, nodeAgent)
	if tok2 == tok || string(s.Data[pki.BootstrapTokenKey]) != tok2 {
		t.Fatalf("token not replaced")
	}
	if _, ok := s.Annotations[pki.BootstrapEnrolledAnnotation]; ok {
		t.Fatal("enrolled mark kept")
	}

	if _, _, err := b.Reissue(ctx, "absent"); !errors.Is(err, pki.ErrUnknownAgent) {
		t.Fatalf("unknown agent: err = %v", err)
	}
	none := &pki.BootstrapTokens{Client: fakeCluster(t, nil), Namespace: testNS}
	if _, _, err := none.Reissue(ctx, hostAgent); !apierrors.IsNotFound(errors.Unwrap(err)) {
		t.Fatalf("no HostConfig: err = %v", err)
	}
}
