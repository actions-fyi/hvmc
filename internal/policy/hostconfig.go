package policy

import (
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/matfax/hvmc/api/v1alpha1"
)

// defaultReserve mirrors the CRD default of HostConfig.spec.reserve, for
// HostConfigs read from files without API-server defaulting.
var defaultReserve = resource.MustParse("4Gi")

// FromHostConfig derives the policy the coordinator proposes to the host
// agent (DESIGN §4.3, §4.6). Every VM in spec.vms is listed:
//
//   - guestOS linux, mode fixed, or an unknown mode: fixed (read-only).
//   - mode static or dynamic with a policy: writable within
//     [policy.min, policy.max].
//   - mode static or dynamic without a policy, including probe VMs: writable
//     (lease grows and probe resizes still need the agent to write; such a
//     VM only has no reconciler), with min = base, because the ledger never
//     targets less than floor ≥ base + margin, and max = capacity − reserve
//     when spec.capacity is set, because the ledger never commits more than
//     that in total; with capacity 0 (read from the host) the spec holds no
//     ceiling and max is Unbounded. max is never below min.
//
// VMs on the host that are not in spec.vms stay unlisted, i.e. read-only.
// Fractional byte values are rounded up. A nil HostConfig yields the empty
// policy. The result is valid for every HostConfig that passes the CRD's
// validation.
func FromHostConfig(hc *v1alpha1.HostConfig) Policy {
	p := Policy{VMs: []VM{}}
	if hc == nil {
		return p
	}
	ceiling := Unbounded
	if c := hc.Spec.Capacity; c != nil && c.Sign() > 0 {
		reserve := defaultReserve
		if hc.Spec.Reserve != nil {
			reserve = *hc.Spec.Reserve
		}
		ceiling = c.Value() - reserve.Value()
	}
	for i := range hc.Spec.VMs {
		spec := &hc.Spec.VMs[i]
		vm := VM{Name: spec.Name}
		switch {
		case spec.GuestOS == v1alpha1.GuestOSLinux,
			spec.Mode != v1alpha1.VMModeStatic && spec.Mode != v1alpha1.VMModeDynamic:
			vm.Fixed = true
		case spec.Policy != nil:
			vm.Min, vm.Max = spec.Policy.Min.Value(), spec.Policy.Max.Value()
		default:
			vm.Min, vm.Max = spec.Base.Value(), max(ceiling, spec.Base.Value())
		}
		p.VMs = append(p.VMs, vm)
	}
	return p
}
