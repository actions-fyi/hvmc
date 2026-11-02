package pki

import (
	"context"
	"crypto/x509"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ReadCACertificate returns the CA certificate for `hvmc ca export`. It
// reads ConfigMap hvmc-ca-bundle in namespace (no Secret access needed) and
// falls back to tls.crt of the CA Secret (secretName, default hvmc-ca) when
// the ConfigMap does not exist yet. The private key is never read.
func ReadCACertificate(ctx context.Context, r client.Reader, namespace, secretName string) (*x509.Certificate, error) {
	var cm corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: CABundleConfigMapName}, &cm)
	if err == nil {
		cert, err := ParseCertificatePEM([]byte(cm.Data[CABundleKey]))
		if err != nil {
			return nil, fmt.Errorf("ConfigMap %s/%s: %w", namespace, CABundleConfigMapName, err)
		}
		return cert, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get ConfigMap %s/%s: %w", namespace, CABundleConfigMapName, err)
	}
	if secretName == "" {
		secretName = CASecretName
	}
	var s corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: secretName}, &s); err != nil {
		return nil, fmt.Errorf("get Secret %s/%s: %w", namespace, secretName, err)
	}
	cert, err := ParseCertificatePEM(s.Data[corev1.TLSCertKey])
	if err != nil {
		return nil, fmt.Errorf("secret %s/%s: %w", namespace, secretName, err)
	}
	return cert, nil
}

// WriteCAExport prints the CA certificate as PEM followed by a line with
// its SHA-256 fingerprint (the value for `--ca-sha256`). Text after the
// PEM block is ignored by PEM parsers, so the output is usable as ca.crt.
func WriteCAExport(w io.Writer, cert *x509.Certificate) error {
	if _, err := w.Write(EncodeCertificatePEM(cert)); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "SHA-256 fingerprint: %s\n", Fingerprint(cert))
	return err
}
