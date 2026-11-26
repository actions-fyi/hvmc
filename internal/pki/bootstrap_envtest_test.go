// envtest runs in the Linux CI job only (see envtest_test.go).

//go:build !windows

package pki_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matfax/hvmc/api/v1alpha1"
	"github.com/matfax/hvmc/internal/pki"
)

// envHostConfig gives HostConfig `default` the spec of enrollHostConfig
// and returns it.
func envHostConfig(t *testing.T) *v1alpha1.HostConfig {
	t.Helper()
	hc := ensureHostConfig(t)
	hc.Spec.Agents = enrollHostConfig().Spec.Agents
	hc.Spec.VMs = enrollHostConfig().Spec.VMs
	if err := k8sClient.Update(context.Background(), hc); err != nil {
		t.Fatalf("update HostConfig: %v", err)
	}
	t.Cleanup(func() {
		var cur v1alpha1.HostConfig
		if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: v1alpha1.HostConfigName}, &cur); err != nil {
			return
		}
		cur.Spec.Agents, cur.Spec.VMs = nil, nil
		_ = k8sClient.Update(context.Background(), &cur)
	})
	return hc
}

func envSecret(t *testing.T, ns, agentName string) *corev1.Secret {
	t.Helper()
	return getSecret(t, ns, pki.BootstrapSecretName(agentName))
}

func TestEnvBootstrapTokenLifecycle(t *testing.T) {
	ctx, ns, _ := setup(t)
	hc := envHostConfig(t)
	clk := newFakeClock()
	b := &pki.BootstrapTokens{Client: k8sClient, Namespace: ns, Clock: clk}

	// Created for every un-enrolled agent, owned by HostConfig.
	reconcile(t, b, hc)
	tokens := envTokens(t, ns, hc)

	// Re-created after expiry.
	clk.Advance(pki.BootstrapTokenValidity)
	reconcile(t, b, hc)
	for _, a := range allAgents {
		if got := string(envSecret(t, ns, a).Data[pki.BootstrapTokenKey]); got == tokens[a] || got == "" {
			t.Fatalf("%s: token not replaced after expiry", a)
		}
	}

	// Marked after enrollment, and left alone by later reconciles.
	h := &pki.EnrollHandler{Client: k8sClient, Namespace: ns, CA: newTestCA(t, clk), Clock: clk}
	tok := string(envSecret(t, ns, guestAgent).Data[pki.BootstrapTokenKey])
	body := requestBody(t, guestAgent, pki.EnrollRoleGuest, csrPEM(t, newKey(t), guestAgent, nil, nil))
	if code := envPost(t, h, tok, body); code != http.StatusOK {
		t.Fatalf("enroll: status %d", code)
	}
	if code := envPost(t, h, tok, body); code != http.StatusUnauthorized {
		t.Fatalf("reuse: status %d, want 401", code)
	}
	clk.Advance(3 * pki.BootstrapTokenValidity)
	reconcile(t, b, hc)
	requireEnvEnrolled(t, ns, hc)

	// Deleted along with HostConfig. envtest runs no garbage collector, so
	// the owner reference checked above is what removes the Secrets in a
	// cluster; the reconciler, called without a HostConfig, removes them
	// as well.
	if err := k8sClient.Delete(ctx, hc); err != nil {
		t.Fatal(err)
	}
	reconcile(t, b, nil)
	requireNoEnvBootstrapSecrets(t, ns)
}

func envPost(t *testing.T, h http.Handler, tok, body string) int {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, pki.EnrollPath, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// envTokens returns every agent's token after checking its Secret.
func envTokens(t *testing.T, ns string, hc *v1alpha1.HostConfig) map[string]string {
	t.Helper()
	tokens := map[string]string{}
	for _, a := range allAgents {
		s := envSecret(t, ns, a)
		requireOwnedByHostConfig(t, s, hc)
		tokens[a] = string(s.Data[pki.BootstrapTokenKey])
		if len(tokens[a]) < 43 {
			t.Fatalf("%s: token %q too short", a, tokens[a])
		}
	}
	return tokens
}

func requireEnvEnrolled(t *testing.T, ns string, hc *v1alpha1.HostConfig) {
	t.Helper()
	s := envSecret(t, ns, guestAgent)
	if _, ok := s.Data[pki.BootstrapTokenKey]; ok {
		t.Fatal("enrolled agent has a token")
	}
	if _, ok := s.Annotations[pki.BootstrapEnrolledAnnotation]; !ok {
		t.Fatalf("enrolled agent not marked: %v", s.Annotations)
	}
	requireOwnedByHostConfig(t, s, hc)
}

func requireNoEnvBootstrapSecrets(t *testing.T, ns string) {
	t.Helper()
	var list corev1.SecretList
	if err := k8sClient.List(context.Background(), &list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	for _, s := range list.Items {
		if strings.HasPrefix(s.Name, pki.BootstrapSecretPrefix) {
			t.Errorf("bootstrap Secret %s left after HostConfig deletion", s.Name)
		}
	}
}
