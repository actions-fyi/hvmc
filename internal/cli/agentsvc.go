package cli

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/matfax/hvmc/internal/agent"
	"github.com/matfax/hvmc/internal/agent/svc"
)

const agentInstallLong = `Install the agent as a native system service (DESIGN §4.5), run as
Administrator. On the first install it enrolls with the coordinator (the
same flags as ` + "`hvmc agent enroll`" + `) and, for the host role, shows the proposed
write policy and activates it after confirmation. Then it copies this binary
to the system location, registers the service, its Event Log source, its
firewall rules and (host role) its Hyper-V Administrators membership, starts
it and waits until the agent reports ready.

Running it again upgrades in place: the configuration, keys and state are
kept, so the enrollment flags are not needed. The host role requires
PowerShell 7; without it nothing is changed.`

func newAgentInstallCommand(e env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install or upgrade the agent as a native system service and enroll it",
		Long:  agentInstallLong,
		Args:  noArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return runAgentInstall(cmd, e) },
	}
	addEnrollFlags(cmd, "enroll")
	addAgentPathFlags(cmd)
	return cmd
}

func runAgentInstall(cmd *cobra.Command, e env) error {
	o := svc.InstallOptions{
		RootCAs: e.rootCAs, Paths: agentPaths(cmd), Defaults: agent.DefaultPaths(), Clock: e.clock(),
		In: cmd.InOrStdin(), Out: cmd.OutOrStdout(), CanRun: canRun,
	}
	for _, f := range []struct {
		name string
		dst  *string
	}{{"enroll", &o.Enroll}, {"token", &o.Token}, {"ca-sha256", &o.CASHA256}, {"agent", &o.Agent}} {
		*f.dst, _ = cmd.Flags().GetString(f.name)
	}
	role, _ := cmd.Flags().GetString("role")
	o.Role = agent.Role(role)
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate this binary: %w", err)
	}
	o.Source = exe
	p, err := e.platform(o.Clock)
	if err != nil {
		return err
	}
	return usageIf(svc.Install(cmd.Context(), p, o))
}

// canRun refuses roles this binary has no agent for yet.
func canRun(role agent.Role) error {
	if _, ok := agentServices[role]; !ok {
		return fmt.Errorf("role %s: %w", role, ErrNotImplemented)
	}
	return nil
}

// usageIf turns svc's argument errors into usage errors (exit code 2).
func usageIf(err error) error {
	if errors.Is(err, svc.ErrUsage) {
		return &usageError{err: err}
	}
	return err
}

func newAgentUninstallCommand(e env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the agent's system service",
		Long: "Stop and remove the service, its firewall rules, its Event Log source, its Hyper-V\n" +
			"Administrators membership and the installed binary, run as Administrator. The\n" +
			"configuration, keys and state stay unless --remove-state is given.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := e.platform(e.clock())
			if err != nil {
				return err
			}
			removeState, _ := cmd.Flags().GetBool("remove-state")
			return svc.Uninstall(cmd.Context(), p, svc.UninstallOptions{
				Paths: agentPaths(cmd), RemoveState: removeState, Out: cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().Bool("remove-state", false, "also remove agent.yaml, the keys, the pending policy, the audit log and the logs")
	addAgentPathFlags(cmd)
	return cmd
}

func newAgentStatusCommand(e env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Print service state, health checks and certificate expiry",
		Long:  "Print the service's state, the readiness checks, the serving certificate's expiry and\nthe host's active and pending policy. It fails unless the agent runs and is ready.",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			clk := e.clock()
			p, err := e.platform(clk)
			if err != nil {
				return err
			}
			return svc.Status(cmd.Context(), p, svc.StatusOptions{Paths: agentPaths(cmd), Clock: clk, Out: cmd.OutOrStdout()})
		},
	}
	addAgentPathFlags(cmd)
	return cmd
}

// envClock is the system clock with env.now as its wall clock.
type envClock struct {
	agent.Clock
	now func() time.Time
}

func (c envClock) Now() time.Time { return c.now() }
