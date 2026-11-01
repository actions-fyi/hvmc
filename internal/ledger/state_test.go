package ledger

import (
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestTransitions drives every legal transition and checks that illegal ones
// are refused without changing anything.
func TestTransitions(t *testing.T) {
	type step struct {
		op      string
		want    Phase
		wantErr error
	}
	for _, tc := range []struct {
		name  string
		bytes int64
		steps []step
	}{
		{
			name: "resize path", bytes: 3 * Gi,
			steps: []step{
				{"commit", PhaseReserved, ErrInvalidTransition}, // never issued
				{"resizing", PhaseResizing, nil},
				{"resizing", PhaseResizing, ErrInvalidTransition},
				{"commit", PhaseGranted, nil},
				{"commit", PhaseGranted, ErrInvalidTransition},
				{"fail", PhaseGranted, ErrInvalidTransition},
				{"release", "", nil},
			},
		},
		{
			name: "no resize: commit straight from Reserved", bytes: 1 * Gi,
			steps: []step{{"commit", PhaseGranted, nil}},
		},
		{
			name: "rollback to pending from Resizing", bytes: 3 * Gi,
			steps: []step{{"resizing", PhaseResizing, nil}, {"requeue", PhasePending, nil}},
		},
		{
			name: "rollback to failed from Reserved", bytes: 3 * Gi,
			steps: []step{{"fail-rollback", "", nil}},
		},
		{
			name: "release while Reserved", bytes: 3 * Gi,
			steps: []step{{"release", "", nil}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := standard(t)
			f.enqueue(ext("r", "vm-a", tc.bytes))
			res := f.reserve("r", "vm-a")
			if _, err := f.l.Reserve("r", "vm-a"); !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("double Reserve: %v", err)
			}
			for i, s := range tc.steps {
				var err error
				switch s.op {
				case "resizing":
					err = f.l.MarkResizing(res)
				case "commit":
					err = f.l.Commit(res, ResizeOutcome{})
				case "fail":
					err = f.l.Fail("r", "x")
				case "release":
					f.l.Release("r")
				case "requeue":
					f.l.Rollback(res, ReasonResizeFailed, true)
				case "fail-rollback":
					f.l.Rollback(res, ReasonResizeFailed, false)
				}
				if !errors.Is(err, s.wantErr) {
					t.Errorf("step %d %s: err %v, want %v", i, s.op, err, s.wantErr)
				}
				if got := f.phase("r"); got != s.want {
					t.Errorf("step %d %s: phase %q, want %q", i, s.op, got, s.want)
				}
			}
		})
	}
}

func TestStaleReservations(t *testing.T) {
	f := standard(t)
	f.enqueue(ext("r", "vm-a", 3*Gi))
	old := f.reserve("r", "vm-a")
	f.l.Rollback(old, ReasonResizeFailed, true)
	cur := f.reserve("r", "vm-a")
	if err := f.l.MarkResizing(old); !errors.Is(err, ErrStaleReservation) {
		t.Errorf("MarkResizing(stale): %v", err)
	}
	if err := f.l.Commit(old, ResizeOutcome{}); !errors.Is(err, ErrStaleReservation) {
		t.Errorf("Commit(stale): %v", err)
	}
	f.l.Rollback(old, ReasonResizeFailed, false) // ignored
	if f.phase("r") != PhaseReserved {
		t.Fatalf("stale rollback changed phase to %s", f.phase("r"))
	}
	if err := f.l.MarkResizing(Reservation{ID: "nope"}); !errors.Is(err, ErrUnknownRequest) {
		t.Errorf("MarkResizing(unknown): %v", err)
	}
	if _, err := f.l.Reserve("nope", "vm-a"); !errors.Is(err, ErrUnknownRequest) {
		t.Errorf("Reserve(unknown): %v", err)
	}
	if err := f.l.MarkResizing(cur); err != nil {
		t.Fatal(err)
	}
}

func TestFailAndExpire(t *testing.T) {
	f := standard(t)
	f.enqueue(Request{ID: "d1", Kind: KindExternal, VM: "vm-a", Bytes: 30 * Gi, Deadline: t0.Add(time.Minute)})
	f.enqueue(Request{ID: "d2", Kind: KindExternal, VM: "vm-a", Bytes: 30 * Gi, Deadline: t0.Add(2 * time.Minute)})
	f.enqueue(Request{ID: "nod", Kind: KindExternal, VM: "vm-a", Bytes: 30 * Gi})
	f.enqueue(Request{ID: "res", Kind: KindExternal, VM: "vm-b", Bytes: Gi, Deadline: t0.Add(time.Second)})
	f.reserve("res", "vm-b")
	if got := f.l.Expire(t0.Add(59 * time.Second)); got != nil {
		t.Errorf("early Expire: %v", got)
	}
	if d := cmp.Diff([]RequestID{"d1", "d2"}, f.l.Expire(t0.Add(2*time.Minute))); d != "" {
		t.Errorf("Expire (-want +got):\n%s", d)
	}
	if f.phase("res") != PhaseReserved {
		t.Errorf("Expire touched a reserved request")
	}
	if err := f.l.Fail("nod", "Rejected"); err != nil {
		t.Fatal(err)
	}
	if err := f.l.Fail("nod", "Rejected"); !errors.Is(err, ErrUnknownRequest) {
		t.Errorf("Fail of failed request: %v", err)
	}
	if err := f.l.Fail("res", "Rejected"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("Fail of reserved request: %v", err)
	}
	f.l.Release("nope") // no-op
}

// TestHoldAfterIssuedResize: when a resize was issued and the reservation ends
// without an outcome, its target stays counted until the VM is observed again.
func TestHoldAfterIssuedResize(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(f *fixture, res Reservation)
	}{
		{"rollback", func(f *fixture, res Reservation) { f.l.Rollback(res, ReasonVerifyTimeout, false) }},
		{"release", func(f *fixture, _ Reservation) { f.l.Release("r") }},
		{"commit without outcome", func(f *fixture, res Reservation) {
			if err := f.l.Commit(res, ResizeOutcome{}); err != nil {
				f.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := standard(t)
			f.enqueue(ext("r", "vm-a", 5*Gi)) // delta 3Gi, target 11Gi
			res := f.reserve("r", "vm-a")
			if err := f.l.MarkResizing(res); err != nil {
				t.Fatal(err)
			}
			tc.end(f, res)
			v := f.vmView("vm-a")
			if v.Committed < 11*Gi || f.l.Snapshot().InFlight != 3*Gi {
				t.Fatalf("hold lost: committed %d inflight %d", v.Committed, f.l.Snapshot().InFlight)
			}
			f.enqueue(ext("other", "vm-a", 3*Gi))
			if got := f.l.Evaluate("other", nil); got[0].Reason != ReasonVMBusy {
				t.Errorf("held VM not busy: %+v", got)
			}
			if _, err := f.l.ReserveShrink("vm-a", 6*Gi); !errors.Is(err, ErrVMBusy) {
				t.Errorf("shrink on held VM: %v", err)
			}
			f.vm("vm-a", 11*Gi, 2*Gi) // same instant: not after the hold
			if f.l.Snapshot().InFlight != 3*Gi {
				t.Errorf("same-instant observation released the hold")
			}
			f.clk.Advance(time.Second)
			f.vm("vm-a", 11*Gi, 2*Gi)
			if s := f.l.Snapshot(); s.InFlight != 3*Gi { // growth now a debt against hostFree
				t.Errorf("growth not counted against hostFree: inflight %d", s.InFlight)
			}
			f.host(9 * Gi)
			if s := f.l.Snapshot(); s.InFlight != 0 || f.vmView("vm-a").Assigned != 11*Gi {
				t.Errorf("after observations: %+v", s)
			}
		})
	}
}

func TestSubscribe(t *testing.T) {
	f := standard(t)
	ch, cancel := f.l.Subscribe()
	f.enqueue(ext("big", "vm-a", 9*Gi))  // delta 7Gi, fits
	f.enqueue(ext("next", "vm-b", 3*Gi)) // delta 1Gi: pool left 1Gi, fits
	f.enqueue(ext("wait", "vm-b", 3*Gi)) // pool short: head
	f.grant("big", "vm-a")               // Reserved, Resizing, Granted
	f.l.Release("big")                   // vm-a stays grown: no pool returns
	f.l.Release("next")                  // "wait" now fits (delta 1Gi): Grantable
	want := []Event{
		{Type: EventPhaseChanged, ID: "big", To: PhasePending},
		{Type: EventPhaseChanged, ID: "next", To: PhasePending},
		{Type: EventPhaseChanged, ID: "wait", To: PhasePending},
		{Type: EventPhaseChanged, ID: "big", From: PhasePending, To: PhaseReserved},
		{Type: EventPhaseChanged, ID: "big", From: PhaseReserved, To: PhaseResizing},
		{Type: EventPhaseChanged, ID: "big", From: PhaseResizing, To: PhaseGranted},
		{Type: EventPhaseChanged, ID: "big", From: PhaseGranted, To: PhaseReleased, Reason: ReasonReleased},
		{Type: EventPhaseChanged, ID: "next", From: PhasePending, To: PhaseReleased, Reason: ReasonReleased},
		{Type: EventGrantable, ID: "wait", From: PhasePending, To: PhasePending},
	}
	var got []Event
	for range want {
		select {
		case e := <-ch:
			e.Kind, e.VM, e.At = "", "", time.Time{}
			got = append(got, e)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out after %d events: %+v", len(got), got)
		}
	}
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("events (-want +got):\n%s", d)
	}
	cancel()
	cancel() // idempotent
	for range ch {
	}
	f.enqueue(ext("after", "vm-a", Gi)) // no subscriber, must not block
}

func TestSubscribeCancelWhileBuffered(t *testing.T) {
	f := standard(t)
	ch, cancel := f.l.Subscribe()
	for i := range 50 {
		f.enqueue(ext(RequestID(rune('a'+i%26))+RequestID(rune('a'+i/26)), "vm-a", Gi))
	}
	<-ch
	cancel()
	for range ch {
	}
}

func TestNotFitError(t *testing.T) {
	for reason, want := range map[Reason]error{
		ReasonHostFrozen:    ErrFrozen,
		ReasonUnknownVM:     ErrUnknownVM,
		ReasonVMUnknown:     ErrVMUnknown,
		ReasonVMDataMissing: ErrVMUnknown,
		ReasonVMBusy:        ErrVMBusy,
		ReasonPoolShort:     ErrDoesNotFit,
		ReasonQueued:        ErrDoesNotFit,
	} {
		err := error(&NotFitError{ID: "r", Fit: Fit{VM: "vm-a", Reason: reason, Delta: Gi}})
		if !errors.Is(err, want) {
			t.Errorf("%s: %v does not unwrap to %v", reason, err, want)
		}
	}
	err := &NotFitError{ID: "r", Fit: Fit{VM: "vm-a", Reason: ReasonPoolShort, Delta: 1, Headroom: 2, Pool: 3}}
	if got, want := err.Error(), "ledger: r does not fit on vm-a: PoolShort (delta 1, headroom 2, pool 3)"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
