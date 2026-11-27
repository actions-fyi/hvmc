package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/matfax/hvmc/internal/agent"
	"github.com/matfax/hvmc/internal/agent/guest"
	"github.com/matfax/hvmc/internal/agent/host"
	"github.com/matfax/hvmc/internal/agent/svc"
	"github.com/matfax/hvmc/internal/policy"
	"github.com/matfax/hvmc/internal/version"
)

// agentServices maps a role to the constructor of its gRPC service: the
// host agent (internal/agent/host) and the guest agent
// (internal/agent/guest).
var agentServices = map[agent.Role]func() agent.Service{
	agent.RoleHost:  host.New,
	agent.RoleGuest: func() agent.Service { return guest.New() },
}

// addAgentPathFlags adds --config and --state-dir with the platform's
// defaults (DESIGN §4.5).
func addAgentPathFlags(cmd *cobra.Command) {
	d := agent.DefaultPaths()
	cmd.Flags().String("config", d.Config, "agent configuration file")
	cmd.Flags().String("state-dir", d.State, "directory for keys, certificates, the pending policy and the audit log")
}

func agentPaths(cmd *cobra.Command) agent.Paths {
	p := agent.DefaultPaths()
	p.Config, _ = cmd.Flags().GetString("config")
	p.State, _ = cmd.Flags().GetString("state-dir")
	if p.Logs != "" {
		// The logs live in the state directory (DESIGN §4.5) and move with it.
		p.Logs = filepath.Join(p.State, "logs")
	}
	return p
}

func newAgentEnrollCommand(e env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "enroll",
		Short: "Enroll the agent with the coordinator's CA",
		Long: "Generate the agent's key, send a CSR with the bootstrap token to the enrollment URL and\n" +
			"store the certificate, the pinned CA and the agent's settings. A host agent's proposed\n" +
			"write policy is stored as pending; activate it with `hvmc agent policy sync`.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runAgentEnroll(cmd, e) },
	}
	addEnrollFlags(cmd, "enroll")
	addAgentPathFlags(cmd)
	return cmd
}

func runAgentEnroll(cmd *cobra.Command, e env) error {
	o := agent.EnrollOptions{Now: e.now, RootCAs: e.rootCAs}
	for _, f := range []struct {
		name string
		dst  *string
	}{{"enroll", &o.Server}, {"token", &o.Token}, {"ca-sha256", &o.CASHA256}, {"agent", &o.Agent}} {
		*f.dst, _ = cmd.Flags().GetString(f.name)
		if *f.dst == "" {
			return usageErrorf("--%s is required", f.name)
		}
	}
	role, _ := cmd.Flags().GetString("role")
	o.Role = agent.Role(role)
	if o.Role != agent.RoleHost && o.Role != agent.RoleGuest {
		return usageErrorf("--role must be %s or %s, got %q", agent.RoleHost, agent.RoleGuest, role)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
	defer cancel()
	en, err := agent.Enroll(ctx, o)
	if err != nil {
		return err
	}
	paths := agentPaths(cmd)
	pending, err := en.Write(paths)
	if err != nil {
		return fmt.Errorf("store enrollment: %w", err)
	}
	return printEnrollment(cmd.OutOrStdout(), en, paths, pending)
}

func printEnrollment(w io.Writer, en *agent.Enrollment, paths agent.Paths, pending bool) error {
	var errs []error
	pr := func(format string, args ...any) {
		_, err := fmt.Fprintf(w, format, args...)
		errs = append(errs, err)
	}
	pr("Enrolled agent %q (role %s).\n", en.Config.Name, en.Config.Role)
	pr("Certificate serial %x, valid until %s.\n", en.Leaf.SerialNumber, en.Leaf.NotAfter.UTC().Format(time.RFC3339))
	pr("Wrote %s and %s.\n", paths.Config, paths.State)
	if pending {
		store := agent.OpenPolicyStore(paths, slog.New(slog.DiscardHandler))
		proposed, _ := store.Pending()
		pr("\nProposed write policy, stored as pending and NOT active:\n%s", policy.Diff(store.Active(), proposed))
		pr("Review and activate it on this machine with `hvmc agent policy sync`.\n")
	}
	return errors.Join(errs...)
}

func newAgentRunCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the agent in the foreground",
		Args:  noArgs,
		RunE:  runAgent,
	}
	addAgentPathFlags(cmd)
	return cmd
}

// runAgent runs the agent in the foreground until SIGINT or SIGTERM, or,
// when the service manager (the Windows Service Control Manager, systemd)
// started it, as the service.
func runAgent(cmd *cobra.Command, _ []string) error {
	paths := agentPaths(cmd)
	isService, err := svc.IsService()
	if err != nil {
		return err
	}
	stderr := cmd.ErrOrStderr()
	if isService && !svc.LogsToStderr {
		stderr = nil // no console: the Event Log and the log file
	}
	log, closeLog, err := agent.NewLogger(agent.LogOptions{Stderr: stderr, Dir: paths.Logs})
	if err != nil {
		return err
	}
	defer func() { _ = closeLog() }()
	clk := agent.SystemClock()
	build := func(hooks svc.Hooks) (svc.Agent, error) {
		cfg, err := agent.LoadConfig(paths.Config)
		if err != nil {
			return nil, err
		}
		newService, ok := agentServices[cfg.Role]
		if !ok {
			return nil, fmt.Errorf("%s: role %s: %w", cmd.CommandPath(), cfg.Role, ErrNotImplemented)
		}
		return agent.New(agent.Options{
			Config: cfg, Paths: paths, Service: newService(), Clock: clk, Logger: log, Version: version.String(),
			OnTick: hooks.OnTick, OnReadyChange: hooks.OnReadyChange,
		})
	}
	if isService {
		return svc.RunService(log, clk, build)
	}
	a, err := build(svc.Hooks{})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return a.Run(ctx)
}
