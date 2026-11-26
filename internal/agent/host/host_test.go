package host

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/matfax/hvmc/internal/agent"
	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
	"github.com/matfax/hvmc/internal/policy"
)

func TestGetHost(t *testing.T) {
	e := newTestEnv(t, newFakeHost(loadFixture(t, "host")), testPolicy())
	resp, err := e.client.GetHost(testCtx(t), &agentv1.GetHostRequest{})
	if err != nil {
		t.Fatal(err)
	}
	want := &agentv1.Host{
		Name: "hyperv-1", TotalBytes: 64 * gib, FreeBytes: 40 * gib,
		Agent: &agentv1.AgentInfo{
			Version: "v1.2.3-test", Role: agentv1.AgentRole_AGENT_ROLE_HOST,
			OperatingSystem: operatingSystem(goos()),
		},
		PolicyDigest: testPolicy().Digest(),
		ObservedAt:   timestamppb.New(t0),
	}
	if diff := cmp.Diff(want, resp.GetHost(), protocmp.Transform()); diff != "" {
		t.Errorf("GetHost (-want +got):\n%s", diff)
	}

	// A pending proposal shows up as its digest.
	proposed := policy.Policy{VMs: []policy.VM{{Name: "windows-node-1", Min: 4 * gib, Max: 32 * gib}}}
	if _, err := e.client.ProposePolicy(testCtx(t), &agentv1.ProposePolicyRequest{Policy: proposed.Proto(), Digest: proposed.Digest()}); err != nil {
		t.Fatal(err)
	}
	resp, err = e.client.GetHost(testCtx(t), &agentv1.GetHostRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.GetHost(); got.GetPendingPolicyDigest() != proposed.Digest() || got.GetPolicyDigest() != testPolicy().Digest() {
		t.Errorf("digests active %q pending %q", got.GetPolicyDigest(), got.GetPendingPolicyDigest())
	}
}

func TestListVMs(t *testing.T) {
	e := newTestEnv(t, newFakeHost(loadFixture(t, "host")), testPolicy())
	resp, err := e.client.ListVMs(testCtx(t), &agentv1.ListVMsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	snap := resp.GetSnapshot()
	var names []string
	for _, v := range snap.GetVms() {
		names = append(names, v.GetName())
	}
	if diff := cmp.Diff([]string{"linux-node-1", "windows-dm-1", "windows-node-1", "windows-off-1", "windows-saved-1"}, names); diff != "" {
		t.Errorf("VM names (-want +got):\n%s", diff)
	}
	want := &agentv1.VM{
		Name: "windows-dm-1", Id: "00000002-0000-4000-8000-000000000002",
		State: agentv1.VMState_VM_STATE_RUNNING, HypervState: "Running",
		AssignedBytes: 4 * gib, StartupBytes: 2 * gib,
		DynamicMemory: &agentv1.DynamicMemory{Enabled: true, MinimumBytes: 2 * gib, MaximumBytes: 8 * gib, BufferPercent: 20, Priority: 50},
		ObservedAt:    timestamppb.New(t0),
	}
	if diff := cmp.Diff(want, snap.GetVms()[1], protocmp.Transform()); diff != "" {
		t.Errorf("DM VM (-want +got):\n%s", diff)
	}
	// Static VMs report no DM settings at all.
	if dm := snap.GetVms()[2].GetDynamicMemory(); dm.GetEnabled() || dm.GetMaximumBytes() != 0 || dm.GetPriority() != 0 {
		t.Errorf("static VM DM = %v", dm)
	}
	if s := snap.GetVms()[4]; s.GetState() != agentv1.VMState_VM_STATE_SAVED {
		t.Errorf("saved VM state %v", s.GetState())
	}
}

func TestReadsWhileVMMSDown(t *testing.T) {
	e := newTestEnv(t, newFakeHost(loadFixture(t, "vmms-down")), testPolicy())
	_, err := e.client.GetHost(testCtx(t), &agentv1.GetHostRequest{})
	wantCode(t, "GetHost", err, codes.Unavailable)
	_, err = e.client.ListVMs(testCtx(t), &agentv1.ListVMsRequest{})
	wantCode(t, "ListVMs", err, codes.Unavailable)
	if err := e.svc.srv.poll.checkVMMS(context.Background()); err == nil {
		t.Error("vmms readiness check passes while vmms is down")
	}
}

func TestSetStartup(t *testing.T) {
	h := newFakeHost(loadFixture(t, "host"))
	e := newTestEnv(t, h, testPolicy())
	ctx := testCtx(t)

	// Grow a running VM.
	resp, err := e.client.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "windows-node-1", StartupBytes: 12 * gib})
	if err != nil {
		t.Fatal(err)
	}
	r := resp.GetResult()
	if r.GetBefore().GetAssignedBytes() != 8*gib || r.GetAfter().GetAssignedBytes() != 12*gib || r.GetAfter().GetStartupBytes() != 12*gib {
		t.Errorf("grow: before %v after %v", r.GetBefore(), r.GetAfter())
	}
	// Shrink it back.
	resp, err = e.client.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "windows-node-1", StartupBytes: 6 * gib})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.GetResult().GetAfter().GetAssignedBytes(); got != 6*gib {
		t.Errorf("shrink: assigned %d", got)
	}
	// An off VM only changes its configuration.
	resp, err = e.client.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "windows-off-1", StartupBytes: 6 * gib})
	if err != nil {
		t.Fatal(err)
	}
	if a := resp.GetResult().GetAfter(); a.GetStartupBytes() != 6*gib || a.GetAssignedBytes() != 0 {
		t.Errorf("off VM after %v", a)
	}
	// Setting the current value succeeds without a change.
	if _, err := e.client.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "windows-node-1", StartupBytes: 6 * gib}); err != nil {
		t.Errorf("no-op write: %v", err)
	}
}

func TestSetStartupRefusedShrink(t *testing.T) {
	e := newTestEnv(t, newFakeHost(loadFixture(t, "refused-shrink")), testPolicy())
	_, err := e.client.SetStartup(testCtx(t), &agentv1.SetStartupRequest{VmName: "windows-node-1", StartupBytes: 4 * gib})
	wantCode(t, "refused shrink", err, codes.Aborted)
	r := resizeDetails(err)
	if r.GetBefore().GetAssignedBytes() != 8*gib || r.GetAfter().GetAssignedBytes() != 8*gib {
		t.Errorf("details before %v after %v", r.GetBefore(), r.GetAfter())
	}
}

func TestSetStartupPartialShrink(t *testing.T) {
	e := newTestEnv(t, newFakeHost(loadFixture(t, "partial-shrink")), testPolicy())
	_, err := e.client.SetStartup(testCtx(t), &agentv1.SetStartupRequest{VmName: "windows-node-1", StartupBytes: 4 * gib})
	wantCode(t, "partial shrink", err, codes.Aborted)
	r := resizeDetails(err)
	// Down to the floor; startup drifted to the requested value.
	if a := r.GetAfter(); a.GetAssignedBytes() != 6*gib || a.GetStartupBytes() != 4*gib {
		t.Errorf("after %v", a)
	}
	if v := e.vm("windows-node-1"); v.GetAssignedBytes() != 6*gib {
		t.Errorf("re-read assigned %d", v.GetAssignedBytes())
	}
}

func TestSetStartupOutOfMemory(t *testing.T) {
	// Win32_OperatingSystem reports 40 GiB free, but Hyper-V has only
	// 1 GiB: the agent's pre-check passes and the cmdlet's out-of-memory
	// error decides.
	e := newTestEnv(t, newFakeHost(loadFixture(t, "out-of-memory")), testPolicy())
	_, err := e.client.SetStartup(testCtx(t), &agentv1.SetStartupRequest{VmName: "windows-node-1", StartupBytes: 12 * gib})
	wantCode(t, "grow", err, codes.ResourceExhausted)
	if r := resizeDetails(err); r.GetAfter().GetAssignedBytes() != 8*gib {
		t.Errorf("details %v", r)
	}
}

func TestSetStartupHostFreeCheck(t *testing.T) {
	h := newFakeHost(loadFixture(t, "host"))
	e := newTestEnv(t, h, testPolicy())
	_, err := e.client.SetStartup(testCtx(t), &agentv1.SetStartupRequest{VmName: "windows-node-1", StartupBytes: 50 * gib})
	wantCode(t, "grow above host free", err, codes.PermissionDenied) // policy max 24Gi ranks first
	e2 := newTestEnv(t, newFakeHost(loadFixture(t, "host")), policy.Policy{VMs: []policy.VM{{Name: "windows-node-1", Min: gib, Max: policy.Unbounded}}})
	_, err = e2.client.SetStartup(testCtx(t), &agentv1.SetStartupRequest{VmName: "windows-node-1", StartupBytes: 50 * gib})
	wantCode(t, "grow above host free", err, codes.ResourceExhausted)
	if r := resizeDetails(err); r.GetBefore().GetAssignedBytes() != 8*gib || r.GetAfter().GetAssignedBytes() != 8*gib {
		t.Errorf("details %v", r)
	}
}

func TestSetStartupHang(t *testing.T) {
	h := newFakeHost(loadFixture(t, "hang"))
	e := newTestEnv(t, h, testPolicy())
	done := make(chan error, 1)
	go func() {
		_, err := e.client.SetStartup(testCtx(t), &agentv1.SetStartupRequest{VmName: "windows-node-1", StartupBytes: 6 * gib})
		done <- err
	}()
	<-h.hung
	e.clk.waitTimer(t) // the call's timer
	e.clk.Advance(DefaultCallTimeout)
	wantCode(t, "hung Set-VMMemory", <-done, codes.DeadlineExceeded)
}

func TestSetDynamicLimits(t *testing.T) {
	e := newTestEnv(t, newFakeHost(loadFixture(t, "host")), testPolicy())
	resp, err := e.client.SetDynamicLimits(testCtx(t), &agentv1.SetDynamicLimitsRequest{
		VmName: "windows-dm-1", MinimumBytes: ptr(1 * gib), MaximumBytes: ptr(12 * gib),
	})
	if err != nil {
		t.Fatal(err)
	}
	dm := resp.GetResult().GetAfter().GetDynamicMemory()
	if dm.GetMinimumBytes() != gib || dm.GetMaximumBytes() != 12*gib {
		t.Errorf("after %v", dm)
	}
	// Only the maximum: the minimum stays.
	resp, err = e.client.SetDynamicLimits(testCtx(t), &agentv1.SetDynamicLimitsRequest{VmName: "windows-dm-1", MaximumBytes: ptr(16 * gib)})
	if err != nil {
		t.Fatal(err)
	}
	if dm := resp.GetResult().GetAfter().GetDynamicMemory(); dm.GetMinimumBytes() != gib || dm.GetMaximumBytes() != 16*gib {
		t.Errorf("after %v", dm)
	}
	if dm := e.vm("windows-dm-1").GetDynamicMemory(); dm.GetMaximumBytes() != 16*gib {
		t.Errorf("re-read %v", dm)
	}
}

func TestDuplicateVMNames(t *testing.T) {
	f := loadFixture(t, "host")
	dup := f.VMs[0]
	dup.ID = "00000009-0000-4000-8000-000000000009"
	f.VMs = append(f.VMs, dup)
	e := newTestEnv(t, newFakeHost(f), testPolicy())
	_, err := e.client.SetStartup(testCtx(t), &agentv1.SetStartupRequest{VmName: "windows-node-1", StartupBytes: 10 * gib})
	wantCode(t, "duplicate name", err, codes.FailedPrecondition)
}

func TestAuditRefusals(t *testing.T) {
	e := newTestEnv(t, newFakeHost(loadFixture(t, "host")), testPolicy())
	ctx := testCtx(t)
	_, err := e.client.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "linux-node-1", StartupBytes: 10 * gib})
	wantCode(t, "fixed VM", err, codes.FailedPrecondition)
	_, err = e.client.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "windows-node-1", StartupBytes: 3})
	wantCode(t, "misaligned", err, codes.InvalidArgument)
	if _, err := e.client.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "windows-node-1", StartupBytes: 10 * gib}); err != nil {
		t.Fatal(err)
	}
	entries := e.audit.entries(t)
	if len(entries) != 3 {
		t.Fatalf("%d audit entries, want 3", len(entries))
	}
	for i, want := range []string{"FailedPrecondition", "InvalidArgument", "OK"} {
		if entries[i].Code != want || entries[i].Method != agentv1.HostAgent_SetStartup_FullMethodName {
			t.Errorf("entry %d: %s %s, want %s", i, entries[i].Method, entries[i].Code, want)
		}
	}
	if len(entries[0].Before) == 0 || len(entries[0].After) != 0 {
		t.Errorf("refusal: before %s after %s", entries[0].Before, entries[0].After)
	}
	if len(entries[2].Before) == 0 || len(entries[2].After) == 0 {
		t.Errorf("success: before %s after %s", entries[2].Before, entries[2].After)
	}
}

func TestPolicyReloadTakesEffect(t *testing.T) {
	e := newTestEnv(t, newFakeHost(loadFixture(t, "host")), testPolicy())
	req := &agentv1.SetStartupRequest{VmName: "windows-node-1", StartupBytes: 30 * gib}
	_, err := e.client.SetStartup(testCtx(t), req)
	wantCode(t, "above max", err, codes.PermissionDenied)
	wider := policy.Policy{VMs: []policy.VM{{Name: "windows-node-1", Min: 4 * gib, Max: 32 * gib}}}
	if err := policy.Save(e.paths.Config, wider); err != nil {
		t.Fatal(err)
	}
	e.store.Reload()
	if _, err := e.client.SetStartup(testCtx(t), req); err != nil {
		t.Errorf("after the policy was widened: %v", err)
	}
}

func TestReadinessChecks(t *testing.T) {
	h := newFakeHost(loadFixture(t, "host"))
	clk := newTestClock()
	e := newTestEnv(t, h, testPolicy(), func(o *envOptions) { o.pollInterval = PollInterval; o.clk = &clk })
	poll := e.svc.srv.poll
	waitFor(t, "first poll", func() <-chan struct{} { _, ch, _ := poll.current(); return ch }, func() bool {
		snap, _, _ := poll.current()
		return snap != nil
	})
	rep := e.ready.Evaluate(context.Background())
	for _, c := range rep.Checks {
		if c.Err != nil {
			t.Errorf("check %s fails: %v", c.Name, c.Err)
		}
	}
	if len(rep.Checks) != 3 {
		t.Errorf("checks %v", rep.Checks)
	}

	// vmms stops: the next poll fails the vmms check, and the reads go
	// stale after 2× the poll interval.
	h.set(func(f *fixture) { f.VMMS = "Stopped" })
	_, ch, _ := poll.current()
	clk.waitTimer(t) // the poller has armed its next timer
	clk.Advance(PollInterval)
	<-ch
	if err := poll.checkVMMS(context.Background()); err == nil {
		t.Error("vmms check passes while vmms is down")
	}
	if err := poll.checkRead(context.Background()); err != nil {
		t.Errorf("read check fails within 2× the interval: %v", err)
	}
	clk.waitTimer(t)
	clk.Advance(2 * PollInterval)
	if err := poll.checkRead(context.Background()); err == nil {
		t.Error("read check passes with stale reads")
	}
}

func TestRegisterNeedsPolicyStore(t *testing.T) {
	err := NewWithOptions(Options{}).Register(nil, depsWithoutPolicy())
	if err == nil {
		t.Error("Register without a policy store succeeds")
	}
	if err := NewWithOptions(Options{}).Run(context.Background()); err == nil {
		t.Error("Run before Register succeeds")
	}
}

func TestRunFailsWhenTheWorkerKeepsFailing(t *testing.T) {
	h := newFakeHost(loadFixture(t, "host"))
	h.failNext = 1 // the first worker fails to start, the second works
	e := newTestEnv(t, h, testPolicy())
	// Crash the running worker; the next starts all fail: five exits.
	h.set(func(*fixture) {
		h.failNext = MaxWorkerExits
		h.crashOn[opHost] = true
	})
	_, err := e.client.GetHost(testCtx(t), &agentv1.GetHostRequest{})
	wantCode(t, "crash", err, codes.Unavailable)
	select {
	case err := <-e.runErr:
		if !errors.Is(err, ErrWorkerFailing) {
			t.Errorf("Run = %v", err)
		}
		e.runErr <- nil
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not give up")
	}
	if err := e.svc.srv.sup.Check(context.Background()); !errors.Is(err, ErrWorkerFailing) {
		t.Errorf("worker check = %v", err)
	}
}

func goos() string { return runtime.GOOS }

func depsWithoutPolicy() agent.Deps { return agent.Deps{Clock: newTestClock()} }
