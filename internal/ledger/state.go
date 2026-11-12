package ledger

import (
	"errors"
	"fmt"
	"time"
)

// Phase is a lease phase (DESIGN §3.3).
type Phase string

// Lease phases. Pending → Reserved → Resizing → Granted → Released, plus
// Failed (from Pending on deadline or Fail, from Reserved/Resizing on error).
const (
	PhasePending  Phase = "Pending"
	PhaseReserved Phase = "Reserved"
	PhaseResizing Phase = "Resizing"
	PhaseGranted  Phase = "Granted"
	PhaseReleased Phase = "Released"
	PhaseFailed   Phase = "Failed"
)

// holdsMemory reports whether a lease in phase p counts in leased(vm).
func (p Phase) holdsMemory() bool {
	return p == PhaseReserved || p == PhaseResizing || p == PhaseGranted
}

// inFlight reports whether a lease in phase p carries a Reservation.
func (p Phase) inFlight() bool { return p == PhaseReserved || p == PhaseResizing }

// Kind is the owner kind of a request (MemoryLease spec.owner.kind).
type Kind string

// Request kinds.
const (
	KindPod        Kind = "Pod"
	KindExternal   Kind = "External"
	KindReconciler Kind = "Reconciler"
	KindProbe      Kind = "Probe"
)

func (k Kind) valid() bool {
	switch k {
	case KindPod, KindExternal, KindReconciler, KindProbe:
		return true
	default:
		return false
	}
}

// RequestID identifies a request; it is the MemoryLease name.
type RequestID string

// Position is a 1-based place in the queue of Pending requests; 0 means the
// request is not Pending.
type Position int

// Request is a memory lease request as the ledger sees it.
type Request struct {
	ID   RequestID
	Kind Kind
	// VM is the target VM. Required for every kind except Pod, whose VM is set
	// once by Reserve (bind) and stays pinned afterwards, because
	// MemoryLease.spec.vm is immutable once set.
	VM VMName
	// Candidates are a pod's candidate VMs from its filter call. Ignored for
	// other kinds, whose only candidate is VM.
	Candidates []VMName
	// Bytes is the leased amount.
	Bytes int64
	// PriorityClass orders the queue, higher first. Reconciler requests
	// count as +∞ when Config.PriorityOverQueue is set.
	PriorityClass int32
	// EnqueuedAt is the queue position (MemoryLease status.enqueuedAt); zero
	// means "now" on Enqueue.
	EnqueuedAt time.Time
	// Deadline is optional; a Pending request past it fails with Timeout.
	Deadline time.Time
}

func (r Request) clone() Request {
	r.Candidates = append([]VMName(nil), r.Candidates...)
	return r
}

// candidates returns the VMs the request may be granted on.
func (r Request) candidates() []VMName {
	if r.VM != "" {
		return []VMName{r.VM}
	}
	if r.Kind != KindPod {
		return nil
	}
	return r.Candidates
}

func (r Request) validate() error {
	switch {
	case r.ID == "":
		return fmt.Errorf("%w: empty ID", ErrInvalidRequest)
	case !r.Kind.valid():
		return fmt.Errorf("%w: kind %q", ErrInvalidRequest, r.Kind)
	case r.Bytes <= 0:
		return fmt.Errorf("%w: bytes must be positive", ErrInvalidRequest)
	case r.Kind != KindPod && r.VM == "":
		return fmt.Errorf("%w: %s request without VM", ErrInvalidRequest, r.Kind)
	}
	return nil
}

// Reason explains a phase or a fit decision. Callers may pass their own
// reasons to Rollback and Fail; the ledger only interprets the ones below.
type Reason string

// Reasons produced or interpreted by the ledger.
const (
	ReasonNone Reason = ""
	// Terminal and recovery reasons.
	ReasonTimeout  Reason = "Timeout"
	ReasonLost     Reason = "Lost"
	ReasonReleased Reason = "Released"
	ReasonInvalid  Reason = "Invalid"
	// Fit reasons.
	ReasonHostFrozen     Reason = "HostFrozen"
	ReasonUnknownRequest Reason = "UnknownRequest"
	ReasonNotPending     Reason = "NotPending"
	ReasonUnknownVM      Reason = "UnknownVM"
	ReasonNotCandidate   Reason = "NotCandidate"
	ReasonVMDataMissing  Reason = "VMDataMissing"
	ReasonVMUnknown      Reason = "VMUnknown"
	ReasonVMBusy         Reason = "VMBusy"
	ReasonNoHeadroom     Reason = "NoHeadroom"
	ReasonExceedsMax     Reason = "ExceedsPolicyMax"
	ReasonPoolShort      Reason = "PoolShort"
	ReasonQueued         Reason = "Queued"
	// Reasons for the grant executor.
	ReasonResizeFailed  Reason = "ResizeFailed"
	ReasonVerifyTimeout Reason = "VerifyTimeout"
)

// Errors returned by the ledger. Use errors.Is.
var (
	ErrInvalidRequest    = errors.New("ledger: invalid request")
	ErrUnknownRequest    = errors.New("ledger: unknown request")
	ErrUnknownVM         = errors.New("ledger: unknown VM")
	ErrInvalidTransition = errors.New("ledger: invalid phase transition")
	ErrStaleReservation  = errors.New("ledger: stale reservation")
	ErrFrozen            = errors.New("ledger: frozen, host agent not ready")
	ErrDoesNotFit        = errors.New("ledger: request does not fit")
	ErrVMUnknown         = errors.New("ledger: VM state unknown")
	ErrVMBusy            = errors.New("ledger: VM has an operation in flight")
	ErrNotResizable      = errors.New("ledger: VM cannot be shrunk")
	ErrNothingToShrink   = errors.New("ledger: target does not shrink the VM")
)

// NotFitError is returned by Reserve when the request cannot be granted on the
// VM now. It unwraps to ErrFrozen, ErrUnknownVM, ErrVMUnknown, ErrVMBusy or
// ErrDoesNotFit depending on Fit.Reason.
type NotFitError struct {
	ID  RequestID
	Fit Fit
}

func (e *NotFitError) Error() string {
	return fmt.Sprintf("ledger: %s does not fit on %s: %s (delta %d, headroom %d, pool %d)",
		e.ID, e.Fit.VM, e.Fit.Reason, e.Fit.Delta, e.Fit.Headroom, e.Fit.Pool)
}

// Unwrap maps the fit reason to a sentinel error.
func (e *NotFitError) Unwrap() error {
	switch e.Fit.Reason {
	case ReasonHostFrozen:
		return ErrFrozen
	case ReasonUnknownVM:
		return ErrUnknownVM
	case ReasonVMUnknown, ReasonVMDataMissing:
		return ErrVMUnknown
	case ReasonVMBusy:
		return ErrVMBusy
	default:
		return ErrDoesNotFit
	}
}

// lease is the ledger's record of one request.
type lease struct {
	req       Request
	phase     Phase
	reason    Reason
	seq       uint64
	grantedAt time.Time
	res       Reservation // valid while phase.inFlight()
	grantable bool
}

// setPhase moves le to phase p and emits an event. Terminal leases are removed.
func (l *Ledger) setPhase(le *lease, p Phase, why Reason) {
	from := le.phase
	le.phase = p
	le.reason = why
	if p == PhasePending {
		// A requeued request carries its rollback reason only in the event.
		le.reason = ReasonNone
	}
	if !p.inFlight() {
		le.res = Reservation{}
	}
	if p == PhaseReleased || p == PhaseFailed {
		delete(l.leases, le.req.ID)
	}
	l.emit(Event{
		Type: EventPhaseChanged, ID: le.req.ID, Kind: le.req.Kind, VM: le.req.VM,
		From: from, To: p, Reason: why, At: l.clk.Now(),
	})
}

// endInFlight records a hold when a reservation with an issued resize ends
// without a known outcome, so the possibly grown memory stays counted until
// the host reports the VM again.
func (l *Ledger) endInFlight(le *lease) {
	if le.phase != PhaseResizing || le.res.Delta == 0 {
		return
	}
	l.vm(le.res.VM).addHold(le.res.Target, l.clk.Now())
}

// Fail moves a Pending request to Failed (non-retryable, nothing reserved).
func (l *Ledger) Fail(id RequestID, why Reason) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	le, ok := l.leases[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownRequest, id)
	}
	if le.phase != PhasePending {
		return fmt.Errorf("%w: Fail from %s", ErrInvalidTransition, le.phase)
	}
	l.setPhase(le, PhaseFailed, why)
	l.refresh("")
	return nil
}

// Expire fails every Pending request whose deadline is not after now with
// reason Timeout and returns their IDs in queue order.
func (l *Ledger) Expire(now time.Time) []RequestID {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []RequestID
	for _, le := range l.queue() {
		if d := le.req.Deadline; !d.IsZero() && !now.Before(d) {
			out = append(out, le.req.ID)
			l.setPhase(le, PhaseFailed, ReasonTimeout)
		}
	}
	if len(out) > 0 {
		l.refresh("")
	}
	return out
}

// Release ends a request in any non-terminal phase. Releasing an unknown or
// already terminal request is a no-op.
func (l *Ledger) Release(id RequestID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	le, ok := l.leases[id]
	if !ok {
		return
	}
	l.endInFlight(le)
	l.setPhase(le, PhaseReleased, ReasonReleased)
	l.refresh("")
}
