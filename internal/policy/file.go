package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

// File format
//
// A policy is stored as YAML under the top-level key `vms`, the same shape as
// the host agent's `agent.yaml` (DESIGN §4.3):
//
//	vms:
//	  - name: linux-node-1
//	    fixed: true        # listed, read-only
//	  - name: windows-node-1
//	    min: 6Gi           # Kubernetes quantity or plain bytes
//	    max: 24Gi
//
// Each entry has `name` (required), and either `fixed: true` or both `min`
// and `max`; unknown entry fields are rejected. Other top-level keys (role,
// listen, health, allowedClients, …) belong to the agent's settings and are
// ignored by [Parse] and kept by [Save], so one file can carry both. The key
// `vms` itself is required; `vms: []` is the empty policy (every VM
// read-only).
//
// The canonical form ([Policy.Canonical]) is this format produced by
// sigs.k8s.io/yaml with nothing but `vms`: entries sorted by name, keys in
// alphabetical order (fixed, max, min, name), `fixed` only when true,
// `min`/`max` only when non-zero, and quantities in the canonical
// binary-SI spelling of resource.Quantity for their byte value (8Gi, 1536Mi,
// "1000000000"; YAML quotes values that would otherwise read as numbers).

// fileKey is the top-level key that holds the policy.
const fileKey = "vms"

// document is the YAML shape of a policy.
type document struct {
	VMs []documentVM `json:"vms"`
}

type documentVM struct {
	Name  string             `json:"name"`
	Fixed bool               `json:"fixed,omitempty"`
	Min   *resource.Quantity `json:"min,omitempty"`
	Max   *resource.Quantity `json:"max,omitempty"`
}

// quantity returns the canonical quantity for b, or nil for 0.
func quantity(b int64) *resource.Quantity {
	if b == 0 {
		return nil
	}
	return resource.NewQuantity(b, resource.BinarySI)
}

// formatBytes spells b the way the canonical form does.
func formatBytes(b int64) string {
	return resource.NewQuantity(b, resource.BinarySI).String()
}

// formatBound spells a policy bound: like formatBytes, but Unbounded reads
// "unbounded".
func formatBound(b int64) string {
	if b == Unbounded {
		return "unbounded"
	}
	return formatBytes(b)
}

// toBytes converts a quantity to whole bytes. Quantities above the int64
// range are already capped at math.MaxInt64 by resource.ParseQuantity.
func toBytes(q *resource.Quantity) (int64, error) {
	if q == nil {
		return 0, nil
	}
	b := q.Value()
	if q.Cmp(*resource.NewQuantity(b, resource.BinarySI)) != 0 {
		return 0, fmt.Errorf("%s is not a whole number of bytes", q)
	}
	return b, nil
}

func (p Policy) document() document {
	vms := p.sorted()
	doc := document{VMs: make([]documentVM, 0, len(vms))}
	for _, vm := range vms {
		doc.VMs = append(doc.VMs, documentVM{Name: vm.Name, Fixed: vm.Fixed, Min: quantity(vm.Min), Max: quantity(vm.Max)})
	}
	return doc
}

// Canonical returns the canonical YAML form of p (see the file format above).
// It is a pure function of the policy's content and is defined for invalid
// policies too.
func (p Policy) Canonical() []byte {
	// Marshalling strings, bools and quantities cannot fail.
	out, err := yaml.Marshal(p.document())
	if err != nil {
		panic(fmt.Sprintf("policy: marshal canonical form: %v", err))
	}
	return out
}

// Digest returns "sha256:" followed by the lowercase hex SHA-256 of
// [Policy.Canonical]. Agent and coordinator compare policies by digest.
func (p Policy) Digest() string {
	sum := sha256.Sum256(p.Canonical())
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Parse decodes a policy from YAML in the file format (see above) and
// validates it. Top-level keys other than `vms` are ignored.
func Parse(data []byte) (Policy, error) {
	j, err := yaml.YAMLToJSONStrict(data)
	if err != nil {
		return Policy{}, fmt.Errorf("%w: %w", ErrInvalidPolicy, err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(j, &top); err != nil {
		return Policy{}, fmt.Errorf("%w: document must be a mapping: %w", ErrInvalidPolicy, err)
	}
	raw, ok := top[fileKey]
	if !ok {
		return Policy{}, fmt.Errorf("%w: missing key %q", ErrInvalidPolicy, fileKey)
	}
	var vms []documentVM
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&vms); err != nil {
		return Policy{}, fmt.Errorf("%w: %s: %w", ErrInvalidPolicy, fileKey, err)
	}
	p := Policy{VMs: make([]VM, 0, len(vms))}
	var errs []error
	for i, d := range vms {
		minimum, err1 := toBytes(d.Min)
		maximum, err2 := toBytes(d.Max)
		if err := errors.Join(err1, err2); err != nil {
			errs = append(errs, fmt.Errorf("vms[%d] %q: %w", i, d.Name, err))
			continue
		}
		if !d.Fixed && (d.Min == nil || d.Max == nil) {
			errs = append(errs, fmt.Errorf("vms[%d] %q: writable VMs need min and max (or fixed: true)", i, d.Name))
			continue
		}
		p.VMs = append(p.VMs, VM{Name: d.Name, Fixed: d.Fixed, Min: minimum, Max: maximum})
	}
	if len(errs) > 0 {
		return Policy{}, fmt.Errorf("%w: %w", ErrInvalidPolicy, errors.Join(errs...))
	}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// Load reads and parses the policy file at path.
func Load(path string) (Policy, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is the agent's own configuration file.
	if err != nil {
		return Policy{}, err
	}
	p, err := Parse(data)
	if err != nil {
		return Policy{}, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// Save validates p and writes it to path, replacing the file atomically
// (temporary file in the same directory, then rename).
//
// If path exists, its top-level keys other than `vms` (the agent's settings)
// are kept and only `vms` is replaced; the file must then be a YAML mapping.
// Comments and key order of an existing file are not preserved: the result
// is written by sigs.k8s.io/yaml with keys sorted. A new file holds the
// canonical form only. The permission bits of an existing file are kept, a
// new file gets 0600; Windows ACLs are inherited from the directory.
func Save(path string, p Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	out := p.Canonical()
	perm := fs.FileMode(0o600)
	old, err := os.ReadFile(path) //nolint:gosec // G304: the path is the agent's own configuration file.
	switch {
	case err == nil:
		if out, err = replaceVMs(old, p); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if st, err := os.Stat(path); err == nil {
			perm = st.Mode().Perm()
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	return writeAtomic(path, out, perm)
}

// replaceVMs returns the YAML document doc with its `vms` key set to p.
func replaceVMs(doc []byte, p Policy) ([]byte, error) {
	var top map[string]any
	if err := yaml.Unmarshal(doc, &top); err != nil {
		return nil, fmt.Errorf("existing file is not a YAML mapping: %w", err)
	}
	if top == nil {
		top = map[string]any{}
	}
	top[fileKey] = p.document().VMs
	return yaml.Marshal(top)
}

func writeAtomic(path string, data []byte, perm fs.FileMode) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err = os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
