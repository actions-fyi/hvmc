package fake_test

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
	"github.com/matfax/hvmc/internal/fake"
)

func TestUpdateIsValidatedAtomically(t *testing.T) {
	e := newEnv(t, nil)
	before := e.sim.Scenario()
	err := e.sim.Update(func(sc *fake.Scenario) {
		sc.Host.VMMSDown = true
		sc.VMs[1].Startup = 3 * fake.MiB // invalid
	})
	if err == nil || !strings.Contains(err.Error(), "multiple of 2Mi") {
		t.Fatalf("invalid update: %v", err)
	}
	if diff := cmp.Diff(before, e.sim.Scenario()); diff != "" {
		t.Errorf("rejected update changed the state:\n%s", diff)
	}
	if err := e.sim.UpdateVM("vm-nope", func(*fake.VMSpec) {}); err == nil {
		t.Errorf("UpdateVM of an unknown VM succeeded")
	}
	if err := e.sim.UpdateGuest("nobody", func(*fake.GuestSpec) {}); err == nil {
		t.Errorf("UpdateGuest of an unknown guest succeeded")
	}
	if err := e.sim.Apply(nil); err == nil {
		t.Errorf("Apply(nil) succeeded")
	}
	// Overcommitting the host by hand is rejected too.
	if err := e.sim.UpdateHost(func(h *fake.HostSpec) { h.Overhead = 60 * fake.GiB }); err == nil {
		t.Errorf("overhead beyond total accepted")
	}
}

// TestApplyScenarioAtRuntime replaces the state from YAML while servers and
// streams keep running, as the coordinator's devFakeAgents mode does with
// its scenario ConfigMap.
func TestApplyScenarioAtRuntime(t *testing.T) {
	e := newEnv(t, nil)
	ctx := testCtx(t)
	stream, err := e.host.WatchVMs(ctx, &agentv1.WatchVMsRequest{})
	e.must(err)
	_, err = stream.Recv()
	e.must(err)

	sc, err := fake.ParseScenario(readFile(t, "testdata/scenario.yaml"))
	e.must(err)
	sc.VMs[1].Startup = 10 * fake.GiB
	sc.VMs[1].Assigned = nil
	sc.Host.Hang = []string{"GetHost"}
	e.must(e.sim.Apply(sc))

	next, err := stream.Recv()
	e.must(err)
	if got := findVM(next.GetSnapshot(), "vm-a").GetAssignedBytes(); got != 10*gib {
		t.Errorf("vm-a after Apply = %d", got)
	}
	_, err = e.host.GetHost(shortCtx(t), &agentv1.GetHostRequest{})
	wantCode(t, err, codes.DeadlineExceeded)
	// The linked guest follows the new assignment.
	if m, _ := e.sim.GuestMemory("vm-a"); m.GetTotalBytes() != 10*gib-256*mib {
		t.Errorf("guest total after Apply = %d", m.GetTotalBytes())
	}
	if v, ok := e.sim.VM("vm-a"); !ok || v.GetStartupBytes() != 10*gib {
		t.Errorf("VM = %v", v)
	}
	if h := e.sim.Host(); h.GetFreeBytes() != 32*gib {
		t.Errorf("free = %d", h.GetFreeBytes())
	}
}

// TestServerStop ends streams the way a vanished agent does.
func TestServerStop(t *testing.T) {
	sim, err := fake.New(loadTestScenario(t), fake.WithClock(fake.NewManualClock(t0)))
	if err != nil {
		t.Fatal(err)
	}
	srv := sim.ServeHost(fake.Bufconn())
	conn, err := srv.Dial()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	stream, err := agentv1.NewHostAgentClient(conn).WatchVMs(testCtx(t), &agentv1.WatchVMsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	srv.Stop()
	_, err = stream.Recv()
	wantCode(t, err, codes.Unavailable)
}

// TestConcurrentControlAndRPCs exercises the control API, RPCs, streams and
// the clock concurrently; run with -race.
func TestConcurrentControlAndRPCs(t *testing.T) {
	e := newEnv(t, func(sc *fake.Scenario) {
		sc.Host.GrowLatency = fake.Duration(time.Second)
		sc.Guests[1].TotalLag = fake.Duration(2 * time.Second)
	})
	ctx := testCtx(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	run := func(f func(i int)) { repeatUntil(&wg, stop, f) }

	// Watchers.
	wg.Add(2)
	go func() {
		defer wg.Done()
		if s, err := e.host.WatchVMs(ctx, &agentv1.WatchVMsRequest{}); err == nil {
			drain(s)
		}
	}()
	go func() {
		defer wg.Done()
		if s, err := e.guest("vm-a").WatchMemory(ctx, &agentv1.WatchMemoryRequest{}); err == nil {
			drain(s)
		}
	}()
	run(func(i int) {
		size := int64(6+i%6) * gib
		_, _ = e.host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-a", StartupBytes: size})
	})
	run(func(int) { _, _ = e.host.GetHost(ctx, &agentv1.GetHostRequest{}) })
	run(func(int) { _, _ = e.guest("vm-a").GetMemory(ctx, &agentv1.GetMemoryRequest{}) })
	run(func(i int) {
		_ = e.sim.SetStale("hyperv-1", time.Duration(i%3)*time.Second)
		_ = e.sim.ClearStale("hyperv-1")
		_ = e.sim.UpdateGuest("vm-a", func(g *fake.GuestSpec) { g.Used = fake.Bytes(i%4) * fake.GiB })
		_ = e.sim.SetNotServing("vm-a", i%2 == 0)
		_ = e.sim.Scenario()
	})
	run(func(int) { e.clk.Advance(500 * time.Millisecond) })

	driveUntilBusy(ctx, t, e)
	close(stop)
	// Let in-flight grows finish and streams end.
	for range 10 {
		e.clk.Advance(time.Second)
	}
	for _, c := range e.conns {
		_ = c.Close()
	}
	wg.Wait()

	// The state is still consistent.
	if err := e.sim.Scenario().Validate(); err != nil {
		t.Errorf("state invalid after concurrent use: %v", err)
	}
}

// repeatUntil calls f(0), f(1), ... on a new goroutine until stop is closed.
func repeatUntil(wg *sync.WaitGroup, stop <-chan struct{}, f func(i int)) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			f(i)
		}
	}()
}

// drain receives from a stream until it ends.
func drain[T any](s grpc.ServerStreamingClient[T]) {
	for {
		if _, err := s.Recv(); err != nil {
			return
		}
	}
}

// driveUntilBusy drives the clock until every kind of call has run a few
// times.
func driveUntilBusy(ctx context.Context, t *testing.T, e *env) {
	t.Helper()
	for e.sim.CallCount("hyperv-1", "SetStartup") < 20 || e.sim.CallCount("vm-a", "GetMemory") < 20 {
		if ctx.Err() != nil {
			t.Fatal("concurrent calls did not make progress")
		}
		e.clk.Advance(100 * time.Millisecond)
		runtime.Gosched()
	}
}
