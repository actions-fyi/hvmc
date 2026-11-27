package svc

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matfax/hvmc/internal/agent"
	"github.com/matfax/hvmc/internal/fake"
	"github.com/matfax/hvmc/internal/pki"
	"github.com/matfax/hvmc/internal/policy"
)

// t0 is the time of every test clock.
var t0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

const testToken = "bootstrap-token-1"

// testClock adapts fake.ManualClock to agent.Clock.
type testClock struct{ *fake.ManualClock }

func newTestClock() testClock { return testClock{fake.NewManualClock(t0)} }

func (c testClock) NewTimer(d time.Duration) agent.Timer { return c.ManualClock.NewTimer(d) }

func (c testClock) AfterFunc(d time.Duration, f func()) agent.Timer {
	return c.ManualClock.AfterFunc(d, f)
}

type caClock struct{}

func (caClock) Now() time.Time { return t0 }

// testPolicy is the policy the enrollment endpoint proposes to a host.
func testPolicy() policy.Policy {
	return policy.Policy{VMs: []policy.VM{{Name: "windows-node-1", Min: 6 << 30, Max: 24 << 30}}}
}

// enrollEndpoint serves /v1/enroll like the coordinator's handler for one
// agent with a single-use token: it signs the CSR with a test CA and
// returns the given ports and, for a host, testPolicy. Its TLS certificate
// is from the same CA, which the tests trust as RootCAs.
type enrollEndpoint struct {
	URL         string
	Fingerprint string
	CA          *pki.CA
	used        atomic.Int32
}

func newEnrollEndpoint(t *testing.T, port, healthPort int) *enrollEndpoint {
	t.Helper()
	ca, err := pki.GenerateCA(pki.WithClock(caClock{}))
	if err != nil {
		t.Fatal(err)
	}
	e := &enrollEndpoint{CA: ca, Fingerprint: ca.Fingerprint()}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var req pki.EnrollRequest
		if r.Header.Get("Authorization") != "Bearer "+testToken || json.NewDecoder(r.Body).Decode(&req) != nil || e.used.Add(1) > 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(pki.EnrollErrorResponse{Error: "token refused"})
			return
		}
		block, _ := pem.Decode([]byte(req.CSR))
		leaf, err := ca.SignCSR(block.Bytes, pki.Identity{CommonName: req.Agent, DNSNames: []string{req.Agent + ".example.com"}})
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		resp := pki.EnrollResponse{
			Certificate:   string(pki.EncodeCertificatePEM(leaf)),
			CACertificate: string(ca.CertPEM()),
			Settings:      pki.AgentSettings{Port: int32(port), HealthPort: int32(healthPort)},
		}
		if req.Role == string(agent.RoleHost) {
			resp.Policy = string(testPolicy().Canonical())
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	kp, err := ca.IssueServing(pki.Identity{DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}})
	if err != nil {
		t.Fatal(err)
	}
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{*kp.TLSCertificate()}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	e.URL = srv.URL + pki.EnrollPath
	return e
}

// readyzServer serves /readyz with the current status (200 or 503).
type readyzServer struct {
	port   int
	status atomic.Int32
}

func newReadyzServer(t *testing.T, status int) *readyzServer {
	t.Helper()
	r := &readyzServer{}
	r.status.Store(int32(status))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		code := int(r.status.Load())
		w.WriteHeader(code)
		if code == http.StatusOK {
			_, _ = fmt.Fprint(w, "[+] certificate\n[+] worker\nready\n")
		} else {
			_, _ = fmt.Fprint(w, "[+] certificate\n[-] worker: pwsh exited\nnot ready\n")
		}
	}))
	t.Cleanup(srv.Close)
	_, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	r.port, _ = strconv.Atoi(p)
	return r
}

// fakePlatform records the calls of Install and Uninstall.
type fakePlatform struct {
	mu      sync.Mutex
	calls   []string
	state   State
	pid     uint32
	failOn  map[string]error
	privErr error
	pwshErr error
	specErr error
	labels  *Labels
	// stopOnStart makes the started service stop at once (a crash).
	stopOnStart bool

	spec   ServiceSpec
	rules  []FirewallRule
	source string
}

func newFakePlatform() *fakePlatform {
	return &fakePlatform{state: NotInstalled, failOn: map[string]error{}}
}

func (f *fakePlatform) call(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	return f.failOn[name]
}

func (f *fakePlatform) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakePlatform) setState(s State) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = s
}

func (f *fakePlatform) CheckPrivileges() error { return f.privErr }

// Describe returns the Windows labels unless labels is set.
func (f *fakePlatform) Describe() Labels {
	if f.labels != nil {
		return *f.labels
	}
	return windowsLabels()
}

func (f *fakePlatform) CheckSpec(ServiceSpec) error { return f.specErr }

func (f *fakePlatform) FindPwsh() (string, error) {
	if f.pwshErr != nil {
		return "", f.pwshErr
	}
	return `C:\Program Files\PowerShell\7\pwsh.exe`, nil
}

func (f *fakePlatform) BinaryPath() string { return `C:\Program Files\hvmc\hvmc.exe` }

func (f *fakePlatform) InstallBinary(src string) error {
	f.mu.Lock()
	f.source = src
	f.mu.Unlock()
	return f.call("InstallBinary")
}

func (f *fakePlatform) RemoveBinary() (bool, error) { return false, f.call("RemoveBinary") }
func (f *fakePlatform) RegisterEventSource() error  { return f.call("RegisterEventSource") }
func (f *fakePlatform) RemoveEventSource() error    { return f.call("RemoveEventSource") }

func (f *fakePlatform) ServiceState() (State, uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.pid, nil
}

func (f *fakePlatform) ConfigureService(spec ServiceSpec) error {
	if err := f.call("ConfigureService"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spec = spec
	if f.state == NotInstalled {
		f.state = Stopped
	}
	return nil
}

func (f *fakePlatform) StartService(context.Context) error {
	if err := f.call("StartService"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state, f.pid = Running, 4242
	if f.stopOnStart {
		f.state, f.pid = Stopped, 0
	}
	return nil
}

func (f *fakePlatform) StopService(context.Context) error {
	if err := f.call("StopService"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state, f.pid = Stopped, 0
	return nil
}

func (f *fakePlatform) RemoveService(context.Context) error {
	if err := f.call("RemoveService"); err != nil {
		return err
	}
	f.setState(NotInstalled)
	return nil
}

func (f *fakePlatform) AddHyperVAdmin() error    { return f.call("AddHyperVAdmin") }
func (f *fakePlatform) RemoveHyperVAdmin() error { return f.call("RemoveHyperVAdmin") }

func (f *fakePlatform) SetFirewallRules(rules []FirewallRule) error {
	f.mu.Lock()
	f.rules = rules
	f.mu.Unlock()
	return f.call("SetFirewallRules")
}

func (f *fakePlatform) RemoveFirewallRules() error { return f.call("RemoveFirewallRules") }

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}
