//go:build windows && servicetest

package svc

// The service round trip on a real machine (DESIGN §4.5): install,
// upgrade and uninstall the host agent as the Windows service, against an
// in-process enrollment endpoint. It changes the machine (Program Files,
// ProgramData, the service, Hyper-V Administrators, the firewall, the
// Event Log), so it is built only with the servicetest tag, which the
// Windows CI job sets in its own step; it needs an elevated administrator
// on a machine without hvmc and fails, rather than skips, elsewhere.
//
// The host agent's worker reads the mock Hyper-V module of
// internal/agent/host/testdata through a test-only Environment value of
// the service, so readiness does not depend on Hyper-V working on the
// runner.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/matfax/hvmc/internal/agent"
	"github.com/matfax/hvmc/internal/fileacl"
)

func TestServiceRoundTrip(t *testing.T) {
	requireCleanMachine(t)
	paths := agent.DefaultPaths()
	p, err := NewPlatform(agent.SystemClock())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := Uninstall(context.Background(), p, UninstallOptions{Paths: paths, RemoveState: true, Out: testWriter{t}}); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	bin := buildBinary(t)
	ep := newEnrollEndpoint(t, 9443, 9090)
	o := InstallOptions{
		Role: agent.RoleHost, Agent: "hyperv-1", Enroll: ep.URL, Token: testToken, CASHA256: ep.Fingerprint,
		RootCAs: ep.CA.Pool(), Source: bin, Paths: paths, Defaults: paths, Clock: agent.SystemClock(),
		In: strings.NewReader("y\n"), Out: testWriter{t}, noStart: true,
	}
	ctx := context.Background()

	// Install (left stopped, to point the worker at the mock first).
	if err := Install(ctx, p, o); err != nil {
		t.Fatalf("install: %v", err)
	}
	checkInstalled(t, p, paths)
	stageMock(t)
	start(t, p, o)
	pid := checkProcess(t, p)

	// Recovery restarts a killed agent.
	killProcess(t, pid)
	pid = waitRestart(t, p, pid)
	t.Logf("restarted after the kill as pid %d", pid)

	// Upgrade in place: no enrollment, same keys, config and policy.
	before := readFiles(t, paths.Config, paths.Serving(), paths.CA())
	up := o
	up.Role, up.Agent, up.Enroll, up.Token, up.CASHA256 = "", "", "", "", ""
	if err := Install(ctx, p, up); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if after := readFiles(t, paths.Config, paths.Serving(), paths.CA()); !slices.Equal(before, after) {
		t.Error("the upgrade changed agent.yaml, the serving pair or the CA")
	}
	if n := ep.used.Load(); n != 1 {
		t.Errorf("%d enrollments, want 1", n)
	}
	checkInstalled(t, p, paths)
	start(t, p, up)
	checkProcess(t, p)
	var status strings.Builder
	if err := Status(ctx, p, StatusOptions{Paths: paths, Out: &status}); err != nil {
		t.Errorf("status: %v\n%s", err, status.String())
	}
	t.Logf("status:\n%s", status.String())

	// Uninstall, keeping the state.
	if err := Uninstall(ctx, p, UninstallOptions{Paths: paths, Out: testWriter{t}}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	checkRemoved(t, p)
	if _, err := os.Stat(paths.Serving()); err != nil {
		t.Errorf("state not kept: %v", err)
	}
	// Again with --remove-state: idempotent, and the state goes too.
	if err := Uninstall(ctx, p, UninstallOptions{Paths: paths, RemoveState: true, Out: testWriter{t}}); err != nil {
		t.Fatalf("uninstall --remove-state: %v", err)
	}
	checkRemoved(t, p)
	if _, err := os.Stat(paths.State); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s: %v, want removed", paths.State, err)
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(b []byte) (int, error) {
	w.t.Helper()
	w.t.Log(strings.TrimRight(string(b), "\n"))
	return len(b), nil
}

// requireCleanMachine fails unless the test runs elevated on a machine
// without hvmc, so it never touches a real installation.
func requireCleanMachine(t *testing.T) {
	t.Helper()
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Fatal("the service round trip needs an elevated administrator (go test -tags servicetest from an elevated prompt)")
	}
	p := &winPlatform{clk: agent.SystemClock()}
	if state, _, err := p.ServiceState(); err != nil || state != NotInstalled {
		t.Fatalf("service %s: %s, %v; the round trip needs a machine without hvmc", ServiceName, state, err)
	}
	for _, dir := range []string{filepath.Dir(p.BinaryPath()), agent.DefaultPaths().State} {
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s exists (%v); the round trip needs a machine without hvmc", dir, err)
		}
	}
}

// buildBinary builds cmd/hvmc into a temporary directory.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "hvmc.exe")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/matfax/hvmc/cmd/hvmc")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// stageMock copies the mock Hyper-V module and the host fixture to a
// directory the service account can read, and points the service at them
// through its Environment value (test only).
func stageMock(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp(os.Getenv("ProgramData"), "hvmc-servicetest-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	src := filepath.Join("..", "host", "testdata")
	for from, to := range map[string]string{
		filepath.Join(src, "mock", "Hyper-V", "Hyper-V.psm1"): filepath.Join(dir, "mock", "Hyper-V", "Hyper-V.psm1"),
		filepath.Join(src, "hyperv", "host.json"):             filepath.Join(dir, "fixture", "host.json"),
		filepath.Join(src, "hyperv", "errors.json"):           filepath.Join(dir, "fixture", "errors.json"),
	} {
		data, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setDACL(t, dir, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;"+fileacl.ServiceSID(ServiceName)+")")
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\`+ServiceName, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = k.Close() }()
	if err := k.SetStringsValue("Environment", []string{
		"PSModulePath=" + filepath.Join(dir, "mock"),
		"HVMC_HYPERV_FIXTURE=" + filepath.Join(dir, "fixture", "host.json"),
	}); err != nil {
		t.Fatal(err)
	}
}

func setDACL(t *testing.T, path, sddl string) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

// start starts the installed service and waits until the agent is ready.
// The service keeps its vmms dependency when vmms runs or starts on this
// machine; otherwise, after the dependency was checked, it is cleared.
func start(t *testing.T, p Platform, o InstallOptions) {
	t.Helper()
	ensureVMMS(t)
	ctx := context.Background()
	if err := p.StartService(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	cfg, err := agent.LoadConfig(o.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, p, o, cfg, &printer{w: testWriter{t}}); err != nil {
		t.Fatalf("ready: %v", err)
	}
}

func ensureVMMS(t *testing.T) {
	t.Helper()
	m, err := mgr.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Disconnect() }()
	v, err := m.OpenService(VMMS)
	if err != nil {
		t.Fatalf("open vmms: %v", err)
	}
	defer func() { _ = v.Close() }()
	q, err := v.Query()
	if err == nil && q.State != svc.Running {
		err = v.Start()
		if err == nil {
			err = waitUntil(context.Background(), agent.SystemClock(), time.Minute, "vmms", func() (bool, error) {
				q, err := v.Query()
				return err == nil && q.State == svc.Running, err
			})
		}
	}
	if err == nil {
		t.Log("vmms runs; the service keeps its dependency")
		return
	}
	t.Logf("vmms does not run on this machine (%v); clearing the service's vmms dependency for the start", err)
	s, err := m.OpenService(ServiceName)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := setDependencies(s.Handle, nil); err != nil {
		t.Fatal(err)
	}
}

// checkInstalled checks the service's configuration, the ACLs, the group,
// the firewall rules and the Event Log source.
func checkInstalled(t *testing.T, p Platform, paths agent.Paths) {
	t.Helper()
	checkServiceConfig(t, p)
	sid := fileacl.ServiceSID(ServiceName)
	for path, want := range map[string]string{
		paths.State:  "(A;OICI;0x1301bf;;;" + sid + ")",
		paths.Config: "(A;;FR;;;" + sid + ")",
	} {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		if s := sd.String(); !strings.HasPrefix(s, "D:P") || !strings.Contains(s, want) || strings.Contains(s, ";;;BU)") {
			t.Errorf("%s: DACL %s, want protected with %s", path, s, want)
		}
	}
	if !slices.Contains(hyperVAdminMembers(t), sid) {
		t.Errorf("%s is not a member of %s", ServiceAccount, HyperVAdminsSID)
	}
	rules := firewallRulesOnMachine(t)
	want := []string{
		"hvmc-agent-grpc 9443 " + p.BinaryPath() + " hvmc-agent Any",
		"hvmc-agent-health 9090 " + p.BinaryPath() + " hvmc-agent Any",
	}
	if !slices.Equal(rules, want) {
		t.Errorf("firewall rules\n got %q\nwant %q", rules, want)
	}
	if ok, err := eventSourceExists(); err != nil || !ok {
		t.Errorf("Event Log source %s: %v, %v", agent.EventSource, ok, err)
	}
}

func checkServiceConfig(t *testing.T, p Platform) {
	t.Helper()
	err := withService(func(_ *mgr.Mgr, s *mgr.Service) error {
		if s == nil {
			return errors.New("not installed")
		}
		c, err := s.Config()
		if err != nil {
			return err
		}
		if c.ServiceStartName != ServiceAccount || c.StartType != mgr.StartAutomatic || !c.DelayedAutoStart ||
			!slices.Equal(c.Dependencies, []string{VMMS}) || c.SidType != windows.SERVICE_SID_TYPE_UNRESTRICTED ||
			c.DisplayName != DisplayName || c.BinaryPathName != `"`+p.BinaryPath()+`" agent run` {
			t.Errorf("service config %+v", c)
		}
		if got := preshutdownTimeout(t, s.Handle); got != 30*time.Second {
			t.Errorf("preshutdown timeout %s, want 30s", got)
		}
		return checkRecovery(t, s)
	})
	if err != nil {
		t.Fatalf("service %s: %v", ServiceName, err)
	}
}

func checkRecovery(t *testing.T, s *mgr.Service) error {
	t.Helper()
	actions, err := s.RecoveryActions()
	if err != nil {
		return err
	}
	var delays []time.Duration
	for _, a := range actions {
		if a.Type != mgr.ServiceRestart {
			t.Errorf("recovery action %+v", a)
		}
		delays = append(delays, a.Delay)
	}
	reset, err := s.ResetPeriod()
	if err != nil {
		return err
	}
	nonCrash, err := s.RecoveryActionsOnNonCrashFailures()
	if err != nil {
		return err
	}
	if !slices.Equal(delays, RecoveryDelays) || reset != 86400 || !nonCrash {
		t.Errorf("recovery %v, reset %d s, on stops with errors %v", delays, reset, nonCrash)
	}
	return nil
}

func preshutdownTimeout(t *testing.T, h windows.Handle) time.Duration {
	t.Helper()
	var info preshutdownInfo
	var needed uint32
	if err := windows.QueryServiceConfig2(h, windows.SERVICE_CONFIG_PRESHUTDOWN_INFO,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), &needed); err != nil {
		t.Fatalf("query preshutdown timeout: %v", err)
	}
	return time.Duration(info.timeoutMillis) * time.Millisecond
}

// checkProcess checks that the running agent is the virtual account, with
// Hyper-V Administrators in its token, and returns its PID.
func checkProcess(t *testing.T, p Platform) uint32 {
	t.Helper()
	state, pid, err := p.ServiceState()
	if err != nil || state != Running || pid == 0 {
		t.Fatalf("service %s, pid %d, %v", state, pid, err)
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tok.Close() }()
	user, err := tok.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if got := user.User.Sid.String(); got != fileacl.ServiceSID(ServiceName) {
		t.Errorf("the agent runs as %s, want %s (%s)", got, ServiceAccount, fileacl.ServiceSID(ServiceName))
	}
	groups, err := tok.GetTokenGroups()
	if err != nil {
		t.Fatal(err)
	}
	var member bool
	for _, g := range groups.AllGroups() {
		member = member || g.Sid.String() == HyperVAdminsSID
	}
	if !member {
		t.Errorf("the agent's token lacks %s (Hyper-V Administrators)", HyperVAdminsSID)
	}
	return pid
}

func killProcess(t *testing.T, pid uint32) {
	t.Helper()
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if err := windows.TerminateProcess(h, 1); err != nil {
		t.Fatal(err)
	}
}

// waitRestart waits until SCM recovery has the service running in a new
// process (the first restart is after 5 s).
func waitRestart(t *testing.T, p Platform, old uint32) uint32 {
	t.Helper()
	var pid uint32
	err := waitUntil(context.Background(), agent.SystemClock(), 2*time.Minute, "the recovery restart", func() (bool, error) {
		state, cur, err := p.ServiceState()
		pid = cur
		return state == Running && cur != 0 && cur != old, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// checkRemoved checks that uninstall left nothing behind but the state.
func checkRemoved(t *testing.T, p Platform) {
	t.Helper()
	if state, _, err := p.ServiceState(); err != nil || state != NotInstalled {
		t.Errorf("service %s, %v", state, err)
	}
	if slices.Contains(hyperVAdminMembers(t), fileacl.ServiceSID(ServiceName)) {
		t.Errorf("%s is still in Hyper-V Administrators", ServiceAccount)
	}
	if rules := firewallRulesOnMachine(t); len(rules) != 0 {
		t.Errorf("firewall rules left: %q", rules)
	}
	if ok, err := eventSourceExists(); err != nil || ok {
		t.Errorf("Event Log source left: %v, %v", ok, err)
	}
	if _, err := os.Stat(filepath.Dir(p.BinaryPath())); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s: %v, want removed", filepath.Dir(p.BinaryPath()), err)
	}
}

func readFiles(t *testing.T, paths ...string) []string {
	t.Helper()
	var out []string
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, string(b))
	}
	return out
}

var (
	procNetLocalGroupGetMembers = netapi32.NewProc("NetLocalGroupGetMembers")
	procNetAPIBufferFree        = netapi32.NewProc("NetApiBufferFree")
)

// hyperVAdminMembers returns the SIDs of the group's members.
func hyperVAdminMembers(t *testing.T) []string {
	t.Helper()
	group, err := hyperVAdminsName()
	if err != nil {
		t.Fatalf("Hyper-V Administrators: %v", err)
	}
	g, _ := windows.UTF16PtrFromString(group)
	var (
		buf          *localGroupMembersInfo0
		read, total  uint32
		resumeHandle uintptr
	)
	r, _, _ := procNetLocalGroupGetMembers.Call(0, uintptr(unsafe.Pointer(g)), 0, uintptr(unsafe.Pointer(&buf)),
		uintptr(0xFFFFFFFF), uintptr(unsafe.Pointer(&read)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&resumeHandle)))
	if r != 0 {
		t.Fatalf("NetLocalGroupGetMembers: %v", windows.Errno(r))
	}
	defer func() { _, _, _ = procNetAPIBufferFree.Call(uintptr(unsafe.Pointer(buf))) }()
	var sids []string
	for _, m := range unsafe.Slice(buf, read) {
		sids = append(sids, m.sid.String())
	}
	return sids
}

// listRulesScript prints the agent's rules: name, port, program, service
// and remote addresses.
const listRulesScript = `Get-NetFirewallRule -Group hvmc-agent -ErrorAction SilentlyContinue | Sort-Object Name | ForEach-Object {
  $port = ($_ | Get-NetFirewallPortFilter).LocalPort
  $prog = ($_ | Get-NetFirewallApplicationFilter).Program
  $svc = ($_ | Get-NetFirewallServiceFilter).Service
  $addr = (($_ | Get-NetFirewallAddressFilter).RemoteAddress) -join ','
  "$($_.Name) $port $prog $svc $addr"
}`

func firewallRulesOnMachine(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command(windowsPowerShell(), "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", listRulesScript).CombinedOutput()
	if err != nil {
		t.Fatalf("list firewall rules: %v\n%s", err, out)
	}
	var rules []string
	for _, l := range strings.Split(string(bytes.TrimSpace(out)), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			rules = append(rules, l)
		}
	}
	return rules
}
