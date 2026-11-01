package ledger

import (
	"errors"
	"testing"
)

func TestReserveShrinkTarget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mod    func(f *fixture)
		vm     VMName
		target int64
		want   int64
	}{
		{"raised to floor", nil, "vm-a", 1 * Gi, 6 * Gi},
		{"quantized up", nil, "vm-a", 6*Gi + 1, 6*Gi + 256*Mi},
		{"above floor kept", nil, "vm-a", 7 * Gi, 7 * Gi},
		{
			"raised to policy min",
			func(f *fixture) {
				f.cfg.VMs[1].Policy.Min = 7 * Gi
				f.l.Configure(f.cfg)
			},
			"vm-b", 1 * Gi, 7 * Gi,
		},
		{
			"granted leases count in floor",
			func(f *fixture) {
				f.enqueue(ext("r", "vm-a", 1*Gi))
				f.grant("r", "vm-a")
			},
			"vm-a", 0, 7 * Gi,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := standard(t)
			if tc.mod != nil {
				tc.mod(f)
			}
			res, err := f.l.ReserveShrink(tc.vm, tc.target)
			if err != nil {
				t.Fatal(err)
			}
			if res.Target != tc.want || res.From != 8*Gi || res.VM != tc.vm {
				t.Errorf("reservation %+v, want target %d", res, tc.want)
			}
			if v := f.vmView(tc.vm); res.Target < v.Floor {
				t.Errorf("target %d below floor %d", res.Target, v.Floor)
			}
			if s := f.l.Snapshot(); len(s.Shrinks) != 1 || s.Shrinks[0] != res {
				t.Errorf("snapshot shrinks %+v", s.Shrinks)
			}
		})
	}
}

func TestShrinkInFlight(t *testing.T) {
	f := standard(t)
	res, err := f.l.ReserveShrink("vm-a", 6*Gi)
	if err != nil {
		t.Fatal(err)
	}
	// Grants see only the shrink target: headroom 0, and resizes are held.
	if v := f.vmView("vm-a"); v.Headroom != 0 || v.Committed != 8*Gi {
		t.Errorf("during shrink: headroom %d committed %d", v.Headroom, v.Committed)
	}
	f.enqueue(ext("r", "vm-a", 1*Gi))
	if _, err := f.l.Reserve("r", "vm-a"); !errors.Is(err, ErrVMBusy) {
		t.Errorf("grow during shrink: %v", err)
	}
	if _, err := f.l.ReserveShrink("vm-a", 6*Gi); !errors.Is(err, ErrVMBusy) {
		t.Errorf("second shrink: %v", err)
	}
	out := ResizeOutcome{AssignedBefore: 8 * Gi, AssignedAfter: 6 * Gi, StartupBefore: 8 * Gi, StartupAfter: 6 * Gi, ObservedAt: f.now()}
	if err := f.l.CommitShrink(res, out); err != nil {
		t.Fatal(err)
	}
	if err := f.l.CommitShrink(res, out); !errors.Is(err, ErrStaleReservation) {
		t.Errorf("second CommitShrink: %v", err)
	}
	if v := f.vmView("vm-a"); v.Assigned != 6*Gi || v.Committed != 6*Gi {
		t.Errorf("after shrink: assigned %d committed %d", v.Assigned, v.Committed)
	}
	if s := f.l.Snapshot(); len(s.Shrinks) != 0 {
		t.Errorf("shrink still listed: %+v", s.Shrinks)
	}
	// A failed shrink re-reads the VM unchanged; a zero outcome keeps the view.
	res2, err := f.l.ReserveShrink("vm-b", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.l.CommitShrink(res2, ResizeOutcome{}); err != nil {
		t.Fatal(err)
	}
	if v := f.vmView("vm-b"); v.Assigned != 8*Gi {
		t.Errorf("zero outcome changed assigned: %d", v.Assigned)
	}
}

func TestReserveShrinkRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mod    func(f *fixture)
		vm     VMName
		target int64
		is     error
	}{
		{"frozen", func(f *fixture) { f.l.SetAgentState(DefaultHostAgent, AgentDegraded) }, "vm-a", 6 * Gi, ErrFrozen},
		{"unknown VM", nil, "nope", 6 * Gi, ErrUnknownVM},
		{"fixed VM", nil, "linux-node-1", 8 * Gi, ErrNotResizable},
		{"dynamic VM", nil, "vm-dm", 2 * Gi, ErrNotResizable},
		{"guest agent not ready", func(f *fixture) { f.l.SetAgentState("vm-a", AgentUnreachable) }, "vm-a", 6 * Gi, ErrVMUnknown},
		{"nothing to shrink", nil, "vm-a", 8 * Gi, ErrNothingToShrink},
		{"floor at assigned", func(f *fixture) { f.guest("vm-a", 8*Gi, 6*Gi) }, "vm-a", 1 * Gi, ErrNothingToShrink},
		{
			"grow in flight",
			func(f *fixture) {
				f.enqueue(ext("r", "vm-a", 3*Gi))
				f.reserve("r", "vm-a")
			},
			"vm-a", 6 * Gi, ErrVMBusy,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := standard(t)
			if tc.mod != nil {
				tc.mod(f)
			}
			if _, err := f.l.ReserveShrink(tc.vm, tc.target); !errors.Is(err, tc.is) {
				t.Errorf("ReserveShrink = %v, want %v", err, tc.is)
			}
		})
	}
	f := standard(t)
	if err := f.l.CommitShrink(ShrinkReservation{VM: "nope"}, ResizeOutcome{}); !errors.Is(err, ErrStaleReservation) {
		t.Errorf("CommitShrink(unknown): %v", err)
	}
}
