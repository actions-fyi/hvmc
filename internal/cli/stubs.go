package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// The commands in this file are declared with their documented flags
// (DESIGN §2, §4.5, §4.6) but not built yet; they fail with
// ErrNotImplemented.

// stub returns a command that fails with ErrNotImplemented.
func stub(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return fmt.Errorf("%s: %w", cmd.CommandPath(), ErrNotImplemented)
		},
	}
}

func newServeCommand() *cobra.Command {
	return stub("serve", "Run the coordinator")
}

func newAgentCommand() *cobra.Command {
	install := stub("install", "Install the agent as a native system service and enroll it")
	install.Flags().String("role", "", "agent role: host or guest")
	addEnrollFlags(install, "enroll")

	enroll := stub("enroll", "Enroll the agent with the coordinator's CA")
	addEnrollFlags(enroll, "server")

	return group("agent", "Run and manage a host or guest agent",
		stub("run", "Run the agent in the foreground"),
		install,
		stub("uninstall", "Remove the agent's system service"),
		enroll,
		stub("status", "Print service state, health checks and certificate expiry"),
		group("policy", "Manage the host agent's local write policy",
			stub("sync", "Show the proposed policy's diff and activate it after confirmation"),
		),
	)
}

// addEnrollFlags adds the flags of enrollment (DESIGN §4.6); urlFlag names
// the enrollment URL flag.
func addEnrollFlags(cmd *cobra.Command, urlFlag string) {
	cmd.Flags().String(urlFlag, "", "enrollment URL")
	cmd.Flags().String("token", "", "single-use bootstrap token")
	cmd.Flags().String("ca-sha256", "", "SHA-256 fingerprint of the hvmc CA certificate")
}

func newCheckCommand() *cobra.Command {
	c := stub("check", "Check connectivity, identity, health and policy of every agent")
	c.Flags().Bool("probe-resize", false, "grow and shrink a probe VM through the production path")
	c.Flags().String("vm", "", "probe VM (with --probe-resize)")
	return c
}

func newTestCommand() *cobra.Command {
	return group("test", "Run and inspect the built-in active tests",
		stub("run", "Start a test run"),
		stub("list", "List test runs"),
		stub("logs", "Show a test run's steps and measurements"),
	)
}

func newCACommand() *cobra.Command {
	return group("ca", "Inspect the hvmc certificate authority",
		stub("export", "Print the CA certificate and its SHA-256 fingerprint"),
	)
}

func newTokenCommand() *cobra.Command {
	create := stub("create", "Re-issue a bootstrap token for an agent")
	create.Flags().String("agent", "", "agent name")
	return group("token", "Manage agent bootstrap tokens", create)
}
