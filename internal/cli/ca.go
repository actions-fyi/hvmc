package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matfax/hvmc/api/v1alpha1"
	"github.com/matfax/hvmc/internal/pki"
)

// readerFactory builds a Kubernetes reader from a kubeconfig path (empty:
// default loading rules) and a namespace (empty: the context's namespace),
// and returns the namespace it resolved.
type readerFactory func(kubeconfig, namespace string) (client.Reader, string, error)

// newCACommand returns the `ca` command with its `export` subcommand
// (DESIGN §2); newReader connects to the cluster (kubeReader in production).
func newCACommand(newReader readerFactory) *cobra.Command {
	return group("ca", "Inspect hvmc's internal certificate authority", newCAExportCommand(newReader))
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
	return kubeClient(kubeconfig, namespace)
}

// kubeClient connects through the caller's kubeconfig with a scheme that
// includes the Kubernetes API types and HostConfig.
func kubeClient(kubeconfig, namespace string) (client.Client, string, error) {
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
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, "", err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, "", err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, "", fmt.Errorf("create client: %w", err)
	}
	return c, ns, nil
}
