package ledger

import (
	"fmt"
	"slices"
)

// Reservation is a request moved to Reserved on a VM (§3.2 step 1). It counts
// in leased, committed and pool until it is committed or rolled back.
type Reservation struct {
	ID    RequestID
	VM    VMName
	Mode  Mode
	Bytes int64
	// Delta is how much the VM grows; 0 means no resize is needed and the
	// reservation can be committed directly.
	Delta int64
	// AssignedBefore is MemoryAssigned when the reservation was made.
	AssignedBefore int64
	// Target is the assignment the grow must reach: AssignedBefore + Delta.
	// Static VMs: the new StartupBytes. Dynamic VMs: MaximumBytes must be at
	// least Target.
	Target int64
	// Seq identifies this reservation; stale copies are rejected.
	Seq uint64
}

// Reserve moves a Pending request to Reserved on vm if it fits there now
// under the queue rule. A pod request gets its VM set (and pinned) here.
// A *NotFitError says why it does not fit; the request then stays Pending.
func (l *Ledger) Reserve(id RequestID, vm VMName) (Reservation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	le, ok := l.leases[id]
	if !ok {
		return Reservation{}, fmt.Errorf("%w: %s", ErrUnknownRequest, id)
	}
	if le.phase != PhasePending {
		return Reservation{}, fmt.Errorf("%w: Reserve from %s", ErrInvalidTransition, le.phase)
	}
	if le.req.Kind == KindPod && le.req.VM == "" && len(le.req.Candidates) > 0 && !slices.Contains(le.req.Candidates, vm) {
		return Reservation{}, &NotFitError{ID: id, Fit: Fit{VM: vm, Reason: ReasonNotCandidate}}
	}
	fits, c := l.evaluateAt(le, []VMName{vm}, nil)
	f := fits[0]
	if !f.Fits {
		return Reservation{}, &NotFitError{ID: id, Fit: f}
	}
	q := c.vms[vm]
	l.resSeq++
	res := Reservation{
		ID: id, VM: vm, Mode: q.cfg.Mode, Bytes: le.req.Bytes, Delta: f.Delta,
		AssignedBefore: q.assigned, Target: q.assigned + f.Delta, Seq: l.resSeq,
	}
	le.req.VM = vm
	le.res = res
	l.setPhase(le, PhaseReserved, ReasonNone)
	l.refresh("")
	return res, nil
}

// current returns the lease holding res, checking that res is its live
// reservation and its phase is one of phases.
func (l *Ledger) current(res Reservation, op string, phases ...Phase) (*lease, error) {
	le, ok := l.leases[res.ID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownRequest, res.ID)
	}
	if !slices.Contains(phases, le.phase) {
		return nil, fmt.Errorf("%w: %s from %s", ErrInvalidTransition, op, le.phase)
	}
	if le.res.Seq != res.Seq {
		return nil, fmt.Errorf("%w: %s for %s", ErrStaleReservation, op, res.ID)
	}
	return le, nil
}

// MarkResizing records that the resize for res was issued (Reserved →
// Resizing); the lease now waits for guest verification.
func (l *Ledger) MarkResizing(res Reservation) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	le, err := l.current(res, "MarkResizing", PhaseReserved)
	if err != nil {
		return err
	}
	l.setPhase(le, PhaseResizing, ReasonNone)
	return nil
}

// Commit grants res (§3.2 step 3). A reservation that needs a resize must be
// Resizing (verified by the caller); one without resize may be committed from
// Reserved. out updates the VM's assignment; a zero out keeps the ledger's
// view and, after a resize, keeps the target counted until the host reports
// the VM again.
func (l *Ledger) Commit(res Reservation, out ResizeOutcome) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	le, err := l.current(res, "Commit", PhaseReserved, PhaseResizing)
	if err != nil {
		return err
	}
	if le.phase == PhaseReserved && le.res.Delta > 0 {
		return fmt.Errorf("%w: Commit of a resize that was never issued", ErrInvalidTransition)
	}
	if out.ObservedAt.IsZero() {
		l.endInFlight(le)
	} else {
		l.applyOutcome(le.res.VM, out)
	}
	le.grantedAt = l.clk.Now()
	l.setPhase(le, PhaseGranted, ReasonNone)
	l.refresh("")
	return nil
}

// Rollback ends a reservation without a grant: back to Pending at its original
// queue position (requeue) or Failed with why. A pod request stays pinned to
// its VM. After a resize was issued, its target stays counted until the host
// reports the VM again, because the memory may have been assigned anyway
// (§4.4). Stale or unknown reservations are ignored.
func (l *Ledger) Rollback(res Reservation, why Reason, requeue bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	le, err := l.current(res, "Rollback", PhaseReserved, PhaseResizing)
	if err != nil {
		return
	}
	l.endInFlight(le)
	if requeue {
		l.setPhase(le, PhasePending, why)
	} else {
		l.setPhase(le, PhaseFailed, why)
	}
	l.refresh("")
}
