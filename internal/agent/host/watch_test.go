package host

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"

	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
)

func assigned(t *testing.T, snap *agentv1.VMSnapshot, name string) int64 {
	t.Helper()
	for _, v := range snap.GetVms() {
		if v.GetName() == name {
			return v.GetAssignedBytes()
		}
	}
	t.Fatalf("no VM %q in the snapshot", name)
	return 0
}

func TestWatchVMs(t *testing.T) {
	h := newFakeHost(loadFixture(t, "host"))
	clk := newTestClock()
	e := newTestEnv(t, h, testPolicy(), func(o *envOptions) { o.pollInterval = PollInterval; o.clk = &clk })
	ctx, cancel := context.WithCancel(testCtx(t))
	defer cancel()
	stream, err := e.client.WatchVMs(ctx, &agentv1.WatchVMsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if got := assigned(t, first.GetSnapshot(), "windows-node-1"); got != 8*gib {
		t.Errorf("first snapshot: assigned %d", got)
	}

	// Reads without a change send nothing: polls on the clock and ListVMs.
	clk.waitTimer(t)
	clk.Advance(PollInterval)
	clk.waitTimer(t)
	clk.Advance(PollInterval)
	e.vm("windows-node-1")

	// A change is sent once it is read.
	if _, err := e.client.SetStartup(testCtx(t), &agentv1.SetStartupRequest{VmName: "windows-node-1", StartupBytes: 10 * gib}); err != nil {
		t.Fatal(err)
	}
	clk.waitTimer(t)
	clk.Advance(PollInterval)
	next, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if got := assigned(t, next.GetSnapshot(), "windows-node-1"); got != 10*gib {
		t.Errorf("after the grow: assigned %d (an unchanged snapshot was sent)", got)
	}

	// vmms goes down: the stream ends with UNAVAILABLE.
	h.set(func(f *fixture) { f.VMMS = "Stopped" })
	_, _ = e.client.ListVMs(testCtx(t), &agentv1.ListVMsRequest{})
	_, err = stream.Recv()
	wantCode(t, "stream after vmms stopped", err, codes.Unavailable)

	// A new stream while Hyper-V is lost ends at once.
	s2, err := e.client.WatchVMs(testCtx(t), &agentv1.WatchVMsRequest{})
	if err == nil {
		_, err = s2.Recv()
	}
	wantCode(t, "new stream", err, codes.Unavailable)

	// Back up: a full snapshot on connect.
	h.set(func(f *fixture) { f.VMMS = "Running" })
	e.vm("windows-node-1")
	s3, err := e.client.WatchVMs(testCtx(t), &agentv1.WatchVMsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if m, err := s3.Recv(); err != nil || len(m.GetSnapshot().GetVms()) != 5 {
		t.Errorf("after recovery: %v, %v", m, err)
	}
}

func TestWatchVMsEndsWithItsContext(t *testing.T) {
	e := newTestEnv(t, newFakeHost(loadFixture(t, "host")), testPolicy(), func(o *envOptions) { o.pollInterval = PollInterval })
	ctx, cancel := context.WithCancel(testCtx(t))
	stream, err := e.client.WatchVMs(ctx, &agentv1.WatchVMsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	cancel()
	_, err = stream.Recv()
	wantCode(t, "cancelled stream", err, codes.Canceled)
}

func TestSameVMsIgnoresObservedAt(t *testing.T) {
	list := []vmRecord{{Name: "a", ID: "1", State: "Running", AssignedBytes: gib}}
	a, b := snapshotMessage(list, t0), snapshotMessage(list, t0.Add(PollInterval))
	if !sameVMs(a, b) {
		t.Error("snapshots differing only in observed_at are not the same")
	}
	list[0].AssignedBytes = 2 * gib
	if sameVMs(a, snapshotMessage(list, t0)) {
		t.Error("a changed VM is not detected")
	}
}
