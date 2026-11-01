package ledger

import (
	"math"
	"testing"
	"time"
)

func TestQuantizeUp(t *testing.T) {
	step := 256 * Mi
	for _, tc := range []struct {
		x, step, want int64
	}{
		{-1, step, 0},
		{0, step, 0},
		{1, step, step},
		{step - 1, step, step},
		{step, step, step},
		{step + 1, step, 2 * step},
		{3*Gi + 1, step, 3*Gi + step},
		{5, 1, 5},
		{5, 0, 5},
		{math.MaxInt64 - Gi, Gi, math.MaxInt64 / Gi * Gi}, // no overflow below the last full step
	} {
		if got := quantizeUp(tc.x, tc.step); got != tc.want {
			t.Errorf("quantizeUp(%d, %d) = %d, want %d", tc.x, tc.step, got, tc.want)
		}
	}
}

func TestEffBaseOf(t *testing.T) {
	for _, tc := range []struct {
		name               string
		base, used, leased int64
		usedKnown          bool
		want               int64
	}{
		{"used unknown → base", 4 * Gi, 10 * Gi, 0, false, 4 * Gi},
		{"used below base", 4 * Gi, 3 * Gi, 0, true, 4 * Gi},
		{"used above base", 4 * Gi, 6 * Gi, 0, true, 6 * Gi},
		{"leases explain usage", 4 * Gi, 10 * Gi, 6 * Gi, true, 4 * Gi},
		{"non-lease usage above base", 4 * Gi, 12 * Gi, 6 * Gi, true, 6 * Gi},
		{"leases larger than used", 4 * Gi, 2 * Gi, 8 * Gi, true, 4 * Gi},
	} {
		if got := effBaseOf(tc.base, tc.used, tc.usedKnown, tc.leased); got != tc.want {
			t.Errorf("%s: effBase = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestFloorOf(t *testing.T) {
	if got := floorOf(4*Gi, 3*Gi, 2*Gi); got != 9*Gi {
		t.Errorf("floor = %d, want 9Gi", got)
	}
	if got := floorOf(4*Gi, 0, 0); got != 4*Gi {
		t.Errorf("floor = %d, want 4Gi", got)
	}
}

func TestCommittedOf(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   committedInput
		want int64
	}{
		{"unmanaged static", committedInput{assigned: 6 * Gi}, 6 * Gi},
		{"unmanaged DM → maximum", committedInput{assigned: 3 * Gi, dynamic: true, dynamicMax: 10 * Gi}, 10 * Gi},
		{"fixed → assigned even below floor", committedInput{managed: true, mode: ModeFixed, assigned: 8 * Gi, floor: 9 * Gi}, 8 * Gi},
		{"static assigned above floor", committedInput{managed: true, mode: ModeStatic, assigned: 8 * Gi, floor: 6 * Gi}, 8 * Gi},
		{"static floor above assigned", committedInput{managed: true, mode: ModeStatic, assigned: 8 * Gi, floor: 9 * Gi}, 9 * Gi},
		{"static in-flight target", committedInput{managed: true, mode: ModeStatic, assigned: 8 * Gi, floor: 9 * Gi, inflightTarget: 10 * Gi}, 10 * Gi},
		{
			"DM maximum accounting", committedInput{
				managed: true, mode: ModeDynamic, dmAcct: DMAccountingMaximum,
				assigned: 4 * Gi, floor: 5 * Gi, dynamic: true, dynamicMax: 12 * Gi,
			}, 12 * Gi,
		},
		{
			"DM maximum accounting, grow above maximum in flight", committedInput{
				managed: true, mode: ModeDynamic, dmAcct: DMAccountingMaximum,
				assigned: 4 * Gi, dynamic: true, dynamicMax: 12 * Gi, inflightTarget: 13 * Gi,
			}, 13 * Gi,
		},
		{
			"DM assigned accounting", committedInput{
				managed: true, mode: ModeDynamic, dmAcct: DMAccountingAssigned,
				assigned: 4 * Gi, floor: 5 * Gi, dynamic: true, dynamicMax: 12 * Gi,
			}, 5 * Gi,
		},
		{
			"DM maximum accounting without DM data falls back to static", committedInput{
				managed: true, mode: ModeDynamic, dmAcct: DMAccountingMaximum, assigned: 4 * Gi, floor: 5 * Gi,
			}, 5 * Gi,
		},
	} {
		if got := committedOf(tc.in); got != tc.want {
			t.Errorf("%s: committed = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestPoolOf(t *testing.T) {
	for _, tc := range []struct {
		name                                     string
		capacity, reserve, committed, free, want int64
	}{
		{"capacity term binds", 64 * Gi, 4 * Gi, 50 * Gi, 20 * Gi, 10 * Gi},
		{"hostFree term binds", 64 * Gi, 4 * Gi, 40 * Gi, 12 * Gi, 8 * Gi},
		{"over-committed is negative", 64 * Gi, 4 * Gi, 62 * Gi, 12 * Gi, -2 * Gi},
		{"hostFree below reserve", 64 * Gi, 4 * Gi, 10 * Gi, 3 * Gi, -1 * Gi},
	} {
		if got := poolOf(tc.capacity, tc.reserve, tc.committed, tc.free); got != tc.want {
			t.Errorf("%s: pool = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestDeltaAndFits(t *testing.T) {
	step := 256 * Mi
	for _, tc := range []struct {
		name                  string
		bytes, headroom, pool int64
		delta                 int64
		fits                  bool
	}{
		{"headroom covers", 2 * Gi, 2 * Gi, 0, 0, true},
		{"headroom covers, pool negative", 1 * Gi, 2 * Gi, -5 * Gi, 0, true},
		{"partial headroom", 3 * Gi, 2 * Gi, 8 * Gi, 1 * Gi, true},
		{"quantized up", 2*Gi + 1, 2 * Gi, 8 * Gi, step, true},
		{"negative headroom counts as zero", 1 * Gi, -3 * Gi, 8 * Gi, 1 * Gi, true},
		{"pool exactly enough", 3 * Gi, 0, 3 * Gi, 3 * Gi, true},
		{"pool short", 3 * Gi, 0, 3*Gi - 1, 3 * Gi, false},
	} {
		d := deltaOf(tc.bytes, tc.headroom, step)
		if d != tc.delta {
			t.Errorf("%s: delta = %d, want %d", tc.name, d, tc.delta)
		}
		if got := fitsOf(d, tc.pool); got != tc.fits {
			t.Errorf("%s: fits = %v, want %v", tc.name, got, tc.fits)
		}
	}
}

// TestSnapshotQuantities checks every §3.1 quantity end to end through
// Observe and Snapshot.
func TestSnapshotQuantities(t *testing.T) {
	f := standard(t)
	f.l.Observe(Observation{VMs: []VMObservation{
		{Name: "other-1", Assigned: 6 * Gi, ObservedAt: f.now()},
		{Name: "other-2", Assigned: 3 * Gi, Dynamic: &DynamicMemory{Maximum: 10 * Gi}, ObservedAt: f.now()},
	}})
	s := f.l.Snapshot()
	want := map[VMName]struct {
		state                                    VMState
		effBase, floor, headroom, committed, use int64
	}{
		"vm-a":         {VMReady, 4 * Gi, 6 * Gi, 2 * Gi, 8 * Gi, 2 * Gi},
		"vm-b":         {VMReady, 4 * Gi, 6 * Gi, 2 * Gi, 8 * Gi, 2 * Gi},
		"linux-node-1": {VMReady, 5 * Gi, 7 * Gi, 9 * Gi, 16 * Gi, 5 * Gi},
		"vm-dm":        {VMReady, 2 * Gi, 4 * Gi, 0, 12 * Gi, 1 * Gi},
		"other-1":      {VMUnmanaged, 0, 0, 0, 6 * Gi, 0},
		"other-2":      {VMUnmanaged, 0, 0, 0, 10 * Gi, 0},
	}
	if len(s.VMs) != len(want) {
		t.Fatalf("got %d VMs, want %d", len(s.VMs), len(want))
	}
	for _, v := range s.VMs {
		w := want[v.Name]
		if v.State != w.state || v.EffBase != w.effBase || v.Floor != w.floor || v.Headroom != w.headroom || v.Committed != w.committed || v.Used != w.use {
			t.Errorf("%s: got state %s effBase %d floor %d headroom %d committed %d used %d, want %+v",
				v.Name, v.State, v.EffBase, v.Floor, v.Headroom, v.Committed, v.Used, w)
		}
	}
	// Σ committed = 8+8+16+12+6+10 = 60; capacity term 64−4−60 = 0; free term 12−4 = 8.
	if s.Committed != 60*Gi || s.Capacity != 64*Gi || s.Pool != 0 {
		t.Errorf("committed %d capacity %d pool %d, want 60Gi 64Gi 0", s.Committed, s.Capacity, s.Pool)
	}
}

func TestSnapshotQuantityVariants(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mod   func(f *fixture)
		vm    VMName
		check snapshotCheck
	}{
		{
			name: "configured capacity overrides host total",
			mod: func(f *fixture) {
				f.cfg.Capacity = 50 * Gi
				f.l.Configure(f.cfg)
			},
			vm:    "vm-a",
			check: checkConfiguredCapacity,
		},
		{
			name: "DM assigned accounting",
			mod: func(f *fixture) {
				f.cfg.DMAccounting = DMAccountingAssigned
				f.l.Configure(f.cfg)
			},
			vm:    "vm-dm",
			check: checkDMAssignedAccounting,
		},
		{
			name: "per-VM margin",
			mod: func(f *fixture) {
				f.cfg.VMs[0].Margin = ptr(1 * Gi)
				f.l.Configure(f.cfg)
			},
			vm:    "vm-a",
			check: checkPerVMMargin,
		},
		{
			name:  "non-lease usage above base raises floor; static committed follows floor",
			mod:   func(f *fixture) { f.guest("vm-a", 8*Gi, 7*Gi) },
			vm:    "vm-a",
			check: checkUsageRaisesFloor,
		},
		{
			name: "stale guest data counts as missing and makes the VM Unknown",
			mod: func(f *fixture) {
				f.guest("vm-a", 8*Gi, 7*Gi)
				f.clk.Advance(31 * time.Second)
				f.host(12 * Gi)
				f.vm("vm-a", 8*Gi, -1)
			},
			vm:    "vm-a",
			check: checkStaleGuestData,
		},
		{
			name:  "stale host data freezes the ledger and marks VMs Missing",
			mod:   func(f *fixture) { f.clk.Advance(31 * time.Second) },
			vm:    "vm-a",
			check: checkStaleHostData,
		},
		{
			name:  "guest agent not Ready makes the VM Unknown",
			mod:   func(f *fixture) { f.l.SetAgentState("vm-a", AgentDegraded) },
			vm:    "vm-a",
			check: checkGuestAgentNotReady,
		},
		{
			name: "managed VM never observed",
			mod: func(f *fixture) {
				f.cfg.VMs = append(f.cfg.VMs, VMConfig{Name: "vm-new", GuestOS: GuestWindows, Mode: ModeStatic, Base: 2 * Gi})
				f.l.Configure(f.cfg)
			},
			vm:    "vm-new",
			check: checkNeverObserved,
		},
		{
			name: "complete VM list drops VMs that are gone",
			mod: func(f *fixture) {
				f.l.Observe(Observation{VMs: []VMObservation{{Name: "other-1", Assigned: 6 * Gi, ObservedAt: f.now()}}})
				f.l.Observe(Observation{CompleteVMList: true, VMs: []VMObservation{
					{Name: "vm-a", Assigned: 8 * Gi, ObservedAt: f.now()},
					{Name: "vm-b", Assigned: 8 * Gi, ObservedAt: f.now()},
					{Name: "linux-node-1", Assigned: 16 * Gi, ObservedAt: f.now()},
				}})
			},
			vm:    "vm-dm",
			check: checkCompleteVMList,
		},
		{
			name: "older observations never replace newer ones",
			mod: func(f *fixture) {
				f.l.Observe(Observation{
					Host:  &HostObservation{Total: 1, Free: 1, ObservedAt: f.now().Add(-time.Second)},
					VMs:   []VMObservation{{Name: "vm-a", Assigned: 1, ObservedAt: f.now().Add(-time.Second)}},
					Guest: &GuestObservation{VM: "vm-a", Total: 8 * Gi, Available: 0, ObservedAt: f.now().Add(-time.Second)},
				})
			},
			vm:    "vm-a",
			check: checkOlderObservations,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := standard(t)
			tc.mod(f)
			tc.check(t, f.l.Snapshot(), f.vmView(tc.vm))
		})
	}
}

// snapshotCheck checks a snapshot and the view of one VM in it.
type snapshotCheck func(t *testing.T, s Snapshot, v VMView)

func checkConfiguredCapacity(t *testing.T, s Snapshot, _ VMView) {
	t.Helper()
	if s.Capacity != 50*Gi || s.Pool != 50*Gi-4*Gi-44*Gi {
		t.Errorf("capacity %d pool %d", s.Capacity, s.Pool)
	}
}

func checkDMAssignedAccounting(t *testing.T, s Snapshot, v VMView) {
	t.Helper()
	if v.Committed != 4*Gi || s.Committed != 36*Gi {
		t.Errorf("committed %d total %d", v.Committed, s.Committed)
	}
}

func checkPerVMMargin(t *testing.T, _ Snapshot, v VMView) {
	t.Helper()
	if v.Margin != 1*Gi || v.Floor != 5*Gi || v.Headroom != 3*Gi {
		t.Errorf("margin %d floor %d headroom %d", v.Margin, v.Floor, v.Headroom)
	}
}

func checkUsageRaisesFloor(t *testing.T, _ Snapshot, v VMView) {
	t.Helper()
	if v.EffBase != 7*Gi || v.Floor != 9*Gi || v.Headroom != -1*Gi || v.Committed != 9*Gi {
		t.Errorf("effBase %d floor %d headroom %d committed %d", v.EffBase, v.Floor, v.Headroom, v.Committed)
	}
}

func checkStaleGuestData(t *testing.T, _ Snapshot, v VMView) {
	t.Helper()
	if v.State != VMUnknown || v.UsedKnown || v.EffBase != 4*Gi || v.Floor != 6*Gi {
		t.Errorf("state %s usedKnown %v effBase %d floor %d", v.State, v.UsedKnown, v.EffBase, v.Floor)
	}
}

func checkStaleHostData(t *testing.T, s Snapshot, v VMView) {
	t.Helper()
	if !s.Frozen || v.State != VMMissing || v.Committed != 8*Gi {
		t.Errorf("frozen %v state %s committed %d", s.Frozen, v.State, v.Committed)
	}
}

func checkGuestAgentNotReady(t *testing.T, _ Snapshot, v VMView) {
	t.Helper()
	if v.State != VMUnknown {
		t.Errorf("state %s", v.State)
	}
}

func checkNeverObserved(t *testing.T, _ Snapshot, v VMView) {
	t.Helper()
	if v.State != VMMissing || v.Assigned != 0 || v.Committed != 4*Gi || v.Headroom != -4*Gi {
		t.Errorf("state %s assigned %d committed %d headroom %d", v.State, v.Assigned, v.Committed, v.Headroom)
	}
}

func checkCompleteVMList(t *testing.T, s Snapshot, v VMView) {
	t.Helper()
	if v.State != VMMissing || v.Assigned != 0 || len(s.VMs) != 4 {
		t.Errorf("state %s assigned %d VMs %d", v.State, v.Assigned, len(s.VMs))
	}
}

func checkOlderObservations(t *testing.T, s Snapshot, v VMView) {
	t.Helper()
	if s.HostFree != 12*Gi || v.Assigned != 8*Gi || v.Used != 2*Gi {
		t.Errorf("hostFree %d assigned %d used %d", s.HostFree, v.Assigned, v.Used)
	}
}

// TestInFlightCountsInPool checks that a reservation counts in committed and
// in the hostFree term before the resize lands, and that a committed grow
// stays counted against hostFree until the host reports again.
func TestInFlightCountsInPool(t *testing.T) {
	f := standard(t)
	f.enqueue(ext("r1", "vm-a", 5*Gi)) // headroom 2Gi → delta 3Gi, target 11Gi
	res := f.reserve("r1", "vm-a")
	if res.Delta != 3*Gi || res.Target != 11*Gi {
		t.Fatalf("delta %d target %d", res.Delta, res.Target)
	}
	s := f.l.Snapshot()
	if s.Pool != 5*Gi || s.InFlight != 3*Gi {
		t.Fatalf("after reserve: pool %d inflight %d, want 5Gi 3Gi", s.Pool, s.InFlight)
	}
	if err := f.l.MarkResizing(res); err != nil {
		t.Fatal(err)
	}
	if err := f.l.Commit(res, ResizeOutcome{AssignedBefore: 8 * Gi, AssignedAfter: 11 * Gi, StartupAfter: 11 * Gi, ObservedAt: f.now()}); err != nil {
		t.Fatal(err)
	}
	s = f.l.Snapshot()
	if s.Pool != 5*Gi || s.InFlight != 3*Gi || f.vmView("vm-a").Assigned != 11*Gi {
		t.Fatalf("after commit: pool %d inflight %d", s.Pool, s.InFlight)
	}
	f.host(12 * Gi) // same instant: may predate the resize, debt stays
	if s = f.l.Snapshot(); s.InFlight != 3*Gi {
		t.Fatalf("same-instant host observation cleared debt: inflight %d", s.InFlight)
	}
	f.clk.Advance(time.Second)
	f.host(9 * Gi)
	if s = f.l.Snapshot(); s.InFlight != 0 || s.Pool != 5*Gi {
		t.Fatalf("after host observation: pool %d inflight %d", s.Pool, s.InFlight)
	}
}
