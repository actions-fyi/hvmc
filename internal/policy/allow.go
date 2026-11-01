package policy

import (
	"errors"
	"fmt"
)

// Op is a write the host agent can perform on a VM (DESIGN §4.2).
type Op int

// Write operations. SetDynamicLimits changes up to two settings; the agent
// checks each one it changes as its own Op.
const (
	// OpSetStartup sets the startup memory of a static-memory VM
	// (SetStartup).
	OpSetStartup Op = iota + 1
	// OpSetDynamicMinimum sets the minimum of a Dynamic Memory VM
	// (SetDynamicLimits.minimum_bytes). It may only be lowered.
	OpSetDynamicMinimum
	// OpSetDynamicMaximum sets the maximum of a Dynamic Memory VM
	// (SetDynamicLimits.maximum_bytes). It may only be raised.
	OpSetDynamicMaximum
)

// String returns the operation's name as used in error messages.
func (o Op) String() string {
	switch o {
	case OpSetStartup:
		return "set startup memory"
	case OpSetDynamicMinimum:
		return "set Dynamic Memory minimum"
	case OpSetDynamicMaximum:
		return "set Dynamic Memory maximum"
	default:
		return fmt.Sprintf("Op(%d)", int(o))
	}
}

// VMState is a VM's current memory configuration as the host agent reads it
// from Hyper-V immediately before a write. Allows needs it for the rules that
// depend on the VM rather than on the policy: SetStartup only on static
// memory, SetDynamicLimits only on Dynamic Memory, DM minimum only lowered
// and DM maximum only raised.
type VMState struct {
	// Name is the Hyper-V VM name.
	Name string
	// DynamicMemory is true if Dynamic Memory is enabled on the VM.
	DynamicMemory bool
	// DynamicMinimum is the current DM minimum in bytes (DM VMs only).
	DynamicMinimum int64
	// DynamicMaximum is the current DM maximum in bytes (DM VMs only).
	DynamicMaximum int64
}

// Error classes of a refused write. They correspond to the gRPC status codes
// of the host agent's error model (host.proto): the agent maps an error
// matching ErrPermissionDenied to PERMISSION_DENIED and one matching
// ErrFailedPrecondition to FAILED_PRECONDITION.
var (
	// ErrPermissionDenied: the policy does not grant the write.
	ErrPermissionDenied = errors.New("permission denied")
	// ErrFailedPrecondition: the VM's mode or current state rules the
	// write out.
	ErrFailedPrecondition = errors.New("failed precondition")
)

// Reasons for a refused write. Each wraps its class, so errors.Is matches
// both the reason and the class.
var (
	// ErrVMNotListed: the VM is not in the policy (PermissionDenied).
	ErrVMNotListed = fmt.Errorf("%w: VM is not in the policy", ErrPermissionDenied)
	// ErrOutOfBounds: the value is outside the VM's [min, max]
	// (PermissionDenied).
	ErrOutOfBounds = fmt.Errorf("%w: value outside the policy's bounds", ErrPermissionDenied)
	// ErrUnknownOp: the operation is not one of the defined Ops
	// (PermissionDenied: whatever the policy does not know, it denies).
	ErrUnknownOp = fmt.Errorf("%w: unknown operation", ErrPermissionDenied)
	// ErrFixedVM: the VM is listed as fixed and therefore read-only
	// (FailedPrecondition).
	ErrFixedVM = fmt.Errorf("%w: VM is fixed (read-only)", ErrFailedPrecondition)
	// ErrMemoryType: SetStartup on a Dynamic Memory VM or SetDynamicLimits on
	// a static-memory VM (FailedPrecondition).
	ErrMemoryType = fmt.Errorf("%w: operation does not match the VM's memory type", ErrFailedPrecondition)
	// ErrDirection: the DM minimum would be raised or the DM maximum lowered
	// (FailedPrecondition).
	ErrDirection = fmt.Errorf("%w: Dynamic Memory minimum may only be lowered and maximum only raised", ErrFailedPrecondition)
)

// WriteError is a write refused by [Policy.Allows]. It unwraps to one of the
// reason errors above (and through it to its class).
type WriteError struct {
	// VM is the VM the write was for.
	VM string
	// Op and Bytes describe the refused write.
	Op    Op
	Bytes int64
	// Reason is ErrVMNotListed, ErrOutOfBounds, ErrUnknownOp, ErrFixedVM,
	// ErrMemoryType or ErrDirection.
	Reason error
	// Detail adds the values the decision was based on (bounds, current
	// value); may be empty.
	Detail string
}

// Error implements error.
func (e *WriteError) Error() string {
	msg := fmt.Sprintf("policy refuses to %s of VM %q to %s: %v", e.Op, e.VM, formatBytes(e.Bytes), e.Reason)
	if e.Detail != "" {
		msg += " (" + e.Detail + ")"
	}
	return msg
}

// Unwrap returns the reason.
func (e *WriteError) Unwrap() error { return e.Reason }

// Allows decides whether the host agent may perform op, setting the value to
// bytes, on the VM in state vm. It returns nil when the write is allowed and
// a *WriteError otherwise.
//
// The rules, checked in this order:
//  1. The VM must be listed (ErrVMNotListed) and op must be known
//     (ErrUnknownOp).
//  2. A fixed VM is read-only (ErrFixedVM).
//  3. OpSetStartup needs static memory, the DM ops need Dynamic Memory
//     (ErrMemoryType).
//  4. The DM minimum may only be lowered and the DM maximum only raised;
//     setting the current value is allowed (ErrDirection).
//  5. bytes must lie within the VM's [Min, Max] (ErrOutOfBounds).
//
// Request validation that does not depend on the policy (positive value,
// 2 MiB alignment, minimum not above maximum) is the agent's job and happens
// before Allows.
func (p Policy) Allows(vm VMState, op Op, bytes int64) error {
	deny := func(reason error, format string, args ...any) error {
		return &WriteError{VM: vm.Name, Op: op, Bytes: bytes, Reason: reason, Detail: fmt.Sprintf(format, args...)}
	}
	entry, ok := p.Lookup(vm.Name)
	if !ok {
		return deny(ErrVMNotListed, "")
	}
	if op < OpSetStartup || op > OpSetDynamicMaximum {
		return deny(ErrUnknownOp, "")
	}
	if entry.Fixed {
		return deny(ErrFixedVM, "")
	}
	if (op != OpSetStartup) != vm.DynamicMemory {
		return deny(ErrMemoryType, "Dynamic Memory enabled: %t", vm.DynamicMemory)
	}
	switch {
	case op == OpSetDynamicMinimum && bytes > vm.DynamicMinimum:
		return deny(ErrDirection, "current minimum %s", formatBytes(vm.DynamicMinimum))
	case op == OpSetDynamicMaximum && bytes < vm.DynamicMaximum:
		return deny(ErrDirection, "current maximum %s", formatBytes(vm.DynamicMaximum))
	}
	if bytes < entry.Min || bytes > entry.Max {
		return deny(ErrOutOfBounds, "allowed [%s, %s]", formatBound(entry.Min), formatBound(entry.Max))
	}
	return nil
}
