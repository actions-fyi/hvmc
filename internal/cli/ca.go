// Package cli holds the implementations of hvmc's subcommands.
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matfax/hvmc/internal/pki"
)

// readerFactory builds a Kubernetes reader from a kubeconfig path (empty:
// default loading rules) and a namespace (empty: the context's namespace),
// and returns the namespace it resolved.
type readerFactory func(kubeconfig, namespace string) (client.Reader, string, error)

// NewCACommand returns the `ca` command with its `export` subcommand
// (DESIGN §2), reading through the caller's kubeconfig.
func NewCACommand() *cobra.Command {
	return newCACommand(kubeReader)
}

func newCACommand(newReader readerFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ca",
		Short: "Inspect hvmc's internal certificate authority",
	}
	cmd.AddCommand(newCAExportCommand(newReader))
	return cmd
}

func newCAExportCommand(newReader readerFactory) *cobra.Command {
	var kubeconfig, namespace, secret string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Print the CA certificate and its SHA-256 fingerprint",
		Long: "Print the CA certificate (PEM) and its SHA-256 fingerprint, the value agents pin with --ca-sha256.\n" +
			"Reads ConfigMap " + pki.CABundleConfigMapName + " in the release namespace and falls back to the CA Secret.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, ns, err := newReader(kubeconfig, namespace)
			if err != nil {
				return err
			}
			cert, err := pki.ReadCACertificate(cmd.Context(), r, ns, secret)
			if err != nil {
				return err
			}
			return pki.WriteCAExport(cmd.OutOrStdout(), cert)
		},
	}
	f := cmd.Flags()
	f.StringVar(&kubeconfig, "kubeconfig", "", "path to the kubeconfig file (default: $KUBECONFIG or ~/.kube/config)")
	f.StringVarP(&namespace, "namespace", "n", "", "release namespace (default: the kubeconfig context's namespace)")
	f.StringVar(&secret, "secret", pki.CASecretName, "CA Secret used when the ConfigMap is absent (pki.existingCASecret)")
	return cmd
}

func kubeReader(kubeconfig, namespace string) (client.Reader, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = kubeconfig
	overrides := &clientcmd.ConfigOverrides{Context: clientcmdapi.Context{Namespace: namespace}}
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("load kubeconfig: %w", err)
	}
	ns, _, err := cc.Namespace()
	if err != nil {
		return nil, "", fmt.Errorf("resolve namespace: %w", err)
	}
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		return nil, "", fmt.Errorf("create client: %w", err)
	}
	return c, ns, nil
}
