package fake_test

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/testing/protocmp"

	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
	"github.com/matfax/hvmc/internal/fake"
)

func TestGetMemory(t *testing.T) {
	e := newEnv(t, nil)
	ctx := testCtx(t)

	linux, err := e.guest("linux-node-1").GetMemory(ctx, &agentv1.GetMemoryRequest{})
	e.must(err)
	total := 8*gib - 200*mib
	want := &agentv1.GetMemoryResponse{
		Memory: &agentv1.GuestMemory{TotalBytes: total, AvailableBytes: total - 2*gib, ObservedAt: timestamp(t0)},
		Agent:  &agentv1.AgentInfo{Version: fake.DefaultAgentVersion, Role: agentv1.AgentRole_AGENT_ROLE_GUEST, OperatingSystem: agentv1.OperatingSystem_OPERATING_SYSTEM_LINUX},
	}
	if diff := cmp.Diff(want, linux, protocmp.Transform()); diff != "" {
		t.Errorf("Linux GetMemory (-want +got):\n%s", diff)
	}

	win, err := e.guest("vm-a").GetMemory(ctx, &agentv1.GetMemoryRequest{})
	e.must(err)
	total = 8*gib - 256*mib
	limit, avail := total+2*gib, total+2*gib-3*gib
	wantMem := &agentv1.GuestMemory{
		TotalBytes: total, AvailableBytes: total - 3*gib,
		CommitLimitBytes: &limit, CommitAvailableBytes: &avail, ObservedAt: timestamp(t0),
	}
	if diff := cmp.Diff(wantMem, win.GetMemory(), protocmp.Transform()); diff != "" {
		t.Errorf("Windows GetMemory (-want +got):\n%s", diff)
	}
	if got := win.GetAgent().GetOperatingSystem(); got != agentv1.OperatingSystem_OPERATING_SYSTEM_WINDOWS {
		t.Errorf("OS = %v", got)
	}

	// Used memory and commit charge are scriptable.
	e.must(e.sim.UpdateGuest("vm-a", func(g *fake.GuestSpec) { g.Used, g.Committed = 7*fake.GiB, 9*fake.GiB }))
	win, err = e.guest("vm-a").GetMemory(ctx, &agentv1.GetMemoryRequest{})
	e.must(err)
	if m := win.GetMemory(); m.GetAvailableBytes() != max(total-7*gib, 0) || m.GetCommitAvailableBytes() != limit-9*gib {
		t.Errorf("after update = %v", m)
	}
}

func TestGuestTotalLag(t *testing.T) {
	e := newEnv(t, func(sc *fake.Scenario) { sc.Guests[1].TotalLag = fake.Duration(3 * time.Second) })
	ctx := testCtx(t)
	guest := e.guest("vm-a")
	stream, err := guest.WatchMemory(ctx, &agentv1.WatchMemoryRequest{})
	e.must(err)
	first, err := stream.Recv()
	e.must(err)
	oldTotal := 8*gib - 256*mib
	if got := first.GetMemory().GetTotalBytes(); got != oldTotal {
		t.Fatalf("initial total = %d", got)
	}

	_, err = e.host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-a", StartupBytes: 12 * gib})
	e.must(err)
	m, err := guest.GetMemory(ctx, &agentv1.GetMemoryRequest{})
	e.must(err)
	if got := m.GetMemory().GetTotalBytes(); got != oldTotal {
		t.Errorf("total right after the grow = %d, want the old %d", got, oldTotal)
	}

	e.clk.Advance(3 * time.Second)
	newTotal := 12*gib - 256*mib
	next, err := stream.Recv()
	e.must(err)
	if got := next.GetMemory(); got.GetTotalBytes() != newTotal || !got.GetObservedAt().AsTime().Equal(t0.Add(3*time.Second)) {
		t.Errorf("WatchMemory after the lag = %v", got)
	}

	// Periodic reading every 5 s without a change.
	e.clk.Advance(2 * time.Second)
	next, err = stream.Recv()
	e.must(err)
	if got := next.GetMemory(); got.GetTotalBytes() != newTotal || !got.GetObservedAt().AsTime().Equal(t0.Add(5*time.Second)) {
		t.Errorf("periodic reading = %v", got)
	}

	// Shrinks show immediately.
	_, err = e.host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-a", StartupBytes: 6 * gib})
	e.must(err)
	next, err = stream.Recv()
	e.must(err)
	if got := next.GetMemory().GetTotalBytes(); got != 6*gib-256*mib {
		t.Errorf("total after shrink = %d", got)
	}

	// A grow superseded by a shrink before its lag passes never shows.
	_, err = e.host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-a", StartupBytes: 10 * gib})
	e.must(err)
	_, err = e.host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-a", StartupBytes: 6 * gib})
	e.must(err)
	e.clk.Advance(3 * time.Second)
	if m, _ := e.sim.GuestMemory("vm-a"); m.GetTotalBytes() != 6*gib-256*mib {
		t.Errorf("superseded grow showed up: %d", m.GetTotalBytes())
	}
}

func TestGuestUnavailable(t *testing.T) {
	e := newEnv(t, nil)
	ctx := testCtx(t)
	guest := e.guest("vm-a")
	stream, err := guest.WatchMemory(ctx, &agentv1.WatchMemoryRequest{})
	e.must(err)
	_, err = stream.Recv()
	e.must(err)

	e.must(e.sim.UpdateGuest("vm-a", func(g *fake.GuestSpec) { g.SourceDown = true }))
	_, err = stream.Recv()
	wantCode(t, err, codes.Unavailable)
	_, err = guest.GetMemory(ctx, &agentv1.GetMemoryRequest{})
	wantCode(t, err, codes.Unavailable)
	e.must(e.sim.UpdateGuest("vm-a", func(g *fake.GuestSpec) { g.SourceDown = false }))
	_, err = guest.GetMemory(ctx, &agentv1.GetMemoryRequest{})
	e.must(err)

	// A guest whose VM is off cannot answer.
	e.must(e.sim.UpdateVM("vm-a", func(v *fake.VMSpec) { v.State, v.Assigned = "Off", nil }))
	_, err = guest.GetMemory(ctx, &agentv1.GetMemoryRequest{})
	wantCode(t, err, codes.Unavailable)

	// A guest removed from the scenario is gone.
	e.must(e.sim.Update(func(sc *fake.Scenario) { sc.Guests = sc.Guests[:1] }))
	_, err = guest.GetMemory(ctx, &agentv1.GetMemoryRequest{})
	wantCode(t, err, codes.Unavailable)
}

func TestGuestStaleAndHang(t *testing.T) {
	e := newEnv(t, nil)
	ctx := testCtx(t)
	guest := e.guest("linux-node-1")

	e.must(e.sim.SetStale("linux-node-1", 45*time.Second))
	e.must(e.sim.UpdateGuest("linux-node-1", func(g *fake.GuestSpec) { g.Used = 5 * fake.GiB }))
	e.clk.Advance(10 * time.Second)
	m, err := guest.GetMemory(ctx, &agentv1.GetMemoryRequest{})
	e.must(err)
	if got := m.GetMemory(); !got.GetObservedAt().AsTime().Equal(t0.Add(-45*time.Second)) || got.GetAvailableBytes() != 8*gib-200*mib-2*gib {
		t.Errorf("stale reading = %v", got)
	}
	e.must(e.sim.ClearStale("linux-node-1"))
	m, err = guest.GetMemory(ctx, &agentv1.GetMemoryRequest{})
	e.must(err)
	if got := m.GetMemory(); !got.GetObservedAt().AsTime().Equal(t0.Add(10*time.Second)) || got.GetAvailableBytes() != 8*gib-200*mib-5*gib {
		t.Errorf("fresh reading = %v", got)
	}

	e.must(e.sim.SetHang("linux-node-1", "GetMemory"))
	_, err = guest.GetMemory(shortCtx(t), &agentv1.GetMemoryRequest{})
	wantCode(t, err, codes.DeadlineExceeded)
	stream, err := guest.WatchMemory(ctx, &agentv1.WatchMemoryRequest{})
	e.must(err)
	if _, err := stream.Recv(); err != nil {
		t.Errorf("WatchMemory does not hang: %v", err)
	}
}
