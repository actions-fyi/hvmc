//go:build linux

package svc

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/coreos/go-systemd/v22/dbus"

	"github.com/matfax/hvmc/internal/agent"
)

func TestRenderUnit(t *testing.T) {
	defaults := agent.DefaultPaths()
	unit, err := renderUnit(serviceSpec(agent.RoleGuest, LinuxBinaryPath, defaults, defaults), LinuxBinaryPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(unit)
	// The directives of DESIGN §4.5, each on its own line.
	for _, line := range []string{
		"Type=notify",
		"NotifyAccess=main",
		"ExecStart=/usr/local/bin/hvmc agent run",
		"WatchdogSec=30s",
		"Restart=on-failure",
		"RestartSec=5s",
		"TimeoutStopSec=30s",
		"DynamicUser=yes",
		"ConfigurationDirectory=hvmc",
		"StateDirectory=hvmc",
		"StateDirectoryMode=0700",
		"CapabilityBoundingSet=",
		"NoNewPrivileges=yes",
		"ProtectSystem=strict",
		"ProtectHome=yes",
		"PrivateTmp=yes",
		"PrivateDevices=yes",
		"RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX",
		"SystemCallFilter=@system-service",
		"MemoryDenyWriteExecute=yes",
		"WantedBy=multi-user.target",
		"Description=" + DisplayName,
	} {
		if !slices.Contains(strings.Split(got, "\n"), line) {
			t.Errorf("unit lacks %q:\n%s", line, got)
		}
	}
	if strings.Contains(got, "{{") || strings.Contains(got, "<no value>") {
		t.Errorf("unrendered template:\n%s", got)
	}
}

func TestRenderUnitRefusesOtherPaths(t *testing.T) {
	defaults := agent.DefaultPaths()
	for name, paths := range map[string]agent.Paths{
		"config": {Config: "/srv/hvmc/agent.yaml", State: defaults.State},
		"state":  {Config: defaults.Config, State: "/srv/hvmc"},
		// Linux paths are case-sensitive: this is not the default.
		"case": {Config: "/etc/HVMC/agent.yaml", State: defaults.State},
	} {
		if _, err := renderUnit(serviceSpec(agent.RoleGuest, LinuxBinaryPath, paths, defaults), LinuxBinaryPath); !errors.Is(err, errNonDefaultPaths) {
			t.Errorf("%s: %v, want errNonDefaultPaths", name, err)
		}
	}
	if _, err := renderUnit(ServiceSpec{Binary: "/tmp/hvmc", Args: []string{"agent", "run"}}, LinuxBinaryPath); err == nil {
		t.Error("a unit for another binary was rendered")
	}
}

// fakeSystemd is systemd's D-Bus API for the platform's tests.
type fakeSystemd struct {
	props   map[string]any
	result  string
	calls   []string
	enabled []string
}

func (f *fakeSystemd) GetUnitPropertiesContext(_ context.Context, unit string) (map[string]any, error) {
	f.calls = append(f.calls, "properties "+unit)
	return f.props, nil
}

func (f *fakeSystemd) GetUnitTypePropertiesContext(_ context.Context, unit, unitType string) (map[string]any, error) {
	f.calls = append(f.calls, "properties "+unit+" "+unitType)
	return map[string]any{"MainPID": f.props["MainPID"]}, nil
}

func (f *fakeSystemd) StartUnitContext(_ context.Context, name, mode string, ch chan<- string) (int, error) {
	f.calls = append(f.calls, "start "+name+" "+mode)
	ch <- f.result
	return 1, nil
}

func (f *fakeSystemd) StopUnitContext(_ context.Context, name, mode string, ch chan<- string) (int, error) {
	f.calls = append(f.calls, "stop "+name+" "+mode)
	ch <- f.result
	return 1, nil
}

func (f *fakeSystemd) RestartUnitContext(_ context.Context, name, mode string, ch chan<- string) (int, error) {
	f.calls = append(f.calls, "restart "+name+" "+mode)
	ch <- f.result
	return 1, nil
}

func (f *fakeSystemd) ReloadContext(context.Context) error {
	f.calls = append(f.calls, "reload")
	return nil
}

func (f *fakeSystemd) EnableUnitFilesContext(_ context.Context, files []string, runtime, force bool) (bool, []dbus.EnableUnitFileChange, error) {
	f.calls = append(f.calls, "enable")
	if runtime || !force {
		return false, nil, errors.New("want a persistent, forced enable")
	}
	f.enabled = files
	return false, nil, nil
}

func (f *fakeSystemd) DisableUnitFilesContext(_ context.Context, files []string, _ bool) ([]dbus.DisableUnitFileChange, error) {
	f.calls = append(f.calls, "disable "+strings.Join(files, " "))
	return nil, nil
}

func (f *fakeSystemd) ResetFailedUnitContext(_ context.Context, name string) error {
	f.calls = append(f.calls, "reset-failed "+name)
	return nil
}

func (f *fakeSystemd) Close() {}

func newTestPlatform(t *testing.T, sd *fakeSystemd) *linuxPlatform {
	t.Helper()
	dir := t.TempDir()
	return &linuxPlatform{
		clk:      newTestClock(),
		dial:     func(context.Context) (systemd, error) { return sd, nil },
		unitPath: filepath.Join(dir, UnitName), binaryPath: filepath.Join(dir, "hvmc"),
		ufw:  ufw{path: filepath.Join(dir, "no-ufw")},
		euid: func() int { return 0 },
	}
}

func TestUnitState(t *testing.T) {
	for _, tc := range []struct {
		load, active string
		pid          uint32
		want         State
		wantPID      uint32
	}{
		{"not-found", "inactive", 0, NotInstalled, 0},
		{"loaded", "inactive", 0, Stopped, 0},
		{"loaded", "failed", 0, Stopped, 0},
		{"loaded", "activating", 41, StartPending, 41},
		{"loaded", "active", 42, Running, 42},
		{"loaded", "deactivating", 42, StopPending, 42},
		{"loaded", "maintenance", 0, Other, 0},
	} {
		sd := &fakeSystemd{props: map[string]any{"LoadState": tc.load, "ActiveState": tc.active, "MainPID": tc.pid}}
		state, pid, err := newTestPlatform(t, sd).ServiceState()
		if err != nil || state != tc.want || pid != tc.wantPID {
			t.Errorf("%s/%s: %s, %d, %v; want %s, %d", tc.load, tc.active, state, pid, err, tc.want, tc.wantPID)
		}
	}
}

func TestConfigureAndRemoveService(t *testing.T) {
	sd := &fakeSystemd{result: "done"}
	p := newTestPlatform(t, sd)
	defaults := agent.DefaultPaths()
	if err := p.ConfigureService(serviceSpec(agent.RoleGuest, p.binaryPath, defaults, defaults)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p.unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("unit mode %v, want 0644", info.Mode().Perm())
	}
	if !slices.Equal(sd.enabled, []string{p.unitPath}) || !slices.Equal(sd.calls, []string{"reload", "enable"}) {
		t.Errorf("configure: calls %q, enabled %q", sd.calls, sd.enabled)
	}

	sd.calls = nil
	if err := p.StartService(context.Background()); err != nil {
		t.Errorf("start: %v", err)
	}
	if err := p.StopService(context.Background()); err != nil {
		t.Errorf("stop: %v", err)
	}
	sd.result = "failed"
	if err := p.StartService(context.Background()); err == nil || !strings.Contains(err.Error(), "journalctl -u hvmc-agent") {
		t.Errorf("failed start job: %v", err)
	}
	want := []string{
		"reset-failed " + UnitName, "start " + UnitName + " replace", "stop " + UnitName + " replace",
		"reset-failed " + UnitName, "start " + UnitName + " replace",
	}
	if !slices.Equal(sd.calls, want) {
		t.Errorf("jobs: %q, want %q", sd.calls, want)
	}

	sd.calls = nil
	for range 2 { // idempotent
		if err := p.RemoveService(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(p.unitPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unit file: %v, want removed", err)
	}
	want = []string{"disable " + UnitName, "reload", "reset-failed " + UnitName, "reload", "reset-failed " + UnitName}
	if !slices.Equal(sd.calls, want) {
		t.Errorf("remove: %q, want %q", sd.calls, want)
	}
}

func TestInstallBinary(t *testing.T) {
	p := newTestPlatform(t, &fakeSystemd{})
	src := filepath.Join(t.TempDir(), "hvmc")
	for _, v := range []string{"v1", "v2"} { // install, then upgrade in place
		installVersion(t, p, src, v)
	}
	info, err := os.Stat(p.binaryPath)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("binary: %v, %v; want mode 0755", info, err)
	}
	// Installing the installed binary changes nothing.
	if err := p.InstallBinary(p.binaryPath); err != nil {
		t.Errorf("same file: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(p.binaryPath))
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temporary file left: %s", e.Name())
		}
	}
	for range 2 {
		if deferred, err := p.RemoveBinary(); err != nil || deferred {
			t.Errorf("remove: %v, deferred %v", err, deferred)
		}
	}
}

func installVersion(t *testing.T, p *linuxPlatform, src, v string) {
	t.Helper()
	if err := os.WriteFile(src, []byte(v), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.InstallBinary(src); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p.binaryPath)
	if err != nil || string(got) != v {
		t.Errorf("installed %q, %v; want %q", got, err, v)
	}
}

func TestLinuxPrerequisites(t *testing.T) {
	p := newTestPlatform(t, &fakeSystemd{})
	if err := p.CheckPrivileges(); err != nil {
		t.Errorf("root: %v", err)
	}
	p.euid = func() int { return 1000 }
	if err := p.CheckPrivileges(); err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Errorf("not root: %v", err)
	}
	if _, err := p.FindPwsh(); err == nil {
		t.Error("the host role is not refused on Linux")
	}
}

// TestInstallRefusesHostOnLinux: the host role is refused before anything
// changes, with the reason.
func TestInstallRefusesHostOnLinux(t *testing.T) {
	e := newInstallEnv(t, http.StatusOK)
	sd := &fakeSystemd{}
	p := newTestPlatform(t, sd)
	err := Install(testCtx(t), p, e.options(agent.RoleHost, "y\n"))
	if err == nil || !strings.Contains(err.Error(), "Windows Hyper-V host only") {
		t.Fatalf("Install --role host: %v", err)
	}
	if len(sd.calls) != 0 || e.enroll.used.Load() != 0 {
		t.Errorf("changed the machine: systemd %q, %d enrollments", sd.calls, e.enroll.used.Load())
	}
	if _, err := os.Stat(e.paths.Config); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("agent.yaml written: %v", err)
	}
}

// TestInstallRefusesOtherPathsOnLinux: the unit supports only the default
// paths, which install checks before it enrolls.
func TestInstallRefusesOtherPathsOnLinux(t *testing.T) {
	e := newInstallEnv(t, http.StatusOK)
	p := newTestPlatform(t, &fakeSystemd{})
	o := e.options(agent.RoleGuest, "")
	o.Defaults = agent.DefaultPaths()
	if err := Install(testCtx(t), p, o); !errors.Is(err, errNonDefaultPaths) {
		t.Fatalf("Install: %v", err)
	}
	if e.enroll.used.Load() != 0 {
		t.Error("enrolled before the refusal")
	}
}

// TestRemoveStateDynamicUser: with DynamicUser=yes systemd moves the state
// to private/hvmc and links the public path to it; --remove-state removes
// both, and keeps them when other files are there.
func TestRemoveStateDynamicUser(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		paths, private := dynamicUserState(t, foreign)
		err := removeState(paths)
		_, linkErr := os.Lstat(paths.State)
		_, dirErr := os.Stat(private)
		switch {
		case foreign && (err == nil || linkErr != nil || dirErr != nil):
			t.Errorf("foreign file: %v; link %v, dir %v; want both kept and reported", err, linkErr, dirErr)
		case !foreign && (err != nil || !errors.Is(linkErr, os.ErrNotExist) || !errors.Is(dirErr, os.ErrNotExist)):
			t.Errorf("removeState: %v; link %v, dir %v; want both removed", err, linkErr, dirErr)
		}
	}
}

// dynamicUserState lays out state the way DynamicUser= does: the files in
// private/hvmc and hvmc a relative link to it; foreign adds a file hvmc did
// not write.
func dynamicUserState(t *testing.T, foreign bool) (agent.Paths, string) {
	t.Helper()
	root := t.TempDir()
	private := filepath.Join(root, "private", "hvmc")
	if err := os.MkdirAll(private, 0o700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "hvmc")
	if err := os.Symlink(filepath.Join("private", "hvmc"), state); err != nil {
		t.Fatal(err)
	}
	files := []string{"ca.crt", "serving.pem"}
	if foreign {
		files = append(files, "notes.txt")
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(private, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return agent.Paths{Config: filepath.Join(root, "etc", "agent.yaml"), State: state}, private
}

func TestCheckEnroll(t *testing.T) {
	unit := filepath.Join(t.TempDir(), UnitName)
	if err := checkEnroll(unit); err != nil {
		t.Errorf("no unit: %v", err)
	}
	if err := os.WriteFile(unit, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkEnroll(unit); !errors.Is(err, ErrServiceInstalled) || !strings.Contains(err.Error(), "hvmc agent install") {
		t.Errorf("installed unit: %v", err)
	}
}
