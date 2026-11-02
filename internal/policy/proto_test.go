package policy

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
)

func TestProto(t *testing.T) {
	want := &agentv1.Policy{Vms: []*agentv1.VMPolicy{
		{Name: "docker-host-1", MinBytes: 4 * gi, MaxBytes: 32 * gi},
		{Name: "linux-node-1", Fixed: true},
		{Name: "windows-node-1", MinBytes: 6 * gi, MaxBytes: 24 * gi},
	}}
	if diff := cmp.Diff(want, example().Proto(), protocmp.Transform()); diff != "" {
		t.Fatalf("Proto() mismatch (-want +got):\n%s", diff)
	}
}

func TestProtoRoundTrip(t *testing.T) {
	for _, p := range []Policy{
		{VMs: []VM{}},
		example(),
		{VMs: []VM{{Name: "u", Min: 1, Max: Unbounded}}},
	} {
		// Through the wire encoding, too.
		b, err := proto.Marshal(p.Proto())
		if err != nil {
			t.Fatal(err)
		}
		var m agentv1.Policy
		if err := proto.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		got, err := FromProto(&m)
		if err != nil {
			t.Fatalf("FromProto() = %v", err)
		}
		if diff := cmp.Diff(Policy{VMs: p.sorted()}, got); diff != "" {
			t.Fatalf("round trip mismatch (-want +got):\n%s", diff)
		}
		if got.Digest() != p.Digest() {
			t.Fatalf("digest changed in round trip")
		}
	}
}

func TestFromProtoNil(t *testing.T) {
	p, err := FromProto(nil)
	if err != nil || len(p.VMs) != 0 || p.Digest() != (Policy{}).Digest() {
		t.Fatalf("FromProto(nil) = %+v, %v; want the empty policy", p, err)
	}
}

func TestFromProtoInvalid(t *testing.T) {
	for name, m := range map[string]*agentv1.Policy{
		"nil entry":     {Vms: []*agentv1.VMPolicy{nil}},
		"fixed bounds":  {Vms: []*agentv1.VMPolicy{{Name: "a", Fixed: true, MaxBytes: gi}}},
		"no bounds":     {Vms: []*agentv1.VMPolicy{{Name: "a"}}},
		"max below min": {Vms: []*agentv1.VMPolicy{{Name: "a", MinBytes: 2 * gi, MaxBytes: gi}}},
		"duplicate":     {Vms: []*agentv1.VMPolicy{{Name: "a", Fixed: true}, {Name: "a", Fixed: true}}},
	} {
		if _, err := FromProto(m); !errors.Is(err, ErrInvalidPolicy) {
			t.Errorf("%s: FromProto() = %v, want ErrInvalidPolicy", name, err)
		}
	}
}
