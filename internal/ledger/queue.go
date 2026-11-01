package ledger

import (
	"fmt"
	"math"
	"slices"
)

// Fit is the evaluation of one request on one VM.
type Fit struct {
	VM   VMName
	Fits bool
	// Delta is how much the VM must grow for the request (0: no resize).
	Delta int64
	// Headroom is the VM's headroom left for this request after the requests
	// ahead of it in the queue.
	Headroom int64
	// Pool is the pool left for this request after the requests ahead of it.
	Pool     int64
	Position Position
	Reason   Reason
}

// priority returns the effective queue priority of a request.
func (l *Ledger) priority(r Request) int64 {
	if r.Kind == KindReconciler && l.cfg.PriorityOverQueue {
		return math.MaxInt64
	}
	return int64(r.PriorityClass)
}

// queue returns the Pending requests in queue order: priority (higher first),
// then EnqueuedAt, then arrival.
func (l *Ledger) queue() []*lease {
	var q []*lease
	for _, le := range l.leases {
		if le.phase == PhasePending {
			q = append(q, le)
		}
	}
	slices.SortFunc(q, func(a, b *lease) int {
		if pa, pb := l.priority(a.req), l.priority(b.req); pa != pb {
			if pa > pb {
				return -1
			}
			return 1
		}
		if c := a.req.EnqueuedAt.Compare(b.req.EnqueuedAt); c != 0 {
			return c
		}
		switch {
		case a.seq < b.seq:
			return -1
		case a.seq > b.seq:
			return 1
		default:
			return 0
		}
	})
	return q
}

// sim evaluates the queue in order (§3.4). Requests ahead that fit are assumed
// granted: they use up headroom of the VM they would be placed on and the pool
// they would draw, so a later request can never take what an earlier one
// needs. H (head) is the first request that does not fit; after it, a request
// fits only with delta == 0 and no candidate VM in common with H.
type sim struct {
	l         *Ledger
	c         *calc
	queue     []*lease
	budget    int64
	consumed  map[VMName]int64
	head      *lease
	headCands map[VMName]bool
}

func (l *Ledger) newSim(c *calc) *sim {
	return &sim{l: l, c: c, queue: l.queue(), budget: c.pool, consumed: map[VMName]int64{}}
}

func (s *sim) overlapsHead(cands []VMName) bool {
	for _, vm := range cands {
		if s.headCands[vm] {
			return true
		}
	}
	return false
}

// fit evaluates le on vm at the current point of the walk. cands is le's
// candidate set for the head-of-line rule.
func (s *sim) fit(le *lease, vm VMName, cands []VMName) Fit {
	f := Fit{VM: vm, Pool: s.budget}
	q := s.c.vms[vm]
	switch {
	case s.c.frozen:
		f.Reason = ReasonHostFrozen
		return f
	case q == nil || !q.managed:
		f.Reason = ReasonUnknownVM
		return f
	case le.req.VM != "" && vm != le.req.VM:
		f.Reason = ReasonNotCandidate
		return f
	}
	avail := max(q.headroom, 0) - s.consumed[vm]
	f.Headroom = q.headroom - s.consumed[vm]
	step := s.l.cfg.Step
	f.Delta = deltaOf(le.req.Bytes, avail, step)
	if le.req.Kind == KindProbe && q.cfg.Mode != ModeFixed {
		// A probe exercises the production grow path, so it always grows.
		f.Delta = max(f.Delta, step)
	}
	afterHead := s.head != nil
	switch {
	case !q.assignedFresh:
		f.Reason = ReasonVMDataMissing
	case afterHead && s.overlapsHead(cands):
		f.Reason = ReasonQueued
	case f.Delta == 0:
		f.Fits = true
	case q.cfg.Mode == ModeFixed:
		f.Reason = ReasonNoHeadroom
	case afterHead:
		f.Reason = ReasonQueued
	case q.state != VMReady:
		f.Reason = ReasonVMUnknown
	case q.busy:
		f.Reason = ReasonVMBusy
	case q.cfg.Policy != nil && q.cfg.Policy.Max > 0 && q.assigned+f.Delta > q.cfg.Policy.Max:
		f.Reason = ReasonExceedsMax
	case !fitsOf(f.Delta, s.budget):
		f.Reason = ReasonPoolShort
	default:
		f.Fits = true
	}
	return f
}

// best returns the placement the walk assumes for le: a fit without resize on
// the VM with the most headroom, else the fit with the smallest delta.
func (s *sim) best(le *lease, cands []VMName) (Fit, bool) {
	var best Fit
	found := false
	for _, vm := range cands {
		f := s.fit(le, vm, cands)
		if !f.Fits {
			continue
		}
		if !found || better(f, best) {
			best, found = f, true
		}
	}
	return best, found
}

func better(a, b Fit) bool {
	if (a.Delta == 0) != (b.Delta == 0) {
		return a.Delta == 0
	}
	if a.Delta == 0 {
		return a.Headroom > b.Headroom
	}
	return a.Delta < b.Delta
}

// advance places le (if it fits anywhere), moves the walk past it and
// reports whether it fits.
func (s *sim) advance(le *lease) bool {
	cands := le.req.candidates()
	if len(cands) == 0 {
		return false
	}
	f, ok := s.best(le, cands)
	if ok {
		s.take(le, f)
		return true
	}
	if s.head == nil {
		s.head = le
		s.headCands = make(map[VMName]bool, len(cands))
		for _, vm := range cands {
			s.headCands[vm] = true
		}
	}
	return false
}

func (s *sim) take(le *lease, f Fit) {
	avail := max(f.Headroom, 0)
	s.consumed[f.VM] += min(le.req.Bytes, avail)
	s.budget -= f.Delta
}

// evaluateAt walks the queue up to le and evaluates it on vms. cands is le's
// candidate set for the head-of-line rule: its stored candidates unless the
// caller passes the filter's current list (a pinned pod always uses its VM).
func (l *Ledger) evaluateAt(le *lease, vms, cands []VMName) ([]Fit, *calc) {
	c := l.compute()
	s := l.newSim(c)
	if le.req.VM != "" || len(cands) == 0 {
		cands = le.req.candidates()
	}
	if len(cands) == 0 {
		cands = vms
	}
	for i, x := range s.queue {
		if x != le {
			s.advance(x)
			continue
		}
		out := make([]Fit, 0, len(vms))
		for _, vm := range vms {
			f := s.fit(le, vm, cands)
			f.Position = Position(i + 1)
			out = append(out, f)
		}
		return out, c
	}
	return nil, c
}

// Enqueue adds a request in Pending. It is idempotent per RequestID: a known
// ID returns its current position (0 when no longer Pending) and changes
// nothing.
func (l *Ledger) Enqueue(r Request) (Position, error) {
	if err := r.validate(); err != nil {
		return 0, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if le, ok := l.leases[r.ID]; ok {
		return l.position(le), nil
	}
	if r.VM != "" {
		if _, ok := l.vmCfg[r.VM]; !ok {
			return 0, fmt.Errorf("%w: %s", ErrUnknownVM, r.VM)
		}
	}
	r = r.clone()
	if r.Kind != KindPod {
		r.Candidates = nil
	}
	if r.EnqueuedAt.IsZero() {
		r.EnqueuedAt = l.clk.Now()
	}
	le := l.insert(r, PhasePending)
	l.emit(Event{Type: EventPhaseChanged, ID: r.ID, Kind: r.Kind, VM: r.VM, To: PhasePending, At: l.clk.Now()})
	l.refresh(r.ID)
	return l.position(le), nil
}

func (l *Ledger) insert(r Request, p Phase) *lease {
	l.seq++
	le := &lease{req: r, phase: p, seq: l.seq}
	l.leases[r.ID] = le
	return le
}

func (l *Ledger) position(le *lease) Position {
	if le.phase != PhasePending {
		return 0
	}
	return Position(slices.Index(l.queue(), le) + 1)
}

// SetCandidates replaces a Pending pod request's candidate VMs (from its
// filter call). A pod already pinned to a VM keeps being evaluated on that VM
// only.
func (l *Ledger) SetCandidates(id RequestID, vms []VMName) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	le, ok := l.leases[id]
	switch {
	case !ok:
		return fmt.Errorf("%w: %s", ErrUnknownRequest, id)
	case le.req.Kind != KindPod:
		return fmt.Errorf("%w: candidates are only set for pod requests", ErrInvalidRequest)
	case le.phase != PhasePending:
		return fmt.Errorf("%w: candidates change only while Pending, not %s", ErrInvalidTransition, le.phase)
	}
	le.req.Candidates = append([]VMName(nil), vms...)
	l.refresh("")
	return nil
}

// Evaluate returns, for each candidate VM, whether the request could be granted
// there now under the queue rule (§3.4). It changes nothing. Empty candidates
// mean the request's own candidates.
func (l *Ledger) Evaluate(id RequestID, candidates []VMName) []Fit {
	l.mu.Lock()
	defer l.mu.Unlock()
	le, ok := l.leases[id]
	if len(candidates) == 0 && ok {
		candidates = le.req.candidates()
	}
	reason := ReasonUnknownRequest
	if ok {
		if le.phase == PhasePending {
			fits, _ := l.evaluateAt(le, candidates, candidates)
			return fits
		}
		reason = ReasonNotPending
	}
	out := make([]Fit, len(candidates))
	for i, vm := range candidates {
		out[i] = Fit{VM: vm, Reason: reason}
	}
	return out
}

// Dispatchable returns the non-pod requests that can be granted now, in queue
// order, each on its VM. Pod requests are granted only from bind.
func (l *Ledger) Dispatchable() []RequestID {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.newSim(l.compute())
	if s.c.frozen {
		return nil
	}
	var out []RequestID
	for _, le := range s.queue {
		if s.advance(le) && le.req.Kind != KindPod {
			out = append(out, le.req.ID)
		}
	}
	return out
}

// refresh recomputes which Pending requests are grantable and emits
// EventGrantable for every request that became grantable. The request quiet
// (just enqueued) gets its flag set without an event.
func (l *Ledger) refresh(quiet RequestID) {
	s := l.newSim(l.compute())
	for _, le := range s.queue {
		ok := s.advance(le)
		if ok && !le.grantable && le.req.ID != quiet {
			l.emit(Event{
				Type: EventGrantable, ID: le.req.ID, Kind: le.req.Kind, VM: le.req.VM,
				From: PhasePending, To: PhasePending, At: s.c.now,
			})
		}
		le.grantable = ok
	}
}
