package policy

import (
	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
)

// Proto returns p as the wire message (policy.proto), VMs sorted by name.
func (p Policy) Proto() *agentv1.Policy {
	vms := p.sorted()
	m := &agentv1.Policy{Vms: make([]*agentv1.VMPolicy, 0, len(vms))}
	for _, vm := range vms {
		m.Vms = append(m.Vms, &agentv1.VMPolicy{Name: vm.Name, Fixed: vm.Fixed, MinBytes: vm.Min, MaxBytes: vm.Max})
	}
	return m
}

// FromProto converts a wire message to a validated Policy. A nil message is
// the empty policy, as for any absent proto3 message.
func FromProto(m *agentv1.Policy) (Policy, error) {
	p := Policy{VMs: make([]VM, 0, len(m.GetVms()))}
	for _, vm := range m.GetVms() {
		p.VMs = append(p.VMs, VM{Name: vm.GetName(), Fixed: vm.GetFixed(), Min: vm.GetMinBytes(), Max: vm.GetMaxBytes()})
	}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}
