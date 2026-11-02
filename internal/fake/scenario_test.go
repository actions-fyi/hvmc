package fake_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/matfax/hvmc/internal/fake"
)

func TestScenarioYAMLRoundTrip(t *testing.T) {
	sc := loadTestScenario(t)
	out, err := sc.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := fake.ParseScenario(out)
	if err != nil {
		t.Fatalf("re-parse:\n%s\n%v", out, err)
	}
	if diff := cmp.Diff(sc, back); diff != "" {
		t.Errorf("scenario changed in a YAML round trip (-want +got):\n%s", diff)
	}
	if !strings.Contains(string(out), "total: 64Gi") {
		t.Errorf("quantities not written as quantities:\n%s", out)
	}

	// The live state of a Sim round-trips too, with defaults filled in.
	sim, err := fake.New(sc)
	if err != nil {
		t.Fatal(err)
	}
	state := sim.Scenario()
	out, err = state.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err = fake.ParseScenario(out)
	if err != nil {
		t.Fatalf("re-parse state:\n%s\n%v", out, err)
	}
	if diff := cmp.Diff(state, back); diff != "" {
		t.Errorf("Sim state changed in a YAML round trip (-want +got):\n%s", diff)
	}
	if got := state.VMs[1]; got.State != "Running" || got.Assigned == nil || *got.Assigned != fake.Bytes(8*gib) || got.ID == "" {
		t.Errorf("defaults not applied to vm-a: %+v", got)
	}
	if got := state.Guests[1].Total; got != fake.Bytes(8*gib-256*mib) {
		t.Errorf("linked guest total = %s, want assigned − offset", got)
	}
	// A new Sim from the state is the same state.
	again, err := fake.New(state)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(state, again.Scenario()); diff != "" {
		t.Errorf("Sim from its own state differs (-want +got):\n%s", diff)
	}
}

func TestBytesQuantities(t *testing.T) {
	tests := []struct {
		yaml string
		want fake.Bytes
		err  bool
	}{
		{yaml: "total: 8Gi", want: 8 * fake.GiB},
		{yaml: "total: 1.5Gi", want: 1536 * fake.MiB},
		{yaml: "total: 4096", want: 4096},
		{yaml: `total: "512Mi"`, want: 512 * fake.MiB},
		{yaml: "total: 0.5", err: true},
		{yaml: "total: lots", err: true},
	}
	for _, tt := range tests {
		sc, err := fake.ParseScenario([]byte("host: {name: h, " + tt.yaml + "}"))
		if tt.err {
			if err == nil {
				t.Errorf("%s: no error", tt.yaml)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tt.yaml, err)
			continue
		}
		if sc.Host.Total != tt.want {
			t.Errorf("%s: got %d, want %d", tt.yaml, sc.Host.Total, tt.want)
		}
	}
	if got := (1536 * fake.MiB).String(); got != "1536Mi" {
		t.Errorf("String = %q", got)
	}
}

func TestScenarioValidation(t *testing.T) {
	base := `
host: {name: hyperv-1, total: 16Gi}
policy:
  vms: [{name: vm-a, min: 2Gi, max: 8Gi}]
vms:
  - {name: vm-a, startup: 4Gi}
guests:
  - {name: vm-a, vm: vm-a}
`
	if _, err := fake.ParseScenario([]byte(base)); err != nil {
		t.Fatalf("base scenario: %v", err)
	}
	tests := []struct {
		name, yaml, want string
	}{
		{"unknown field", "host: {name: h, total: 1Gi, bogus: 1}", "bogus"},
		{"no host name", "host: {total: 1Gi}", "host.name"},
		{"no total", "host: {name: h}", "host.total"},
		{"misaligned startup", "host: {name: h, total: 8Gi}\nvms: [{name: v, startup: 1Mi}]", "multiple of 2Mi"},
		{"duplicate VM", "host: {name: h, total: 8Gi}\nvms: [{name: v, startup: 1Gi}, {name: v, startup: 1Gi}]", "duplicate"},
		{"unknown state", "host: {name: h, total: 8Gi}\nvms: [{name: v, startup: 1Gi, state: Sleeping}]", "unknown state"},
		{"assigned while off", "host: {name: h, total: 8Gi}\nvms: [{name: v, startup: 1Gi, state: Off, assigned: 1Gi}]", "must be 0"},
		{"over total", "host: {name: h, total: 8Gi, overhead: 2Gi}\nvms: [{name: v, startup: 8Gi}]", "exceed total"},
		{"DM startup outside", "host: {name: h, total: 8Gi}\nvms: [{name: v, startup: 1Gi, dynamicMemory: {minimum: 2Gi, maximum: 4Gi}}]", "minimum ≤ startup"},
		{"DM buffer", "host: {name: h, total: 8Gi}\nvms: [{name: v, startup: 2Gi, dynamicMemory: {minimum: 2Gi, maximum: 4Gi, buffer: 1}}]", "buffer"},
		{"policy fixed with bounds", "host: {name: h, total: 8Gi}\npolicy: {vms: [{name: v, fixed: true, min: 1Gi}]}", "fixed VMs carry no min"},
		{"policy min above max", "host: {name: h, total: 8Gi}\npolicy: {vms: [{name: v, min: 4Gi, max: 2Gi}]}", "min ≤ max"},
		{"policy duplicate", "host: {name: h, total: 8Gi}\npolicy: {vms: [{name: v, fixed: true}, {name: v, fixed: true}]}", "twice"},
		{"guest of missing VM", "host: {name: h, total: 8Gi}\nguests: [{name: g, vm: nope}]", "does not exist"},
		{"guest named like host", "host: {name: h, total: 8Gi}\nguests: [{name: h}]", "duplicate agent"},
		{"guest os", "host: {name: h, total: 8Gi}\nguests: [{name: g, os: plan9}]", "os must be"},
		{"hang method", "host: {name: h, total: 8Gi, hang: [GetMemory]}", "unknown method"},
		{"cert IP", "host: {name: h, total: 8Gi, certificate: {ipAddresses: [nope]}}", "invalid IP"},
		{"duration", "host: {name: h, total: 8Gi, growLatency: 5}", "duration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := fake.ParseScenario([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}
