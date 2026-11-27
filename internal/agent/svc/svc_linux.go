//go:build linux

package svc

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"text/template"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"

	"github.com/matfax/hvmc/internal/agent"
)

// The systemd integration (DESIGN §4.5): the binary in /usr/local/bin, the
// unit hvmc-agent.service rendered from unit.tmpl with fixed values only,
// controlled through systemd's D-Bus API (go-systemd/dbus), and ufw rules
// while ufw is active.
const (
	// LinuxBinaryPath is where install puts the binary.
	LinuxBinaryPath = "/usr/local/bin/hvmc"
	// UnitName is the systemd unit of the agent.
	UnitName = ServiceName + ".service"
	// UnitPath is the unit file install writes.
	UnitPath = "/etc/systemd/system/" + UnitName
	// RestartDelay is the unit's RestartSec.
	RestartDelay = 5 * time.Second
	// StopTimeout is the unit's TimeoutStopSec: the drain plus a margin,
	// like the Windows preshutdown timeout.
	StopTimeout = agent.DrainTimeout + 10*time.Second
)

//go:embed unit.tmpl
var unitTemplate string

var unitTmpl = template.Must(template.New("unit").Option("missingkey=error").Parse(unitTemplate))

// unitData holds the values unit.tmpl renders. Every one is a constant of
// this package; nothing an operator passes reaches the unit.
type unitData struct {
	Path, Description, ExecStart            string
	WatchdogSec, RestartSec, TimeoutStopSec string
}

// errNonDefaultPaths refuses a unit for other paths than the defaults:
// the unit's sandbox (ProtectSystem=strict, StateDirectory=,
// ConfigurationDirectory=) covers only /etc/hvmc and /var/lib/hvmc.
var errNonDefaultPaths = errors.New("the systemd unit supports only the default paths /etc/hvmc/agent.yaml and /var/lib/hvmc; omit --config and --state-dir")

// checkUnitSpec accepts only the service install builds for the default
// paths: the installed binary running `agent run`.
func checkUnitSpec(spec ServiceSpec, binary string) error {
	if spec.Binary != binary {
		return fmt.Errorf("unit binary %q: want %s", spec.Binary, binary)
	}
	if !slices.Equal(spec.Args, []string{"agent", "run"}) {
		return errNonDefaultPaths
	}
	return nil
}

// renderUnit renders the unit file of spec, the service of binary.
func renderUnit(spec ServiceSpec, binary string) ([]byte, error) {
	if err := checkUnitSpec(spec, binary); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	err := unitTmpl.Execute(&b, unitData{
		Path:           UnitPath,
		Description:    DisplayName,
		ExecStart:      binary + " agent run",
		WatchdogSec:    systemdSeconds(WatchdogTimeout),
		RestartSec:     systemdSeconds(RestartDelay),
		TimeoutStopSec: systemdSeconds(StopTimeout),
	})
	return b.Bytes(), err
}

func systemdSeconds(d time.Duration) string { return fmt.Sprintf("%ds", int64(d/time.Second)) }

// systemd is the part of go-systemd's *dbus.Conn the platform uses.
type systemd interface {
	GetUnitPropertiesContext(ctx context.Context, unit string) (map[string]any, error)
	GetUnitTypePropertiesContext(ctx context.Context, unit, unitType string) (map[string]any, error)
	StartUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error)
	StopUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error)
	RestartUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error)
	ReloadContext(ctx context.Context) error
	EnableUnitFilesContext(ctx context.Context, files []string, runtime, force bool) (bool, []dbus.EnableUnitFileChange, error)
	DisableUnitFilesContext(ctx context.Context, files []string, runtime bool) ([]dbus.DisableUnitFileChange, error)
	ResetFailedUnitContext(ctx context.Context, name string) error
	Close()
}

// CheckEnroll refuses `hvmc agent enroll` while the unit is installed: the
// service's state directory belongs to its dynamic user, which systemd
// does not hand files written by root later, so a re-enrollment would
// leave keys the agent cannot read. Re-enrolling an installed machine
// goes through `hvmc agent install` (an upgrade keeps the enrollment) or
// uninstall with --remove-state and install again.
func CheckEnroll() error { return checkEnroll(UnitPath) }

func checkEnroll(unitPath string) error {
	_, err := os.Stat(unitPath)
	switch {
	case err == nil:
		return fmt.Errorf("%w: %s exists; use `hvmc agent install` to upgrade, or `hvmc agent uninstall --remove-state` "+
			"and `hvmc agent install` with the enrollment flags to enroll again", ErrServiceInstalled, unitPath)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return err
	}
}

// linuxPlatform implements Platform with systemd and ufw.
type linuxPlatform struct {
	clk agent.Clock
	// dial connects to systemd's system bus.
	dial func(ctx context.Context) (systemd, error)
	// unitPath and binaryPath are UnitPath and LinuxBinaryPath; tests
	// change them.
	unitPath, binaryPath string
	ufw                  ufw
	euid                 func() int
}

// caseInsensitivePaths is false: Linux paths are case-sensitive.
const caseInsensitivePaths = false

// NewPlatform returns the systemd integration.
func NewPlatform(clk agent.Clock) (Platform, error) {
	return &linuxPlatform{
		clk: clk,
		dial: func(ctx context.Context) (systemd, error) {
			return dbus.NewSystemConnectionContext(ctx)
		},
		unitPath: UnitPath, binaryPath: LinuxBinaryPath,
		ufw:  ufw{path: ufwPath, run: runUFW},
		euid: os.Geteuid,
	}, nil
}

// Describe names the systemd integration's steps; Linux has no Event Log
// source and no Hyper-V Administrators.
func (p *linuxPlatform) Describe() Labels {
	return Labels{
		ConfigureService:    "Configured and enabled unit " + p.unitPath + " (DynamicUser, Type=notify)",
		RemoveService:       "Removed unit " + p.unitPath,
		Firewall:            "in ufw (only while ufw is active)",
		RemoveFirewallRules: "Removed the ufw rules (comment " + ufwComment + ")",
		Logs:                "journalctl -u " + ServiceName,
	}
}

// CheckSpec accepts only the service of the default paths (checkUnitSpec).
func (p *linuxPlatform) CheckSpec(spec ServiceSpec) error { return checkUnitSpec(spec, p.binaryPath) }

func (p *linuxPlatform) CheckPrivileges() error {
	if p.euid() != 0 {
		return errors.New("installing the agent's systemd service needs root; run it with sudo")
	}
	return nil
}

// FindPwsh fails: the host agent manages Hyper-V and runs on Windows only.
func (p *linuxPlatform) FindPwsh() (string, error) {
	return "", errors.New("the host agent runs on the Windows Hyper-V host only; install the guest role on Linux")
}

func (p *linuxPlatform) BinaryPath() string { return p.binaryPath }

// InstallBinary copies src to the binary path through a temporary file in
// the same directory, so the binary is replaced in one rename.
func (p *linuxPlatform) InstallBinary(src string) error {
	if same, err := sameFile(src, p.binaryPath); err != nil || same {
		return err
	}
	in, err := os.Open(src) //nolint:gosec // G304: the running binary, chosen by the caller.
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(p.binaryPath), "."+filepath.Base(p.binaryPath)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p.binaryPath)
}

func sameFile(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(fa, fb), nil
}

// RemoveBinary removes the binary; a running program may be unlinked on
// Linux, so the removal is never deferred.
func (p *linuxPlatform) RemoveBinary() (bool, error) {
	if err := os.Remove(p.binaryPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return false, nil
}

// The Event Log and Hyper-V Administrators do not exist on Linux: journald
// takes the agent's stderr, and the host role is refused (FindPwsh).
func (p *linuxPlatform) RegisterEventSource() error { return nil }
func (p *linuxPlatform) RemoveEventSource() error   { return nil }
func (p *linuxPlatform) AddHyperVAdmin() error      { return nil }
func (p *linuxPlatform) RemoveHyperVAdmin() error   { return nil }

// withSystemd runs f with a connection to systemd.
func (p *linuxPlatform) withSystemd(ctx context.Context, f func(systemd) error) error {
	conn, err := p.dial(ctx)
	if err != nil {
		return fmt.Errorf("connect to systemd: %w", err)
	}
	defer conn.Close()
	return f(conn)
}

func (p *linuxPlatform) ServiceState() (State, uint32, error) {
	var (
		state State
		pid   uint32
	)
	err := p.withSystemd(context.Background(), func(c systemd) error {
		var err error
		state, pid, err = unitState(context.Background(), c)
		return err
	})
	return state, pid, err
}

// unitState maps the unit's LoadState and ActiveState to a State.
func unitState(ctx context.Context, c systemd) (State, uint32, error) {
	props, err := c.GetUnitPropertiesContext(ctx, UnitName)
	if err != nil {
		return "", 0, err
	}
	load, _ := props["LoadState"].(string)
	active, _ := props["ActiveState"].(string)
	if load == "not-found" && (active == "inactive" || active == "") {
		return NotInstalled, 0, nil
	}
	// MainPID is a property of the Service interface, not of Unit.
	svcProps, err := c.GetUnitTypePropertiesContext(ctx, UnitName, "Service")
	if err != nil {
		return "", 0, err
	}
	pid, _ := svcProps["MainPID"].(uint32)
	switch active {
	case "active", "reloading":
		return Running, pid, nil
	case "activating":
		return StartPending, pid, nil
	case "deactivating":
		return StopPending, pid, nil
	case "inactive", "failed":
		return Stopped, 0, nil
	default:
		return Other, pid, nil
	}
}

// ConfigureService writes the unit, reloads systemd and enables the unit,
// so it starts at boot (`systemctl enable`).
func (p *linuxPlatform) ConfigureService(spec ServiceSpec) error {
	unit, err := renderUnit(spec, p.binaryPath)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(p.unitPath, unit, 0o644); err != nil {
		return err
	}
	ctx := context.Background()
	return p.withSystemd(ctx, func(c systemd) error {
		if err := c.ReloadContext(ctx); err != nil {
			return fmt.Errorf("daemon-reload: %w", err)
		}
		if _, _, err := c.EnableUnitFilesContext(ctx, []string{p.unitPath}, false, true); err != nil {
			return fmt.Errorf("enable %s: %w", UnitName, err)
		}
		return nil
	})
}

// StartService starts the unit and waits for the start job. With
// Type=notify the job is done only once the agent sent READY=1.
func (p *linuxPlatform) StartService(ctx context.Context) error {
	return p.withSystemd(ctx, func(c systemd) error {
		_ = c.ResetFailedUnitContext(ctx, UnitName) // a failed unit from an earlier attempt
		return runJob(ctx, "start", func(ch chan<- string) (int, error) { return c.StartUnitContext(ctx, UnitName, "replace", ch) })
	})
}

// StopService stops the unit and waits for the stop job.
func (p *linuxPlatform) StopService(ctx context.Context) error {
	return p.withSystemd(ctx, func(c systemd) error {
		return runJob(ctx, "stop", func(ch chan<- string) (int, error) { return c.StopUnitContext(ctx, UnitName, "replace", ch) })
	})
}

// runJob queues a systemd job and waits for its result.
func runJob(ctx context.Context, what string, queue func(chan<- string) (int, error)) error {
	ch := make(chan string, 1)
	if _, err := queue(ch); err != nil {
		return fmt.Errorf("%s %s: %w", what, UnitName, err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case result := <-ch:
		if result != "done" {
			return fmt.Errorf("%s %s: job %s (see journalctl -u %s)", what, UnitName, result, ServiceName)
		}
		return nil
	}
}

// RemoveService disables the unit, removes its file and reloads systemd.
func (p *linuxPlatform) RemoveService(ctx context.Context) error {
	return p.withSystemd(ctx, func(c systemd) error {
		var errs []error
		if _, err := os.Stat(p.unitPath); err == nil {
			if _, err := c.DisableUnitFilesContext(ctx, []string{UnitName}, false); err != nil {
				errs = append(errs, fmt.Errorf("disable %s: %w", UnitName, err))
			}
		}
		if err := os.Remove(p.unitPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
		if err := c.ReloadContext(ctx); err != nil {
			errs = append(errs, fmt.Errorf("daemon-reload: %w", err))
		}
		_ = c.ResetFailedUnitContext(ctx, UnitName) // forget a failed state
		return errors.Join(errs...)
	})
}

// SetFirewallRules replaces the agent's ufw rules while ufw is active;
// otherwise it touches no firewall (DESIGN §4.5).
func (p *linuxPlatform) SetFirewallRules(rules []FirewallRule) error {
	return p.ufw.set(context.Background(), rules)
}

// RemoveFirewallRules removes the agent's ufw rules, active or not.
func (p *linuxPlatform) RemoveFirewallRules() error {
	return p.ufw.remove(context.Background())
}

// writeFileAtomic writes data to path through a temporary file in its
// directory and a rename.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
