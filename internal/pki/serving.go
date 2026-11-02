package pki

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/matfax/hvmc/api/v1alpha1"
)

// Services the coordinator serves TLS on with a certificate from the
// internal CA (DESIGN §7.2, §7.3, §11).
const (
	ExtenderServiceName = "hvmc-extender"
	WebhookServiceName  = "hvmc-webhook"
)

// ErrUnknownAgent is returned for an agent name HostConfig does not declare.
var ErrUnknownAgent = errors.New("agent not declared in HostConfig")

// ServiceIdentity returns the identity of a serving certificate for a
// Service: its in-cluster DNS names `<svc>.<ns>.svc` and
// `<svc>.<ns>.svc.cluster.local`.
func ServiceIdentity(service, namespace string) Identity {
	short := service + "." + namespace + ".svc"
	return Identity{CommonName: short, DNSNames: []string{short, short + ".cluster.local"}}
}

// AgentIdentity returns the identity an agent's serving certificate must
// carry, derived from HostConfig only (DESIGN §4.6):
//   - a host agent (spec.agents[].name): its dnsName;
//   - a guest agent (spec.vms[].name): its guestAgent.dnsName if set and, for
//     a VM mapped to a node, the node name and the Node's InternalIP
//     addresses (the address the coordinator dials, §4.7).
//
// The address override is not a SAN: the coordinator verifies the agent
// against these names whatever address it dials.
func AgentIdentity(ctx context.Context, r client.Reader, hc *v1alpha1.HostConfig, agent string) (Identity, error) {
	host := slices.IndexFunc(hc.Spec.Agents, func(a v1alpha1.AgentSpec) bool { return a.Name == agent })
	vm := slices.IndexFunc(hc.Spec.VMs, func(v v1alpha1.VMSpec) bool { return v.Name == agent })
	switch {
	case host >= 0 && vm >= 0:
		return Identity{}, fmt.Errorf("agent %q: name is both a host agent and a VM", agent)
	case host >= 0:
		return hostAgentIdentity(hc.Spec.Agents[host])
	case vm >= 0:
		return guestAgentIdentity(ctx, r, &hc.Spec.VMs[vm])
	default:
		return Identity{}, fmt.Errorf("%w: %q", ErrUnknownAgent, agent)
	}
}

func hostAgentIdentity(a v1alpha1.AgentSpec) (Identity, error) {
	if a.DNSName == "" {
		return Identity{}, fmt.Errorf("agent %q: no dnsName", a.Name)
	}
	return Identity{CommonName: a.DNSName, DNSNames: []string{a.DNSName}}, nil
}

func guestAgentIdentity(ctx context.Context, r client.Reader, vm *v1alpha1.VMSpec) (Identity, error) {
	var id Identity
	if vm.GuestAgent != nil && vm.GuestAgent.DNSName != "" {
		id.DNSNames = append(id.DNSNames, vm.GuestAgent.DNSName)
	}
	if vm.Node != "" {
		ips, err := nodeInternalIPs(ctx, r, vm.Node)
		if err != nil {
			return Identity{}, fmt.Errorf("agent %q: %w", vm.Name, err)
		}
		if !slices.Contains(id.DNSNames, vm.Node) {
			id.DNSNames = append(id.DNSNames, vm.Node)
		}
		id.IPAddresses = ips
	}
	if len(id.DNSNames) == 0 {
		return Identity{}, fmt.Errorf("agent %q: VM has neither guestAgent.dnsName nor node", vm.Name)
	}
	id.CommonName = id.DNSNames[0]
	return id, nil
}

// nodeInternalIPs returns the InternalIP addresses of a Node; it fails when
// the Node does not exist or reports none.
func nodeInternalIPs(ctx context.Context, r client.Reader, name string) ([]net.IP, error) {
	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: name}, &node); err != nil {
		return nil, fmt.Errorf("node %q: %w", name, err)
	}
	var ips []net.IP
	for _, a := range node.Status.Addresses {
		if a.Type != corev1.NodeInternalIP {
			continue
		}
		if ip := net.ParseIP(a.Address); ip != nil {
			ips = append(ips, ip)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("node %q reports no InternalIP", name)
	}
	return ips, nil
}
