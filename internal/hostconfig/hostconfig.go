// Package hostconfig turns a HostConfig (api/v1alpha1) into the ledger's
// configuration and reads HostConfig manifests from YAML.
//
// The ledger never imports the API types (DESIGN §2: internal/ledger is pure);
// everything that feeds a HostConfig into a ledger — the HostConfig
// controller, `hvmc plan` — converts it here, so the defaults below exist
// exactly once outside the CRD schema.
package hostconfig

import (
	"errors"
	"fmt"
	"os"

	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	"github.com/matfax/hvmc/api/v1alpha1"
	"github.com/matfax/hvmc/internal/ledger"
)

// Kind is the kind of a HostConfig manifest.
const Kind = "HostConfig"

// Parse reads one HostConfig manifest from YAML or JSON. Unknown fields are
// rejected. apiVersion and kind may be omitted; when present they must be
// hvmc.fax.fyi/v1alpha1 and HostConfig. CRD defaults are not applied here:
// ToLedgerConfig fills them in.
func Parse(data []byte) (*v1alpha1.HostConfig, error) {
	var hc v1alpha1.HostConfig
	if err := yaml.UnmarshalStrict(data, &hc); err != nil {
		return nil, fmt.Errorf("parse HostConfig: %w", err)
	}
	if v := hc.APIVersion; v != "" && v != v1alpha1.GroupVersion.String() {
		return nil, fmt.Errorf("parse HostConfig: apiVersion %q, want %q", v, v1alpha1.GroupVersion.String())
	}
	if k := hc.Kind; k != "" && k != Kind {
		return nil, fmt.Errorf("parse HostConfig: kind %q, want %q", k, Kind)
	}
	return &hc, nil
}

// Load reads a HostConfig manifest from a file (see Parse).
func Load(path string) (*v1alpha1.HostConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // reading a caller-chosen manifest is the point
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// ToLedgerConfig converts a HostConfig spec into a ledger configuration.
//
// Fields left unset get the CRD defaults (DESIGN §5): capacity 0 (read from
// the host agent), reserve 4Gi, defaultMargin 2Gi, step 256Mi, dmAccounting
// maximum, staleAfter 30s, reconciler.priorityOverQueue true. The host agent
// is the single spec.agents entry with role host (the ledger's default name
// when there is none); guest agents are named after their VM, as in
// HostConfig status.agents. Only what the ledger uses is converted: of a VM
// policy only min and max, the reconciler tuning stays with the reconciler.
//
// The result is checked with ledger.Config.Validate. Quantities must be whole
// bytes, and step and staleAfter, when set, must be positive.
func ToLedgerConfig(hc *v1alpha1.HostConfig) (ledger.Config, error) {
	if hc == nil {
		return ledger.Config{}, errors.New("hostconfig: nil HostConfig")
	}
	spec := &hc.Spec
	c := &converter{}
	cfg := ledger.DefaultConfig()
	cfg.Capacity = c.optional("spec.capacity", spec.Capacity, 0)
	cfg.Reserve = c.optional("spec.reserve", spec.Reserve, ledger.DefaultReserve)
	cfg.DefaultMargin = c.optional("spec.defaultMargin", spec.DefaultMargin, ledger.DefaultMargin)
	cfg.Step = c.optional("spec.step", spec.Step, ledger.DefaultStep)
	c.check(spec.Step != nil && cfg.Step <= 0, "spec.step must be positive")
	if spec.DMAccounting != "" {
		cfg.DMAccounting = ledger.DMAccounting(spec.DMAccounting)
	}
	if d := spec.StaleAfter; d != nil {
		cfg.StaleAfter = d.Duration
		c.check(d.Duration <= 0, "spec.staleAfter must be positive")
	}
	if r := spec.Reconciler; r != nil && r.PriorityOverQueue != nil {
		cfg.PriorityOverQueue = *r.PriorityOverQueue
	}
	cfg.HostAgent = c.hostAgent(spec.Agents)
	for i := range spec.VMs {
		cfg.VMs = append(cfg.VMs, c.vm(fmt.Sprintf("spec.vms[%d]", i), &spec.VMs[i]))
	}
	if len(c.errs) == 0 {
		c.errs = append(c.errs, cfg.Validate())
	}
	if err := errors.Join(c.errs...); err != nil {
		return ledger.Config{}, fmt.Errorf("hostconfig: %w", err)
	}
	return cfg, nil
}

// converter collects conversion errors.
type converter struct{ errs []error }

func (c *converter) check(bad bool, format string, args ...any) {
	if bad {
		c.errs = append(c.errs, fmt.Errorf(format, args...))
	}
}

// bytes returns q in bytes; it must be a whole number of bytes.
func (c *converter) bytes(field string, q resource.Quantity) int64 {
	v := q.Value()
	c.check(resource.NewQuantity(v, resource.BinarySI).Cmp(q) != 0,
		"%s: %s is not a whole number of bytes", field, q.String())
	return v
}

// optional returns q in bytes, or def when q is unset.
func (c *converter) optional(field string, q *resource.Quantity, def int64) int64 {
	if q == nil {
		return def
	}
	return c.bytes(field, *q)
}

// hostAgent returns the name of the host agent.
func (c *converter) hostAgent(agents []v1alpha1.AgentSpec) ledger.AgentName {
	var hosts []string
	for _, a := range agents {
		if a.Role == "" || a.Role == v1alpha1.AgentRoleHost {
			hosts = append(hosts, a.Name)
		}
	}
	c.check(len(hosts) > 1, "spec.agents: %d host agents %q, want at most one", len(hosts), hosts)
	if len(hosts) == 0 {
		return ledger.DefaultHostAgent
	}
	return ledger.AgentName(hosts[0])
}

// vm converts one spec.vms entry.
func (c *converter) vm(path string, v *v1alpha1.VMSpec) ledger.VMConfig {
	out := ledger.VMConfig{
		Name:    ledger.VMName(v.Name),
		Node:    v.Node,
		GuestOS: ledger.GuestOS(v.GuestOS),
		Mode:    ledger.Mode(v.Mode),
		Base:    c.bytes(path+".base", v.Base),
		Probe:   v.Probe,
	}
	if v.Margin != nil {
		m := c.bytes(path+".margin", *v.Margin)
		out.Margin = &m
	}
	if p := v.Policy; p != nil {
		out.Policy = &ledger.Policy{
			Min: c.bytes(path+".policy.min", p.Min),
			Max: c.bytes(path+".policy.max", p.Max),
		}
	}
	return out
}
