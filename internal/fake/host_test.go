package fake_test

import (
	"net"
	"regexp"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/testing/protocmp"

	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
	"github.com/matfax/hvmc/internal/fake"
	"github.com/matfax/hvmc/internal/policy"
)

func TestGetHostAndListVMs(t *testing.T) {
	e := newEnv(t, nil)
	ctx := testCtx(t)

	resp, err := e.host.GetHost(ctx, &agentv1.GetHostRequest{})
	e.must(err)
	sc := loadTestScenario(t)
	want := &agentv1.Host{
		Name:         "hyperv-1",
		TotalBytes:   64 * gib,
		FreeBytes:    34 * gib,
		Agent:        &agentv1.AgentInfo{Version: fake.DefaultAgentVersion, Role: agentv1.AgentRole_AGENT_ROLE_HOST, OperatingSystem: agentv1.OperatingSystem_OPERATING_SYSTEM_WINDOWS},
		PolicyDigest: canonicalDigest(t, policyProto(sc.Policy)),
		ObservedAt:   timestamp(t0),
	}
	if diff := cmp.Diff(want, resp.GetHost(), protocmp.Transform()); diff != "" {
		t.Errorf("GetHost (-want +got):\n%s", diff)
	}

	list, err := e.host.ListVMs(ctx, &agentv1.ListVMsRequest{})
	e.must(err)
	snap := list.GetSnapshot()
	if got := len(snap.GetVms()); got != 5 {
		t.Fatalf("%d VMs, want 5", got)
	}
	if !snap.GetObservedAt().AsTime().Equal(t0) {
		t.Errorf("snapshot observed_at = %v", snap.GetObservedAt().AsTime())
	}
	guid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	for _, v := range snap.GetVms() {
		if !guid.MatchString(v.GetId()) {
			t.Errorf("%s: ID %q is not a lowercase GUID", v.GetName(), v.GetId())
		}
	}
	dm := e.vm(ctx, "vm-dm")
	wantDM := &agentv1.VM{
		Name: "vm-dm", Id: dm.GetId(), State: agentv1.VMState_VM_STATE_RUNNING, HypervState: "Running",
		AssignedBytes: 6 * gib, StartupBytes: 4 * gib,
		DynamicMemory: &agentv1.DynamicMemory{Enabled: true, MinimumBytes: 4 * gib, MaximumBytes: 12 * gib, BufferPercent: 20, Priority: 50},
		ObservedAt:    timestamp(t0),
	}
	if diff := cmp.Diff(wantDM, dm, protocmp.Transform()); diff != "" {
		t.Errorf("vm-dm (-want +got):\n%s", diff)
	}
	off := e.vm(ctx, "vm-off")
	if off.GetState() != agentv1.VMState_VM_STATE_OFF || off.GetAssignedBytes() != 0 || off.GetDynamicMemory() == nil || off.GetDynamicMemory().GetEnabled() {
		t.Errorf("vm-off = %v", off)
	}
	if got := e.vm(ctx, "vm-unmanaged").GetId(); got != "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0" {
		t.Errorf("explicit ID not kept: %s", got)
	}

	// Raw Hyper-V states map to the coarse enum.
	e.must(e.sim.UpdateVM("vm-unmanaged", func(v *fake.VMSpec) { v.State = "RunningCritical" }))
	if got := e.vm(ctx, "vm-unmanaged"); got.GetState() != agentv1.VMState_VM_STATE_OTHER || got.GetHypervState() != "RunningCritical" {
		t.Errorf("RunningCritical maps to %v", got.GetState())
	}
}

func TestSetStartupGrowAndShrink(t *testing.T) {
	e := newEnv(t, nil)
	ctx := testCtx(t)

	resp, err := e.host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-a", StartupBytes: 12 * gib})
	e.must(err)
	r := resp.GetResult()
	if r.GetBefore().GetAssignedBytes() != 8*gib || r.GetAfter().GetAssignedBytes() != 12*gib || r.GetAfter().GetStartupBytes() != 12*gib {
		t.Errorf("grow result = %v", r)
	}
	if got := e.free(ctx); got != 30*gib {
		t.Errorf("free after grow = %d GiB, want 30", got/gib)
	}

	resp, err = e.host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-a", StartupBytes: 6 * gib})
	e.must(err)
	if got := resp.GetResult().GetAfter(); got.GetAssignedBytes() != 6*gib || got.GetStartupBytes() != 6*gib {
		t.Errorf("shrink result = %v", got)
	}
	if got := e.free(ctx); got != 36*gib {
		t.Errorf("free after shrink = %d GiB, want 36", got/gib)
	}

	// Setting the current value succeeds without a change.
	resp, err = e.host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-a", StartupBytes: 6 * gib})
	e.must(err)
	if diff := cmp.Diff(resp.GetResult().GetBefore(), resp.GetResult().GetAfter(), protocmp.Transform()); diff != "" {
		t.Errorf("no-op SetStartup changed the VM:\n%s", diff)
	}

	// An Off VM only changes its configuration.
	resp, err = e.host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-off", StartupBytes: 6 * gib})
	e.must(err)
	if got := resp.GetResult().GetAfter(); got.GetStartupBytes() != 6*gib || got.GetAssignedBytes() != 0 {
		t.Errorf("Off VM result = %v", got)
	}
	if got := e.free(ctx); got != 36*gib {
		t.Errorf("free changed by an Off VM: %d GiB", got/gib)
	}
}

func TestSetStartupErrors(t *testing.T) {
	tests := []struct {
		name  string
		vm    string
		bytes int64
		edit  func(*fake.Scenario)
		want  codes.Code
	}{
		{name: "no VM name", bytes: 8 * gib, want: codes.InvalidArgument},
		{name: "zero bytes", vm: "vm-a", want: codes.InvalidArgument},
		{name: "misaligned", vm: "vm-a", bytes: 8*gib + mib, want: codes.InvalidArgument},
		{name: "unknown VM", vm: "vm-nope", bytes: 8 * gib, want: codes.NotFound},
		{name: "not in policy", vm: "vm-unmanaged", bytes: 6 * gib, want: codes.PermissionDenied},
		{name: "fixed VM", vm: "linux-node-1", bytes: 8 * gib, want: codes.FailedPrecondition},
		{name: "DM VM", vm: "vm-dm", bytes: 6 * gib, want: codes.FailedPrecondition},
		{name: "below policy min", vm: "vm-a", bytes: 2 * gib, want: codes.PermissionDenied},
		{name: "above policy max", vm: "vm-a", bytes: 18 * gib, want: codes.PermissionDenied},
		{name: "saved VM", vm: "vm-a", bytes: 10 * gib, want: codes.FailedPrecondition, edit: func(sc *fake.Scenario) {
			sc.VMs[1].State, sc.VMs[1].Assigned = "Saved", ptr(fake.Bytes(0))
		}},
		{name: "saved VM above policy max", vm: "vm-a", bytes: 18 * gib, want: codes.FailedPrecondition, edit: func(sc *fake.Scenario) {
			sc.VMs[1].State, sc.VMs[1].Assigned = "Saved", ptr(fake.Bytes(0))
		}},
		{name: "saved VM not in policy", vm: "vm-unmanaged", bytes: 6 * gib, want: codes.PermissionDenied, edit: func(sc *fake.Scenario) {
			sc.VMs[3].State, sc.VMs[3].Assigned = "Saved", ptr(fake.Bytes(0))
		}},
		{name: "host out of memory", vm: "vm-a", bytes: 16 * gib, want: codes.ResourceExhausted, edit: func(sc *fake.Scenario) {
			sc.Host.Overhead = 32 * fake.GiB // free 2Gi
		}},
		{name: "vmms down", vm: "vm-a", bytes: 10 * gib, want: codes.Unavailable, edit: func(sc *fake.Scenario) {
			sc.Host.VMMSDown = true
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, tt.edit)
			_, err := e.host.SetStartup(testCtx(t), &agentv1.SetStartupRequest{VmName: tt.vm, StartupBytes: tt.bytes})
			wantCode(t, err, tt.want)
		})
	}

	// RESOURCE_EXHAUSTED carries the VM as re-read, unchanged.
	e := newEnv(t, func(sc *fake.Scenario) { sc.Host.Overhead = 32 * fake.GiB })
	_, err := e.host.SetStartup(testCtx(t), &agentv1.SetStartupRequest{VmName: "vm-a", StartupBytes: 16 * gib})
	if r := resizeDetails(t, err); r.GetAfter().GetAssignedBytes() != 8*gib {
		t.Errorf("details after = %v", r.GetAfter())
	}
}

func TestSetDynamicLimits(t *testing.T) {
	tests := []struct {
		name     string
		vm       string
		min, max *int64
		want     codes.Code
	}{
		{name: "no VM name", max: ptr(16 * gib), want: codes.InvalidArgument},
		{name: "neither limit", vm: "vm-dm", want: codes.InvalidArgument},
		{name: "misaligned", vm: "vm-dm", max: ptr(16*gib + mib), want: codes.InvalidArgument},
		{name: "min above max", vm: "vm-dm", min: ptr(8 * gib), max: ptr(6 * gib), want: codes.InvalidArgument},
		{name: "min above current max", vm: "vm-dm", min: ptr(14 * gib), want: codes.InvalidArgument},
		{name: "unknown VM", vm: "vm-nope", max: ptr(16 * gib), want: codes.NotFound},
		{name: "not in policy", vm: "vm-unmanaged", max: ptr(16 * gib), want: codes.PermissionDenied},
		{name: "fixed VM", vm: "linux-node-1", max: ptr(16 * gib), want: codes.FailedPrecondition},
		{name: "static VM", vm: "vm-a", max: ptr(16 * gib), want: codes.FailedPrecondition},
		{name: "lower max", vm: "vm-dm", max: ptr(10 * gib), want: codes.FailedPrecondition},
		{name: "raise min", vm: "vm-dm", min: ptr(6 * gib), want: codes.FailedPrecondition},
		{name: "min below policy", vm: "vm-dm", min: ptr(1 * gib), want: codes.PermissionDenied},
		{name: "max above policy", vm: "vm-dm", max: ptr(32 * gib), want: codes.PermissionDenied},
		{name: "min below policy, lower max", vm: "vm-dm", min: ptr(1 * gib), max: ptr(10 * gib), want: codes.FailedPrecondition},
		{name: "raise max", vm: "vm-dm", max: ptr(16 * gib), want: codes.OK},
		{name: "lower min", vm: "vm-dm", min: ptr(2 * gib), want: codes.OK},
		{name: "same values", vm: "vm-dm", min: ptr(4 * gib), max: ptr(12 * gib), want: codes.OK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, nil)
			resp, err := e.host.SetDynamicLimits(testCtx(t), &agentv1.SetDynamicLimitsRequest{VmName: tt.vm, MinimumBytes: tt.min, MaximumBytes: tt.max})
			wantCode(t, err, tt.want)
			if err != nil {
				return
			}
			after := resp.GetResult().GetAfter().GetDynamicMemory()
			if tt.min != nil && after.GetMinimumBytes() != *tt.min || tt.max != nil && after.GetMaximumBytes() != *tt.max {
				t.Errorf("after = %v", after)
			}
			if got := e.vm(testCtx(t), "vm-dm").GetDynamicMemory(); !cmp.Equal(got, after, protocmp.Transform()) {
				t.Errorf("ListVMs shows %v, result said %v", got, after)
			}
		})
	}
}

// TestSetDynamicLimitsChecksChangedSettingsOnly: like the agent
// (policy.Policy.Allows), the fake checks only the limits a request sets,
// so raising the maximum works while the current minimum lies below the
// policy's min.
func TestSetDynamicLimitsChecksChangedSettingsOnly(t *testing.T) {
	e := newEnv(t, func(sc *fake.Scenario) { sc.Policy.VMs[2].Min = 6 * fake.GiB })
	ctx := testCtx(t)
	_, err := e.host.SetDynamicLimits(ctx, &agentv1.SetDynamicLimitsRequest{VmName: "vm-dm", MaximumBytes: ptr(16 * gib)})
	e.must(err)
	_, err = e.host.SetDynamicLimits(ctx, &agentv1.SetDynamicLimitsRequest{VmName: "vm-dm", MinimumBytes: ptr(4 * gib)})
	wantCode(t, err, codes.PermissionDenied)
}

func TestGrowLatency(t *testing.T) {
	e := newEnv(t, func(sc *fake.Scenario) { sc.Host.GrowLatency = fake.Duration(3 * time.Second) })
	ctx := testCtx(t)

	type result struct {
		resp *agentv1.SetStartupResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := e.host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-a", StartupBytes: 12 * gib})
		done <- result{resp, err}
	}()
	e.must(e.clk.BlockUntil(ctx, 1))

	// In flight: the grow is reserved in host free memory, not yet assigned.
	if got := e.free(ctx); got != 30*gib {
		t.Errorf("free during grow = %d GiB, want 30", got/gib)
	}
	if got := e.vm(ctx, "vm-a").GetAssignedBytes(); got != 8*gib {
		t.Errorf("assigned during grow = %d GiB, want 8", got/gib)
	}
	e.clk.Advance(3*time.Second - time.Nanosecond)
	select {
	case r := <-done:
		t.Fatalf("SetStartup returned before the latency passed: %v", r.err)
	default:
	}
	e.clk.Advance(time.Nanosecond)
	r := <-done
	e.must(r.err)
	if got := r.resp.GetResult().GetAfter(); got.GetAssignedBytes() != 12*gib || !got.GetObservedAt().AsTime().Equal(t0.Add(3*time.Second)) {
		t.Errorf("after = %v", got)
	}
	if got := e.free(ctx); got != 30*gib {
		t.Errorf("free after grow = %d GiB, want 30", got/gib)
	}

	// Shrinks are not delayed.
	_, err := e.host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-a", StartupBytes: 8 * gib})
	e.must(err)
}

// TestGrowOutlivesCmdletTimeout covers a grow slower than the agent's
// cmdlet timeout: the caller gets DEADLINE_EXCEEDED, the grow still lands.
func TestGrowOutlivesCmdletTimeout(t *testing.T) {
	e := newEnv(t, func(sc *fake.Scenario) {
		sc.Host.GrowLatency = fake.Duration(40 * time.Second)
		sc.Host.CmdletTimeout = fake.Duration(30 * time.Second)
	})
	ctx := testCtx(t)
	done := make(chan error, 1)
	go func() {
		_, err := e.host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-a", StartupBytes: 12 * gib})
		done <- err
	}()
	e.must(e.clk.BlockUntil(ctx, 2)) // grow and cmdlet timeout
	e.clk.Advance(30 * time.Second)
	wantCode(t, <-done, codes.DeadlineExceeded)
	if got := e.vm(ctx, "vm-a").GetAssignedBytes(); got != 8*gib {
		t.Errorf("assigned at the timeout = %d GiB, want 8", got/gib)
	}
	e.clk.Advance(10 * time.Second)
	if got := e.vm(ctx, "vm-a").GetAssignedBytes(); got != 12*gib {
		t.Errorf("assigned after the grow landed = %d GiB, want 12", got/gib)
	}
}

func TestShrinkRefusedPartialAndDrift(t *testing.T) {
	t.Run("refused", testShrinkRefused)
	t.Run("partial then midpoint retry", testShrinkPartial)
	t.Run("startup drift and repair", testShrinkDrift)
}

func testShrinkRefused(t *testing.T) {
	ctx := testCtx(t)
	e := newEnv(t, func(sc *fake.Scenario) { sc.VMs[1].ShrinkFloor = ptr(8 * fake.GiB) })
	_, err := e.host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-a", StartupBytes: 6 * gib})
	wantCode(t, err, codes.Aborted)
	r := resizeDetails(t, err)
	if r.GetAfter().GetAssignedBytes() != 8*gib || r.GetAfter().GetStartupBytes() != 8*gib {
		t.Errorf("after = %v", r.GetAfter())
	}
	if got := e.vm(ctx, "vm-a"); got.GetAssignedBytes() != 8*gib || got.GetStartupBytes() != 8*gib {
		t.Errorf("VM = %v", got)
	}
}

func testShrinkPartial(t *testing.T) {
	ctx := testCtx(t)
	e := newEnv(t, func(sc *fake.Scenario) { sc.VMs[1].ShrinkFloor = ptr(6 * fake.GiB) })
	_, err := e.host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-a", StartupBytes: 4 * gib})
	wantCode(t, err, codes.Aborted)
	if got := e.vm(ctx, "vm-a"); got.GetAssignedBytes() != 6*gib || got.GetStartupBytes() != 6*gib {
		t.Errorf("after partial shrink VM = %v", got)
	}
	if got := e.free(ctx); got != 36*gib {
		t.Errorf("free after partial shrink = %d GiB, want 36", got/gib)
	}
	// Retrying at a value the guest accepts succeeds.
	_, err = e.host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-a", StartupBytes: 6 * gib})
	e.must(err)
}

func testShrinkDrift(t *testing.T) {
	ctx := testCtx(t)
	e := newEnv(t, func(sc *fake.Scenario) {
		sc.VMs[1].ShrinkFloor = ptr(8 * fake.GiB)
		sc.VMs[1].StartupDrift = true
	})
	_, err := e.host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-a", StartupBytes: 6 * gib})
	wantCode(t, err, codes.Aborted)
	got := e.vm(ctx, "vm-a")
	if got.GetAssignedBytes() != 8*gib || got.GetStartupBytes() != 6*gib {
		t.Fatalf("after refused shrink VM = %v, want assigned 8Gi, startup drifted to 6Gi", got)
	}
	// DESIGN §3.5: restore MemoryStartup to the assigned value.
	resp, err := e.host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-a", StartupBytes: 8 * gib})
	e.must(err)
	if a := resp.GetResult().GetAfter(); a.GetAssignedBytes() != 8*gib || a.GetStartupBytes() != 8*gib {
		t.Errorf("after repair = %v", a)
	}
}

func TestVMMSDown(t *testing.T) {
	e := newEnv(t, nil)
	ctx := testCtx(t)
	stream, err := e.host.WatchVMs(ctx, &agentv1.WatchVMsRequest{})
	e.must(err)
	_, err = stream.Recv()
	e.must(err)

	e.must(e.sim.SetVMMSDown(true))
	_, err = stream.Recv()
	wantCode(t, err, codes.Unavailable)
	_, err = e.host.GetHost(ctx, &agentv1.GetHostRequest{})
	wantCode(t, err, codes.Unavailable)
	_, err = e.host.ListVMs(ctx, &agentv1.ListVMsRequest{})
	wantCode(t, err, codes.Unavailable)
	_, err = e.host.SetDynamicLimits(ctx, &agentv1.SetDynamicLimitsRequest{VmName: "vm-dm", MaximumBytes: ptr(16 * gib)})
	wantCode(t, err, codes.Unavailable)
	again, err := e.host.WatchVMs(ctx, &agentv1.WatchVMsRequest{})
	e.must(err)
	_, err = again.Recv()
	wantCode(t, err, codes.Unavailable)
	// The policy and certificate RPCs do not need Hyper-V.
	active, _ := e.sim.PolicyDigests()
	_, err = e.host.ProposePolicy(ctx, &agentv1.ProposePolicyRequest{Policy: policyProto(loadTestScenario(t).Policy), Digest: active})
	e.must(err)

	e.must(e.sim.SetVMMSDown(false))
	stream, err = e.host.WatchVMs(ctx, &agentv1.WatchVMsRequest{})
	e.must(err)
	if _, err := stream.Recv(); err != nil {
		t.Errorf("WatchVMs after recovery: %v", err)
	}
}

func TestHostHang(t *testing.T) {
	e := newEnv(t, nil)
	ctx := testCtx(t)
	e.must(e.sim.SetHang("hyperv-1", "GetHost", "WatchVMs"))

	// Hang until the caller's deadline.
	_, err := e.host.GetHost(shortCtx(t), &agentv1.GetHostRequest{})
	wantCode(t, err, codes.DeadlineExceeded)
	stream, err := e.host.WatchVMs(shortCtx(t), &agentv1.WatchVMsRequest{})
	e.must(err)
	_, err = stream.Recv()
	wantCode(t, err, codes.DeadlineExceeded)
	// Other methods still answer.
	_, err = e.host.ListVMs(ctx, &agentv1.ListVMsRequest{})
	e.must(err)

	// Hyper-V calls also end at the agent's cmdlet timeout, on the clock.
	e.must(e.sim.SetHang("hyperv-1", "*"))
	done := make(chan error, 1)
	go func() {
		_, err := e.host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-a", StartupBytes: 10 * gib})
		done <- err
	}()
	e.must(e.clk.BlockUntil(ctx, 1))
	e.clk.Advance(30 * time.Second)
	wantCode(t, <-done, codes.DeadlineExceeded)
	if got := e.sim.CallCount("hyperv-1", "SetStartup"); got != 1 {
		t.Errorf("SetStartup calls = %d", got)
	}

	e.must(e.sim.SetHang("hyperv-1"))
	_, err = e.host.GetHost(ctx, &agentv1.GetHostRequest{})
	e.must(err)
}

func TestHostStale(t *testing.T) {
	e := newEnv(t, nil)
	ctx := testCtx(t)
	e.must(e.sim.SetStale("hyperv-1", 2*time.Minute))
	e.clk.Advance(time.Minute)

	_, err := e.host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-a", StartupBytes: 12 * gib})
	e.must(err)
	h, err := e.host.GetHost(ctx, &agentv1.GetHostRequest{})
	e.must(err)
	if got := h.GetHost(); !got.GetObservedAt().AsTime().Equal(t0.Add(-2*time.Minute)) || got.GetFreeBytes() != 34*gib {
		t.Errorf("stale host = %v, want frozen at t0−2m with 34Gi free", got)
	}
	vm := e.vm(ctx, "vm-a")
	if !vm.GetObservedAt().AsTime().Equal(t0.Add(-2*time.Minute)) || vm.GetAssignedBytes() != 8*gib {
		t.Errorf("stale VM = %v", vm)
	}

	e.must(e.sim.ClearStale("hyperv-1"))
	vm = e.vm(ctx, "vm-a")
	if !vm.GetObservedAt().AsTime().Equal(t0.Add(time.Minute)) || vm.GetAssignedBytes() != 12*gib {
		t.Errorf("fresh VM = %v", vm)
	}

	// Stale from YAML is frozen at start.
	e = newEnv(t, func(sc *fake.Scenario) { sc.Host.Stale = &fake.StaleSpec{Age: fake.Duration(time.Hour)} })
	h, err = e.host.GetHost(ctx, &agentv1.GetHostRequest{})
	e.must(err)
	if got := h.GetHost().GetObservedAt().AsTime(); !got.Equal(t0.Add(-time.Hour)) {
		t.Errorf("observed_at = %v", got)
	}
}

func TestWatchVMs(t *testing.T) {
	e := newEnv(t, nil)
	ctx := testCtx(t)
	stream, err := e.host.WatchVMs(ctx, &agentv1.WatchVMsRequest{})
	e.must(err)
	first, err := stream.Recv()
	e.must(err)
	if got := len(first.GetSnapshot().GetVms()); got != 5 {
		t.Fatalf("initial snapshot has %d VMs", got)
	}

	// Changes that leave every VM as it was send nothing: the next message
	// is the resize.
	e.must(e.sim.UpdateHost(func(h *fake.HostSpec) { h.Overhead = 2 * fake.GiB }))
	e.clk.Advance(time.Second)
	_, err = e.host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-a", StartupBytes: 10 * gib})
	e.must(err)
	next, err := stream.Recv()
	e.must(err)
	if got := findVM(next.GetSnapshot(), "vm-a"); got.GetAssignedBytes() != 10*gib {
		t.Errorf("snapshot after resize: vm-a = %v", got)
	}
	if got := next.GetSnapshot().GetObservedAt().AsTime(); !got.Equal(t0.Add(time.Second)) {
		t.Errorf("observed_at = %v", got)
	}

	// Added and removed VMs show up in full snapshots.
	e.must(e.sim.Update(func(sc *fake.Scenario) {
		sc.VMs = append(sc.VMs, fake.VMSpec{Name: "vm-new", Startup: 2 * fake.GiB})
	}))
	next, err = stream.Recv()
	e.must(err)
	if findVM(next.GetSnapshot(), "vm-new") == nil {
		t.Errorf("added VM missing from snapshot")
	}
	e.must(e.sim.Update(func(sc *fake.Scenario) { sc.VMs = sc.VMs[:len(sc.VMs)-1] }))
	next, err = stream.Recv()
	e.must(err)
	if findVM(next.GetSnapshot(), "vm-new") != nil {
		t.Errorf("removed VM still in snapshot")
	}
}

func TestPolicyDigestsAndProposals(t *testing.T) {
	e := newEnv(t, nil)
	ctx := testCtx(t)
	active := loadTestScenario(t).Policy
	activeDigest := canonicalDigest(t, policyProto(active))

	proposed := policyProto(active)
	proposed.Vms = append(proposed.Vms, &agentv1.VMPolicy{Name: "vm-unmanaged", MinBytes: 2 * gib, MaxBytes: 8 * gib})
	proposedDigest := canonicalDigest(t, proposed)

	_, err := e.host.ProposePolicy(ctx, &agentv1.ProposePolicyRequest{Policy: proposed, Digest: "not-the-digest"})
	wantCode(t, err, codes.InvalidArgument)
	for _, bad := range []*agentv1.Policy{
		{Vms: []*agentv1.VMPolicy{{Name: "vm-a", MinBytes: 8 * gib, MaxBytes: 4 * gib}}},
		{Vms: []*agentv1.VMPolicy{{Name: "vm-a", Fixed: true, MaxBytes: 4 * gib}}},
		{Vms: []*agentv1.VMPolicy{{Name: "vm-a", Fixed: true}, {Name: "vm-a", Fixed: true}}},
		{Vms: []*agentv1.VMPolicy{{Name: "vm\na", Fixed: true}}},
	} {
		_, err = e.host.ProposePolicy(ctx, &agentv1.ProposePolicyRequest{Policy: bad, Digest: rawDigest(bad)})
		wantCode(t, err, codes.InvalidArgument)
	}

	resp, err := e.host.ProposePolicy(ctx, &agentv1.ProposePolicyRequest{Policy: proposed, Digest: proposedDigest})
	e.must(err)
	if resp.GetActiveDigest() != activeDigest || resp.GetPendingDigest() != proposedDigest {
		t.Errorf("ProposePolicy = %v", resp)
	}
	h, err := e.host.GetHost(ctx, &agentv1.GetHostRequest{})
	e.must(err)
	if h.GetHost().GetPolicyDigest() != activeDigest || h.GetHost().GetPendingPolicyDigest() != proposedDigest {
		t.Errorf("GetHost digests = %q, %q", h.GetHost().GetPolicyDigest(), h.GetHost().GetPendingPolicyDigest())
	}
	// The pending policy grants nothing yet.
	_, err = e.host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-unmanaged", StartupBytes: 6 * gib})
	wantCode(t, err, codes.PermissionDenied)

	// Proposing the active policy discards the pending one; VM order does
	// not matter.
	reordered := policyProto(active)
	reordered.Vms[0], reordered.Vms[1] = reordered.Vms[1], reordered.Vms[0]
	resp, err = e.host.ProposePolicy(ctx, &agentv1.ProposePolicyRequest{Policy: reordered, Digest: activeDigest})
	e.must(err)
	if resp.GetPendingDigest() != "" {
		t.Errorf("pending digest = %q, want empty", resp.GetPendingDigest())
	}
	if err := e.sim.ActivatePendingPolicy(); err == nil {
		t.Errorf("ActivatePendingPolicy without a pending policy succeeded")
	}

	// The operator activates a proposal on the host.
	_, err = e.host.ProposePolicy(ctx, &agentv1.ProposePolicyRequest{Policy: proposed, Digest: proposedDigest})
	e.must(err)
	e.must(e.sim.ActivatePendingPolicy())
	if a, p := e.sim.PolicyDigests(); a != proposedDigest || p != "" {
		t.Errorf("after activation digests = %q, %q", a, p)
	}
	_, err = e.host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-unmanaged", StartupBytes: 6 * gib})
	e.must(err)
	if got := e.sim.CallCount("hyperv-1", "ProposePolicy"); got != 8 {
		t.Errorf("ProposePolicy calls = %d, want 8", got)
	}

	// A missing policy is the empty policy, as for policy.FromProto.
	empty := policy.Policy{}.Digest()
	resp, err = e.host.ProposePolicy(ctx, &agentv1.ProposePolicyRequest{Digest: empty})
	e.must(err)
	if resp.GetPendingDigest() != empty {
		t.Errorf("pending digest = %q, want the empty policy's %q", resp.GetPendingDigest(), empty)
	}
	e.must(e.sim.ActivatePendingPolicy())
	_, err = e.host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-unmanaged", StartupBytes: 4 * gib})
	wantCode(t, err, codes.PermissionDenied)
}

func TestPolicyDigestSkew(t *testing.T) {
	e := newEnv(t, func(sc *fake.Scenario) { sc.Host.PolicyDigestSkew = true })
	ctx := testCtx(t)
	p := policyProto(loadTestScenario(t).Policy)
	h, err := e.host.GetHost(ctx, &agentv1.GetHostRequest{})
	e.must(err)
	if h.GetHost().GetPolicyDigest() == canonicalDigest(t, p) {
		t.Errorf("skewed agent reports the coordinator's digest")
	}
	_, err = e.host.ProposePolicy(ctx, &agentv1.ProposePolicyRequest{Policy: p, Digest: canonicalDigest(t, p)})
	wantCode(t, err, codes.InvalidArgument)
}

func TestRealListener(t *testing.T) {
	sim, err := fake.New(loadTestScenario(t), fake.WithAgentVersion("v1.2.3"))
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := sim.ServeHost(lis)
	t.Cleanup(srv.Stop)
	conn, err := srv.Dial()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	h, err := agentv1.NewHostAgentClient(conn).GetHost(testCtx(t), &agentv1.GetHostRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.GetHost().GetAgent().GetVersion(); got != "v1.2.3" {
		t.Errorf("version = %q", got)
	}
}

func TestRoleServicesUnimplemented(t *testing.T) {
	e := newEnv(t, nil)
	ctx := testCtx(t)
	_, err := e.guest("hyperv-1").GetMemory(ctx, &agentv1.GetMemoryRequest{})
	wantCode(t, err, codes.Unimplemented)
	_, err = agentv1.NewHostAgentClient(e.conns["vm-a"]).GetHost(ctx, &agentv1.GetHostRequest{})
	wantCode(t, err, codes.Unimplemented)
	if _, err := e.sim.ServeGuest("no-such-guest", fake.Bufconn()); err == nil {
		t.Errorf("ServeGuest of an unknown guest succeeded")
	}
}

func findVM(s *agentv1.VMSnapshot, name string) *agentv1.VM {
	for _, v := range s.GetVms() {
		if v.GetName() == name {
			return v
		}
	}
	return nil
}

// canonicalDigest returns the digest the coordinator computes for p.
func canonicalDigest(t *testing.T, p *agentv1.Policy) string {
	t.Helper()
	pol, err := policy.FromProto(p)
	if err != nil {
		t.Fatal(err)
	}
	return pol.Digest()
}

// rawDigest returns the canonical digest of p without validating it, so an
// invalid proposal is refused for its content and not for its digest.
func rawDigest(p *agentv1.Policy) string {
	pol := policy.Policy{}
	for _, v := range p.GetVms() {
		pol.VMs = append(pol.VMs, policy.VM{Name: v.GetName(), Fixed: v.GetFixed(), Min: v.GetMinBytes(), Max: v.GetMaxBytes()})
	}
	return pol.Digest()
}

func policyProto(p *fake.PolicySpec) *agentv1.Policy {
	out := &agentv1.Policy{}
	for _, v := range p.VMs {
		out.Vms = append(out.Vms, &agentv1.VMPolicy{Name: v.Name, Fixed: v.Fixed, MinBytes: int64(v.Min), MaxBytes: int64(v.Max)})
	}
	return out
}
