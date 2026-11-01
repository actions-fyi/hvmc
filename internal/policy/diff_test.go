package policy

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/")

func TestDiffGolden(t *testing.T) {
	tests := []struct {
		name             string
		active, proposed Policy
	}{
		{name: "no-changes", active: example(), proposed: Policy{VMs: []VM{example().VMs[2], example().VMs[0], example().VMs[1]}}},
		{name: "from-empty", active: Policy{}, proposed: example()},
		{name: "to-empty", active: example(), proposed: Policy{}},
		{
			name:   "mixed",
			active: example(),
			proposed: Policy{VMs: []VM{
				{Name: "windows-node-1", Min: 6 * gi, Max: 32 * gi},       // max raised
				{Name: "linux-node-1", Min: 4 * gi, Max: 8 * gi},          // fixed -> writable
				{Name: "probe-1", Min: 2 * gi, Max: Unbounded},            // added
				{Name: "windows-node-2", Fixed: true},                     // added, fixed
				{Name: "docker-host-1", Min: 4*gi + 512*mi, Max: 16 * gi}, // both bounds
			}},
		},
		{
			name:     "to-fixed",
			active:   Policy{VMs: []VM{{Name: "windows-node-1", Min: 6 * gi, Max: 24 * gi}}},
			proposed: Policy{VMs: []VM{{Name: "windows-node-1", Fixed: true}}},
		},
		{
			name:     "crafted-name",
			active:   Policy{},
			proposed: Policy{VMs: []VM{{Name: "evil\n- linux-node-1: read-only (fixed)", Min: gi, Max: Unbounded}, {Name: "with space", Fixed: true}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Diff(tt.active, tt.proposed).String()
			golden := filepath.Join("testdata", "diff", tt.name+".txt")
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update to create)", err)
			}
			if diff := cmp.Diff(string(want), got); diff != "" {
				t.Fatalf("Diff mismatch with %s (-want +got):\n%s", golden, diff)
			}
		})
	}
}

func TestDiffStructure(t *testing.T) {
	active := example()
	proposed := Policy{VMs: []VM{
		{Name: "linux-node-1", Min: 4 * gi, Max: 8 * gi},
		{Name: "windows-node-1", Min: 6 * gi, Max: 24 * gi},
		{Name: "a-new", Fixed: true},
	}}
	got := Diff(active, proposed)
	want := Changes{
		{Kind: Added, VM: "a-new", New: &VM{Name: "a-new", Fixed: true}},
		{Kind: Removed, VM: "docker-host-1", Old: &VM{Name: "docker-host-1", Min: 4 * gi, Max: 32 * gi}},
		{
			Kind: Changed, VM: "linux-node-1",
			Old: &VM{Name: "linux-node-1", Fixed: true},
			New: &VM{Name: "linux-node-1", Min: 4 * gi, Max: 8 * gi},
			Fields: []FieldChange{
				{Field: "access", Old: "read-only", New: "writable"},
				{Field: "min", Old: "none", New: "4Gi"},
				{Field: "max", Old: "none", New: "8Gi"},
			},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("Diff mismatch (-want +got):\n%s", diff)
	}
	if got.Empty() {
		t.Fatal("Empty() = true for changes")
	}
	if none := Diff(example(), example()); !none.Empty() || none.String() != "" {
		t.Fatalf("Diff(p, p) = %v, want empty", none)
	}
}
