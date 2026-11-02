package pki

import (
	"bytes"
	"context"
	"fmt"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// CA bundle ConfigMap (DESIGN §4.6, §7.2): mounted by hvmc-scheduler to
// verify the extender, and read by `hvmc ca export`.
const (
	CABundleConfigMapName = "hvmc-ca-bundle"
	// CABundleKey is the ConfigMap key holding the PEM CA certificate.
	CABundleKey = "ca.crt"
)

// PublishCABundle creates or updates ConfigMap hvmc-ca-bundle in namespace
// with the PEM CA certificate, owner-referenced to HostConfig `default`.
// Concurrent writers (replicas) are retried, so the call is idempotent.
//
// The client's scheme must include corev1 and v1alpha1.
func PublishCABundle(ctx context.Context, c client.Client, namespace string, caPEM []byte) error {
	owner, err := hostConfigOwnerRef(ctx, c)
	if err != nil {
		return err
	}
	retriable := func(err error) bool { return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) }
	err = retry.OnError(retry.DefaultRetry, retriable, func() error {
		cm := &corev1.ConfigMap{}
		cm.Namespace, cm.Name = namespace, CABundleConfigMapName
		_, err := controllerutil.CreateOrUpdate(ctx, c, cm, func() error {
			ensureOwnerRef(cm, owner)
			if cm.Data == nil {
				cm.Data = map[string]string{}
			}
			cm.Data[CABundleKey] = string(caPEM)
			return nil
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("publish ConfigMap %s/%s: %w", namespace, CABundleConfigMapName, err)
	}
	return nil
}

// InjectWebhookCABundle sets clientConfig.caBundle of every webhook in the
// named MutatingWebhookConfiguration to caPEM. It patches only when a value
// differs and retries on conflicting writes (e.g. a concurrent helm upgrade).
func InjectWebhookCABundle(ctx context.Context, c client.Client, name string, caPEM []byte) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cfg admissionregistrationv1.MutatingWebhookConfiguration
		if err := c.Get(ctx, types.NamespacedName{Name: name}, &cfg); err != nil {
			return err
		}
		orig := cfg.DeepCopy()
		changed := false
		for i := range cfg.Webhooks {
			cc := &cfg.Webhooks[i].ClientConfig
			if !bytes.Equal(cc.CABundle, caPEM) {
				cc.CABundle = bytes.Clone(caPEM)
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return c.Patch(ctx, &cfg, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
	})
	if err != nil {
		return fmt.Errorf("inject caBundle into MutatingWebhookConfiguration %q: %w", name, err)
	}
	return nil
}
