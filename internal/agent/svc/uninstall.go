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

// removeState removes the state directory and agent.yaml, and the
// configuration's directory when that is empty then (/etc/hvmc).
func removeState(paths agent.Paths) error {
	errs := []error{os.RemoveAll(paths.State)}
	if err := os.Remove(paths.Config); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	if dir := filepath.Dir(paths.Config); !samePath(dir, paths.State) {
		// Fails harmlessly when the directory holds anything else.
		if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) && !isNotEmpty(dir) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func isNotEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}
