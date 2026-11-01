package ledger

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// standardObs are the observations of the standard fixture.
func standardObs(at time.Time) []Observation {
	guest := func(vm VMName, total, used int64) Observation {
		return Observation{Guest: &GuestObservation{VM: vm, Total: total, Available: total - used, ObservedAt: at}}
	}
	return []Observation{
		{
			Host: &HostObservation{Total: 64 * Gi, Free: 12 * Gi, ObservedAt: at}, CompleteVMList: true,
			VMs: []VMObservation{
				{Name: "vm-a", Assigned: 8 * Gi, Startup: 8 * Gi, ObservedAt: at},
				{Name: "vm-b", Assigned: 8 * Gi, Startup: 8 * Gi, ObservedAt: at},
				{Name: "linux-node-1", Assigned: 16 * Gi, Startup: 16 * Gi, ObservedAt: at},
				{Name: "vm-dm", Assigned: 4 * Gi, Startup: 4 * Gi, ObservedAt: at, Dynamic: &DynamicMemory{Minimum: Gi, Maximum: 12 * Gi}},
			},
		},
		guest("vm-a", 8*Gi, 2*Gi),
		guest("vm-b", 8*Gi, 2*Gi),
		guest("linux-node-1", 16*Gi, 5*Gi),
		guest("vm-dm", 4*Gi, 1*Gi),
	}
}

func rec(id RequestID, vm VMName, bytes int64, p Phase, enq time.Duration) LeaseRecord {
	return LeaseRecord{
		Request: Request{ID: id, Kind: KindExternal, VM: vm, Bytes: bytes, EnqueuedAt: t0.Add(enq)},
		Phase:   p, GrantedAt: t0.Add(enq + time.Second),
	}
}

// TestRebuild has one row per phase (§3.5, WP 1.3 acceptance) plus the
// orderings and degenerate records Rebuild has to handle. vm-a: assigned 8Gi,
// base 4Gi, floor without leases 6Gi. Granted leases are Lost only when
// assigned < base + Σ leased; Reserved/Resizing ones need floor.
func TestRebuild(t *testing.T) {
	type act struct {
		kind   RecoveryKind
		id     RequestID
		to     Phase
		reason Reason
	}
	for _, tc := range []struct {
		name      string
		obs       []Observation // nil: standardObs
		records   []LeaseRecord
		actions   []act
		phases    map[RequestID]Phase
		positions map[RequestID]Position
	}{
		{
			name: "Pending: re-enqueued at original enqueuedAt",
			records: []LeaseRecord{
				rec("p2", "vm-a", Gi, PhasePending, -time.Minute),
				rec("p1", "vm-a", Gi, PhasePending, -2*time.Minute),
			},
			actions:   []act{{RecoveryEnqueue, "p1", PhasePending, ""}, {RecoveryEnqueue, "p2", PhasePending, ""}},
			phases:    map[RequestID]Phase{"p1": PhasePending, "p2": PhasePending},
			positions: map[RequestID]Position{"p1": 1, "p2": 2},
		},
		{
			name:    "Reserved covered: verify then grant",
			records: []LeaseRecord{rec("r", "vm-a", 2*Gi, PhaseReserved, 0)},
			actions: []act{{RecoveryVerify, "r", PhaseResizing, ""}},
			phases:  map[RequestID]Phase{"r": PhaseResizing},
		},
		{
			name: "Reserved not covered: Pending at original position",
			records: []LeaseRecord{
				rec("p", "vm-b", Gi, PhasePending, -time.Minute),
				rec("r", "vm-a", 3*Gi, PhaseReserved, -3*time.Minute),
			},
			actions:   []act{{RecoveryRequeue, "r", PhasePending, ""}, {RecoveryEnqueue, "p", PhasePending, ""}},
			phases:    map[RequestID]Phase{"r": PhasePending, "p": PhasePending},
			positions: map[RequestID]Position{"r": 1, "p": 2},
		},
		{
			name:    "Resizing covered: verify then grant",
			records: []LeaseRecord{rec("r", "vm-a", 2*Gi, PhaseResizing, 0)},
			actions: []act{{RecoveryVerify, "r", PhaseResizing, ""}},
			phases:  map[RequestID]Phase{"r": PhaseResizing},
		},
		{
			name:      "Resizing not covered: Pending",
			records:   []LeaseRecord{rec("r", "vm-a", 3*Gi, PhaseResizing, 0)},
			actions:   []act{{RecoveryRequeue, "r", PhasePending, ""}},
			phases:    map[RequestID]Phase{"r": PhasePending},
			positions: map[RequestID]Position{"r": 1},
		},
		{
			name:    "Granted covered: unchanged",
			records: []LeaseRecord{rec("g", "vm-a", 2*Gi, PhaseGranted, 0)},
			actions: []act{{RecoveryKeep, "g", PhaseGranted, ""}},
			phases:  map[RequestID]Phase{"g": PhaseGranted},
		},
		{
			name:    "Granted not covered (assigned < base + leased): Failed(Lost)",
			records: []LeaseRecord{rec("g", "vm-a", 5*Gi, PhaseGranted, 0)},
			actions: []act{{RecoveryLost, "g", PhaseFailed, ReasonLost}},
			phases:  map[RequestID]Phase{"g": ""},
		},
		{
			name:    "Released: not loaded",
			records: []LeaseRecord{rec("x", "vm-a", Gi, PhaseReleased, 0)},
			actions: []act{{RecoveryDrop, "x", PhaseReleased, ""}},
			phases:  map[RequestID]Phase{"x": ""},
		},
		{
			name:    "Failed: not loaded",
			records: []LeaseRecord{rec("x", "vm-a", Gi, PhaseFailed, 0)},
			actions: []act{{RecoveryDrop, "x", PhaseFailed, ""}},
			phases:  map[RequestID]Phase{"x": ""},
		},
		{
			name: "Granted leases are kept oldest grant first while covered",
			records: []LeaseRecord{
				{Request: Request{ID: "late", Kind: KindExternal, VM: "vm-a", Bytes: 2 * Gi, EnqueuedAt: t0}, Phase: PhaseGranted, GrantedAt: t0.Add(time.Hour)},
				{Request: Request{ID: "early", Kind: KindExternal, VM: "vm-a", Bytes: 2 * Gi, EnqueuedAt: t0.Add(time.Minute)}, Phase: PhaseGranted, GrantedAt: t0.Add(2 * time.Minute)},
				{Request: Request{ID: "third", Kind: KindExternal, VM: "vm-a", Bytes: 2 * Gi, EnqueuedAt: t0.Add(time.Minute)}, Phase: PhaseGranted, GrantedAt: t0.Add(2 * time.Hour)},
			},
			actions: []act{
				{RecoveryKeep, "late", PhaseGranted, ""},
				{RecoveryKeep, "early", PhaseGranted, ""},
				{RecoveryLost, "third", PhaseFailed, ReasonLost},
			},
			phases: map[RequestID]Phase{"late": PhaseGranted, "early": PhaseGranted, "third": ""},
		},
		{
			// vm-a: assigned 8Gi, used 7.5Gi (free 0.5Gi < margin 2Gi), so
			// floor = 9.5Gi > assigned; base 4Gi + leased 4Gi = 8Gi still fits.
			name: "busy VM with free < margin keeps its Granted leases",
			obs: append(standardObs(t0), Observation{Guest: &GuestObservation{
				VM: "vm-a", Total: 8 * Gi, Available: Gi / 2, ObservedAt: t0,
			}}),
			records: []LeaseRecord{
				rec("g1", "vm-a", 3*Gi, PhaseGranted, 0),
				rec("g2", "vm-a", 1*Gi, PhaseGranted, time.Minute),
			},
			actions: []act{{RecoveryKeep, "g1", PhaseGranted, ""}, {RecoveryKeep, "g2", PhaseGranted, ""}},
			phases:  map[RequestID]Phase{"g1": PhaseGranted, "g2": PhaseGranted},
		},
		{
			name: "Reserved checked after Granted",
			records: []LeaseRecord{
				rec("r", "vm-a", Gi, PhaseReserved, 0),
				rec("g", "vm-a", Gi, PhaseGranted, time.Minute),
			},
			actions: []act{{RecoveryVerify, "r", PhaseResizing, ""}, {RecoveryKeep, "g", PhaseGranted, ""}},
			phases:  map[RequestID]Phase{"r": PhaseResizing, "g": PhaseGranted},
		},
		{
			name: "no data for the VM: Granted kept, Reserved back to Pending",
			records: []LeaseRecord{
				rec("g", "gone", Gi, PhaseGranted, 0),
				rec("r", "gone", Gi, PhaseReserved, time.Minute),
			},
			actions: []act{{RecoveryKeep, "g", PhaseGranted, ReasonVMDataMissing}, {RecoveryRequeue, "r", PhasePending, ""}},
			phases:  map[RequestID]Phase{"g": PhaseGranted, "r": PhasePending},
		},
		{
			name: "duplicates and invalid records are dropped",
			records: []LeaseRecord{
				rec("d", "vm-a", Gi, PhasePending, 0),
				rec("d", "vm-a", Gi, PhaseGranted, time.Minute),
				rec("z", "vm-a", 0, PhasePending, 2*time.Minute),
				{Request: Request{ID: "nvm", Kind: KindPod, Bytes: Gi, EnqueuedAt: t0.Add(3 * time.Minute)}, Phase: PhaseGranted},
				rec("bad", "vm-a", Gi, "Bogus", 4*time.Minute),
			},
			actions: []act{
				{RecoveryEnqueue, "d", PhasePending, ""},
				{RecoveryDrop, "d", PhaseGranted, ReasonInvalid},
				{RecoveryDrop, "z", PhasePending, ReasonInvalid},
				{RecoveryDrop, "nvm", PhaseGranted, ReasonInvalid},
				{RecoveryDrop, "bad", "Bogus", ReasonInvalid},
			},
			phases: map[RequestID]Phase{"d": PhasePending, "z": "", "nvm": "", "bad": ""},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := NewFakeClock(t0.Add(5 * time.Second))
			obs := tc.obs
			if obs == nil {
				obs = standardObs(t0)
			}
			l, actions := Rebuild(testConfig(), tc.records, obs, clk)
			var got []act
			for _, a := range actions {
				got = append(got, act{a.Kind, a.ID, a.To, a.Reason})
				if (a.Kind == RecoveryVerify) != (a.Reservation != nil) {
					t.Errorf("%s: reservation %v for kind %s", a.ID, a.Reservation, a.Kind)
				}
			}
			if d := cmp.Diff(tc.actions, got, cmp.AllowUnexported(act{})); d != "" {
				t.Errorf("actions (-want +got):\n%s", d)
			}
			for id, want := range tc.phases {
				lv, ok := l.Lease(id)
				if !ok {
					lv.Phase = ""
				}
				if lv.Phase != want {
					t.Errorf("%s: phase %q, want %q", id, lv.Phase, want)
				}
				if p, ok := tc.positions[id]; ok && lv.Position != p {
					t.Errorf("%s: position %d, want %d", id, lv.Position, p)
				}
			}
			// Verify actions can be committed by the executor.
			l.SetAgentState(DefaultHostAgent, AgentReady)
			for _, a := range actions {
				if a.Kind == RecoveryVerify {
					if a.Reservation.Delta != 0 || a.Reservation.Target != 8*Gi {
						t.Errorf("verify reservation %+v", a.Reservation)
					}
					if err := l.Commit(*a.Reservation, ResizeOutcome{}); err != nil {
						t.Errorf("Commit after Verify: %v", err)
					}
				}
			}
			// Idempotence: the same inputs and the rebuilt ledger's own records
			// produce the same ledger.
			l1, a1 := Rebuild(testConfig(), tc.records, obs, clk)
			l2, a2 := Rebuild(testConfig(), tc.records, obs, clk)
			if d := cmp.Diff(a1, a2); d != "" {
				t.Errorf("actions not deterministic:\n%s", d)
			}
			if d := cmp.Diff(l1.Snapshot(), l2.Snapshot()); d != "" {
				t.Errorf("ledger not deterministic:\n%s", d)
			}
			l3, _ := Rebuild(testConfig(), l1.Records(), obs, clk)
			if d := cmp.Diff(l1.Snapshot(), l3.Snapshot()); d != "" {
				t.Errorf("rebuild from own records differs:\n%s", d)
			}
		})
	}
}
