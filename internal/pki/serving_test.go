package pki_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/matfax/hvmc/api/v1alpha1"
	"github.com/matfax/hvmc/internal/pki"
)

func TestServiceIdentity(t *testing.T) {
	got := pki.ServiceIdentity("hvmc-webhook", "ns1")
	want := pki.Identity{
		CommonName: "hvmc-webhook.ns1.svc",
		DNSNames:   []string{"hvmc-webhook.ns1.svc", "hvmc-webhook.ns1.svc.cluster.local"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("(-want +got):\n%s", diff)
	}
}

func agentHostConfig() *v1alpha1.HostConfig {
	return &v1alpha1.HostConfig{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.HostConfigName},
		Spec: v1alpha1.HostConfigSpec{
			Agents: []v1alpha1.AgentSpec{{Name: "host", DNSName: "hyperv.example.com", Address: "192.0.2.1"}},
			VMs: []v1alpha1.VMSpec{
				{Name: "vm-node", Node: "node-1"},
				{Name: "vm-node-dns", Node: "node-2", GuestAgent: &v1alpha1.GuestAgentSpec{DNSName: "vm2.example.com"}},
				{Name: "vm-facade", GuestAgent: &v1alpha1.GuestAgentSpec{DNSName: "docker.example.com", Address: "192.0.2.9"}},
				{Name: "vm-no-node-ip", Node: "node-3"},
				{Name: "vm-missing-node", Node: "node-absent"},
				{Name: "vm-nothing"},
				{Name: "host"},
			},
		},
	}
}

func node(name string, addrs ...corev1.NodeAddress) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{Addresses: addrs}}
}

func addr(t corev1.NodeAddressType, a string) corev1.NodeAddress {
	return corev1.NodeAddress{Type: t, Address: a}
}

func nodeClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithObjects(objs...).Build()
}

func TestAgentIdentity(t *testing.T) {
	r := nodeClient(
		node("node-1", addr(corev1.NodeHostName, "node-1"), addr(corev1.NodeInternalIP, "10.0.0.11"),
			addr(corev1.NodeExternalIP, "203.0.113.1"), addr(corev1.NodeInternalIP, "fd00::11")),
		node("node-2", addr(corev1.NodeInternalIP, "10.0.0.12")),
		node("node-3", addr(corev1.NodeExternalIP, "203.0.113.3")),
	)
	hc := agentHostConfig()
	hc.Spec.VMs = hc.Spec.VMs[:len(hc.Spec.VMs)-1] // drop the name clash
	tests := []struct {
		agent string
		want  pki.Identity
	}{
		{"host", pki.Identity{CommonName: "hyperv.example.com", DNSNames: []string{"hyperv.example.com"}}},
		{"vm-node", pki.Identity{
			CommonName: "node-1", DNSNames: []string{"node-1"},
			IPAddresses: []net.IP{net.ParseIP("10.0.0.11"), net.ParseIP("fd00::11")},
		}},
		{"vm-node-dns", pki.Identity{
			CommonName: "vm2.example.com", DNSNames: []string{"vm2.example.com", "node-2"},
			IPAddresses: []net.IP{net.ParseIP("10.0.0.12")},
		}},
		{"vm-facade", pki.Identity{CommonName: "docker.example.com", DNSNames: []string{"docker.example.com"}}},
	}
	for _, tt := range tests {
		t.Run(tt.agent, func(t *testing.T) {
			got, err := pki.AgentIdentity(context.Background(), r, hc, tt.agent)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tt.want, got, cmp.Comparer(func(a, b net.IP) bool { return a.Equal(b) })); diff != "" {
				t.Fatalf("(-want +got):\n%s", diff)
			}
		})
	}
}

func TestAgentIdentityErrors(t *testing.T) {
	r := nodeClient(node("node-3", addr(corev1.NodeExternalIP, "203.0.113.3")))
	hc := agentHostConfig()
	tests := []struct {
		agent   string
		wantErr string
	}{
		{"unknown", "not declared"},
		{"host", "both a host agent and a VM"},
		{"vm-no-node-ip", "no InternalIP"},
		{"vm-missing-node", `node "node-absent"`},
		{"vm-nothing", "neither guestAgent.dnsName nor node"},
	}
	for _, tt := range tests {
		t.Run(tt.agent, func(t *testing.T) {
			_, err := pki.AgentIdentity(context.Background(), r, hc, tt.agent)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
	_, err := pki.AgentIdentity(context.Background(), r, hc, "unknown")
	if !errors.Is(err, pki.ErrUnknownAgent) {
		t.Fatalf("unknown agent: error %v is not ErrUnknownAgent", err)
	}
	hc.Spec.Agents[0].DNSName = ""
	hc.Spec.VMs = nil
	if _, err := pki.AgentIdentity(context.Background(), r, hc, "host"); err == nil {
		t.Fatal("host agent without dnsName got an identity")
	}
}

// TestRotationAfterAddressChange: the agent's CSR repeats the SANs of its
// current certificate (cert.proto); after the node's InternalIP changed the
// signed certificate must carry the new address from HostConfig/Node.
func TestRotationAfterAddressChange(t *testing.T) {
	ctx := context.Background()
	ca := newTestCA(t, newFakeClock())
	hc := agentHostConfig()
	n := node("node-1", addr(corev1.NodeInternalIP, "10.0.0.11"))
	r := nodeClient(n)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	id, err := pki.AgentIdentity(ctx, r, hc, "vm-node")
	if err != nil {
		t.Fatal(err)
	}
	first, err := ca.SignCSR(mustCSR(t, key, id), id)
	if err != nil {
		t.Fatal(err)
	}

	n.Status.Addresses = []corev1.NodeAddress{addr(corev1.NodeInternalIP, "10.0.0.99")}
	if err := r.Status().Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	newKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	csr := mustCSR(t, newKey, pki.Identity{CommonName: first.Subject.CommonName, DNSNames: first.DNSNames, IPAddresses: first.IPAddresses})
	id, err = pki.AgentIdentity(ctx, r, hc, "vm-node")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ca.SignCSR(csr, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.VerifyHostname("10.0.0.99"); err != nil {
		t.Errorf("renewed certificate lacks the new address: %v", err)
	}
	if second.VerifyHostname("10.0.0.11") == nil {
		t.Error("renewed certificate still carries the old address")
	}
}
