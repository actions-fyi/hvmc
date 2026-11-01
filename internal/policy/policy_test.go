package policy

import (
	"errors"
	"strings"
	"testing"
)

const (
	mi = int64(1) << 20
	gi = int64(1) << 30
)

// example is the policy of DESIGN §4.3.
func example() Policy {
	return Policy{VMs: []VM{
		{Name: "linux-node-1", Fixed: true},
		{Name: "windows-node-1", Min: 6 * gi, Max: 24 * gi},
		{Name: "docker-host-1", Min: 4 * gi, Max: 32 * gi},
	}}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		vms  []VM
		want string // substring of the error; "" = valid
	}{
		{name: "empty policy", vms: nil},
		{name: "example", vms: example().VMs},
		{name: "min equals max", vms: []VM{{Name: "a", Min: gi, Max: gi}}},
		{name: "unbounded max", vms: []VM{{Name: "a", Min: gi, Max: Unbounded}}},
		{name: "empty name", vms: []VM{{Min: gi, Max: gi}}, want: "vms[0]: empty name"},
		{name: "control character", vms: []VM{{Name: "a\nb", Fixed: true}}, want: "control characters"},
		{name: "duplicate", vms: []VM{{Name: "a", Fixed: true}, {Name: "a", Fixed: true}}, want: `vms[1]: duplicate name "a"`},
		{name: "fixed with min", vms: []VM{{Name: "a", Fixed: true, Min: gi}}, want: "fixed VMs must not set min or max"},
		{name: "fixed with max", vms: []VM{{Name: "a", Fixed: true, Max: gi}}, want: "fixed VMs must not set min or max"},
		{name: "writable without min", vms: []VM{{Name: "a", Max: gi}}, want: "min must be positive"},
		{name: "negative min", vms: []VM{{Name: "a", Min: -gi, Max: gi}}, want: "min must be positive, got -1Gi"},
		{name: "max below min", vms: []VM{{Name: "a", Min: 2 * gi, Max: gi}}, want: "max 1Gi is below min 2Gi"},
		{name: "all errors reported", vms: []VM{{Name: ""}, {Name: "b", Min: 2 * gi, Max: gi}}, want: "empty name\nvm \"b\": max 1Gi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Policy{VMs: tt.vms}.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidPolicy) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() = %v, want ErrInvalidPolicy containing %q", err, tt.want)
			}
		})
	}
}

func TestLookup(t *testing.T) {
	p := example()
	vm, ok := p.Lookup("windows-node-1")
	if !ok || vm != (VM{Name: "windows-node-1", Min: 6 * gi, Max: 24 * gi}) {
		t.Fatalf("Lookup(windows-node-1) = %+v, %t", vm, ok)
	}
	if vm, ok := p.Lookup("nope"); ok || vm != (VM{}) {
		t.Fatalf("Lookup(nope) = %+v, %t, want zero, false", vm, ok)
	}
}

func TestSortedIsTotal(t *testing.T) {
	// Duplicates only occur in invalid policies, but the canonical form must
	// not depend on their input order either.
	a := Policy{VMs: []VM{{Name: "x", Min: 2, Max: 3}, {Name: "x", Fixed: true}, {Name: "x", Min: 1, Max: 5}, {Name: "x", Min: 1, Max: 4}}}
	b := Policy{VMs: []VM{a.VMs[2], a.VMs[1], a.VMs[3], a.VMs[0]}}
	if a.Digest() != b.Digest() {
		t.Fatalf("digest depends on order of duplicate entries:\n%s\n%s", a.Canonical(), b.Canonical())
	}
}
