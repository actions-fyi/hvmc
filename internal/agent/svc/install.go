package svc

import (
	"bufio"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/matfax/hvmc/internal/agent"
	"github.com/matfax/hvmc/internal/fileacl"
	"github.com/matfax/hvmc/internal/policy"
)

// DefaultReadyTimeout bounds how long install waits for the started agent
// to report ready.
const DefaultReadyTimeout = 2 * time.Minute

var (
	// ErrUsage marks a mistake in install's arguments.
	ErrUsage = errors.New("invalid arguments")
	// ErrPolicyDeclined means the operator did not confirm the host's
	// proposed write policy, so install stopped before the service.
	ErrPolicyDeclined = errors.New("the proposed write policy was not confirmed")
	// ErrNotReady means the agent did not report ready.
	ErrNotReady = errors.New("the agent is not ready")
)

// InstallOptions configure Install.
type InstallOptions struct {
	// Role is host or guest. It may be empty when the machine is already
	// enrolled; it then comes from agent.yaml.
	Role agent.Role
	// Agent, Enroll, Token and CASHA256 are the enrollment flags (DESIGN
	// §4.6). They are required for the first install only: an enrolled
	// machine keeps its keys, and the token is single-use.
	Agent, Enroll, Token, CASHA256 string
	// RootCAs verify the enrollment endpoint; nil means the system roots.
	RootCAs *x509.CertPool
	// Source is the binary to install, normally the running one.
	Source string
	// Paths are the agent's files; Defaults the platform's, so the service
	// gets --config and --state-dir only when they differ.
	Paths, Defaults agent.Paths
	// CanRun reports why this binary cannot run a role's agent; nil
	// allows every role.
	CanRun func(agent.Role) error
	// Clock times the waits; HTTP reads /readyz (nil: a client with a
	// short timeout).
	Clock agent.Clock
	HTTP  *http.Client
	// In answers the policy confirmation; Out receives the progress.
	In  io.Reader
	Out io.Writer
	// ReadyTimeout bounds the wait for readiness (0: DefaultReadyTimeout).
	ReadyTimeout time.Duration

	// noStart leaves the service stopped (tests that adjust it first).
	noStart bool
}

// plan is what Install found before changing anything.
type plan struct {
	role     agent.Role
	enrolled bool
}

// Install installs or upgrades the agent as a service (DESIGN §4.5). It
// checks everything it can before it changes anything: the arguments,
// the privileges and, for the host role, PowerShell 7. Then it enrolls (a
// machine that is enrolled already keeps its configuration, keys and
// state), asks the operator to confirm a host's first write policy, stops
// a running service, installs the binary, registers the Event Log source,
// creates or updates the service, adds a host's service account to Hyper-V
// Administrators, writes the firewall rules, starts the service and waits
// until the agent reports ready.
func Install(ctx context.Context, p Platform, o InstallOptions) error {
	o.defaults()
	pl, err := prepare(p, o)
	if err != nil {
		return err
	}
	out := &printer{w: o.Out}
	if err := fileacl.MkdirSecure(o.Paths.State); err != nil {
		return err
	}
	if err := enroll(ctx, o, pl, out); err != nil {
		return err
	}
	if pl.role == agent.RoleHost {
		if err := confirmPolicy(ctx, o, out); err != nil {
			return err
		}
	}
	cfg, err := agent.LoadConfig(o.Paths.Config)
	if err != nil {
		return err
	}
	if err := configureMachine(ctx, p, o, pl, cfg, out); err != nil {
		return err
	}
	if o.noStart {
		return out.err
	}
	out.f("Starting the service...\n")
	if err := p.StartService(ctx); err != nil {
		return fmt.Errorf("start service %s: %w", ServiceName, err)
	}
	if err := waitReady(ctx, p, o, cfg, out); err != nil {
		return err
	}
	return out.err
}

func (o *InstallOptions) defaults() {
	if o.Clock == nil {
		o.Clock = agent.SystemClock()
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 5 * time.Second}
	}
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = DefaultReadyTimeout
	}
	if o.In == nil {
		o.In = strings.NewReader("")
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
}

// prepare checks the arguments and the machine without changing anything.
func prepare(p Platform, o InstallOptions) (plan, error) {
	if o.Role != "" && o.Role != agent.RoleHost && o.Role != agent.RoleGuest {
		return plan{}, fmt.Errorf("%w: --role must be %s or %s, got %q", ErrUsage, agent.RoleHost, agent.RoleGuest, o.Role)
	}
	pl, err := installed(o.Paths)
	if err != nil {
		return plan{}, err
	}
	switch {
	case pl.enrolled && o.Role != "" && o.Role != pl.role:
		return plan{}, fmt.Errorf("%w: this machine is enrolled as a %s agent (%s), not %s; uninstall with --remove-state first",
			ErrUsage, pl.role, o.Paths.Config, o.Role)
	case !pl.enrolled:
		if err := checkEnrollFlags(o); err != nil {
			return plan{}, err
		}
		pl.role = o.Role
	}
	if o.Source == "" {
		return plan{}, fmt.Errorf("%w: no binary to install", ErrUsage)
	}
	if o.CanRun != nil {
		if err := o.CanRun(pl.role); err != nil {
			return plan{}, err
		}
	}
	if err := p.CheckPrivileges(); err != nil {
		return plan{}, err
	}
	if pl.role == agent.RoleHost {
		if _, err := p.FindPwsh(); err != nil {
			return plan{}, fmt.Errorf("refusing to install the host agent before anything is changed: %w", err)
		}
	}
	return pl, nil
}

// installed reports whether the machine is enrolled: agent.yaml loads and
// the pinned CA and the serving pair exist.
func installed(paths agent.Paths) (plan, error) {
	cfg, err := agent.LoadConfig(paths.Config)
	if errors.Is(err, fs.ErrNotExist) {
		return plan{}, nil
	}
	if err != nil {
		return plan{}, err
	}
	for _, f := range []string{paths.CA(), paths.Serving()} {
		if _, err := os.Stat(f); err != nil {
			return plan{}, nil //nolint:nilerr // a missing file means not enrolled yet
		}
	}
	return plan{role: cfg.Role, enrolled: true}, nil
}

func checkEnrollFlags(o InstallOptions) error {
	var missing []string
	for _, f := range []struct{ name, v string }{
		{"role", string(o.Role)}, {"agent", o.Agent}, {"enroll", o.Enroll}, {"token", o.Token}, {"ca-sha256", o.CASHA256},
	} {
		if f.v == "" {
			missing = append(missing, "--"+f.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s required: the machine is not enrolled yet", ErrUsage, strings.Join(missing, ", "))
	}
	if _, err := agent.ParseFingerprint(o.CASHA256); err != nil {
		return fmt.Errorf("%w: --ca-sha256: %w", ErrUsage, err)
	}
	return nil
}

// enroll enrolls a machine that is not enrolled yet.
func enroll(ctx context.Context, o InstallOptions, pl plan, out *printer) error {
	if pl.enrolled {
		out.f("Already enrolled as %s agent; keeping the configuration, keys and state in %s.\n", pl.role, o.Paths.State)
		if o.Token != "" {
			out.f("The enrollment flags are ignored; re-enroll with `hvmc agent enroll`.\n")
		}
		return nil
	}
	ectx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	en, err := agent.Enroll(ectx, agent.EnrollOptions{
		Server: o.Enroll, Token: o.Token, CASHA256: o.CASHA256, Agent: o.Agent, Role: pl.role,
		RootCAs: o.RootCAs, Now: o.Clock.Now,
	})
	if err != nil {
		return err
	}
	if _, err := en.Write(o.Paths); err != nil { //nolint:contextcheck // Write does local file I/O only
		return fmt.Errorf("store enrollment: %w", err)
	}
	out.f("Enrolled agent %q (role %s); certificate valid until %s.\n",
		en.Config.Name, en.Config.Role, en.Leaf.NotAfter.UTC().Format(time.RFC3339))
	return nil
}

// confirmPolicy asks the operator to confirm the host's first write policy:
// a pending policy while the active one allows no write. Later proposals
// are left to `hvmc agent policy sync`.
func confirmPolicy(ctx context.Context, o InstallOptions, out *printer) error {
	store := agent.OpenPolicyStore(o.Paths, slog.New(slog.DiscardHandler))
	if err := store.Check(ctx); err != nil {
		return err
	}
	proposed, ok := store.Pending()
	if !ok {
		return nil
	}
	if len(store.Active().VMs) > 0 {
		out.f("A proposed write policy is pending; review and activate it with `hvmc agent policy sync`.\n")
		return nil
	}
	digest := proposed.Digest()
	changes := policy.Diff(policy.Policy{}, proposed)
	out.f("\nProposed write policy %s (+ allowed writes):\n%s\n", digest, changes)
	if !changes.Empty() {
		yes, err := confirm(o.In, out, "Activate this policy and install the service? [y/N]: ")
		if err != nil {
			return err
		}
		if !yes {
			return fmt.Errorf("%w: nothing else was changed; the enrollment is stored and the policy stays pending in %s; "+
				"run `hvmc agent install` again or activate it with `hvmc agent policy sync`", ErrPolicyDeclined, o.Paths.PendingPolicy())
		}
	}
	if err := store.Activate(digest); err != nil {
		return fmt.Errorf("activate policy %s: %w", digest, err)
	}
	out.f("Activated policy %s.\n", digest)
	return nil
}

// confirm asks a yes/no question; anything but y or yes, including the end
// of input, is no.
func confirm(r io.Reader, out *printer, question string) (bool, error) {
	out.f("%s", question)
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	if errors.Is(err, io.EOF) {
		out.f("\n")
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// configureMachine stops a running service and installs or updates the
// binary, the Event Log source, the service, the group membership and the
// firewall rules.
func configureMachine(ctx context.Context, p Platform, o InstallOptions, pl plan, cfg agent.Config, out *printer) error {
	rules, err := firewallRules(cfg)
	if err != nil {
		return err
	}
	state, _, err := p.ServiceState()
	if err != nil {
		return err
	}
	if state != NotInstalled && state != Stopped {
		out.f("Stopping the running service for the upgrade...\n")
		if err := p.StopService(ctx); err != nil {
			return fmt.Errorf("stop service %s: %w", ServiceName, err)
		}
	}
	spec := serviceSpec(pl.role, p.BinaryPath(), o.Paths, o.Defaults)
	steps := []step{
		{"Installed " + p.BinaryPath(), func() error { return p.InstallBinary(o.Source) }},
		{"Registered the Event Log source " + agent.EventSource, p.RegisterEventSource},
		{"Configured service " + ServiceName + " (" + ServiceAccount + ", delayed automatic start)", func() error { return p.ConfigureService(spec) }},
	}
	if pl.role == agent.RoleHost {
		steps = append(steps, step{"Added " + ServiceAccount + " to Hyper-V Administrators (" + HyperVAdminsSID + ")", p.AddHyperVAdmin})
	}
	steps = append(steps, step{firewallSummary(rules), func() error { return p.SetFirewallRules(rules) }})
	for _, s := range steps {
		if err := s.do(); err != nil {
			return err
		}
		out.f("%s.\n", s.what)
	}
	return nil
}

// step is one change to the machine and what to print once it is done.
type step struct {
	what string
	do   func() error
}

func firewallSummary(rules []FirewallRule) string {
	parts := make([]string, 0, len(rules))
	for _, r := range rules {
		parts = append(parts, fmt.Sprintf("TCP %d", r.Port))
	}
	from := "any source"
	if len(rules) > 0 && len(rules[0].RemoteAddresses) > 0 {
		from = strings.Join(rules[0].RemoteAddresses, ", ")
	}
	return "Allowed inbound " + strings.Join(parts, " and ") + " from " + from + " in the firewall"
}

// waitReady waits until the agent's /readyz reports ready and prints the
// report. It fails when the service stops or the timeout passes.
func waitReady(ctx context.Context, p Platform, o InstallOptions, cfg agent.Config, out *printer) error {
	url, err := healthURL(cfg)
	if err != nil {
		return err
	}
	var last string
	var lastErr error
	err = waitUntil(ctx, o.Clock, o.ReadyTimeout, "the agent to report ready", func() (bool, error) {
		state, _, err := p.ServiceState()
		if err != nil {
			return false, err
		}
		if state == Stopped {
			return false, fmt.Errorf("service %s stopped while starting (see the Event Log, source %s)", ServiceName, agent.EventSource)
		}
		report, ready, err := readiness(ctx, o.HTTP, url)
		if err != nil {
			// The health port is not up yet: keep waiting.
			lastErr = err
			return false, nil //nolint:nilerr // retried until the timeout, then reported
		}
		last = report
		return ready, nil
	})
	if err != nil {
		if last == "" && lastErr != nil {
			last = lastErr.Error() + "\n"
		}
		return fmt.Errorf("%w: %w; last readiness:\n%s", ErrNotReady, err, last)
	}
	out.f("The agent is ready:\n%s", last)
	return nil
}

// printer writes to w and keeps the first error.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) f(format string, args ...any) {
	if _, err := fmt.Fprintf(p.w, format, args...); err != nil && p.err == nil {
		p.err = err
	}
}
