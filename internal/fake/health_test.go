package fake_test

import (
	"testing"

	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/matfax/hvmc/internal/fake"
)

func TestHealth(t *testing.T) {
	e := newEnv(t, nil)
	ctx := testCtx(t)
	check := func(agent, service string) healthpb.HealthCheckResponse_ServingStatus {
		t.Helper()
		resp, err := e.health(agent).Check(ctx, &healthpb.HealthCheckRequest{Service: service})
		if err != nil {
			t.Fatalf("Check(%s, %q): %v", agent, service, err)
		}
		return resp.GetStatus()
	}
	const serving, notServing = healthpb.HealthCheckResponse_SERVING, healthpb.HealthCheckResponse_NOT_SERVING
	for _, c := range []struct{ agent, service string }{
		{"hyperv-1", ""}, {"hyperv-1", fake.HostAgentService}, {"vm-a", ""}, {"vm-a", fake.GuestAgentService},
	} {
		if got := check(c.agent, c.service); got != serving {
			t.Errorf("%s %q = %v, want SERVING", c.agent, c.service, got)
		}
	}

	watch, err := e.health("hyperv-1").Watch(ctx, &healthpb.HealthCheckRequest{Service: fake.HostAgentService})
	e.must(err)
	first, err := watch.Recv()
	e.must(err)
	if first.GetStatus() != serving {
		t.Fatalf("Watch initial = %v", first.GetStatus())
	}

	e.must(e.sim.SetNotServing("hyperv-1", true))
	next, err := watch.Recv()
	e.must(err)
	if next.GetStatus() != notServing || check("hyperv-1", "") != notServing {
		t.Errorf("after SetNotServing: watch %v, check %v", next.GetStatus(), check("hyperv-1", ""))
	}
	e.must(e.sim.SetNotServing("hyperv-1", false))
	next, err = watch.Recv()
	e.must(err)
	if next.GetStatus() != serving {
		t.Errorf("after recovery: %v", next.GetStatus())
	}

	// Readiness follows vmms (host) and the memory source and VM state
	// (guest); the other agents are not affected.
	e.must(e.sim.SetVMMSDown(true))
	if got := check("hyperv-1", fake.HostAgentService); got != notServing {
		t.Errorf("host with vmms down = %v", got)
	}
	if got := check("vm-a", ""); got != serving {
		t.Errorf("guest while vmms is down = %v", got)
	}
	e.must(e.sim.UpdateGuest("vm-a", func(g *fake.GuestSpec) { g.SourceDown = true }))
	if got := check("vm-a", fake.GuestAgentService); got != notServing {
		t.Errorf("guest with source down = %v", got)
	}
	e.must(e.sim.UpdateVM("linux-node-1", func(v *fake.VMSpec) { v.State, v.Assigned = "Off", nil }))
	if got := check("linux-node-1", ""); got != notServing {
		t.Errorf("guest of an Off VM = %v", got)
	}

	// From YAML.
	e = newEnv(t, func(sc *fake.Scenario) { sc.Guests[0].NotServing = true })
	if got := check("linux-node-1", fake.GuestAgentService); got != notServing {
		t.Errorf("notServing from the scenario = %v", got)
	}
	if err := e.sim.SetNotServing("nobody", true); err == nil {
		t.Errorf("SetNotServing of an unknown agent succeeded")
	}
}
