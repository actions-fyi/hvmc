//go:build linux && servicetest

package svc

// The systemd round trip on a real machine (DESIGN §4.5, §12): install,
// upgrade and uninstall the guest agent as hvmc-agent.service against an
// in-process enrollment endpoint, with Type=notify readiness and a watchdog
// restart of a hung agent. It changes the machine (/usr/local/bin,
// /etc/hvmc, /var/lib/hvmc, the unit, ufw), so it is built only with the
// servicetest tag, which the Linux CI job sets in its own step; it needs
// root and systemd as PID 1 on a machine without hvmc and fails, rather
// than skips, elsewhere.
//
// The installed binary is built with the servicetesthooks tag (or taken
// from HVMC_SERVICETEST_BINARY, built that way by CI): its hooks hang the
// main loop or hold back READY=1 when a drop-in of the test sets their
// variables (hooks_servicetest_linux.go). No other build has them.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/matfax/hvmc/internal/agent"
)

const dropInDir = "/etc/systemd/system/" + UnitName + ".d"

func TestServiceRoundTrip(t *testing.T) {
	requireCleanMachine(t)
	paths := agent.DefaultPaths()
	pl, err := NewPlatform(agent.SystemClock())
	if err != nil {
		t.Fatal(err)
	}
	p := pl.(*linuxPlatform)
	t.Cleanup(func() {
		_ = os.RemoveAll(dropInDir)
		if err := Uninstall(context.Background(), p, UninstallOptions{Paths: paths, RemoveState: true, Out: testWriter{t}}); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	ufwBefore := ufwRules(t, p)
	ep := newEnrollEndpoint(t, 9443, 9090)
	o := InstallOptions{
		Role: agent.RoleGuest, Agent: "vm-1", Enroll: ep.URL, Token: testToken, CASHA256: ep.Fingerprint,
		RootCAs: ep.CA.Pool(), Source: testBinary(t), Paths: paths, Defaults: paths, Clock: agent.SystemClock(),
		Out: testWriter{t},
	}
	if err := Install(context.Background(), p, o); err != nil {
		t.Fatalf("install: %v", err)
	}
	pid := checkInstalled(t, p, paths)
	checkUFWUntouched(t, p, ufwBefore)

	// Restart=on-failure restarts a killed agent.
	if err := syscall.Kill(int(pid), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	pid = waitRunning(t, p, pid)
	t.Logf("restarted after SIGKILL as pid %d", pid)

	checkNotifyReadiness(t, p)
	checkWatchdog(t, p)

	checkUpgrade(t, p, o, ep.used.Load)
	checkUninstall(t, p, paths, ufwBefore)
}

// checkUpgrade installs again without the enrollment flags: no
// enrollment, the same keys and config, a restarted agent and a good
// status.
func checkUpgrade(t *testing.T, p *linuxPlatform, o InstallOptions, enrollments func() int32) {
	t.Helper()
	ctx := context.Background()
	paths := o.Paths
	before := readFiles(t, paths.Config, paths.Serving(), paths.CA())
	_, pid, _ := p.ServiceState()
	up := o
	up.Role, up.Agent, up.Enroll, up.Token, up.CASHA256 = "", "", "", "", ""
	if err := Install(ctx, p, up); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if after := readFiles(t, paths.Config, paths.Serving(), paths.CA()); !slices.Equal(before, after) {
		t.Error("the upgrade changed agent.yaml, the serving pair or the CA")
	}
	if n := enrollments(); n != 1 {
		t.Errorf("%d enrollments, want 1", n)
	}
	if next := checkInstalled(t, p, paths); next == pid {
		t.Errorf("the upgrade did not restart the agent (pid %d)", pid)
	}
	var status strings.Builder
	if err := Status(ctx, p, StatusOptions{Paths: paths, Out: &status}); err != nil {
		t.Errorf("status: %v\n%s", err, status.String())
	}
	t.Logf("status:\n%s", status.String())
}

// checkUninstall uninstalls keeping the state, then again with
// --remove-state; nothing of hvmc is left.
func checkUninstall(t *testing.T, p *linuxPlatform, paths agent.Paths, ufwBefore [][]string) {
	t.Helper()
	ctx := context.Background()
	if err := Uninstall(ctx, p, UninstallOptions{Paths: paths, Out: testWriter{t}}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	checkRemoved(t, p, ufwBefore)
	if _, err := os.Stat(paths.Serving()); err != nil {
		t.Errorf("state not kept: %v", err)
	}
	// Again with --remove-state: idempotent, and the state goes too.
	if err := Uninstall(ctx, p, UninstallOptions{Paths: paths, RemoveState: true, Out: testWriter{t}}); err != nil {
		t.Fatalf("uninstall --remove-state: %v", err)
	}
	checkRemoved(t, p, ufwBefore)
	for _, dir := range []string{paths.State, "/var/lib/private/hvmc", filepath.Dir(paths.Config)} {
		if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: %v, want removed", dir, err)
		}
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(b []byte) (int, error) {
	w.t.Helper()
	w.t.Log(strings.TrimRight(string(b), "\n"))
	return len(b), nil
}

// requireCleanMachine fails unless the test runs as root under systemd on
// a machine without hvmc, so it never touches a real installation.
func requireCleanMachine(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatal("the systemd round trip needs root (sudo)")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		t.Fatalf("systemd is not PID 1 (%v); the round trip needs a systemd machine", err)
	}
	for _, f := range []string{LinuxBinaryPath, UnitPath, dropInDir, "/etc/hvmc", "/var/lib/hvmc", "/var/lib/private/hvmc"} {
		if _, err := os.Lstat(f); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s exists (%v); the round trip needs a machine without hvmc", f, err)
		}
	}
}

// testBinary returns the hvmc binary with the test hooks: CI builds it
// (HVMC_SERVICETEST_BINARY), so the test needs no Go toolchain as root.
func testBinary(t *testing.T) string {
	t.Helper()
	if bin := os.Getenv("HVMC_SERVICETEST_BINARY"); bin != "" {
		return bin
	}
	bin := filepath.Join(t.TempDir(), "hvmc")
	cmd := exec.Command("go", "build", "-tags", "servicetesthooks", "-o", bin, "github.com/matfax/hvmc/cmd/hvmc")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// unitProps returns the unit's D-Bus properties of interface Unit, or
// of Service with service set.
func unitProps(t *testing.T, p *linuxPlatform, service ...bool) map[string]any {
	t.Helper()
	var props map[string]any
	err := p.withSystemd(context.Background(), func(c systemd) error {
		var err error
		if len(service) > 0 && service[0] {
			props, err = c.GetUnitTypePropertiesContext(context.Background(), UnitName, "Service")
		} else {
			props, err = c.GetUnitPropertiesContext(context.Background(), UnitName)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return props
}

// checkInstalled checks the installed machine and returns the agent's PID.
func checkInstalled(t *testing.T, p *linuxPlatform, paths agent.Paths) uint32 {
	t.Helper()
	state, pid, err := p.ServiceState()
	if err != nil || state != Running || pid == 0 {
		t.Fatalf("service: %s, pid %d, %v; want running", state, pid, err)
	}
	if err := CheckEnroll(); !errors.Is(err, ErrServiceInstalled) {
		t.Errorf("hvmc agent enroll with the unit installed: %v, want ErrServiceInstalled", err)
	}
	props := unitProps(t, p)
	if props["UnitFileState"] != "enabled" {
		t.Errorf("unit file state %v, want enabled", props["UnitFileState"])
	}
	checkMode(t, LinuxBinaryPath, 0o755, 0)
	checkMode(t, UnitPath, 0o644, 0)
	unit, err := os.ReadFile(UnitPath)
	if err != nil || !strings.Contains(string(unit), "Type=notify") || !strings.Contains(string(unit), "DynamicUser=yes") {
		t.Errorf("unit: %v\n%s", err, unit)
	}
	// DynamicUser: the agent runs as an unprivileged user that owns its
	// state; agent.yaml is root's, readable, without secrets.
	uid := processUID(t, pid)
	if uid == 0 {
		t.Error("the agent runs as root")
	}
	checkMode(t, paths.Config, 0o644, 0)
	checkMode(t, filepath.Dir(paths.Config), 0o755, 0)
	for _, f := range []string{paths.CA(), paths.Serving()} {
		checkMode(t, f, 0o600, uid)
	}
	if target, err := os.Readlink(paths.State); err != nil || !strings.HasSuffix(target, "private/hvmc") {
		t.Errorf("%s: link %q, %v; want the DynamicUser link to private/hvmc", paths.State, target, err)
	}
	checkMode(t, "/var/lib/private/hvmc", 0o700, uid)
	if report, ready, err := readiness(context.Background(), http.DefaultClient, "http://127.0.0.1:9090/readyz"); err != nil || !ready {
		t.Errorf("readyz: ready %v, %v\n%s", ready, err, report)
	}
	return pid
}

func checkMode(t *testing.T, path string, mode fs.FileMode, uid uint32) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Error(err)
		return
	}
	st, _ := fi.Sys().(*syscall.Stat_t)
	if fi.Mode().Perm() != mode || st == nil || st.Uid != uid {
		t.Errorf("%s: mode %v, owner %v; want %v, uid %d", path, fi.Mode().Perm(), st.Uid, mode, uid)
	}
}

// processUID reads the real UID of a process from /proc.
func processUID(t *testing.T, pid uint32) uint32 {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(data)) {
		if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
			f := strings.Fields(rest)
			if len(f) > 0 {
				uid, err := strconv.ParseUint(f[0], 10, 32)
				if err == nil {
					return uint32(uid)
				}
			}
		}
	}
	t.Fatalf("no Uid in /proc/%d/status", pid)
	return 0
}

// waitRunning waits until the agent runs with another PID than old.
func waitRunning(t *testing.T, p *linuxPlatform, old uint32) uint32 {
	t.Helper()
	var pid uint32
	err := waitUntil(context.Background(), agent.SystemClock(), 2*time.Minute, "the restarted agent", func() (bool, error) {
		state, cur, err := p.ServiceState()
		pid = cur
		return state == Running && cur != 0 && cur != old, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// setDropIn writes the test's drop-in with env (none removes it), reloads
// systemd and restarts the agent; wait waits for the start job.
func setDropIn(t *testing.T, p *linuxPlatform, env string, wait bool) {
	t.Helper()
	ctx := context.Background()
	if err := os.RemoveAll(dropInDir); err != nil {
		t.Fatal(err)
	}
	if env != "" {
		if err := os.MkdirAll(dropInDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dropInDir, "servicetest.conf"), []byte("[Service]\nEnvironment="+env+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	err := p.withSystemd(ctx, func(c systemd) error {
		if err := c.ReloadContext(ctx); err != nil {
			return err
		}
		ch := make(chan string, 1)
		if _, err := c.RestartUnitContext(ctx, UnitName, "replace", ch); err != nil {
			return err
		}
		if !wait {
			return nil
		}
		if r := <-ch; r != "done" {
			return fmt.Errorf("restart job %s", r)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// checkNotifyReadiness: with READY=1 held back, the unit stays activating
// although /readyz reports ready; it becomes active once READY=1 is sent.
func checkNotifyReadiness(t *testing.T, p *linuxPlatform) {
	t.Helper()
	hold := "/run/hvmc-servicetest-hold"
	if err := os.WriteFile(hold, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(hold) }()
	setDropIn(t, p, "HVMC_SERVICETEST_HOLD_READY="+hold, false)
	err := waitUntil(context.Background(), agent.SystemClock(), time.Minute, "/readyz while activating", func() (bool, error) {
		state, _, err := p.ServiceState()
		if err != nil || state == Running {
			return false, fmt.Errorf("service %s, %w, before READY=1", state, err)
		}
		_, ready, err := readiness(context.Background(), http.DefaultClient, "http://127.0.0.1:9090/readyz")
		return err == nil && ready && state == StartPending, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("ready on /readyz, unit still activating: READY=1 is held back")
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	err = waitUntil(context.Background(), agent.SystemClock(), time.Minute, "active after READY=1", func() (bool, error) {
		state, _, err := p.ServiceState()
		return state == Running, err
	})
	if err != nil {
		t.Fatal(err)
	}
	setDropIn(t, p, "", true)
}

// checkWatchdog: a main loop that hangs after the agent is ready stops the
// WATCHDOG=1 pings, and systemd kills and restarts the agent.
func checkWatchdog(t *testing.T, p *linuxPlatform) {
	t.Helper()
	restarts := func() uint32 {
		n, _ := unitProps(t, p, true)["NRestarts"].(uint32)
		return n
	}
	setDropIn(t, p, "HVMC_SERVICETEST_HANG=1", true)
	before := restarts()
	_, pid, _ := p.ServiceState()
	t.Logf("hanging agent pid %d, %d restarts so far", pid, before)
	err := waitUntil(context.Background(), agent.SystemClock(), 3*time.Minute, "a watchdog restart", func() (bool, error) {
		return restarts() > before, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("journalctl", "-u", UnitName, "--no-pager", "-o", "cat", "-n", "200").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Watchdog timeout") {
		t.Errorf("journal shows no watchdog timeout (%v):\n%s", err, out)
	}
	setDropIn(t, p, "", true)
	if state, _, err := p.ServiceState(); err != nil || state != Running {
		t.Fatalf("after the watchdog test: %s, %v", state, err)
	}
}

func readFiles(t *testing.T, files ...string) []string {
	t.Helper()
	out := make([]string, len(files))
	for i, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = string(data)
	}
	return out
}

// ufwRules returns the agent's rules in `ufw show added`, if ufw is there.
func ufwRules(t *testing.T, p *linuxPlatform) [][]string {
	t.Helper()
	if ok, err := p.ufw.installed(); err != nil || !ok {
		return nil
	}
	out, err := p.ufw.run(context.Background(), p.ufw.path, "show", "added")
	if err != nil {
		t.Fatal(err)
	}
	rules, err := ownUFWRules(string(out))
	if err != nil {
		t.Fatal(err)
	}
	return rules
}

// checkUFWUntouched: the runner's ufw is inactive (the test never enables
// it, DESIGN §4.5), so install added no rule. The active path is covered
// by the unit tests of ufw_linux_test.go.
func checkUFWUntouched(t *testing.T, p *linuxPlatform, before [][]string) {
	t.Helper()
	active, err := p.ufw.active(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if active {
		t.Fatal("ufw is active on this machine; the round trip expects it inactive and never changes it")
	}
	if rules := ufwRules(t, p); len(rules) != len(before) {
		t.Errorf("install with ufw inactive added rules: %v (before: %v)", rules, before)
	}
	t.Log("ufw inactive: no firewall rule added")
}

// checkRemoved: no unit, no binary, no agent ufw rule.
func checkRemoved(t *testing.T, p *linuxPlatform, ufwBefore [][]string) {
	t.Helper()
	if state, _, err := p.ServiceState(); err != nil || state != NotInstalled {
		t.Errorf("service %s, %v; want not installed", state, err)
	}
	for _, f := range []string{UnitPath, LinuxBinaryPath} {
		if _, err := os.Lstat(f); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: %v, want removed", f, err)
		}
	}
	if rules := ufwRules(t, p); len(rules) != 0 || len(ufwBefore) != 0 {
		t.Errorf("ufw rules of the agent: %v (before the test: %v)", rules, ufwBefore)
	}
}
