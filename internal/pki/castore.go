package pki

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matfax/hvmc/api/v1alpha1"
)

// CASecretName is the Secret, in the release namespace, holding the CA that
// hvmc creates when pki.existingCASecret is not set.
const CASecretName = "hvmc-ca"

// StoreConfig locates the CA Secret.
type StoreConfig struct {
	// Namespace is the release namespace.
	Namespace string
	// ExistingSecret names an operator-provided CA Secret
	// (pki.existingCASecret, layout kubernetes.io/tls: tls.crt, tls.key).
	// When empty, hvmc uses CASecretName and creates it if absent.
	ExistingSecret string
	// Clock is used for validity periods; nil means SystemClock.
	Clock Clock
}

func (s StoreConfig) secretName() string {
	if s.ExistingSecret != "" {
		return s.ExistingSecret
	}
	return CASecretName
}

func (s StoreConfig) options() []Option {
	if s.Clock == nil {
		return nil
	}
	return []Option{WithClock(s.Clock)}
}

// LoadOrCreateCA returns the CA from its Secret. Without ExistingSecret, a
// missing hvmc-ca Secret is created with a new CA and an owner reference to
// HostConfig `default`, so uninstalling removes it (DESIGN §11); when
// several replicas race, the first Secret written wins and the others load
// it. An operator-provided Secret is never created or modified.
//
// The client's scheme must include corev1 and v1alpha1.
func LoadOrCreateCA(ctx context.Context, c client.Client, cfg StoreConfig) (*CA, error) {
	key := types.NamespacedName{Namespace: cfg.Namespace, Name: cfg.secretName()}
	ca, err := loadCA(ctx, c, key, cfg.options())
	if err == nil || cfg.ExistingSecret != "" || !apierrors.IsNotFound(err) {
		return ca, err
	}
	return createCA(ctx, c, key, cfg.options())
}

func loadCA(ctx context.Context, r client.Reader, key types.NamespacedName, opts []Option) (*CA, error) {
	var s corev1.Secret
	if err := r.Get(ctx, key, &s); err != nil {
		return nil, fmt.Errorf("get CA Secret %s: %w", key, err)
	}
	ca, err := ParseCA(s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey], opts...)
	if err != nil {
		return nil, fmt.Errorf("CA Secret %s: %w", key, err)
	}
	return ca, nil
}

func createCA(ctx context.Context, c client.Client, key types.NamespacedName, opts []Option) (*CA, error) {
	owner, err := hostConfigOwnerRef(ctx, c)
	if err != nil {
		return nil, err
	}
	ca, err := GenerateCA(opts...)
	if err != nil {
		return nil, err
	}
	keyPEM, err := ca.KeyPEM()
	if err != nil {
		return nil, err
	}
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       key.Namespace,
			Name:            key.Name,
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{corev1.TLSCertKey: ca.CertPEM(), corev1.TLSPrivateKeyKey: keyPEM},
	}
	err = c.Create(ctx, s)
	if apierrors.IsAlreadyExists(err) {
		return loadCA(ctx, c, key, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("create CA Secret %s: %w", key, err)
	}
	return ca, nil
}

// hostConfigOwnerRef returns an owner reference to HostConfig `default`.
// It is not a controller reference and does not block owner deletion, so
// writing it needs no permission on HostConfig finalizers.
func hostConfigOwnerRef(ctx context.Context, r client.Reader) (metav1.OwnerReference, error) {
	var hc v1alpha1.HostConfig
	if err := r.Get(ctx, types.NamespacedName{Name: v1alpha1.HostConfigName}, &hc); err != nil {
		return metav1.OwnerReference{}, fmt.Errorf("get HostConfig %q for owner reference: %w", v1alpha1.HostConfigName, err)
	}
	return metav1.OwnerReference{
		APIVersion: v1alpha1.GroupVersion.String(),
		Kind:       "HostConfig",
		Name:       hc.Name,
		UID:        hc.UID,
	}, nil
}

// ensureOwnerRef adds ref to obj's owner references unless an owner with
// the same UID is already present; it reports whether obj changed.
func ensureOwnerRef(obj metav1.Object, ref metav1.OwnerReference) bool {
	refs := obj.GetOwnerReferences()
	for _, r := range refs {
		if r.UID == ref.UID {
			return false
		}
	}
	obj.SetOwnerReferences(append(refs, ref))
	return true
}
