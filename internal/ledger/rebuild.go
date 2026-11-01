package ledger

import (
	"slices"
	"strings"
)

// RecoveryKind says what Rebuild did with a lease and what the caller must do.
type RecoveryKind string

// Recovery kinds (§3.5).
const (
	// RecoveryEnqueue: a Pending lease, re-enqueued at its original position.
	RecoveryEnqueue RecoveryKind = "Enqueue"
	// RecoveryKeep: a Granted lease whose VM still covers its floor (or whose
	// VM has no fresh data, since nothing is revoked for missing data).
	RecoveryKeep RecoveryKind = "Keep"
	// RecoveryVerify: a Reserved/Resizing lease whose VM covers its floor. It
	// is loaded as Resizing; the caller re-runs guest verification and then
	// calls Commit (or Rollback) with Reservation.
	RecoveryVerify RecoveryKind = "Verify"
	// RecoveryRequeue: a Reserved/Resizing lease not covered; back to Pending
	// at its original position. The caller persists the phase.
	RecoveryRequeue RecoveryKind = "Requeue"
	// RecoveryLost: a Granted lease not covered; Failed(Lost). The caller
	// persists the phase and emits an event; the VM is never re-grown.
	RecoveryLost RecoveryKind = "Lost"
	// RecoveryDrop: Released, Failed, duplicate or invalid record; not loaded.
	RecoveryDrop RecoveryKind = "Drop"
)

// RecoveryAction is the result of Rebuild for one lease record.
type RecoveryAction struct {
	Kind   RecoveryKind
	ID     RequestID
	VM     VMName
	From   Phase
	To     Phase
	Reason Reason
	// Reservation is set for RecoveryVerify.
	Reservation *Reservation
}

// Rebuild creates a ledger from persisted leases and fresh agent data (§3.5).
// Observations are applied in order. Granted leases are checked in grant order
// and kept while the VM's assignment covers floor(vm) with them counted;
// Reserved/Resizing leases are then checked in queue order the same way.
// Actions are returned in record order (EnqueuedAt, then ID). Rebuild is
// deterministic: the same inputs yield the same ledger and actions.
func Rebuild(cfg Config, records []LeaseRecord, obs []Observation, clk Clock) (*Ledger, []RecoveryAction) {
	l := New(cfg, clk)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, o := range obs {
		l.observe(o)
	}
	recs := slices.Clone(records)
	slices.SortStableFunc(recs, compareRecords)

	actions := make([]RecoveryAction, len(recs))
	var granted, inflight []int
	seen := map[RequestID]bool{}
	for i, rec := range recs {
		r := rec.Request.clone()
		actions[i] = RecoveryAction{Kind: RecoveryDrop, ID: r.ID, VM: r.VM, From: rec.Phase, To: rec.Phase}
		if seen[r.ID] || r.validate() != nil || (rec.Phase.holdsMemory() && r.VM == "") {
			actions[i].Reason = ReasonInvalid
			continue
		}
		seen[r.ID] = true
		if r.Kind != KindPod {
			r.Candidates = nil
		}
		switch rec.Phase {
		case PhasePending:
			l.insert(r, PhasePending)
			actions[i].Kind = RecoveryEnqueue
		case PhaseGranted:
			le := l.insert(r, PhaseFailed) // placeholder, decided below
			le.grantedAt = rec.GrantedAt
			granted = append(granted, i)
		case PhaseReserved, PhaseResizing:
			l.insert(r, PhaseFailed)
			inflight = append(inflight, i)
		case PhaseReleased, PhaseFailed:
			// terminal: not loaded
		default:
			actions[i].Reason = ReasonInvalid
		}
	}

	slices.SortStableFunc(granted, func(a, b int) int {
		if c := recs[a].GrantedAt.Compare(recs[b].GrantedAt); c != 0 {
			return c
		}
		return strings.Compare(string(recs[a].Request.ID), string(recs[b].Request.ID))
	})
	for _, i := range granted {
		le := l.leases[recs[i].Request.ID]
		le.phase = PhaseGranted
		if covered, known := l.covers(le.req.VM); !known {
			actions[i].Kind, actions[i].Reason = RecoveryKeep, ReasonVMDataMissing
		} else if covered {
			actions[i].Kind = RecoveryKeep
		} else {
			delete(l.leases, le.req.ID)
			actions[i].Kind, actions[i].To, actions[i].Reason = RecoveryLost, PhaseFailed, ReasonLost
		}
	}

	for _, i := range inflight {
		le := l.leases[recs[i].Request.ID]
		le.phase = PhaseResizing
		v := l.vm(le.req.VM)
		le.res = Reservation{
			ID: le.req.ID, VM: le.req.VM, Mode: l.vmCfg[le.req.VM].Mode, Bytes: le.req.Bytes,
			AssignedBefore: v.assigned, Target: v.assigned,
		}
		if covered, known := l.covers(le.req.VM); known && covered {
			l.resSeq++
			le.res.Seq = l.resSeq
			res := le.res
			actions[i].Kind, actions[i].To, actions[i].Reservation = RecoveryVerify, PhaseResizing, &res
			continue
		}
		le.phase, le.res = PhasePending, Reservation{}
		actions[i].Kind, actions[i].To = RecoveryRequeue, PhasePending
	}
	l.refresh("")
	return l, actions
}

// covers reports whether vm's assignment covers its floor with the leases
// currently loaded. known is false when the VM is not managed or has no fresh
// host data, so nothing can be decided.
func (l *Ledger) covers(vm VMName) (covered, known bool) {
	q := l.compute().vms[vm]
	if q == nil || !q.managed || !q.assignedFresh {
		return false, false
	}
	return q.assigned >= q.floor, true
}
