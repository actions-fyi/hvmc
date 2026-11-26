package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"

	"github.com/matfax/hvmc/internal/agent"
	"github.com/matfax/hvmc/internal/policy"
)

const policySyncLong = `Show how the pending write policy, proposed by the coordinator, differs from
the active one in agent.yaml, and activate it after confirmation (DESIGN
§4.3). Run it on the Hyper-V host as Administrator: agent.yaml is read-only
to the agent service. The running agent picks up the activated policy at its
next main-loop tick, without a restart.`

func newAgentPolicySyncCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Show the proposed policy's diff and activate it after confirmation",
		Long:  policySyncLong,
		Args:  noArgs,
		RunE:  runAgentPolicySync,
	}
	addAgentPathFlags(cmd)
	return cmd
}

func runAgentPolicySync(cmd *cobra.Command, _ []string) error {
	paths := agentPaths(cmd)
	store := agent.OpenPolicyStore(paths, slog.New(slog.DiscardHandler))
	w := cmd.OutOrStdout()
	pending, ok := store.Pending()
	if !ok {
		_, err := fmt.Fprintln(w, "No pending policy; the active policy is unchanged.")
		return err
	}
	digest := pending.Digest()
	changes := policy.Diff(store.Active(), pending)
	if err := printPolicyDiff(cmd.Context(), w, store, digest, changes); err != nil {
		return err
	}
	if !changes.Empty() {
		yes, err := confirm(cmd.InOrStdin(), w, "Activate this policy? [y/N]: ")
		if err != nil {
			return err
		}
		if !yes {
			_, err := fmt.Fprintln(w, "Not activated; the policy stays pending.")
			return err
		}
	}
	if err := store.Activate(digest); err != nil {
		return fmt.Errorf("activate policy %s: %w", digest, err)
	}
	_, err := fmt.Fprintf(w, "Activated policy %s in %s.\n", digest, paths.Config)
	return err
}

func printPolicyDiff(ctx context.Context, w io.Writer, store *agent.PolicyStore, digest string, changes policy.Changes) error {
	var errs []error
	pr := func(format string, args ...any) {
		_, err := fmt.Fprintf(w, format, args...)
		errs = append(errs, err)
	}
	if err := store.Check(ctx); err != nil {
		pr("Warning: the active policy cannot be read (%v); the agent refuses every write until a policy is activated.\n", err)
	}
	pr("Active policy:  %s\n", store.ActiveDigest())
	pr("Pending policy: %s\n", digest)
	if changes.Empty() {
		pr("The pending policy grants the same writes as the active one.\n")
	} else {
		pr("\nChanges (+ added, - removed, ~ changed):\n%s\n", changes)
	}
	return errors.Join(errs...)
}

// confirm asks a yes/no question; anything but y or yes, including the
// end of input, is no.
func confirm(r io.Reader, w io.Writer, question string) (bool, error) {
	if _, err := fmt.Fprint(w, question); err != nil {
		return false, err
	}
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		if errors.Is(err, io.EOF) {
			_, _ = fmt.Fprintln(w)
		}
		return false, nil
	}
}
