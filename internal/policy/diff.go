package policy

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// ChangeKind is how a VM differs between two policies.
type ChangeKind string

// Change kinds.
const (
	Added   ChangeKind = "added"
	Removed ChangeKind = "removed"
	Changed ChangeKind = "changed"
)

// FieldChange is one differing field of a changed VM. Field is "access"
// (values "read-only" and "writable"), "min" or "max" (values in canonical
// quantity spelling, "none" for unset, "unbounded" for Unbounded).
type FieldChange struct {
	Field string
	Old   string
	New   string
}

// Change is the difference for one VM.
type Change struct {
	Kind ChangeKind
	// VM is the VM name.
	VM string
	// Old is the entry in the active policy (nil when Added), New the entry
	// in the proposed policy (nil when Removed).
	Old, New *VM
	// Fields lists the differing fields of a Changed VM, in the order access,
	// min, max.
	Fields []FieldChange
}

// Changes is the result of Diff, ordered by VM name.
type Changes []Change

// Diff returns how proposed differs from active, one Change per VM that was
// added, removed or changed, ordered by VM name. Both policies are expected
// to be valid; for duplicate names only the last entry counts.
func Diff(active, proposed Policy) Changes {
	old := index(active)
	nu := index(proposed)
	var changes Changes
	for _, name := range mergedNames(active, proposed) {
		o, inOld := old[name]
		n, inNew := nu[name]
		switch {
		case !inOld:
			changes = append(changes, Change{Kind: Added, VM: name, New: &n})
		case !inNew:
			changes = append(changes, Change{Kind: Removed, VM: name, Old: &o})
		case o != n:
			changes = append(changes, Change{Kind: Changed, VM: name, Old: &o, New: &n, Fields: fieldChanges(o, n)})
		}
	}
	return changes
}

func index(p Policy) map[string]VM {
	m := make(map[string]VM, len(p.VMs))
	for _, vm := range p.VMs {
		m[vm.Name] = vm
	}
	return m
}

// mergedNames returns the distinct VM names of a and b in sorted order.
func mergedNames(a, b Policy) []string {
	both := Policy{VMs: append(a.sorted(), b.VMs...)}.sorted()
	names := make([]string, 0, len(both))
	for _, vm := range both {
		if len(names) == 0 || names[len(names)-1] != vm.Name {
			names = append(names, vm.Name)
		}
	}
	return names
}

func fieldChanges(o, n VM) []FieldChange {
	var fields []FieldChange
	if o.Fixed != n.Fixed {
		fields = append(fields, FieldChange{Field: "access", Old: access(o), New: access(n)})
	}
	if o.Min != n.Min {
		fields = append(fields, FieldChange{Field: "min", Old: optBytes(o.Min), New: optBytes(n.Min)})
	}
	if o.Max != n.Max {
		fields = append(fields, FieldChange{Field: "max", Old: optBytes(o.Max), New: optBytes(n.Max)})
	}
	return fields
}

func access(vm VM) string {
	if vm.Fixed {
		return "read-only"
	}
	return "writable"
}

func optBytes(b int64) string {
	if b == 0 {
		return "none"
	}
	return formatBound(b)
}

// displayName quotes a name that contains non-printable characters, so a
// crafted name cannot fake extra diff lines.
func displayName(name string) string {
	if strings.IndexFunc(name, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		return strconv.Quote(name)
	}
	return name
}

func describe(vm VM) string {
	if vm.Fixed {
		return "read-only (fixed)"
	}
	return fmt.Sprintf("writable, min %s, max %s", optBytes(vm.Min), optBytes(vm.Max))
}

// Empty reports whether there are no changes.
func (c Changes) Empty() bool { return len(c) == 0 }

// String renders the changes for `hvmc agent policy sync`, one line per VM
// in VM-name order: "+ name: description" for an added VM, "- name:
// description" for a removed one and "~ name: field old -> new, ..." for a
// changed one, e.g. "~ windows-node-1: max 24Gi -> 32Gi" (more examples in
// testdata/diff). Names with non-printable characters are quoted.
//
// It returns "" when there are no changes. The output is stable: it depends
// only on the content of the two policies.
func (c Changes) String() string {
	var b strings.Builder
	for _, ch := range c {
		switch ch.Kind {
		case Added:
			fmt.Fprintf(&b, "+ %s: %s\n", displayName(ch.VM), describe(*ch.New))
		case Removed:
			fmt.Fprintf(&b, "- %s: %s\n", displayName(ch.VM), describe(*ch.Old))
		default:
			parts := make([]string, 0, len(ch.Fields))
			for _, f := range ch.Fields {
				parts = append(parts, fmt.Sprintf("%s %s -> %s", f.Field, f.Old, f.New))
			}
			fmt.Fprintf(&b, "~ %s: %s\n", displayName(ch.VM), strings.Join(parts, ", "))
		}
	}
	return b.String()
}
