package svc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/matfax/hvmc/internal/agent"
)

func TestServiceSpec(t *testing.T) {
	defaults := agent.Paths{Config: `C:\ProgramData\hvmc\agent.yaml`, State: `C:\ProgramData\hvmc`}
	s := serviceSpec(agent.RoleHost, `C:\Program Files\hvmc\hvmc.exe`, agent.Paths{Config: `c:\programdata\HVMC\agent.yaml`, State: `c:\programdata\hvmc`}, defaults)
	want := ServiceSpec{
		Name: "hvmc-agent", DisplayName: "Hyper-V Memory Coordinator Agent", Description: Description,
		Binary: `C:\Program Files\hvmc\hvmc.exe`, Args: []string{"agent", "run"}, Account: `NT SERVICE\hvmc-agent`,
		Dependencies:   []string{"vmms"},
		RecoveryDelays: []time.Duration{5 * time.Second, 30 * time.Second, 60 * time.Second}, RecoveryReset: 24 * time.Hour,
		PreshutdownTimeout: 30 * time.Second,
	}
	if diff := cmp.Diff(want, s); diff != "" {
		t.Errorf("host spec (-want +got):\n%s", diff)
	}
	g := serviceSpec(agent.RoleGuest, "hvmc.exe", agent.Paths{Config: filepath.Join("d", "a.yaml"), State: "s"}, defaults)
	if len(g.Dependencies) != 0 || !cmp.Equal(g.Args, []string{"agent", "run", "--config", filepath.Join("d", "a.yaml"), "--state-dir", "s"}) {
		t.Errorf("guest spec %+v", g)
	}
}

func TestFirewallRules(t *testing.T) {
	c := agent.Config{
		Listen: "0.0.0.0:9443", Health: "127.0.0.1:9090",
		Firewall: &agent.FirewallConfig{RemoteAddresses: []string{"192.0.2.10", "198.51.100.0/24", "203.0.113.1-203.0.113.9", "2001:db8::/32"}},
	}
	rules, err := firewallRules(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 || rules[0].Port != 9443 || rules[1].Port != 9090 || len(rules[1].RemoteAddresses) != 4 {
		t.Errorf("rules %+v", rules)
	}
	if got := firewallSummary(rules); !strings.Contains(got, "TCP 9443 and TCP 9090 from 192.0.2.10, 198.51.100.0/24") {
		t.Errorf("summary %q", got)
	}
	for _, bad := range []string{"LocalSubnet", "any", "192.0.2.1; Remove-Item", "203.0.113.9-203.0.113.1", "192.0.2.1-2001:db8::1", ""} {
		c.Firewall.RemoteAddresses = []string{bad}
		if _, err := firewallRules(c); err == nil {
			t.Errorf("remote address %q accepted", bad)
		}
	}
	c.Firewall = nil
	c.Listen = "0.0.0.0:0"
	if _, err := firewallRules(c); err == nil {
		t.Error("port 0 accepted")
	}
}

// TestFirewallParams: values reach the fixed scripts only as JSON.
func TestFirewallParams(t *testing.T) {
	rules, err := firewallRules(agent.Config{Listen: ":9443", Health: ":9090"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(firewallParams{Group: FirewallGroup, Program: `C:\Program Files\hvmc\hvmc.exe`, Service: ServiceName, Rules: rules})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"group":"hvmc-agent","program":"C:\\Program Files\\hvmc\\hvmc.exe","service":"hvmc-agent","rules":[` +
		`{"name":"hvmc-agent-grpc","displayName":"hvmc agent (gRPC, mTLS)","port":9443},` +
		`{"name":"hvmc-agent-health","displayName":"hvmc agent (health and metrics)","port":9090}]}`
	if string(b) != want {
		t.Errorf("params\n got %s\nwant %s", b, want)
	}
	for _, s := range []string{firewallSetScript, firewallRemoveScript} {
		if strings.Contains(s, "hvmc") || strings.Contains(s, "Invoke-Expression") || strings.Contains(s, "-EncodedCommand") {
			t.Errorf("script holds a value or evaluates code:\n%s", s)
		}
	}
}

func TestHealthURL(t *testing.T) {
	for addr, want := range map[string]string{
		"0.0.0.0:9090":   "http://127.0.0.1:9090/readyz",
		":9090":          "http://127.0.0.1:9090/readyz",
		"[::]:9090":      "http://[::1]:9090/readyz",
		"192.0.2.5:9091": "http://192.0.2.5:9091/readyz",
	} {
		if got, err := healthURL(agent.Config{Health: addr}); err != nil || got != want {
			t.Errorf("healthURL(%s) = %q, %v, want %q", addr, got, err, want)
		}
	}
}

func TestUninstall(t *testing.T) {
	e := newInstallEnv(t, http.StatusOK)
	p := newFakePlatform()
	if err := Install(testCtx(t), p, e.options(agent.RoleHost, "y\n")); err != nil {
		t.Fatal(err)
	}
	p.calls = nil
	var out strings.Builder
	if err := Uninstall(testCtx(t), p, UninstallOptions{Paths: e.paths, Out: &out}); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	want := []string{"StopService", "RemoveHyperVAdmin", "RemoveFirewallRules", "RemoveService", "RemoveEventSource", "RemoveBinary"}
	if diff := cmp.Diff(want, p.recorded()); diff != "" {
		t.Errorf("calls (-want +got):\n%s", diff)
	}
	if _, err := os.Stat(e.paths.Serving()); err != nil || !strings.Contains(out.String(), "--remove-state") {
		t.Errorf("state not kept: %v\n%s", err, out.String())
	}

	// Again, with a failing step: the others still run.
	p.calls = nil
	p.failOn["RemoveFirewallRules"] = errors.New("access denied")
	out.Reset()
	err := Uninstall(testCtx(t), p, UninstallOptions{Paths: e.paths, RemoveState: true, Out: &out})
	if err == nil || !strings.Contains(err.Error(), "access denied") || !strings.Contains(out.String(), "Failed: removed the firewall rules") {
		t.Errorf("Uninstall: %v\n%s", err, out.String())
	}
	if diff := cmp.Diff(want[1:], p.recorded()); diff != "" { // not running: no stop
		t.Errorf("calls (-want +got):\n%s", diff)
	}
	for _, path := range []string{e.paths.State, e.paths.Config, filepath.Dir(e.paths.Config)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: %v, want removed", path, err)
		}
	}
}

func TestUninstallNotElevated(t *testing.T) {
	p := newFakePlatform()
	p.privErr = errors.New("run as Administrator")
	if err := Uninstall(testCtx(t), p, UninstallOptions{Paths: agent.Paths{State: t.TempDir()}}); err == nil || len(p.recorded()) != 0 {
		t.Errorf("Uninstall: %v, calls %v", err, p.recorded())
	}
}

func TestStatus(t *testing.T) {
	e := newInstallEnv(t, http.StatusOK)
	p := newFakePlatform()
	if err := Install(testCtx(t), p, e.options(agent.RoleHost, "y\n")); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := Status(testCtx(t), p, StatusOptions{Paths: e.paths, Clock: newTestClock(), Out: &out}); err != nil {
		t.Fatalf("Status: %v\n%s", err, out.String())
	}
	for _, s := range []string{
		"Service hvmc-agent: running (pid 4242)", `Agent: hyperv-1 (role host)`, "valid until", "(720h0m0s left)",
		"Active policy:  " + testPolicy().Digest(), "Pending policy: none", "[+] worker", "ready",
	} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("status lacks %q:\n%s", s, out.String())
		}
	}

	e.readyz.status.Store(http.StatusServiceUnavailable)
	out.Reset()
	if err := Status(testCtx(t), p, StatusOptions{Paths: e.paths, Clock: newTestClock(), Out: &out}); !errors.Is(err, ErrNotReady) ||
		!strings.Contains(out.String(), "[-] worker: pwsh exited") {
		t.Errorf("not ready: %v\n%s", err, out.String())
	}

	p.setState(Stopped)
	out.Reset()
	if err := Status(testCtx(t), p, StatusOptions{Paths: e.paths, Clock: newTestClock(), Out: &out}); !errors.Is(err, ErrNotReady) ||
		!strings.Contains(out.String(), "Service hvmc-agent: stopped") || !strings.Contains(out.String(), "not checked") {
		t.Errorf("stopped: %v\n%s", err, out.String())
	}

	out.Reset()
	empty := agent.Paths{Config: filepath.Join(t.TempDir(), "agent.yaml"), State: t.TempDir()}
	if err := Status(testCtx(t), newFakePlatform(), StatusOptions{Paths: empty, Out: &out}); !errors.Is(err, ErrNotReady) ||
		!strings.Contains(out.String(), "not installed") || !strings.Contains(out.String(), "Configuration:") {
		t.Errorf("not installed: %v\n%s", err, out.String())
	}
}

func TestWatchdog(t *testing.T) {
	clk := newTestClock()
	w := newWatchdog(clk, WatchdogTimeout)
	hung := make(chan time.Duration, 1)
	ctx, cancel := context.WithCancel(testCtx(t))
	defer cancel()
	// Building and starting the agent took longer than the timeout: arm
	// counts from the start of serving.
	clk.Advance(2 * WatchdogTimeout)
	go w.arm(ctx, func(age time.Duration) { hung <- age })
	// Ticks keep it quiet.
	for range 4 {
		if err := clk.BlockUntil(ctx, 1); err != nil {
			t.Fatal(err)
		}
		clk.Advance(WatchdogTimeout / 2)
		w.Tick()
	}
	select {
	case age := <-hung:
		t.Fatalf("hung after %s despite ticks", age)
	default:
	}
	// No tick for longer than the timeout.
	for range 3 {
		if err := clk.BlockUntil(ctx, 1); err != nil {
			t.Fatal(err)
		}
		clk.Advance(WatchdogTimeout / 2)
	}
	if age := <-hung; age <= WatchdogTimeout {
		t.Errorf("hung reported at %s, want > %s", age, WatchdogTimeout)
	}
}

func TestWaitUntil(t *testing.T) {
	clk := newTestClock()
	n := 0
	err := waitUntil(testCtx(t), clk, time.Minute, "x", func() (bool, error) { n++; return n == 1, nil })
	if err != nil || n != 1 {
		t.Errorf("immediately done: %v, %d calls", err, n)
	}
	boom := errors.New("boom")
	if err := waitUntil(testCtx(t), clk, time.Minute, "x", func() (bool, error) { return false, boom }); !errors.Is(err, boom) {
		t.Errorf("error: %v", err)
	}
	ctx, cancel := context.WithCancel(testCtx(t))
	cancel()
	if err := waitUntil(ctx, clk, time.Minute, "x", func() (bool, error) { return false, nil }); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
}
