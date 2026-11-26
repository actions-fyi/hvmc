package pki_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matfax/hvmc/internal/agent"
	"github.com/matfax/hvmc/internal/pki"
	"github.com/matfax/hvmc/internal/policy"
)

// enrollFixture is an EnrollHandler on a fake cluster with tokens for
// every agent of enrollHostConfig.
type enrollFixture struct {
	t      *testing.T
	c      client.Client
	clk    *fakeClock
	ca     *pki.CA
	h      *pki.EnrollHandler
	logBuf *syncBuffer
}

func newEnrollFixture(t *testing.T) *enrollFixture {
	t.Helper()
	hc := enrollHostConfig()
	c := fakeCluster(t, hc)
	clk := newFakeClock()
	reconcile(t, &pki.BootstrapTokens{Client: c, Namespace: testNS, Clock: clk}, hc)
	ca := newTestCA(t, clk)
	logBuf := &syncBuffer{}
	return &enrollFixture{
		t: t, c: c, clk: clk, ca: ca, logBuf: logBuf,
		h: &pki.EnrollHandler{
			Client: c, Namespace: testNS, CA: ca, Clock: clk,
			Log: slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		},
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// csrPEM returns a PEM CSR for key with subject CN cn and the given SANs.
func csrPEM(t *testing.T, key crypto.Signer, cn string, dns []string, ips []net.IP) string {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: cn}, DNSNames: dns, IPAddresses: ips,
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func requestBody(t *testing.T, agentName, role, csr string) string {
	t.Helper()
	b, err := json.Marshal(pki.EnrollRequest{Agent: agentName, Role: role, CSR: csr})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// post sends a request to the handler and returns the status and body.
func (f *enrollFixture) post(tok, body string) (int, []byte) {
	f.t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, pki.EnrollPath, strings.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	res := rec.Result()
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		f.t.Errorf("Content-Type = %q", ct)
	}
	return res.StatusCode, raw
}

// enroll posts a valid request for an agent.
func (f *enrollFixture) enroll(agentName, role string) (int, []byte) {
	f.t.Helper()
	return f.post(token(f.t, f.c, agentName), requestBody(f.t, agentName, role, csrPEM(f.t, newKey(f.t), agentName, nil, nil)))
}

func decodeResponse(t *testing.T, raw []byte) *pki.EnrollResponse {
	t.Helper()
	var r pki.EnrollResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("decode response %s: %v", raw, err)
	}
	return &r
}

func TestEnrollHandlerHost(t *testing.T) {
	f := newEnrollFixture(t)
	tok := token(t, f.c, hostAgent)
	key := newKey(t)
	code, raw := f.post(tok, requestBody(t, hostAgent, pki.EnrollRoleHost, csrPEM(t, key, hostAgent, nil, nil)))
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	r := decodeResponse(t, raw)
	cert, err := pki.ParseCertificatePEM([]byte(r.Certificate))
	if err != nil {
		t.Fatal(err)
	}
	verifyChain(t, f.ca, cert, t0, x509.ExtKeyUsageServerAuth)
	if !key.PublicKey.Equal(cert.PublicKey) {
		t.Fatal("certificate is not for the CSR's key")
	}
	requireDNSNamesOnly(t, cert, []string{"hyperv-01.example.com"})
	if r.CACertificate != string(f.ca.CertPEM()) {
		t.Error("caCertificate is not the CA")
	}
	if diff := cmp.Diff(pki.AgentSettings{Port: 9443}, r.Settings); diff != "" {
		t.Errorf("settings (-want +got):\n%s", diff)
	}
	want := string(policy.FromHostConfig(enrollHostConfig()).Canonical())
	if r.Policy != want || !strings.Contains(r.Policy, guestAgent) {
		t.Errorf("policy = %q, want %q", r.Policy, want)
	}

	// The token is consumed and the agent marked enrolled.
	s := bootstrapSecret(t, f.c, hostAgent)
	if _, ok := s.Data[pki.BootstrapTokenKey]; ok {
		t.Error("token still stored after enrollment")
	}
	if got := s.Annotations[pki.BootstrapEnrolledAnnotation]; got != t0.Format(time.RFC3339) {
		t.Errorf("enrolled-at = %q", got)
	}
	if strings.Contains(f.logBuf.String(), tok) {
		t.Fatal("token appears in the log")
	}
}

func TestEnrollHandlerGuest(t *testing.T) {
	f := newEnrollFixture(t)
	code, raw := f.enroll(nodeAgent, pki.EnrollRoleGuest)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	r := decodeResponse(t, raw)
	cert, err := pki.ParseCertificatePEM([]byte(r.Certificate))
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{nodeName}, cert.DNSNames); diff != "" {
		t.Errorf("DNS SANs (-want +got):\n%s", diff)
	}
	if len(cert.IPAddresses) != 1 || cert.IPAddresses[0].String() != nodeIP {
		t.Errorf("IP SANs = %v, want %s", cert.IPAddresses, nodeIP)
	}
	if r.Policy != "" || !cmp.Equal(r.Settings, pki.AgentSettings{}) {
		t.Errorf("guest got policy %q settings %+v", r.Policy, r.Settings)
	}

	// A guest may ask for the SANs HostConfig lists for it.
	code, raw = f.post(token(t, f.c, guestAgent), requestBody(t, guestAgent, pki.EnrollRoleGuest,
		csrPEM(t, newKey(t), guestAgent, []string{"docker-host-1.example.com"}, nil)))
	if code != http.StatusOK {
		t.Fatalf("listed SAN: status %d: %s", code, raw)
	}
	if got := decodeResponse(t, raw).Settings; got.Port != 9444 {
		t.Errorf("settings = %+v, want port 9444", got)
	}
}

func TestEnrollHandlerForbidden(t *testing.T) {
	cases := []struct {
		name, agent, role, cn string
		dns                   []string
		ips                   []net.IP
	}{
		{name: "wrong role", agent: hostAgent, role: pki.EnrollRoleGuest, cn: hostAgent},
		{name: "guest as host", agent: guestAgent, role: pki.EnrollRoleHost, cn: guestAgent},
		{name: "wrong CN", agent: guestAgent, role: pki.EnrollRoleGuest, cn: hostAgent},
		{name: "wrong SAN", agent: guestAgent, role: pki.EnrollRoleGuest, cn: guestAgent, dns: []string{"evil.example.com"}},
		{
			name: "extra SAN", agent: nodeAgent, role: pki.EnrollRoleGuest, cn: nodeAgent,
			dns: []string{nodeName, "evil.example.com"},
		},
		{
			name: "extra IP SAN", agent: nodeAgent, role: pki.EnrollRoleGuest, cn: nodeAgent,
			ips: []net.IP{net.ParseIP(nodeIP), net.ParseIP("198.51.100.1")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEnrollFixture(t)
			tok := token(t, f.c, tc.agent)
			code, raw := f.post(tok, requestBody(t, tc.agent, tc.role, csrPEM(t, newKey(t), tc.cn, tc.dns, tc.ips)))
			if code != http.StatusForbidden {
				t.Fatalf("status %d, want 403: %s", code, raw)
			}
			requireErrorBody(t, raw)
			if token(t, f.c, tc.agent) != tok {
				t.Fatal("a refused request consumed the token")
			}
		})
	}
}

func requireErrorBody(t *testing.T, raw []byte) {
	t.Helper()
	var e pki.EnrollErrorResponse
	if err := json.Unmarshal(raw, &e); err != nil || e.Error == "" {
		t.Fatalf("error body %q: %v", raw, err)
	}
}

func TestEnrollHandlerTokenRefused(t *testing.T) {
	t.Run("reused", func(t *testing.T) {
		f := newEnrollFixture(t)
		tok := token(t, f.c, guestAgent)
		body := requestBody(t, guestAgent, pki.EnrollRoleGuest, csrPEM(t, newKey(t), guestAgent, nil, nil))
		if code, raw := f.post(tok, body); code != http.StatusOK {
			t.Fatalf("first use: %d %s", code, raw)
		}
		code, raw := f.post(tok, body)
		requireStatus(t, code, raw, http.StatusUnauthorized)
	})
	t.Run("expired", func(t *testing.T) {
		f := newEnrollFixture(t)
		tok := token(t, f.c, guestAgent)
		f.clk.Advance(pki.BootstrapTokenValidity)
		code, raw := f.post(tok, requestBody(t, guestAgent, pki.EnrollRoleGuest, csrPEM(t, newKey(t), guestAgent, nil, nil)))
		requireStatus(t, code, raw, http.StatusUnauthorized)
	})
	t.Run("wrong", func(t *testing.T) {
		f := newEnrollFixture(t)
		// Another agent's valid token does not work either.
		code, raw := f.post(token(t, f.c, hostAgent), requestBody(t, guestAgent, pki.EnrollRoleGuest, csrPEM(t, newKey(t), guestAgent, nil, nil)))
		requireStatus(t, code, raw, http.StatusUnauthorized)
	})
	t.Run("missing", func(t *testing.T) {
		f := newEnrollFixture(t)
		code, raw := f.post("", requestBody(t, guestAgent, pki.EnrollRoleGuest, csrPEM(t, newKey(t), guestAgent, nil, nil)))
		requireStatus(t, code, raw, http.StatusUnauthorized)
	})
	t.Run("no Secret", func(t *testing.T) {
		f := newEnrollFixture(t)
		tok := token(t, f.c, guestAgent)
		if err := f.c.Delete(context.Background(), bootstrapSecret(t, f.c, guestAgent)); err != nil {
			t.Fatal(err)
		}
		code, raw := f.post(tok, requestBody(t, guestAgent, pki.EnrollRoleGuest, csrPEM(t, newKey(t), guestAgent, nil, nil)))
		requireStatus(t, code, raw, http.StatusUnauthorized)
	})
}

func requireStatus(t *testing.T, code int, raw []byte, want int) {
	t.Helper()
	if code != want {
		t.Fatalf("status %d, want %d: %s", code, want, raw)
	}
	requireErrorBody(t, raw)
}

func TestEnrollHandlerUnknownAgent(t *testing.T) {
	f := newEnrollFixture(t)
	code, raw := f.post("some-token", requestBody(t, "absent", pki.EnrollRoleGuest, csrPEM(t, newKey(t), "absent", nil, nil)))
	requireStatus(t, code, raw, http.StatusNotFound)

	// Without HostConfig no agent is declared.
	f.h.Client = fakeCluster(t, nil)
	code, raw = f.post("some-token", requestBody(t, hostAgent, pki.EnrollRoleHost, csrPEM(t, newKey(t), hostAgent, nil, nil)))
	requireStatus(t, code, raw, http.StatusNotFound)
}

func TestEnrollHandlerMalformed(t *testing.T) {
	f := newEnrollFixture(t)
	key := newKey(t)
	good := csrPEM(t, key, guestAgent, nil, nil)
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(good))
	tampered := bytes.Clone(block.Bytes)
	tampered[len(tampered)-1] ^= 0xff
	cases := map[string]string{
		"not JSON":       "{",
		"array":          "[]",
		"unknown field":  `{"agent":"docker-host-1","role":"guest","csr":"x","extra":1}`,
		"trailing data":  requestBody(t, guestAgent, pki.EnrollRoleGuest, good) + "{}",
		"bad agent":      requestBody(t, "Not_A_Label", pki.EnrollRoleGuest, good),
		"empty agent":    requestBody(t, "", pki.EnrollRoleGuest, good),
		"unknown role":   requestBody(t, guestAgent, "admin", good),
		"CSR not PEM":    requestBody(t, guestAgent, pki.EnrollRoleGuest, "not a csr"),
		"CSR wrong type": requestBody(t, guestAgent, pki.EnrollRoleGuest, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: block.Bytes}))),
		"CSR garbage":    requestBody(t, guestAgent, pki.EnrollRoleGuest, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: []byte("x")}))),
		"CSR signature":  requestBody(t, guestAgent, pki.EnrollRoleGuest, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: tampered}))),
		"CSR P-384":      requestBody(t, guestAgent, pki.EnrollRoleGuest, csrPEM(t, p384, guestAgent, nil, nil)),
		"too large":      `{"agent":"docker-host-1","role":"guest","csr":"` + strings.Repeat("A", pki.MaxEnrollRequestBytes) + `"}`,
	}
	tok := token(t, f.c, guestAgent)
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			code, raw := f.post(tok, body)
			requireStatus(t, code, raw, http.StatusBadRequest)
		})
	}
	if token(t, f.c, guestAgent) != tok {
		t.Fatal("a malformed request consumed the token")
	}
}

func TestEnrollHandlerMethod(t *testing.T) {
	f := newEnrollFixture(t)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, pki.EnrollPath, http.NoBody)
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET: %d, Allow %q", rec.Code, rec.Header().Get("Allow"))
	}
}

// TestEnrollHandlerConcurrentUse sends the same token in parallel: exactly
// one request may succeed.
func TestEnrollHandlerConcurrentUse(t *testing.T) {
	f := newEnrollFixture(t)
	tok := token(t, f.c, guestAgent)
	const n = 8
	bodies := make([]string, n)
	for i := range bodies {
		bodies[i] = requestBody(t, guestAgent, pki.EnrollRoleGuest, csrPEM(t, newKey(t), guestAgent, nil, nil))
	}
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { codes[i], _ = f.post(tok, bodies[i]) })
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusUnauthorized:
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	if ok != 1 {
		t.Fatalf("%d requests succeeded with one token, want 1 (%v)", ok, codes)
	}
}

// TestEnrollRoundTrip runs the agent's enrollment client against the
// handler over TLS.
func TestEnrollRoundTrip(t *testing.T) {
	f := newEnrollFixture(t)
	srv := httptest.NewTLSServer(f.h)
	t.Cleanup(srv.Close)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())

	for _, tc := range []struct {
		name string
		role agent.Role
	}{{hostAgent, agent.RoleHost}, {nodeAgent, agent.RoleGuest}} {
		t.Run(tc.name, func(t *testing.T) { roundTrip(t, f, srv.URL, roots, tc.name, tc.role) })
	}

	// The used token is refused, and the client reports it as such.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := agent.Enroll(ctx, agent.EnrollOptions{
		Server: srv.URL + pki.EnrollPath, Token: "used-or-wrong", CASHA256: f.ca.Fingerprint(),
		Agent: hostAgent, Role: agent.RoleHost, RootCAs: roots, Now: f.clk.Now,
	})
	if !errors.Is(err, agent.ErrEnrollToken) {
		t.Fatalf("second enrollment: err = %v, want ErrEnrollToken", err)
	}
}

// roundTrip enrolls one agent with the real client and checks the result.
func roundTrip(t *testing.T, f *enrollFixture, url string, roots *x509.CertPool, name string, role agent.Role) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	e, err := agent.Enroll(ctx, agent.EnrollOptions{
		Server: url + pki.EnrollPath, Token: token(t, f.c, name), CASHA256: f.ca.Fingerprint(),
		Agent: name, Role: role, RootCAs: roots, Now: f.clk.Now,
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if !e.CA.Equal(f.ca.Certificate()) || e.Config.Name != name || e.Config.Role != role {
		t.Fatalf("enrollment CA/config mismatch: %+v", e.Config)
	}
	if (e.Policy != nil) != (role == agent.RoleHost) {
		t.Fatalf("policy = %v for role %s", e.Policy, role)
	}
	if e.Policy != nil && e.Policy.Digest() != policy.FromHostConfig(enrollHostConfig()).Digest() {
		t.Fatal("policy digest differs from HostConfig's")
	}
}
