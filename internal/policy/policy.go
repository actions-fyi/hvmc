// Package policy is the host agent's write policy (DESIGN §4.3): which VMs a
// host agent may resize and within which bounds.
//
// The same package is used on both sides of the agent API. The host agent
// loads its active policy from its local file, checks every write with
// [Policy.Allows] and reports [Policy.Digest] in GetHost. The coordinator
// derives the policy it wants from HostConfig ([FromHostConfig]), compares
// digests to set PolicyInSync and proposes the derived policy on a mismatch.
// `hvmc agent policy sync` shows [Diff] between the active and the pending
// policy before activating it.
//
// The digest is computed over the canonical form ([Policy.Canonical]), so it
// depends only on the policy's content: not on VM order, YAML formatting,
// key order or how a quantity is spelled ("8Gi" and "8192Mi" are equal).
//
// The package is pure apart from [Load] and [Save]. All sizes are bytes.
package policy

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode"
)

// Unbounded is the max of a writable VM whose writes have no upper bound
// besides the host's physical memory, which Hyper-V enforces itself (see
// [FromHostConfig]).
const Unbounded int64 = math.MaxInt64

// VM is the write permission for one Hyper-V VM.
type VM struct {
	// Name is the Hyper-V VM name.
	Name string
	// Fixed marks a listed but read-only VM: every write is refused. Min and
	// Max are 0 then.
	Fixed bool
	// Min is the lowest value a write may set, in bytes: the startup memory
	// of a static-memory VM, the minimum of a Dynamic Memory VM. Writable VMs
	// need Min > 0.
	Min int64
	// Max is the highest value a write may set, in bytes: the startup memory
	// of a static-memory VM, the maximum of a Dynamic Memory VM. Writable VMs
	// need Max ≥ Min.
	Max int64
}

// Policy is the set of write permissions of one host agent. A VM that is not
// listed is read-only. Reads are never restricted by the policy.
//
// VM order carries no meaning. Names must be unique ([Policy.Validate]).
type Policy struct {
	VMs []VM
}

// ErrInvalidPolicy is returned (wrapped) for a policy that violates the rules
// of [Policy.Validate], and for a policy file or message that cannot be
// decoded.
var ErrInvalidPolicy = errors.New("invalid policy")

// Validate checks that every VM has a unique, non-empty name without control
// characters, that fixed VMs carry no bounds and that writable VMs have
// 0 < Min ≤ Max.
func (p Policy) Validate() error {
	var errs []error
	seen := make(map[string]bool, len(p.VMs))
	for i, vm := range p.VMs {
		switch {
		case vm.Name == "":
			errs = append(errs, fmt.Errorf("vms[%d]: empty name", i))
			continue
		case strings.IndexFunc(vm.Name, unicode.IsControl) >= 0:
			errs = append(errs, fmt.Errorf("vms[%d]: name %q contains control characters", i, vm.Name))
			continue
		case seen[vm.Name]:
			errs = append(errs, fmt.Errorf("vms[%d]: duplicate name %q", i, vm.Name))
			continue
		}
		seen[vm.Name] = true
		switch {
		case vm.Fixed && (vm.Min != 0 || vm.Max != 0):
			errs = append(errs, fmt.Errorf("vm %q: fixed VMs must not set min or max", vm.Name))
		case vm.Fixed:
		case vm.Min <= 0:
			errs = append(errs, fmt.Errorf("vm %q: min must be positive, got %s", vm.Name, formatBytes(vm.Min)))
		case vm.Max < vm.Min:
			errs = append(errs, fmt.Errorf("vm %q: max %s is below min %s", vm.Name, formatBytes(vm.Max), formatBytes(vm.Min)))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrInvalidPolicy, errors.Join(errs...))
}

// Lookup returns the entry for the named VM.
func (p Policy) Lookup(name string) (VM, bool) {
	i := slices.IndexFunc(p.VMs, func(vm VM) bool { return vm.Name == name })
	if i < 0 {
		return VM{}, false
	}
	return p.VMs[i], true
}

// sorted returns the VMs ordered by name; ties (only possible in invalid
// policies) are broken by the remaining fields so the order is total.
func (p Policy) sorted() []VM {
	vms := slices.Clone(p.VMs)
	slices.SortFunc(vms, func(a, b VM) int {
		if c := cmp.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		if a.Fixed != b.Fixed {
			if a.Fixed {
				return 1
			}
			return -1
		}
		if c := cmp.Compare(a.Min, b.Min); c != 0 {
			return c
		}
		return cmp.Compare(a.Max, b.Max)
	})
	return vms
}
