//go:build windows

package svc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/matfax/hvmc/internal/agent"
	"github.com/matfax/hvmc/internal/agent/host"
	"github.com/matfax/hvmc/internal/fileacl"
)

// Timeouts of the service manager waits.
const (
	startTimeout  = 2 * time.Minute
	stopTimeout   = agent.DrainTimeout + 30*time.Second
	removeTimeout = time.Minute
)

// eventSourceKey is the registry key of the Event Log source.
const eventSourceKey = `SYSTEM\CurrentControlSet\Services\EventLog\Application\` + agent.EventSource

// winPlatform is the Windows Platform: SCM through svc/mgr, the Event Log
// through svc/eventlog, the group through netapi32 and the firewall
// through the built-in NetSecurity cmdlets (DESIGN §2.1).
type winPlatform struct {
	clk agent.Clock
}

// caseInsensitivePaths is true: Windows compares paths case-insensitively.
const caseInsensitivePaths = true

// NewPlatform returns the platform's service integration.
func NewPlatform(clk agent.Clock) (Platform, error) {
	if clk == nil {
		clk = agent.SystemClock()
	}
	return &winPlatform{clk: clk}, nil
}

// Describe names the Windows service's steps.
func (*winPlatform) Describe() Labels { return windowsLabels() }

// CheckSpec accepts every service: SCM takes any binary and arguments.
func (*winPlatform) CheckSpec(ServiceSpec) error { return nil }

func (*winPlatform) CheckPrivileges() error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("run this command as Administrator (from an elevated prompt)")
	}
	return nil
}

func (*winPlatform) FindPwsh() (string, error) { return host.FindPwsh() }

// BinaryPath is %ProgramFiles%\hvmc\hvmc.exe.
func (*winPlatform) BinaryPath() string {
	pf := os.Getenv("ProgramFiles")
	if pf == "" {
		pf = `C:\Program Files`
	}
	return filepath.Join(pf, "hvmc", "hvmc.exe")
}

// InstallBinary copies src next to the destination and renames it into
// place, so an interrupted copy never leaves a truncated binary. The
// directory keeps the Program Files ACL (users read and execute only).
func (w *winPlatform) InstallBinary(src string) error {
	dst := w.BinaryPath()
	if samePath(src, dst) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	in, err := os.Open(src) //nolint:gosec // G304: the running binary or the operator's choice.
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".hvmc.exe.tmp-*")
	if err != nil {
		return err
	}
	_, err = io.Copy(tmp, in)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("install %s: %w", dst, err)
	}
	return nil
}

func (w *winPlatform) RemoveBinary() (bool, error) {
	dst := w.BinaryPath()
	dir := filepath.Dir(dst)
	if exe, err := os.Executable(); err == nil && samePath(exe, dst) {
		// A running image cannot be deleted: remove it at the next restart.
		for _, p := range []string{dst, dir} {
			p16, err := windows.UTF16PtrFromString(p)
			if err != nil {
				return false, err
			}
			if err := windows.MoveFileEx(p16, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT); err != nil {
				return false, fmt.Errorf("schedule removal of %s: %w", p, err)
			}
		}
		return true, nil
	}
	for _, p := range []string{dst, dir} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

func (*winPlatform) RegisterEventSource() error {
	exists, err := eventSourceExists()
	if err != nil || exists {
		return err
	}
	if err := eventlog.InstallAsEventCreate(agent.EventSource, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
		return fmt.Errorf("register Event Log source %s: %w", agent.EventSource, err)
	}
	return nil
}

func (*winPlatform) RemoveEventSource() error {
	exists, err := eventSourceExists()
	if err != nil || !exists {
		return err
	}
	if err := eventlog.Remove(agent.EventSource); err != nil {
		return fmt.Errorf("remove Event Log source %s: %w", agent.EventSource, err)
	}
	return nil
}

func eventSourceExists() (bool, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, eventSourceKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_ = k.Close()
	return true, nil
}

// withService opens the service; s is nil when it does not exist.
func withService(f func(m *mgr.Mgr, s *mgr.Service) error) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service manager: %w", err)
	}
	defer func() { _ = m.Disconnect() }()
	s, err := m.OpenService(ServiceName)
	switch {
	case errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST):
		return f(m, nil)
	case err != nil:
		return fmt.Errorf("open service %s: %w", ServiceName, err)
	}
	defer func() { _ = s.Close() }()
	return f(m, s)
}

func (*winPlatform) ServiceState() (State, uint32, error) {
	var (
		state = NotInstalled
		pid   uint32
	)
	err := withService(func(_ *mgr.Mgr, s *mgr.Service) error {
		if s == nil {
			return nil
		}
		q, err := s.Query()
		if err != nil {
			return fmt.Errorf("query service %s: %w", ServiceName, err)
		}
		state, pid = stateOf(q.State), q.ProcessId
		return nil
	})
	return state, pid, err
}

func stateOf(s svc.State) State {
	switch s {
	case svc.Stopped:
		return Stopped
	case svc.StartPending:
		return StartPending
	case svc.StopPending:
		return StopPending
	case svc.Running:
		return Running
	default:
		return Other
	}
}

// serviceConfig is the SCM configuration of spec.
func serviceConfig(spec ServiceSpec) mgr.Config {
	cmd := syscall.EscapeArg(spec.Binary)
	for _, a := range spec.Args {
		cmd += " " + syscall.EscapeArg(a)
	}
	return mgr.Config{
		ServiceType:      windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true,
		ErrorControl:     mgr.ErrorNormal,
		BinaryPathName:   cmd,
		DisplayName:      spec.DisplayName,
		Description:      spec.Description,
		ServiceStartName: spec.Account,
		Dependencies:     spec.Dependencies,
		SidType:          windows.SERVICE_SID_TYPE_UNRESTRICTED,
	}
}

// ConfigureService creates the service, or updates an existing one to
// spec, and sets its dependencies, recovery actions and preshutdown
// timeout.
func (*winPlatform) ConfigureService(spec ServiceSpec) error {
	return withService(func(m *mgr.Mgr, s *mgr.Service) error {
		c := serviceConfig(spec)
		if s == nil {
			created, err := m.CreateService(spec.Name, spec.Binary, c, spec.Args...)
			if err != nil {
				return fmt.Errorf("create service %s: %w", spec.Name, err)
			}
			defer func() { _ = created.Close() }()
			s = created
		} else if err := s.UpdateConfig(c); err != nil {
			return fmt.Errorf("update service %s: %w", spec.Name, err)
		}
		return configureExtras(s, spec)
	})
}

func configureExtras(s *mgr.Service, spec ServiceSpec) error {
	// UpdateConfig leaves the dependencies alone when there are none.
	if err := setDependencies(s.Handle, spec.Dependencies); err != nil {
		return fmt.Errorf("set dependencies of %s: %w", spec.Name, err)
	}
	actions := make([]mgr.RecoveryAction, 0, len(spec.RecoveryDelays))
	for _, d := range spec.RecoveryDelays {
		actions = append(actions, mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: d})
	}
	if err := s.SetRecoveryActions(actions, toUint32(int64(spec.RecoveryReset/time.Second))); err != nil {
		return fmt.Errorf("set recovery actions of %s: %w", spec.Name, err)
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("set recovery on stops with errors of %s: %w", spec.Name, err)
	}
	if err := setPreshutdownTimeout(s.Handle, spec.PreshutdownTimeout); err != nil {
		return fmt.Errorf("set preshutdown timeout of %s: %w", spec.Name, err)
	}
	return nil
}

// setDependencies replaces the service's dependencies; none clears them.
func setDependencies(h windows.Handle, deps []string) error {
	var block []uint16
	for _, d := range deps {
		u, err := windows.UTF16FromString(d) // NUL-terminated
		if err != nil {
			return err
		}
		block = append(block, u...)
	}
	block = append(block, 0)
	if len(deps) == 0 {
		block = append(block, 0)
	}
	return windows.ChangeServiceConfig(h, windows.SERVICE_NO_CHANGE, windows.SERVICE_NO_CHANGE, windows.SERVICE_NO_CHANGE,
		nil, nil, nil, &block[0], nil, nil, nil)
}

// preshutdownInfo is SERVICE_PRESHUTDOWN_INFO.
type preshutdownInfo struct{ timeoutMillis uint32 }

func setPreshutdownTimeout(h windows.Handle, d time.Duration) error {
	info := preshutdownInfo{timeoutMillis: toUint32(d.Milliseconds())}
	return windows.ChangeServiceConfig2(h, windows.SERVICE_CONFIG_PRESHUTDOWN_INFO, (*byte)(unsafe.Pointer(&info))) //nolint:gosec // G103: SERVICE_PRESHUTDOWN_INFO for ChangeServiceConfig2
}

// StartService starts the service and waits until SCM reports it running,
// which the agent does once it serves (RunService).
func (w *winPlatform) StartService(ctx context.Context) error {
	return withService(func(_ *mgr.Mgr, s *mgr.Service) error {
		if s == nil {
			return fmt.Errorf("service %s is not installed", ServiceName)
		}
		if err := s.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			return err
		}
		return waitUntil(ctx, w.clk, startTimeout, "the service to run", func() (bool, error) {
			q, err := s.Query()
			if err != nil {
				return false, err
			}
			if q.State == svc.Stopped {
				return false, fmt.Errorf("service stopped (exit code %d, service-specific %d); see the Event Log, source %s",
					q.Win32ExitCode, q.ServiceSpecificExitCode, agent.EventSource)
			}
			return q.State == svc.Running, nil
		})
	})
}

func (w *winPlatform) StopService(ctx context.Context) error {
	return withService(func(_ *mgr.Mgr, s *mgr.Service) error {
		if s == nil {
			return nil
		}
		return stopAndWait(ctx, w.clk, s)
	})
}

// stopAndWait sends stop until the service accepts it (it may still be
// starting) and waits until it stopped.
func stopAndWait(ctx context.Context, clk agent.Clock, s *mgr.Service) error {
	return waitUntil(ctx, clk, stopTimeout, "the service to stop", func() (bool, error) {
		q, err := s.Query()
		if err != nil {
			return false, err
		}
		if q.State == svc.Stopped {
			return true, nil
		}
		if q.State == svc.Running || q.State == svc.StartPending || q.State == svc.PausePending || q.State == svc.Paused {
			_, err := s.Control(svc.Stop)
			if err != nil && !errors.Is(err, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL) && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
				return false, err
			}
		}
		return false, nil
	})
}

// RemoveService stops and deletes the service and waits until SCM has
// removed it.
func (w *winPlatform) RemoveService(ctx context.Context) error {
	err := withService(func(_ *mgr.Mgr, s *mgr.Service) error {
		if s == nil {
			return nil
		}
		if err := stopAndWait(ctx, w.clk, s); err != nil {
			return err
		}
		if err := s.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			return fmt.Errorf("delete service %s: %w", ServiceName, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return waitUntil(ctx, w.clk, removeTimeout, "the service to be removed", func() (bool, error) {
		state, _, err := w.ServiceState()
		return state == NotInstalled, err
	})
}

// Hyper-V Administrators membership through netapi32 (DESIGN §2.1). The
// group is found by its well-known SID; its name, which the API needs, is
// looked up, because it is localized.
var (
	netapi32                    = windows.NewLazySystemDLL("netapi32.dll")
	procNetLocalGroupAddMembers = netapi32.NewProc("NetLocalGroupAddMembers")
	procNetLocalGroupDelMembers = netapi32.NewProc("NetLocalGroupDelMembers")
)

// localGroupMembersInfo0 is LOCALGROUP_MEMBERS_INFO_0: a member by SID.
type localGroupMembersInfo0 struct{ sid *windows.SID }

// hyperVAdminsName returns the local name of BUILTIN\Hyper-V
// Administrators.
func hyperVAdminsName() (string, error) {
	sid, err := windows.StringToSid(HyperVAdminsSID)
	if err != nil {
		return "", err
	}
	name, _, _, err := sid.LookupAccount("")
	return name, err
}

func (*winPlatform) AddHyperVAdmin() error {
	group, err := hyperVAdminsName()
	if err != nil {
		return fmt.Errorf("look up Hyper-V Administrators (%s): %w", HyperVAdminsSID, err)
	}
	return changeMember(procNetLocalGroupAddMembers, group, windows.ERROR_MEMBER_IN_ALIAS)
}

func (*winPlatform) RemoveHyperVAdmin() error {
	group, err := hyperVAdminsName()
	if errors.Is(err, windows.ERROR_NONE_MAPPED) {
		return nil // no such group on this machine: nothing to remove
	}
	if err != nil {
		return fmt.Errorf("look up Hyper-V Administrators (%s): %w", HyperVAdminsSID, err)
	}
	return changeMember(procNetLocalGroupDelMembers, group, windows.ERROR_MEMBER_NOT_IN_ALIAS)
}

// changeMember adds or removes the service account's SID; done is the
// status that means it already was in the requested state.
func changeMember(proc *windows.LazyProc, group string, done syscall.Errno) error {
	if err := proc.Find(); err != nil {
		return err
	}
	member, err := windows.StringToSid(fileacl.ServiceSID(ServiceName))
	if err != nil {
		return err
	}
	g, err := windows.UTF16PtrFromString(group)
	if err != nil {
		return err
	}
	info := localGroupMembersInfo0{sid: member}
	r, _, _ := proc.Call(0, uintptr(unsafe.Pointer(g)), 0, uintptr(unsafe.Pointer(&info)), 1) //nolint:gosec // G103: NetLocalGroup*Members(NULL, group, 0, &info, 1)
	if r == 0 || syscall.Errno(r) == done {
		return nil
	}
	return fmt.Errorf("%s %s in %s: %w", proc.Name, ServiceAccount, HyperVAdminsSID, syscall.Errno(r))
}

func (w *winPlatform) SetFirewallRules(rules []FirewallRule) error {
	return runFirewallScript(firewallSetScript, firewallParams{
		Group: FirewallGroup, Program: w.BinaryPath(), Service: ServiceName, Rules: rules,
	})
}

func (*winPlatform) RemoveFirewallRules() error {
	return runFirewallScript(firewallRemoveScript, firewallParams{Group: FirewallGroup})
}
