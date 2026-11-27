package svc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/matfax/hvmc/internal/agent"
)

// UninstallOptions configure Uninstall.
type UninstallOptions struct {
	// Paths are the agent's files.
	Paths agent.Paths
	// RemoveState also removes agent.yaml, the keys, the pending policy,
	// the audit log and the logs. Without it they stay, so a later install
	// upgrades in place without re-enrolling.
	RemoveState bool
	// Out receives the progress.
	Out io.Writer
}

// Uninstall reverses Install (DESIGN §4.5): it stops and removes the
// service and removes the service account from Hyper-V Administrators, the
// firewall rules, the Event Log source and the installed binary; the state
// only with RemoveState. It runs every step even when one fails (a
// partial install is removed as far as it exists) and returns the errors
// joined.
func Uninstall(ctx context.Context, p Platform, o UninstallOptions) error {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if err := p.CheckPrivileges(); err != nil {
		return err
	}
	out := &printer{w: o.Out}
	steps := []step{
		{"Stopped the service", func() error { return stopIfRunning(ctx, p) }},
		// While the service still exists, so its account resolves.
		{"Removed " + ServiceAccount + " from Hyper-V Administrators", p.RemoveHyperVAdmin},
		{"Removed the firewall rules (group " + FirewallGroup + ")", p.RemoveFirewallRules},
		{"Removed service " + ServiceName, func() error { return p.RemoveService(ctx) }},
		{"Removed the Event Log source " + agent.EventSource, p.RemoveEventSource},
		{"Removed " + p.BinaryPath(), func() error { return removeBinary(p, out) }},
	}
	if o.RemoveState {
		steps = append(steps, step{"Removed the configuration and state in " + o.Paths.State, func() error { return removeState(o.Paths) }})
	}
	var errs []error
	for _, s := range steps {
		if err := s.do(); err != nil {
			errs = append(errs, err)
			out.f("Failed: %s: %v\n", strings.ToLower(s.what[:1])+s.what[1:], err)
			continue
		}
		out.f("%s.\n", s.what)
	}
	if !o.RemoveState {
		out.f("Kept the configuration, keys and state (%s, %s); remove them with --remove-state.\n", o.Paths.Config, o.Paths.State)
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("uninstall incomplete: %w", err)
	}
	return out.err
}

func stopIfRunning(ctx context.Context, p Platform) error {
	state, _, err := p.ServiceState()
	if err != nil || state == NotInstalled || state == Stopped {
		return err
	}
	return p.StopService(ctx)
}

func removeBinary(p Platform, out *printer) error {
	deferred, err := p.RemoveBinary()
	if err == nil && deferred {
		out.f("%s is the running program; it is removed at the next restart.\n", p.BinaryPath())
	}
	return err
}

// removeState removes the files the agent creates: agent.yaml, the
// pinned CA, the serving pairs, the pending policy, the audit log and the
// log files with their rotations, and leftover temporary files of their
// atomic writes. Then it removes the log, state and configuration
// directories if that leaves them empty. It never deletes a tree: a
// mistyped --state-dir loses nothing that hvmc did not write, and a
// directory that still holds other files is kept and reported.
func removeState(paths agent.Paths) error {
	var errs []error
	errs = append(errs, removeOwn(filepath.Dir(paths.Config), filepath.Base(paths.Config)))
	errs = append(errs, removeOwn(paths.State, filepath.Base(paths.CA()), filepath.Base(paths.Serving()),
		filepath.Base(paths.PreviousServing()), filepath.Base(paths.PendingPolicy()), filepath.Base(paths.AuditLog())))
	if paths.Logs != "" {
		errs = append(errs, removeOwn(paths.Logs, logFile), removeEmptyDir(paths.Logs))
	}
	errs = append(errs, removeEmptyDir(paths.State))
	if dir := filepath.Dir(paths.Config); !samePath(dir, paths.State) {
		errs = append(errs, removeEmptyDir(dir))
	}
	return errors.Join(errs...)
}

// removeOwn removes the files of dir that are one of names, a rotation of
// one (name.N) or a temporary file of an atomic write (.name.tmp-*).
func removeOwn(dir string, names ...string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !ownFile(e.Name(), names) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func ownFile(file string, names []string) bool {
	for _, n := range names {
		if strings.EqualFold(file, n) || strings.HasPrefix(file, "."+n+".tmp-") {
			return true
		}
		if rest, ok := strings.CutPrefix(file, n+"."); ok && rest != "" && strings.Trim(rest, "0123456789") == "" {
			return true
		}
	}
	return false
}

// logFile is the agent's log file in Paths.Logs (agent.NewLogger).
const logFile = "agent.log"

// removeEmptyDir removes dir if it is empty; a directory that still holds
// files is an error that names it, a missing one is not.
func removeEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case len(entries) > 0:
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return fmt.Errorf("kept %s: it holds files hvmc did not create (%s)", dir, strings.Join(names, ", "))
	}
	return os.Remove(dir)
}
