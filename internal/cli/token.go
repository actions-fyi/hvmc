package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matfax/hvmc/internal/pki"
)

// clientFactory builds a Kubernetes client from a kubeconfig path (empty:
// default loading rules) and a namespace (empty: the context's namespace),
// and returns the namespace it resolved. The client's scheme includes
// corev1 and v1alpha1.
type clientFactory func(kubeconfig, namespace string) (client.Client, string, error)

// clockFunc adapts a time function to pki.Clock.
type clockFunc func() time.Time

func (f clockFunc) Now() time.Time { return f() }

// newTokenCommand returns the `token` command with its `create` subcommand
// (DESIGN §2, §4.6); newClient connects to the cluster (kubeClient in
// production).
func newTokenCommand(e env, newClient clientFactory) *cobra.Command {
	return group("token", "Manage agent bootstrap tokens", newTokenCreateCommand(e, newClient))
}

func newTokenCreateCommand(e env, newClient clientFactory) *cobra.Command {
	var kubeconfig, namespace, agentName string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Re-issue a bootstrap token for an agent",
		Long: "Replace the agent's bootstrap token with a new one, valid for 24 hours and usable once, and print it.\n" +
			"The token is stored in Secret " + pki.BootstrapSecretPrefix + "<agent> in the release namespace. This also\n" +
			"works for an agent that has enrolled, so that it can enroll again. Needs get on HostConfig and\n" +
			"get, create and update on Secrets in the release namespace.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if agentName == "" {
				return usageErrorf("--agent is required")
			}
			c, ns, err := newClient(kubeconfig, namespace)
			if err != nil {
				return err
			}
			b := &pki.BootstrapTokens{Client: c, Namespace: ns, Clock: clockFunc(e.now)}
			token, expires, err := b.Reissue(cmd.Context(), agentName)
			if err != nil {
				return err
			}
			secret := pki.BootstrapSecretName(agentName)
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"Bootstrap token for agent %q stored in Secret %s/%s, valid until %s. Read it again with:\n"+
					"  kubectl -n %s get secret %s -o jsonpath='{.data.%s}' | base64 -d\n",
				agentName, ns, secret, expires.Format(time.RFC3339), ns, secret, pki.BootstrapTokenKey)
			_, err = fmt.Fprintln(cmd.OutOrStdout(), token)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&agentName, "agent", "", "agent name in HostConfig: the host agent's name or the VM name")
	f.StringVar(&kubeconfig, "kubeconfig", "", "path to the kubeconfig file (default: $KUBECONFIG or ~/.kube/config)")
	f.StringVarP(&namespace, "namespace", "n", "", "release namespace (default: the kubeconfig context's namespace)")
	return cmd
}
