package svc

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/matfax/hvmc/internal/agent"
	"github.com/matfax/hvmc/internal/agent/host"
)

// installEnv is an enrollment endpoint, a /readyz server whose port the
// endpoint hands out as the health port, and the options of a first
// install into a temporary directory.
type installEnv struct {
	t      *testing.T
	enroll *enrollEndpoint
	readyz *readyzServer
	paths  agent.Paths
	out    *strings.Builder
}

func newInstallEnv(t *testing.T, readyStatus int) *installEnv {
	t.Helper()
	readyz := newReadyzServer(t, readyStatus)
	dir := t.TempDir()
	return &installEnv{
		t: t, enroll: newEnrollEndpoint(t, 9443, readyz.port), readyz: readyz,
		paths: agent.Paths{Config: filepath.Join(dir, "etc", "agent.yaml"), State: filepath.Join(dir, "state")},
		out:   &strings.Builder{},
	}
}

func (e *installEnv) options(role agent.Role, answer string) InstallOptions {
	return InstallOptions{
		Role: role, Agent: "hyperv-1", Enroll: e.enroll.URL, Token: testToken, CASHA256: e.enroll.Fingerprint,
		RootCAs: e.enroll.CA.Pool(), Source: filepath.Join(e.t.TempDir(), "hvmc.exe"),
		Paths: e.paths, Defaults: e.paths, Clock: newTestClock(),
		In: strings.NewReader(answer), Out: e.out,
	}
}

func (e *installEnv) policyStore() *agent.PolicyStore {
	s := agent.OpenPolicyStore(e.paths, slog.New(slog.DiscardHandler))
	if err := s.Check(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	return s
}

func TestInstallHost(t *testing.T) {
	e := newInstallEnv(t, http.StatusOK)
	p := newFakePlatform()
	o := e.options(agent.RoleHost, "y\n")
	if err := Install(testCtx(t), p, o); err != nil {
		t.Fatalf("Install: %v\n%s", err, e.out)
	}
	want := []string{"InstallBinary", "RegisterEventSource", "ConfigureService", "AddHyperVAdmin", "SetFirewallRules", "StartService"}
	if diff := cmp.Diff(want, p.recorded()); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
	if p.source != o.Source {
		t.Errorf("installed %q, want %q", p.source, o.Source)
	}
	if p.spec.Account != `NT SERVICE\hvmc-agent` || !cmp.Equal(p.spec.Dependencies, []string{"vmms"}) ||
		!cmp.Equal(p.spec.Args, []string{"agent", "run"}) || p.spec.Binary != p.BinaryPath() {
		t.Errorf("service %+v", p.spec)
	}
	wantRules := []FirewallRule{
		{Name: "hvmc-agent-grpc", DisplayName: "hvmc agent (gRPC, mTLS)", Port: 9443},
		{Name: "hvmc-agent-health", DisplayName: "hvmc agent (health and metrics)", Port: e.readyz.port},
	}
	if diff := cmp.Diff(wantRules, p.rules); diff != "" {
		t.Errorf("firewall rules (-want +got):\n%s", diff)
	}
	store := e.policyStore()
	if _, pending := store.Pending(); pending || store.ActiveDigest() != testPolicy().Digest() {
		t.Errorf("active policy %s (pending %v), want %s", store.ActiveDigest(), pending, testPolicy().Digest())
	}
	for _, s := range []string{
		`Enrolled agent "hyperv-1" (role host)`, "windows-node-1", "Activate this policy", "Activated policy",
		"Hyper-V Administrators (S-1-5-32-578)", "The agent is ready", "[+] worker",
	} {
		if !strings.Contains(e.out.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, e.out)
		}
	}
}

func TestInstallGuest(t *testing.T) {
	e := newInstallEnv(t, http.StatusOK)
	p := newFakePlatform()
	p.pwshErr = host.ErrNoPwsh // not needed by a guest
	if err := Install(testCtx(t), p, e.options(agent.RoleGuest, "")); err != nil {
		t.Fatalf("Install: %v\n%s", err, e.out)
	}
	want := []string{"InstallBinary", "RegisterEventSource", "ConfigureService", "SetFirewallRules", "StartService"}
	if diff := cmp.Diff(want, p.recorded()); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
	if len(p.spec.Dependencies) != 0 {
		t.Errorf("guest dependencies %v", p.spec.Dependencies)
	}
	if _, err := os.Stat(e.paths.PendingPolicy()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("guest pending policy: %v", err)
	}
}

func TestInstallPolicyDeclined(t *testing.T) {
	for _, answer := range []string{"n\n", "", "maybe\n"} {
		e := newInstallEnv(t, http.StatusOK)
		p := newFakePlatform()
		err := Install(testCtx(t), p, e.options(agent.RoleHost, answer))
		if !errors.Is(err, ErrPolicyDeclined) {
			t.Fatalf("answer %q: %v, want ErrPolicyDeclined", answer, err)
		}
		if calls := p.recorded(); len(calls) != 0 {
			t.Errorf("answer %q: the machine was changed: %v", answer, calls)
		}
		store := e.policyStore()
		if _, pending := store.Pending(); !pending || len(store.Active().VMs) != 0 {
			t.Errorf("answer %q: policy activated (pending %v, active %v)", answer, pending, store.Active())
		}
	}
}

// TestInstallRefusesBeforeChanges: a missing prerequisite or privilege
// changes nothing, not even the state directory.
func TestInstallRefusesBeforeChanges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakePlatform)
		want  error
	}{
		{"no pwsh", func(p *fakePlatform) { p.pwshErr = host.ErrNoPwsh }, host.ErrNoPwsh},
		{"not elevated", func(p *fakePlatform) { p.privErr = errors.New("run as Administrator") }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newInstallEnv(t, http.StatusOK)
			p := newFakePlatform()
			tc.setup(p)
			err := Install(testCtx(t), p, e.options(agent.RoleHost, "y\n"))
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("Install: %v, want %v", err, tc.want)
			}
			if _, err := os.Stat(e.paths.State); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("state directory: %v, want none", err)
			}
			if n := e.enroll.used.Load(); n != 0 || len(p.recorded()) != 0 {
				t.Errorf("enrollments %d, calls %v", n, p.recorded())
			}
		})
	}
}

func TestInstallUsage(t *testing.T) {
	e := newInstallEnv(t, http.StatusOK)
	for _, tc := range []struct {
		name string
		edit func(*InstallOptions)
		want string
	}{
		{"bad role", func(o *InstallOptions) { o.Role = "hypervisor" }, `--role must be host or guest, got "hypervisor"`},
		{"missing flags", func(o *InstallOptions) { o.Role, o.Token, o.Enroll = "", "", "" }, "--role, --enroll, --token required"},
		{"bad fingerprint", func(o *InstallOptions) { o.CASHA256 = "00" }, "--ca-sha256"},
		{"no source", func(o *InstallOptions) { o.Source = "" }, "no binary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := e.options(agent.RoleHost, "y\n")
			tc.edit(&o)
			p := newFakePlatform()
			err := Install(testCtx(t), p, o)
			if !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Install: %v, want usage error with %q", err, tc.want)
			}
		})
	}
	o := e.options(agent.RoleGuest, "")
	o.CanRun = func(agent.Role) error { return errors.New("role guest: not implemented yet") }
	if err := Install(testCtx(t), newFakePlatform(), o); err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("unsupported role: %v", err)
	}
}

// TestUpgrade: a second install keeps the enrollment (the token is
// single-use), stops the running service first and starts it again.
func TestUpgrade(t *testing.T) {
	e := newInstallEnv(t, http.StatusOK)
	p := newFakePlatform()
	if err := Install(testCtx(t), p, e.options(agent.RoleHost, "y\n")); err != nil {
		t.Fatal(err)
	}
	serving, err := os.ReadFile(e.paths.Serving())
	if err != nil {
		t.Fatal(err)
	}
	p.calls = nil
	e.out.Reset()
	o := e.options("", "")
	o.Agent, o.Enroll, o.Token, o.CASHA256 = "", "", "", ""
	if err := Install(testCtx(t), p, o); err != nil {
		t.Fatalf("upgrade: %v\n%s", err, e.out)
	}
	want := []string{"StopService", "InstallBinary", "RegisterEventSource", "ConfigureService", "AddHyperVAdmin", "SetFirewallRules", "StartService"}
	if diff := cmp.Diff(want, p.recorded()); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
	if after, _ := os.ReadFile(e.paths.Serving()); string(after) != string(serving) {
		t.Error("the upgrade replaced the serving pair")
	}
	if e.enroll.used.Load() != 1 || !strings.Contains(e.out.String(), "Already enrolled as host agent") {
		t.Errorf("enrollments %d, output:\n%s", e.enroll.used.Load(), e.out)
	}
	if e.policyStore().ActiveDigest() != testPolicy().Digest() {
		t.Error("the upgrade changed the active policy")
	}

	// The enrollment flags are ignored, not used again.
	e.out.Reset()
	if err := Install(testCtx(t), p, e.options(agent.RoleHost, "")); err != nil || !strings.Contains(e.out.String(), "flags are ignored") {
		t.Errorf("upgrade with flags: %v\n%s", err, e.out)
	}
	// Another role is refused.
	if err := Install(testCtx(t), p, e.options(agent.RoleGuest, "")); !errors.Is(err, ErrUsage) {
		t.Errorf("upgrade to guest: %v, want usage error", err)
	}
}

// TestUpgradeAfterDecline: an install whose policy was declined asks again.
func TestUpgradeAfterDecline(t *testing.T) {
	e := newInstallEnv(t, http.StatusOK)
	p := newFakePlatform()
	if err := Install(testCtx(t), p, e.options(agent.RoleHost, "no\n")); !errors.Is(err, ErrPolicyDeclined) {
		t.Fatal(err)
	}
	o := e.options("", "yes\n")
	o.Token = ""
	if err := Install(testCtx(t), p, o); err != nil {
		t.Fatalf("second install: %v\n%s", err, e.out)
	}
	if e.policyStore().ActiveDigest() != testPolicy().Digest() {
		t.Error("policy not activated on the second install")
	}
}

func TestInstallNotReady(t *testing.T) {
	e := newInstallEnv(t, http.StatusServiceUnavailable)
	p := newFakePlatform()
	o := e.options(agent.RoleGuest, "")
	clk := newTestClock()
	o.Clock = clk
	ctx, cancel := context.WithCancel(testCtx(t))
	done := make(chan error, 1)
	go func() {
		err := Install(testCtx(t), p, o)
		cancel()
		done <- err
	}()
	// Advance through the poll timers until Install gives up.
	for clk.BlockUntil(ctx, 1) == nil {
		clk.Advance(PollInterval)
	}
	err := <-done
	if !errors.Is(err, ErrNotReady) || !strings.Contains(err.Error(), "[-] worker: pwsh exited") ||
		!strings.Contains(err.Error(), "timed out after 2m0s") {
		t.Fatalf("Install: %v, want ErrNotReady with the last report", err)
	}
}

func TestInstallServiceStops(t *testing.T) {
	e := newInstallEnv(t, http.StatusServiceUnavailable)
	p := newFakePlatform()
	p.stopOnStart = true
	err := Install(testCtx(t), p, e.options(agent.RoleGuest, ""))
	if !errors.Is(err, ErrNotReady) || !strings.Contains(err.Error(), "stopped while starting") {
		t.Errorf("Install: %v", err)
	}
}

func TestInstallStepFails(t *testing.T) {
	e := newInstallEnv(t, http.StatusOK)
	p := newFakePlatform()
	boom := errors.New("access denied")
	p.failOn["ConfigureService"] = boom
	if err := Install(testCtx(t), p, e.options(agent.RoleGuest, "")); !errors.Is(err, boom) {
		t.Fatalf("Install: %v", err)
	}
	if diff := cmp.Diff([]string{"InstallBinary", "RegisterEventSource", "ConfigureService"}, p.recorded()); diff != "" {
		t.Errorf("calls after the failure (-want +got):\n%s", diff)
	}
}

// TestInstallRefusesBrokenEnrollment: part of an enrollment, or an invalid
// one, is neither kept nor enrolled over.
func TestInstallRefusesBrokenEnrollment(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(agent.Paths) error
	}{
		{"no serving pair", func(p agent.Paths) error { return os.Remove(p.Serving()) }},
		{"no CA", func(p agent.Paths) error { return os.Remove(p.CA()) }},
		{"config does not parse", func(p agent.Paths) error { return os.WriteFile(p.Config, []byte("role: [\n"), 0o600) }},
		{"config only", func(p agent.Paths) error {
			return errors.Join(os.Remove(p.Serving()), os.Remove(p.CA()))
		}},
		{"foreign CA", func(p agent.Paths) error {
			other := newEnrollEndpoint(t, 9443, 9090)
			return os.WriteFile(p.CA(), other.CA.CertPEM(), 0o600)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newInstallEnv(t, http.StatusOK)
			p := newFakePlatform()
			if err := Install(testCtx(t), p, e.options(agent.RoleGuest, "")); err != nil {
				t.Fatal(err)
			}
			if err := tc.damage(e.paths); err != nil {
				t.Fatal(err)
			}
			p.calls = nil
			err := Install(testCtx(t), p, e.options(agent.RoleGuest, ""))
			if !errors.Is(err, ErrBrokenEnrollment) || !strings.Contains(err.Error(), "hvmc agent enroll") {
				t.Errorf("Install: %v, want ErrBrokenEnrollment", err)
			}
			if len(p.recorded()) != 0 || e.enroll.used.Load() != 1 {
				t.Errorf("calls %v, enrollments %d", p.recorded(), e.enroll.used.Load())
			}
		})
	}
}
