package ledger

import "fmt"

// ShrinkReservation is an in-flight shrink of a static VM (§9, §3.5). While
// it exists, grants on the VM see only Target as assigned, so no grant can
// take memory the shrink removes, and no other resize of the VM starts.
type ShrinkReservation struct {
	VM VMName
	// From is MemoryAssigned when the shrink was reserved.
	From int64
	// Target is the new assignment: never below floor(vm) or policy.min,
	// quantized up to step.
	Target int64
	Seq    uint64
}

// ReserveShrink reserves a shrink of a static VM towards target. The target is
// raised to floor(vm) and policy.min and quantized up to step; it is never
// below floor. ErrNothingToShrink is returned when that leaves nothing to
// shrink.
func (l *Ledger) ReserveShrink(vm VMName, target int64) (ShrinkReservation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.compute()
	q := c.vms[vm]
	switch {
	case c.frozen:
		return ShrinkReservation{}, ErrFrozen
	case q == nil || !q.managed:
		return ShrinkReservation{}, fmt.Errorf("%w: %s", ErrUnknownVM, vm)
	case q.cfg.Mode != ModeStatic:
		return ShrinkReservation{}, fmt.Errorf("%w: %s is %s", ErrNotResizable, vm, q.cfg.Mode)
	case q.state != VMReady:
		return ShrinkReservation{}, fmt.Errorf("%w: %s is %s", ErrVMUnknown, vm, q.state)
	case q.busy:
		return ShrinkReservation{}, fmt.Errorf("%w: %s", ErrVMBusy, vm)
	}
	step := l.cfg.Step
	t := max(quantizeUp(target, step), quantizeUp(q.floor, step))
	if p := q.cfg.Policy; p != nil {
		t = max(t, quantizeUp(p.Min, step))
	}
	if t >= q.assigned {
		return ShrinkReservation{}, fmt.Errorf("%w: %s target %d, assigned %d", ErrNothingToShrink, vm, t, q.assigned)
	}
	l.resSeq++
	res := ShrinkReservation{VM: vm, From: q.assigned, Target: t, Seq: l.resSeq}
	cp := res
	l.vm(vm).shrink = &cp
	l.refresh("")
	return res, nil
}

// CommitShrink ends a shrink reservation, successful or not: out is the
// re-read of MemoryAssigned/MemoryStartup after the attempt (§3.5).
func (l *Ledger) CommitShrink(res ShrinkReservation, out ResizeOutcome) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	v := l.vms[res.VM]
	if v == nil || v.shrink == nil || v.shrink.Seq != res.Seq {
		return fmt.Errorf("%w: shrink of %s", ErrStaleReservation, res.VM)
	}
	v.shrink = nil
	l.applyOutcome(res.VM, out)
	l.refresh("")
	return nil
}
