package pki

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matfax/hvmc/api/v1alpha1"
)

// Bootstrap token Secrets (DESIGN §4.6).
//
// Every agent HostConfig declares (spec.agents[].name for the host agent,
// spec.vms[].name for a guest agent) gets a Secret hvmc-bootstrap-<agent>
// in the release namespace until it has enrolled. The token is stored in
// data key `token` as is, so that the operator can read it for the install
// command (NOTES.txt); the enrollment handler compares digests in constant
// time. The Secret also records:
//
//   - annotation hvmc.fax.fyi/token-expires-at: the end of the token's 24 h
//     validity (RFC 3339, UTC). An expired token is replaced by a new one.
//   - annotation hvmc.fax.fyi/enrolled-at: set when the token is used. A
//     successful enrollment removes the `token` key and sets this annotation
//     in one update that is conditional on the resourceVersion the handler
//     read, so of two requests with the same token only one can succeed and
//     the token can never be used again. A Secret with this annotation marks
//     the agent as enrolled; the reconciler leaves it alone, and only
//     `hvmc token create` issues a new token for it.
//
// Every Secret carries label hvmc.fax.fyi/bootstrap-agent=<agent> and an
// owner reference to HostConfig `default` (DESIGN §11).

// Names, keys and the validity of bootstrap tokens.
const (
	// BootstrapSecretPrefix prefixes the agent name in the Secret's name.
	BootstrapSecretPrefix = "hvmc-bootstrap-" //nolint:gosec // G101: a Secret name prefix, not a credential
	// BootstrapTokenKey is the data key holding the token.
	BootstrapTokenKey = "token"
	// BootstrapAgentLabel names the agent a bootstrap Secret belongs to.
	BootstrapAgentLabel = "hvmc.fax.fyi/bootstrap-agent"
	// BootstrapExpiresAnnotation holds the token's expiry (RFC 3339).
	BootstrapExpiresAnnotation = "hvmc.fax.fyi/token-expires-at"
	// BootstrapEnrolledAnnotation holds the time the token was used
	// (RFC 3339); its presence marks the agent as enrolled.
	BootstrapEnrolledAnnotation = "hvmc.fax.fyi/enrolled-at"
	// BootstrapTokenValidity is how long a bootstrap token is valid.
	BootstrapTokenValidity = 24 * time.Hour

	// tokenBytes is the token's entropy: 256 bits.
	tokenBytes = 32
)

// BootstrapSecretName returns the name of an agent's bootstrap token Secret.
// Agent names are DNS-1123 labels, so they are used as is.
func BootstrapSecretName(agent string) string { return BootstrapSecretPrefix + agent }

// NewBootstrapToken returns a new random token: 256 bits from crypto/rand,
// base64url-encoded without padding (43 characters).
func NewBootstrapToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate bootstrap token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// declaredAgent is an agent HostConfig declares.
type declaredAgent struct {
	name string
	role string // EnrollRoleHost or EnrollRoleGuest
	port int32
}

// declaredAgents returns the host agents (spec.agents) followed by one
// guest agent per VM (spec.vms).
func declaredAgents(hc *v1alpha1.HostConfig) []declaredAgent {
	out := make([]declaredAgent, 0, len(hc.Spec.Agents)+len(hc.Spec.VMs))
	for _, a := range hc.Spec.Agents {
		out = append(out, declaredAgent{name: a.Name, role: EnrollRoleHost, port: a.Port})
	}
	for i := range hc.Spec.VMs {
		vm := &hc.Spec.VMs[i]
		a := declaredAgent{name: vm.Name, role: EnrollRoleGuest}
		if vm.GuestAgent != nil {
			a.port = vm.GuestAgent.Port
		}
		out = append(out, a)
	}
	return out
}

// lookupAgent returns the declared agent with the given name.
func lookupAgent(hc *v1alpha1.HostConfig, name string) (declaredAgent, bool) {
	agents := declaredAgents(hc)
	i := slices.IndexFunc(agents, func(a declaredAgent) bool { return a.name == name })
	if i < 0 {
		return declaredAgent{}, false
	}
	return agents[i], true
}

// BootstrapTokens maintains the bootstrap token Secrets of one HostConfig.
// The HostConfig controller calls Reconcile; `hvmc token create` calls
// Reissue.
type BootstrapTokens struct {
	// Client reads and writes Secrets in Namespace and reads HostConfig; its
	// scheme must include corev1 and v1alpha1.
	Client client.Client
	// Namespace is the release namespace.
	Namespace string
	// Clock is the time source; nil means SystemClock.
	Clock Clock
}

func (b *BootstrapTokens) now() time.Time {
	if b.Clock == nil {
		return SystemClock.Now()
	}
	return b.Clock.Now()
}

// Reconcile ensures a valid token Secret for every agent of hc that has not
// enrolled: it creates missing Secrets, replaces expired or unreadable
// tokens with new ones and adds a missing owner reference. Secrets of agents
// hc no longer declares are deleted; with a nil hc (HostConfig deleted)
// every bootstrap Secret in the namespace is deleted, which garbage
// collection would do as well.
//
// It returns the earliest expiry of the tokens it leaves in place; the
// caller requeues for that time. The zero time means no token expires.
func (b *BootstrapTokens) Reconcile(ctx context.Context, hc *v1alpha1.HostConfig) (time.Time, error) {
	declared := map[string]bool{}
	var next time.Time
	if hc != nil {
		owner := hostConfigRef(hc)
		for _, a := range declaredAgents(hc) {
			declared[a.name] = true
			exp, err := b.ensure(ctx, owner, a.name)
			if err != nil {
				return time.Time{}, err
			}
			if !exp.IsZero() && (next.IsZero() || exp.Before(next)) {
				next = exp
			}
		}
	}
	return next, b.prune(ctx, declared)
}

// ensure brings one agent's Secret up to date and returns its token's
// expiry (zero for an enrolled agent).
func (b *BootstrapTokens) ensure(ctx context.Context, owner metav1.OwnerReference, agent string) (time.Time, error) {
	var s corev1.Secret
	err := b.Client.Get(ctx, b.key(agent), &s)
	if apierrors.IsNotFound(err) {
		return b.create(ctx, owner, agent)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("get bootstrap Secret %s: %w", b.key(agent), err)
	}
	changed := ensureOwnerRef(&s, owner)
	var exp time.Time
	switch {
	case isEnrolled(&s):
	case tokenUsable(&s, b.now()):
		exp, _ = tokenExpiry(&s)
	default:
		if exp, err = setNewToken(&s, b.now()); err != nil {
			return time.Time{}, err
		}
		changed = true
	}
	if !changed {
		return exp, nil
	}
	if err := b.Client.Update(ctx, &s); err != nil {
		return time.Time{}, fmt.Errorf("update bootstrap Secret %s: %w", b.key(agent), err)
	}
	return exp, nil
}

func (b *BootstrapTokens) create(ctx context.Context, owner metav1.OwnerReference, agent string) (time.Time, error) {
	s := b.newSecret(owner, agent)
	exp, err := setNewToken(s, b.now())
	if err != nil {
		return time.Time{}, err
	}
	if err := b.Client.Create(ctx, s); err != nil {
		return time.Time{}, fmt.Errorf("create bootstrap Secret %s: %w", b.key(agent), err)
	}
	return exp, nil
}

func (b *BootstrapTokens) newSecret(owner metav1.OwnerReference, agent string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       b.Namespace,
			Name:            BootstrapSecretName(agent),
			Labels:          map[string]string{BootstrapAgentLabel: agent},
			OwnerReferences: []metav1.OwnerReference{owner},
		},
		Type: corev1.SecretTypeOpaque,
	}
}

// prune deletes the bootstrap Secrets of agents not in declared.
func (b *BootstrapTokens) prune(ctx context.Context, declared map[string]bool) error {
	var list corev1.SecretList
	if err := b.Client.List(ctx, &list, client.InNamespace(b.Namespace), client.HasLabels{BootstrapAgentLabel}); err != nil {
		return fmt.Errorf("list bootstrap Secrets: %w", err)
	}
	for i := range list.Items {
		s := &list.Items[i]
		if declared[s.Labels[BootstrapAgentLabel]] {
			continue
		}
		if err := b.Client.Delete(ctx, s); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete bootstrap Secret %s/%s: %w", s.Namespace, s.Name, err)
		}
	}
	return nil
}

// Reissue issues a new bootstrap token for a declared agent, enrolled or
// not, replacing any earlier token (`hvmc token create`). It returns the
// token and its expiry. An agent HostConfig `default` does not declare
// yields ErrUnknownAgent.
func (b *BootstrapTokens) Reissue(ctx context.Context, agent string) (string, time.Time, error) {
	var hc v1alpha1.HostConfig
	if err := b.Client.Get(ctx, types.NamespacedName{Name: v1alpha1.HostConfigName}, &hc); err != nil {
		return "", time.Time{}, fmt.Errorf("get HostConfig %q: %w", v1alpha1.HostConfigName, err)
	}
	if _, ok := lookupAgent(&hc, agent); !ok {
		return "", time.Time{}, fmt.Errorf("%w: %q", ErrUnknownAgent, agent)
	}
	owner := hostConfigRef(&hc)
	var s corev1.Secret
	err := b.Client.Get(ctx, b.key(agent), &s)
	switch {
	case apierrors.IsNotFound(err):
		return b.writeNewToken(ctx, b.newSecret(owner, agent), true)
	case err != nil:
		return "", time.Time{}, fmt.Errorf("get bootstrap Secret %s: %w", b.key(agent), err)
	}
	ensureOwnerRef(&s, owner)
	if s.Labels == nil {
		s.Labels = map[string]string{}
	}
	s.Labels[BootstrapAgentLabel] = agent
	delete(s.Annotations, BootstrapEnrolledAnnotation)
	return b.writeNewToken(ctx, &s, false)
}

// writeNewToken puts a new token into s and creates or updates it.
func (b *BootstrapTokens) writeNewToken(ctx context.Context, s *corev1.Secret, create bool) (string, time.Time, error) {
	exp, err := setNewToken(s, b.now())
	if err != nil {
		return "", time.Time{}, err
	}
	if create {
		err = b.Client.Create(ctx, s)
	} else {
		err = b.Client.Update(ctx, s)
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("write bootstrap Secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	return string(s.Data[BootstrapTokenKey]), exp, nil
}

func (b *BootstrapTokens) key(agent string) types.NamespacedName {
	return types.NamespacedName{Namespace: b.Namespace, Name: BootstrapSecretName(agent)}
}

// setNewToken stores a new token valid from now in s and returns its expiry.
func setNewToken(s *corev1.Secret, now time.Time) (time.Time, error) {
	token, err := NewBootstrapToken()
	if err != nil {
		return time.Time{}, err
	}
	exp := now.Add(BootstrapTokenValidity).UTC().Truncate(time.Second)
	if s.Data == nil {
		s.Data = map[string][]byte{}
	}
	s.Data[BootstrapTokenKey] = []byte(token)
	if s.Annotations == nil {
		s.Annotations = map[string]string{}
	}
	s.Annotations[BootstrapExpiresAnnotation] = exp.Format(time.RFC3339)
	return exp, nil
}

// markEnrolled removes the token from s and records the enrollment.
func markEnrolled(s *corev1.Secret, now time.Time) {
	delete(s.Data, BootstrapTokenKey)
	delete(s.Annotations, BootstrapExpiresAnnotation)
	if s.Annotations == nil {
		s.Annotations = map[string]string{}
	}
	s.Annotations[BootstrapEnrolledAnnotation] = now.UTC().Format(time.RFC3339)
}

func isEnrolled(s *corev1.Secret) bool {
	_, ok := s.Annotations[BootstrapEnrolledAnnotation]
	return ok
}

// tokenExpiry parses the expiry annotation.
func tokenExpiry(s *corev1.Secret) (time.Time, bool) {
	exp, err := time.Parse(time.RFC3339, s.Annotations[BootstrapExpiresAnnotation])
	return exp, err == nil
}

// tokenUsable reports whether s holds a token that is unexpired at now and
// not used.
func tokenUsable(s *corev1.Secret, now time.Time) bool {
	if isEnrolled(s) || len(s.Data[BootstrapTokenKey]) == 0 {
		return false
	}
	exp, ok := tokenExpiry(s)
	return ok && now.Before(exp)
}

// errTokenUsed is returned when a token was consumed concurrently.
var errTokenUsed = errors.New("bootstrap token already used")
