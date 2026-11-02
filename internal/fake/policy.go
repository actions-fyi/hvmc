package fake

import (
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/matfax/hvmc/internal/policy"
)

// toPolicy converts p to the canonical policy model (internal/policy), which
// the fakes use for validation, digests and write checks exactly as a real
// host agent does. A nil p is the empty policy.
func (p *PolicySpec) toPolicy() policy.Policy {
	out := policy.Policy{}
	if p == nil {
		return out
	}
	for _, v := range p.VMs {
		out.VMs = append(out.VMs, policy.VM{Name: v.Name, Fixed: v.Fixed, Min: int64(v.Min), Max: int64(v.Max)})
	}
	return out
}

// fromPolicy converts a canonical policy back to the scenario shape.
func fromPolicy(p policy.Policy) *PolicySpec {
	out := &PolicySpec{}
	for _, v := range p.VMs {
		out.VMs = append(out.VMs, VMPolicySpec{Name: v.Name, Fixed: v.Fixed, Min: Bytes(v.Min), Max: Bytes(v.Max)})
	}
	return out
}

// vmState is the VM state Policy.Allows needs for a write to v.
func vmState(v *VMSpec) policy.VMState {
	st := policy.VMState{Name: v.Name}
	if dm := v.DynamicMemory; dm != nil {
		st.DynamicMemory = true
		st.DynamicMinimum, st.DynamicMaximum = int64(dm.Minimum), int64(dm.Maximum)
	}
	return st
}

// firstRefusal picks the error a write fails with: the first refusal that
// is not a policy bounds violation, else the first bounds violation. Bounds
// are the policy's last rule (Policy.Allows), so a FAILED_PRECONDITION from
// another written setting, or from a VM check outside the policy package,
// takes precedence over them.
func firstRefusal(errs ...error) error {
	var bounds error
	for _, err := range errs {
		switch {
		case err == nil:
		case !errors.Is(err, policy.ErrOutOfBounds):
			return err
		case bounds == nil:
			bounds = err
		}
	}
	return bounds
}

// refusalStatus maps a refusal from Policy.Allows to the error model's code
// (PERMISSION_DENIED or FAILED_PRECONDITION). Other errors, already status
// errors, pass through unchanged.
func refusalStatus(err error) error {
	switch {
	case errors.Is(err, policy.ErrPermissionDenied):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, policy.ErrFailedPrecondition):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return err
	}
}
