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
	// RecoveryKeep: a Granted lease whose VM still covers base + leases (or whose
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
// Observations are applied in order. Granted leases are checked oldest grant
// first and kept while the VM's assignment covers base(vm) + Σ leased with them
// counted; the rest are Lost. Reserved/Resizing leases are then checked in
// queue order against floor(vm) with them counted.
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

	actions, granted, inflight := l.loadRecords(recs)
	sortByGrant(recs, granted)
	for _, i := range granted {
		l.recoverGranted(l.leases[recs[i].Request.ID], &actions[i])
	}
	for _, i := range inflight {
		l.recoverInflight(l.leases[recs[i].Request.ID], &actions[i])
	}
	l.refresh("")
	return l, actions
}

// loadRecords inserts the loadable records of recs (sorted by compareRecords)
// and returns one action per record. Pending leases are final; the indexes of
// Granted and of Reserved/Resizing records are returned for the later passes,
// their leases inserted with a placeholder phase.
func (l *Ledger) loadRecords(recs []LeaseRecord) (actions []RecoveryAction, granted, inflight []int) {
	actions = make([]RecoveryAction, len(recs))
	seen := map[RequestID]bool{}
	for i, rec := range recs {
		r := rec.Request.clone()
		actions[i] = RecoveryAction{Kind: RecoveryDrop, ID: r.ID, VM: r.VM, From: rec.Phase, To: rec.Phase}
		if !loadable(rec.Phase, r, seen) {
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
			le := l.insert(r, PhaseFailed) // placeholder, decided by recoverGranted
			le.grantedAt = rec.GrantedAt
			granted = append(granted, i)
		case PhaseReserved, PhaseResizing:
			l.insert(r, PhaseFailed) // placeholder, decided by recoverInflight
			inflight = append(inflight, i)
		case PhaseReleased, PhaseFailed:
			// terminal: not loaded
		default:
			actions[i].Reason = ReasonInvalid
		}
	}
	return actions, granted, inflight
}

// loadable reports whether a record in phase p with request r may be loaded:
// its ID is not seen before, the request is valid, and a lease holding memory
// names its VM.
func loadable(p Phase, r Request, seen map[RequestID]bool) bool {
	if seen[r.ID] || r.validate() != nil {
		return false
	}
	return !p.holdsMemory() || r.VM != ""
}

// sortByGrant orders the record indexes idx by GrantedAt, then request ID.
func sortByGrant(recs []LeaseRecord, idx []int) {
	slices.SortStableFunc(idx, func(a, b int) int {
		if c := recs[a].GrantedAt.Compare(recs[b].GrantedAt); c != 0 {
			return c
		}
		return strings.Compare(string(recs[a].Request.ID), string(recs[b].Request.ID))
	})
}

// recoverGranted keeps a Granted lease while its VM covers base + Σ leased
// (or has no fresh data) and drops it as Lost otherwise.
func (l *Ledger) recoverGranted(le *lease, a *RecoveryAction) {
	le.phase = PhaseGranted
	covered, known := l.covers(le.req.VM, true)
	switch {
	case !known:
		a.Kind, a.Reason = RecoveryKeep, ReasonVMDataMissing
	case covered:
		a.Kind = RecoveryKeep
	default:
		delete(l.leases, le.req.ID)
		a.Kind, a.To, a.Reason = RecoveryLost, PhaseFailed, ReasonLost
	}
}

// recoverInflight loads a Reserved/Resizing lease as Resizing with a
// zero-delta reservation when its VM covers floor(vm), and requeues it as
// Pending otherwise.
func (l *Ledger) recoverInflight(le *lease, a *RecoveryAction) {
	le.phase = PhaseResizing
	v := l.vm(le.req.VM)
	le.res = Reservation{
		ID: le.req.ID, VM: le.req.VM, Mode: l.vmCfg[le.req.VM].Mode, Bytes: le.req.Bytes,
		AssignedBefore: v.assigned, Target: v.assigned,
	}
	if covered, known := l.covers(le.req.VM, false); known && covered {
		l.resSeq++
		le.res.Seq = l.resSeq
		res := le.res
		a.Kind, a.To, a.Reservation = RecoveryVerify, PhaseResizing, &res
		return
	}
	le.phase, le.res = PhasePending, Reservation{}
	a.Kind, a.To = RecoveryRequeue, PhasePending
}

// covers reports whether vm's assignment covers the leases currently loaded.
// For granted leases the bound is static, base + Σ leased: margin and effBase
// steer admission and the reconciler, and a VM under normal load (free below
// margin) keeps its leases. For in-flight leases it is floor(vm), as for a
// new grant. known is false when the VM is not managed or has no fresh host
// data, so nothing can be decided.
func (l *Ledger) covers(vm VMName, granted bool) (covered, known bool) {
	q := l.compute().vms[vm]
	if q == nil || !q.managed || !q.assignedFresh {
		return false, false
	}
	if granted {
		return q.assigned >= q.cfg.Base+q.leased, true
	}
	return q.assigned >= q.floor, true
}
