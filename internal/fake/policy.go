package fake

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
)

// DigestFunc computes the digest of a policy. The canonical function is
// defined by internal/policy (WP 1.7); until then the fakes use
// PlaceholderDigest. Coordinator tests must configure the fake with the same
// function the coordinator uses (WithPolicyDigest).
type DigestFunc func(*agentv1.Policy) string

// PlaceholderDigest is a stand-in policy digest: SHA-256 (hex) of the
// deterministic protobuf encoding of the policy with its VMs sorted by name.
// It is order-independent like the canonical digest but not compatible with
// it.
func PlaceholderDigest(p *agentv1.Policy) string {
	c := &agentv1.Policy{}
	if p != nil {
		c = proto.CloneOf(p)
	}
	slices.SortFunc(c.Vms, func(a, b *agentv1.VMPolicy) int { return strings.Compare(a.GetName(), b.GetName()) })
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	if err != nil {
		panic(fmt.Sprintf("fake: marshal policy: %v", err)) // plain message, cannot fail
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// validatePolicy applies the structural rules of policy.proto.
func validatePolicy(p *agentv1.Policy) error {
	if p == nil {
		return errors.New("policy is required")
	}
	var errs []error
	seen := map[string]bool{}
	for _, v := range p.GetVms() {
		name := v.GetName()
		switch {
		case name == "":
			errs = append(errs, errors.New("VM name is required"))
		case seen[name]:
			errs = append(errs, fmt.Errorf("VM %q listed twice", name))
		}
		seen[name] = true
		if v.GetFixed() {
			if v.GetMinBytes() != 0 || v.GetMaxBytes() != 0 {
				errs = append(errs, fmt.Errorf("VM %q: fixed VMs carry no min or max", name))
			}
			continue
		}
		if v.GetMinBytes() <= 0 || v.GetMaxBytes() < v.GetMinBytes() {
			errs = append(errs, fmt.Errorf("VM %q: need 0 < min ≤ max", name))
		}
	}
	return errors.Join(errs...)
}

func toProtoPolicy(p *PolicySpec) *agentv1.Policy {
	out := &agentv1.Policy{}
	if p == nil {
		return out
	}
	for _, v := range p.VMs {
		out.Vms = append(out.Vms, &agentv1.VMPolicy{
			Name: v.Name, Fixed: v.Fixed, MinBytes: int64(v.Min), MaxBytes: int64(v.Max),
		})
	}
	return out
}

func fromProtoPolicy(p *agentv1.Policy) *PolicySpec {
	out := &PolicySpec{}
	for _, v := range p.GetVms() {
		out.VMs = append(out.VMs, VMPolicySpec{
			Name: v.GetName(), Fixed: v.GetFixed(), Min: Bytes(v.GetMinBytes()), Max: Bytes(v.GetMaxBytes()),
		})
	}
	return out
}

func (p *PolicySpec) rule(vm string) (VMPolicySpec, bool) {
	if p == nil {
		return VMPolicySpec{}, false
	}
	for _, r := range p.VMs {
		if r.Name == vm {
			return r, true
		}
	}
	return VMPolicySpec{}, false
}
