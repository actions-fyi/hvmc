package policy

import (
	"errors"
	"testing"
)

func TestAllows(t *testing.T) {
	p := Policy{VMs: []VM{
		{Name: "fixed", Fixed: true},
		{Name: "static", Min: 6 * gi, Max: 24 * gi},
		{Name: "dm", Min: 2 * gi, Max: 16 * gi},
	}}
	static := VMState{Name: "static"}
	dm := VMState{Name: "dm", DynamicMemory: true, DynamicMinimum: 4 * gi, DynamicMaximum: 8 * gi}

	tests := []struct {
		name  string
		vm    VMState
		op    Op
		bytes int64
		want  error // reason; nil = allowed
		class error
	}{
		// Rule 1: listed VMs only, known ops only.
		{name: "unlisted VM", vm: VMState{Name: "other"}, op: OpSetStartup, bytes: 8 * gi, want: ErrVMNotListed, class: ErrPermissionDenied},
		{name: "unlisted DM VM", vm: VMState{Name: "other", DynamicMemory: true}, op: OpSetDynamicMaximum, bytes: 8 * gi, want: ErrVMNotListed, class: ErrPermissionDenied},
		{name: "unknown op zero", vm: static, op: 0, bytes: 8 * gi, want: ErrUnknownOp, class: ErrPermissionDenied},
		{name: "unknown op high", vm: static, op: OpSetDynamicMaximum + 1, bytes: 8 * gi, want: ErrUnknownOp, class: ErrPermissionDenied},

		// Rule 2: fixed VMs are read-only, whatever the op and value.
		{name: "fixed startup", vm: VMState{Name: "fixed"}, op: OpSetStartup, bytes: 8 * gi, want: ErrFixedVM, class: ErrFailedPrecondition},
		{name: "fixed DM minimum", vm: VMState{Name: "fixed", DynamicMemory: true}, op: OpSetDynamicMinimum, bytes: gi, want: ErrFixedVM, class: ErrFailedPrecondition},
		{name: "fixed DM maximum", vm: VMState{Name: "fixed", DynamicMemory: true}, op: OpSetDynamicMaximum, bytes: gi, want: ErrFixedVM, class: ErrFailedPrecondition},

		// Rule 3: op must match the memory type.
		{name: "startup on DM VM", vm: dm, op: OpSetStartup, bytes: 8 * gi, want: ErrMemoryType, class: ErrFailedPrecondition},
		{name: "DM minimum on static VM", vm: static, op: OpSetDynamicMinimum, bytes: 8 * gi, want: ErrMemoryType, class: ErrFailedPrecondition},
		{name: "DM maximum on static VM", vm: static, op: OpSetDynamicMaximum, bytes: 8 * gi, want: ErrMemoryType, class: ErrFailedPrecondition},

		// Rule 4: DM minimum only lowered, DM maximum only raised.
		{name: "DM minimum raised", vm: dm, op: OpSetDynamicMinimum, bytes: 4*gi + 2*mi, want: ErrDirection, class: ErrFailedPrecondition},
		{name: "DM minimum lowered", vm: dm, op: OpSetDynamicMinimum, bytes: 3 * gi},
		{name: "DM minimum unchanged", vm: dm, op: OpSetDynamicMinimum, bytes: 4 * gi},
		{name: "DM maximum lowered", vm: dm, op: OpSetDynamicMaximum, bytes: 8*gi - 2*mi, want: ErrDirection, class: ErrFailedPrecondition},
		{name: "DM maximum raised", vm: dm, op: OpSetDynamicMaximum, bytes: 12 * gi},
		{name: "DM maximum unchanged", vm: dm, op: OpSetDynamicMaximum, bytes: 8 * gi},

		// Rule 5: within [min, max], both ends inclusive.
		{name: "startup at min", vm: static, op: OpSetStartup, bytes: 6 * gi},
		{name: "startup at max", vm: static, op: OpSetStartup, bytes: 24 * gi},
		{name: "startup grow inside", vm: static, op: OpSetStartup, bytes: 12 * gi},
		{name: "startup below min", vm: static, op: OpSetStartup, bytes: 6*gi - 2*mi, want: ErrOutOfBounds, class: ErrPermissionDenied},
		{name: "startup above max", vm: static, op: OpSetStartup, bytes: 24*gi + 2*mi, want: ErrOutOfBounds, class: ErrPermissionDenied},
		{name: "startup zero", vm: static, op: OpSetStartup, bytes: 0, want: ErrOutOfBounds, class: ErrPermissionDenied},
		{name: "DM minimum lowered below policy min", vm: dm, op: OpSetDynamicMinimum, bytes: gi, want: ErrOutOfBounds, class: ErrPermissionDenied},
		{name: "DM minimum lowered to policy min", vm: dm, op: OpSetDynamicMinimum, bytes: 2 * gi},
		{name: "DM maximum raised above policy max", vm: dm, op: OpSetDynamicMaximum, bytes: 20 * gi, want: ErrOutOfBounds, class: ErrPermissionDenied},
		{name: "DM maximum raised to policy max", vm: dm, op: OpSetDynamicMaximum, bytes: 16 * gi},
		{
			name: "DM maximum outside bounds but not raised", op: OpSetDynamicMaximum, bytes: 20 * gi,
			vm: VMState{Name: "dm", DynamicMemory: true, DynamicMinimum: gi, DynamicMaximum: 20 * gi}, want: ErrOutOfBounds, class: ErrPermissionDenied,
		},
		{
			name: "DM minimum lowered but current already below policy min", op: OpSetDynamicMinimum, bytes: gi / 2,
			vm: VMState{Name: "dm", DynamicMemory: true, DynamicMinimum: gi, DynamicMaximum: 8 * gi}, want: ErrOutOfBounds, class: ErrPermissionDenied,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := p.Allows(tt.vm, tt.op, tt.bytes)
			if tt.want == nil {
				if err != nil {
					t.Fatalf("Allows() = %v, want nil", err)
				}
				return
			}
			var we *WriteError
			if !errors.As(err, &we) {
				t.Fatalf("Allows() = %v (%T), want *WriteError", err, err)
			}
			if !errors.Is(err, tt.want) || !errors.Is(err, tt.class) {
				t.Fatalf("Allows() = %v, want reason %v of class %v", err, tt.want, tt.class)
			}
			other := ErrPermissionDenied
			if errors.Is(tt.class, ErrPermissionDenied) {
				other = ErrFailedPrecondition
			}
			if errors.Is(err, other) {
				t.Fatalf("Allows() = %v matches both classes", err)
			}
			if we.VM != tt.vm.Name || we.Op != tt.op || we.Bytes != tt.bytes || !errors.Is(we.Reason, tt.want) {
				t.Fatalf("WriteError = %+v, want VM %q op %v bytes %d", we, tt.vm.Name, tt.op, tt.bytes)
			}
		})
	}
}

func TestAllowsEmptyPolicyDeniesEverything(t *testing.T) {
	for _, op := range []Op{OpSetStartup, OpSetDynamicMinimum, OpSetDynamicMaximum} {
		if err := (Policy{}).Allows(VMState{Name: "vm"}, op, gi); !errors.Is(err, ErrVMNotListed) {
			t.Errorf("%v: Allows() = %v, want ErrVMNotListed", op, err)
		}
	}
}

func TestWriteErrorMessage(t *testing.T) {
	p := Policy{VMs: []VM{{Name: "w", Min: 6 * gi, Max: 24 * gi}, {Name: "f", Fixed: true}}}
	tests := []struct {
		vm    VMState
		op    Op
		bytes int64
		want  string
	}{
		{
			vm: VMState{Name: "w"}, op: OpSetStartup, bytes: 32 * gi,
			want: `policy refuses to set startup memory of VM "w" to 32Gi: permission denied: value outside the policy's bounds (allowed [6Gi, 24Gi])`,
		},
		{
			vm: VMState{Name: "f"}, op: OpSetStartup, bytes: 8 * gi,
			want: `policy refuses to set startup memory of VM "f" to 8Gi: failed precondition: VM is fixed (read-only)`,
		},
		{
			vm: VMState{Name: "w", DynamicMemory: true, DynamicMinimum: 8 * gi}, op: OpSetDynamicMinimum, bytes: 10 * gi,
			want: `policy refuses to set Dynamic Memory minimum of VM "w" to 10Gi: failed precondition: Dynamic Memory minimum may only be lowered and maximum only raised (current minimum 8Gi)`,
		},
		{
			vm: VMState{Name: "w", DynamicMemory: true, DynamicMaximum: 8 * gi}, op: OpSetDynamicMaximum, bytes: 6 * gi,
			want: `policy refuses to set Dynamic Memory maximum of VM "w" to 6Gi: failed precondition: Dynamic Memory minimum may only be lowered and maximum only raised (current maximum 8Gi)`,
		},
		{
			vm: VMState{Name: "w"}, op: OpSetDynamicMaximum, bytes: 8 * gi,
			want: `policy refuses to set Dynamic Memory maximum of VM "w" to 8Gi: failed precondition: operation does not match the VM's memory type (Dynamic Memory enabled: false)`,
		},
		{
			vm: VMState{Name: "x"}, op: Op(9), bytes: 1000,
			want: `policy refuses to Op(9) of VM "x" to 1k: permission denied: VM is not in the policy`,
		},
		{
			vm: VMState{Name: "w"}, op: Op(9), bytes: 1024,
			want: `policy refuses to Op(9) of VM "w" to 1Ki: permission denied: unknown operation`,
		},
	}
	for _, tt := range tests {
		err := p.Allows(tt.vm, tt.op, tt.bytes)
		if err == nil || err.Error() != tt.want {
			t.Errorf("Allows(%+v, %v, %d) =\n  %v\nwant\n  %s", tt.vm, tt.op, tt.bytes, err, tt.want)
		}
	}
}
