package ledger

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"pgregory.net/rapid"
)

// world is a host drawn by rapid. Non-lease usage stays at or below base, so
// guest data going stale or fresh never moves a floor: every change of the
// invariants below is caused by a ledger operation, never by new data.
type world struct {
	cfg       Config
	assigned  map[VMName]int64
	used      map[VMName]int64
	dynMax    map[VMName]int64
	unmanaged map[VMName]int64
	hostTotal int64
	hostFree  int64
}

var managedVMs = []VMName{"vm-a", "vm-b", "linux-node-1", "vm-dm"}

func drawWorld(t *rapid.T) *world {
	cfg := testConfig()
	cfg.PriorityOverQueue = rapid.Bool().Draw(t, "priorityOverQueue")
	cfg.DMAccounting = rapid.SampledFrom([]DMAccounting{DMAccountingMaximum, DMAccountingAssigned}).Draw(t, "dmAccounting")
	cfg.Step = rapid.SampledFrom([]int64{2 * Mi, 128 * Mi, 256 * Mi}).Draw(t, "step")
	cfg.VMs[1].Policy.Max = rapid.Int64Range(8, 40).Draw(t, "vmBMaxGi") * Gi
	w := &world{
		cfg: cfg, assigned: map[VMName]int64{}, used: map[VMName]int64{},
		dynMax: map[VMName]int64{}, unmanaged: map[VMName]int64{}, hostTotal: 128 * Gi,
	}
	for _, vm := range cfg.VMs {
		w.assigned[vm.Name] = rapid.Int64Range(2*Ki, 16*Ki).Draw(t, "assignedMi-"+string(vm.Name)) * Mi
		w.used[vm.Name] = rapid.Int64Range(0, vm.Base).Draw(t, "used-"+string(vm.Name))
	}
	w.dynMax["vm-dm"] = w.assigned["vm-dm"] + rapid.Int64Range(0, 8*Ki).Draw(t, "dmHeadMi")*Mi
	w.unmanaged["other-1"] = rapid.Int64Range(0, 8*Ki).Draw(t, "other1Mi") * Mi

	probe := New(w.cfg, NewFakeClock(t0))
	for _, o := range w.observations(t0) {
		probe.Observe(o)
	}
	w.cfg.Capacity = probe.Snapshot().Committed + w.cfg.Reserve + rapid.Int64Range(0, 24*Ki).Draw(t, "capacityExtraMi")*Mi
	w.hostFree = w.cfg.Reserve + rapid.Int64Range(0, 24*Ki).Draw(t, "freeExtraMi")*Mi
	return w
}

func (w *world) observations(at time.Time) []Observation {
	o := Observation{Host: &HostObservation{Total: w.hostTotal, Free: w.hostFree, ObservedAt: at}, CompleteVMList: true}
	var out []Observation
	for _, vm := range w.cfg.VMs {
		vo := VMObservation{Name: vm.Name, Assigned: w.assigned[vm.Name], Startup: w.assigned[vm.Name], ObservedAt: at}
		if m, ok := w.dynMax[vm.Name]; ok {
			vo.Dynamic = &DynamicMemory{Minimum: Gi, Maximum: m}
		}
		o.VMs = append(o.VMs, vo)
		a := w.assigned[vm.Name]
		out = append(out, Observation{Guest: &GuestObservation{VM: vm.Name, Total: a, Available: a - w.used[vm.Name], ObservedAt: at}})
	}
	for name, a := range w.unmanaged {
		o.VMs = append(o.VMs, VMObservation{Name: name, Assigned: a, ObservedAt: at})
	}
	return append([]Observation{o}, out...)
}

func (w *world) build(clk *FakeClock) *Ledger {
	l := New(w.cfg, clk)
	for _, o := range w.observations(clk.Now()) {
		l.Observe(o)
	}
	l.SetAgentState(DefaultHostAgent, AgentReady)
	for _, vm := range w.cfg.VMs {
		l.SetAgentState(AgentName(vm.Name), AgentReady)
	}
	return l
}

func drawRequest(t *rapid.T, id RequestID) Request {
	kind := rapid.SampledFrom([]Kind{KindPod, KindExternal, KindReconciler, KindProbe}).Draw(t, "kind")
	r := Request{
		ID: id, Kind: kind,
		Bytes:         rapid.Int64Range(1, 10*Ki).Draw(t, "bytesMi")*Mi - rapid.Int64Range(0, 1).Draw(t, "odd"),
		PriorityClass: rapid.Int32Range(0, 2).Draw(t, "priority"),
	}
	if kind == KindPod {
		r.Candidates = rapid.SliceOfDistinct(rapid.SampledFrom(managedVMs), func(v VMName) VMName { return v }).Draw(t, "candidates")
	} else {
		r.VM = rapid.SampledFrom(managedVMs).Draw(t, "vm")
	}
	if rapid.Bool().Draw(t, "hasDeadline") {
		r.Deadline = t0.Add(time.Duration(rapid.IntRange(1, 300).Draw(t, "deadlineS")) * time.Second)
	}
	return r
}

// machine is the rapid state machine over the ledger.
type machine struct {
	w      *world
	clk    *FakeClock
	l      *Ledger
	phases map[RequestID]Phase
	res    map[RequestID]Reservation
	shrink map[VMName]ShrinkReservation
	ids    []RequestID
}

func (m *machine) pick(t *rapid.T, phases ...Phase) (RequestID, bool) {
	var c []RequestID
	for _, id := range m.ids {
		if len(phases) == 0 || slices.Contains(phases, m.phases[id]) {
			c = append(c, id)
		}
	}
	if len(c) == 0 {
		return "", false
	}
	return rapid.SampledFrom(c).Draw(t, "id"), true
}

func (m *machine) leaseOf(id RequestID) (LeaseView, bool) { return m.l.Lease(id) }

func (m *machine) enqueue(t *rapid.T) {
	id := RequestID(fmt.Sprintf("r-%d", len(m.ids)))
	r := drawRequest(t, id)
	if _, err := m.l.Enqueue(r); err != nil {
		t.Fatalf("Enqueue(%+v): %v", r, err)
	}
	m.ids = append(m.ids, id)
	m.phases[id] = PhasePending
}

func (m *machine) setCandidates(t *rapid.T) {
	id, ok := m.pick(t, PhasePending)
	if !ok {
		return
	}
	vms := rapid.SliceOfDistinct(rapid.SampledFrom(managedVMs), func(v VMName) VMName { return v }).Draw(t, "candidates")
	err := m.l.SetCandidates(id, vms)
	lv, _ := m.leaseOf(id)
	if (lv.Request.Kind == KindPod) != (err == nil) {
		t.Fatalf("SetCandidates(%s, kind %s) = %v", id, lv.Request.Kind, err)
	}
}

// queueLocks is an outside model of what waiting requests block (§3.4 as
// decided for hvmc): a waiting request that could use the pool (a static or
// dynamic candidate) blocks pool draws and every request sharing a candidate
// with it; a waiting request with only fixed candidates blocks placements on
// those VMs. locksBefore builds it from the Pending requests ahead of index i.
type queueLocks struct {
	pool  bool
	cands map[VMName]RequestID
	vms   map[VMName]RequestID
}

func locksBefore(s Snapshot, i int) queueLocks {
	resizable := map[VMName]bool{}
	for _, v := range s.VMs {
		resizable[v.Name] = v.Managed && v.Mode != ModeFixed
	}
	lk := queueLocks{cands: map[VMName]RequestID{}, vms: map[VMName]RequestID{}}
	for _, lv := range s.Leases[:i] {
		cands := lv.Request.candidates()
		if lv.Phase != PhasePending || lv.Grantable || len(cands) == 0 {
			continue
		}
		into := lk.vms
		if slices.ContainsFunc(cands, func(vm VMName) bool { return resizable[vm] }) {
			lk.pool, into = true, lk.cands
		}
		for _, vm := range cands {
			into[vm] = lv.Request.ID
		}
	}
	return lk
}

// violation explains why granting lv with fit f would overtake a waiting
// request ahead of it, or returns "".
func (lk queueLocks) violation(lv LeaseView, f Fit) string {
	if lk.pool && f.Delta != 0 {
		return fmt.Sprintf("draws %d from the pool behind a waiting pool user", f.Delta)
	}
	if id, ok := lk.vms[f.VM]; ok {
		return fmt.Sprintf("is placed on %s, a candidate of waiting fixed-only %s", f.VM, id)
	}
	for _, c := range lv.Request.candidates() {
		if id, ok := lk.cands[c]; ok {
			return fmt.Sprintf("shares candidate %s with waiting %s", c, id)
		}
	}
	return ""
}

func (m *machine) reserve(t *rapid.T) {
	id, ok := m.pick(t, PhasePending)
	if !ok {
		return
	}
	vm := rapid.SampledFrom(managedVMs).Draw(t, "vm")
	before := m.l.Snapshot()
	lv, _ := m.leaseOf(id)
	evalOn := []VMName{vm}
	if lv.Request.Kind == KindPod && lv.Request.VM == "" && len(lv.Request.Candidates) > 0 {
		evalOn = lv.Request.Candidates // the filter's view
	}
	var fit Fit
	for _, f := range m.l.Evaluate(id, evalOn) {
		if f.VM == vm {
			fit = f
		}
	}
	res, err := m.l.Reserve(id, vm)
	if fit.VM == "" { // vm is not a candidate of the pod
		if reasonOf(err) != ReasonNotCandidate {
			t.Fatalf("Reserve(%s) on non-candidate %s: %v", id, vm, err)
		}
		return
	}
	fits := []Fit{fit}
	if err != nil {
		if fits[0].Fits {
			t.Fatalf("Evaluate said %s fits on %s, Reserve failed: %v", id, vm, err)
		}
		var nf *NotFitError
		if !errors.As(err, &nf) {
			t.Fatalf("Reserve error %v is not a NotFitError", err)
		}
		return
	}
	if !fits[0].Fits || fits[0].Delta != res.Delta {
		t.Fatalf("Reserve succeeded but Evaluate said %+v", fits[0])
	}
	// The head of the queue is never overtaken by a pool-drawing or
	// same-candidate request (or, behind a fixed-only head, by a request on
	// its VMs).
	mine := slices.IndexFunc(before.Leases, func(lv LeaseView) bool { return lv.Request.ID == id })
	if why := locksBefore(before, mine).violation(before.Leases[mine], fit); why != "" {
		t.Fatalf("%s reserved on %s although it %s", id, vm, why)
	}
	m.phases[id] = PhaseReserved
	m.res[id] = res
}

// reserveRollback: Rollback restores the pre-Reserve state exactly (a pod
// stays pinned to the VM it was bound to; see Rollback).
func (m *machine) reserveRollback(t *rapid.T) {
	id, ok := m.pick(t, PhasePending)
	if !ok {
		return
	}
	vm := rapid.SampledFrom(managedVMs).Draw(t, "vm")
	before := m.l.Snapshot()
	lv, _ := m.leaseOf(id)
	res, err := m.l.Reserve(id, vm)
	if err != nil {
		return
	}
	m.l.Rollback(res, ReasonResizeFailed, true)
	if lv.Request.VM == "" {
		// Undo the pod's pin, the one intended difference, then compare.
		m.l.mu.Lock()
		m.l.leases[id].req.VM = ""
		m.l.mu.Unlock()
	}
	if d := cmp.Diff(before, m.l.Snapshot()); d != "" {
		t.Fatalf("Rollback did not restore the state (-before +after):\n%s", d)
	}
}

func (m *machine) markResizing(t *rapid.T) {
	id, ok := m.pick(t, PhaseReserved, PhaseResizing)
	if !ok {
		return
	}
	err := m.l.MarkResizing(m.res[id])
	if (m.phases[id] == PhaseReserved) != (err == nil) {
		t.Fatalf("MarkResizing(%s in %s) = %v", id, m.phases[id], err)
	}
	m.phases[id] = PhaseResizing
}

func (m *machine) commit(t *rapid.T) {
	id, ok := m.pick(t, PhaseReserved, PhaseResizing)
	if !ok {
		return
	}
	res := m.res[id]
	var out ResizeOutcome
	if res.Delta > 0 {
		out = ResizeOutcome{AssignedBefore: res.AssignedBefore, AssignedAfter: res.Target, StartupAfter: res.Target, ObservedAt: m.clk.Now()}
		if mx, ok := m.w.dynMax[res.VM]; ok {
			out.MaximumAfter = max(mx, res.Target)
		}
	}
	err := m.l.Commit(res, out)
	legal := m.phases[id] == PhaseResizing || res.Delta == 0
	if legal != (err == nil) {
		t.Fatalf("Commit(%s in %s, delta %d) = %v", id, m.phases[id], res.Delta, err)
	}
	if !legal {
		return
	}
	if res.Delta > 0 {
		m.w.hostFree -= res.Target - m.w.assigned[res.VM]
		m.w.assigned[res.VM] = res.Target
		if out.MaximumAfter > 0 {
			m.w.dynMax[res.VM] = out.MaximumAfter
		}
	}
	m.phases[id] = PhaseGranted
	delete(m.res, id)
}

func (m *machine) rollback(t *rapid.T) {
	id, ok := m.pick(t, PhaseReserved, PhaseResizing)
	if !ok {
		return
	}
	requeue := rapid.Bool().Draw(t, "requeue")
	m.l.Rollback(m.res[id], ReasonResizeFailed, requeue)
	delete(m.res, id)
	if requeue {
		m.phases[id] = PhasePending
	} else {
		m.phases[id] = PhaseFailed
	}
}

func (m *machine) release(t *rapid.T) {
	id, ok := m.pick(t)
	if !ok {
		return
	}
	m.l.Release(id)
	if p := m.phases[id]; p != PhaseFailed {
		m.phases[id] = PhaseReleased
	}
	delete(m.res, id)
}

func (m *machine) fail(t *rapid.T) {
	id, ok := m.pick(t)
	if !ok {
		return
	}
	err := m.l.Fail(id, "Rejected")
	if (m.phases[id] == PhasePending) != (err == nil) {
		t.Fatalf("Fail(%s in %s) = %v", id, m.phases[id], err)
	}
	if err == nil {
		m.phases[id] = PhaseFailed
	}
}

func (m *machine) expire(t *rapid.T) {
	now := m.clk.Advance(time.Duration(rapid.IntRange(0, 40).Draw(t, "advanceS")) * time.Second)
	got := m.l.Expire(now)
	var want []RequestID
	for _, lv := range m.l.Snapshot().Leases {
		if lv.Phase == PhasePending && !lv.Request.Deadline.IsZero() && !now.Before(lv.Request.Deadline) {
			want = append(want, lv.Request.ID)
		}
	}
	if len(want) > 0 {
		t.Fatalf("Expire left expired requests pending: %v", want)
	}
	for _, id := range got {
		if m.phases[id] != PhasePending {
			t.Fatalf("Expire(%s in %s)", id, m.phases[id])
		}
		m.phases[id] = PhaseFailed
	}
}

// observe re-reads the world. Agent data is always taken after the ledger
// operations before it; with equal timestamps the ledger would (correctly)
// not know whether a resize is reflected and count it twice.
func (m *machine) observe(_ *rapid.T) {
	for _, o := range m.w.observations(m.clk.Advance(time.Millisecond)) {
		m.l.Observe(o)
	}
}

func (m *machine) agent(t *rapid.T) {
	a := rapid.SampledFrom([]AgentName{DefaultHostAgent, "vm-a", "vm-b", "linux-node-1", "vm-dm"}).Draw(t, "agent")
	s := rapid.SampledFrom([]AgentState{AgentReady, AgentReady, AgentDegraded, AgentUnreachable}).Draw(t, "state")
	m.l.SetAgentState(a, s)
}

func (m *machine) reserveShrink(t *rapid.T) {
	vm := rapid.SampledFrom([]VMName{"vm-a", "vm-b"}).Draw(t, "vm")
	if _, busy := m.shrink[vm]; busy {
		return
	}
	target := rapid.Int64Range(0, 16*Ki).Draw(t, "targetMi") * Mi
	res, err := m.l.ReserveShrink(vm, target)
	if err != nil {
		return
	}
	v := m.l.Snapshot()
	var view VMView
	for _, x := range v.VMs {
		if x.Name == vm {
			view = x
		}
	}
	if res.Target < view.Floor || res.Target < target || res.Target%m.w.cfg.Step != 0 || res.Target >= res.From {
		t.Fatalf("shrink %+v: floor %d, asked %d", res, view.Floor, target)
	}
	if p := m.w.cfg.VMs[1].Policy; vm == "vm-b" && res.Target < p.Min {
		t.Fatalf("shrink below policy min: %+v", res)
	}
	m.shrink[vm] = res
}

func (m *machine) commitShrink(t *rapid.T) {
	if len(m.shrink) == 0 {
		return
	}
	var vms []VMName
	for vm := range m.shrink {
		vms = append(vms, vm)
	}
	slices.Sort(vms)
	vm := rapid.SampledFrom(vms).Draw(t, "vm")
	res := m.shrink[vm]
	out := ResizeOutcome{AssignedBefore: res.From, AssignedAfter: res.Target, StartupAfter: res.Target, ObservedAt: m.clk.Now()}
	if !rapid.Bool().Draw(t, "applied") {
		out.AssignedAfter, out.StartupAfter = res.From, res.From // refused shrink
	}
	if err := m.l.CommitShrink(res, out); err != nil {
		t.Fatalf("CommitShrink: %v", err)
	}
	m.w.hostFree += res.From - out.AssignedAfter
	m.w.assigned[vm] = out.AssignedAfter
	delete(m.shrink, vm)
}

func (m *machine) dispatch(t *rapid.T) {
	ids := m.l.Dispatchable()
	for _, id := range ids {
		lv, ok := m.leaseOf(id)
		if !ok || lv.Request.Kind == KindPod || lv.Phase != PhasePending {
			t.Fatalf("Dispatchable returned %s: %+v", id, lv)
		}
		if f := m.l.Evaluate(id, nil); len(f) != 1 || !f[0].Fits {
			t.Fatalf("Dispatchable returned non-fitting %s: %+v", id, f)
		}
	}
	if len(ids) == 0 {
		return
	}
	lv, _ := m.leaseOf(ids[0])
	res, err := m.l.Reserve(ids[0], lv.Request.VM)
	if err != nil {
		t.Fatalf("first dispatchable %s not reservable: %v", ids[0], err)
	}
	m.phases[ids[0]] = PhaseReserved
	m.res[ids[0]] = res
}

func (m *machine) check(t *rapid.T) {
	s := m.l.Snapshot()
	if s.Pool < 0 {
		t.Fatalf("pool negative: %d (%+v)", s.Pool, s)
	}
	if s.Committed > s.Capacity-s.Reserve {
		t.Fatalf("Σ committed %d > capacity − reserve %d", s.Committed, s.Capacity-s.Reserve)
	}
	// Phases follow the model (legal transitions only).
	for _, id := range m.ids {
		want := m.phases[id]
		lv, ok := m.l.Lease(id)
		switch {
		case want == PhaseReleased || want == PhaseFailed:
			if ok {
				t.Fatalf("%s: terminal in model, %s in ledger", id, lv.Phase)
			}
		case !ok || lv.Phase != want:
			t.Fatalf("%s: phase %s, model %s", id, lv.Phase, want)
		}
	}
	// FIFO within a class: priority never rises along the queue and arrival
	// time never decreases within one priority.
	var prev *LeaseView
	for i := range s.Leases {
		lv := &s.Leases[i]
		if lv.Phase != PhasePending {
			break
		}
		if prev != nil {
			pp, cp := m.l.priority(prev.Request), m.l.priority(lv.Request)
			if cp > pp || (cp == pp && lv.Request.EnqueuedAt.Before(prev.Request.EnqueuedAt)) {
				t.Fatalf("queue order broken at %s after %s", lv.Request.ID, prev.Request.ID)
			}
		}
		prev = lv
	}
	// Nothing grantable behind a waiting request takes what that one needs.
	for i, lv := range s.Leases {
		if lv.Phase != PhasePending || !lv.Grantable {
			continue
		}
		lk := locksBefore(s, i)
		for _, f := range m.l.Evaluate(lv.Request.ID, nil) {
			if why := lk.violation(lv, f); f.Fits && why != "" {
				t.Fatalf("%s grantable on %s although it %s", lv.Request.ID, f.VM, why)
			}
		}
	}
	// Shrink reservations never go below floor while in flight.
	for _, sh := range s.Shrinks {
		for _, v := range s.VMs {
			if v.Name == sh.VM && v.Floor > sh.Target {
				t.Fatalf("floor %d of %s above shrink target %d", v.Floor, sh.VM, sh.Target)
			}
		}
	}
	// Frozen: nothing is dispatchable.
	if s.Frozen && m.l.Dispatchable() != nil {
		t.Fatalf("frozen ledger dispatches")
	}
}

// TestPropertyLedger runs random operation sequences and checks after every
// step: pool never negative, Σ committed ≤ capacity − reserve, legal
// transitions only, FIFO within a class, the head never overtaken, shrinks
// never below floor, Dispatchable never returns a pod or non-fitting request,
// and Rollback restoring the pre-Reserve state.
func TestPropertyLedger(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		w := drawWorld(t)
		clk := NewFakeClock(t0)
		m := &machine{
			w: w, clk: clk, l: w.build(clk),
			phases: map[RequestID]Phase{}, res: map[RequestID]Reservation{}, shrink: map[VMName]ShrinkReservation{},
		}
		t.Repeat(map[string]func(*rapid.T){
			"":                m.check,
			"enqueue":         m.enqueue,
			"setCandidates":   m.setCandidates,
			"reserve":         m.reserve,
			"reserveRollback": m.reserveRollback,
			"markResizing":    m.markResizing,
			"commit":          m.commit,
			"rollback":        m.rollback,
			"release":         m.release,
			"fail":            m.fail,
			"expire":          m.expire,
			"observe":         m.observe,
			"agent":           m.agent,
			"reserveShrink":   m.reserveShrink,
			"commitShrink":    m.commitShrink,
			"dispatch":        m.dispatch,
		})
	})
}

// TestPropertyRollbackAfterResize: a rollback after the resize was issued
// restores the state exactly once the host reports the VM again (the hold
// keeps the possibly grown memory counted until then).
func TestPropertyRollbackAfterResize(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		w := drawWorld(t)
		clkA, clkB := NewFakeClock(t0), NewFakeClock(t0)
		a, b := w.build(clkA), w.build(clkB)
		n := rapid.IntRange(1, 6).Draw(t, "n")
		var reqs []Request
		for i := range n {
			r := drawRequest(t, RequestID(fmt.Sprintf("r-%d", i)))
			r.Deadline = time.Time{}
			reqs = append(reqs, r)
		}
		// Find a request that fits somewhere, on a scratch ledger.
		scratch := w.build(NewFakeClock(t0))
		type pair struct {
			i  int
			vm VMName
		}
		var fitting []pair
		for _, r := range reqs {
			if _, err := scratch.Enqueue(r); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
		}
		for i, r := range reqs {
			on := managedVMs
			if r.Kind == KindPod && len(r.Candidates) > 0 {
				on = nil // its own candidates
			}
			for _, f := range scratch.Evaluate(r.ID, on) {
				if f.Fits {
					fitting = append(fitting, pair{i, f.VM})
				}
			}
		}
		if len(fitting) == 0 {
			t.Skip("nothing fits")
		}
		p := rapid.SampledFrom(fitting).Draw(t, "pair")
		pick, vm := p.i, p.vm
		id := reqs[pick].ID
		for i, r := range reqs {
			if _, err := a.Enqueue(r); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			if i == pick && r.Kind == KindPod {
				// The untouched ledger gets the pod already pinned, the one
				// intended effect of a rolled-back bind.
				r.VM = vm
			}
			if _, err := b.Enqueue(r); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
		}
		res, err := a.Reserve(id, vm)
		if err != nil {
			t.Fatalf("Reserve(%s, %s): %v", id, vm, err)
		}
		if res.Delta > 0 {
			if err := a.MarkResizing(res); err != nil {
				t.Fatal(err)
			}
		}
		a.Rollback(res, ReasonVerifyTimeout, true)
		for _, l := range []*Ledger{a, b} {
			l.clk.(*FakeClock).Advance(time.Second)
			for _, o := range w.observations(l.clk.Now()) {
				l.Observe(o)
			}
		}
		sa, sb := a.Snapshot(), b.Snapshot()
		if d := cmp.Diff(sb, sa); d != "" {
			t.Fatalf("state after rollback differs (-untouched +rolled back):\n%s", d)
		}
	})
}

// TestPropertyRebuildIdempotent: Rebuild is deterministic, and rebuilding from
// a rebuilt ledger's own records changes nothing.
func TestPropertyRebuildIdempotent(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		w := drawWorld(t)
		n := rapid.IntRange(0, 10).Draw(t, "n")
		var recs []LeaseRecord
		for i := range n {
			r := drawRequest(t, RequestID(fmt.Sprintf("r-%d", rapid.IntRange(0, 12).Draw(t, "idn"))))
			r.EnqueuedAt = t0.Add(time.Duration(rapid.IntRange(-100, 0).Draw(t, "enqS")) * time.Second)
			p := rapid.SampledFrom([]Phase{PhasePending, PhaseReserved, PhaseResizing, PhaseGranted, PhaseReleased, PhaseFailed}).Draw(t, "phase")
			if p != PhasePending && r.VM == "" && len(r.Candidates) > 0 {
				r.VM = r.Candidates[0]
			}
			recs = append(recs, LeaseRecord{Request: r, Phase: p, GrantedAt: t0.Add(time.Duration(i) * time.Second)})
		}
		clk := NewFakeClock(t0)
		obs := w.observations(t0)
		l1, a1 := Rebuild(w.cfg, recs, obs, clk)
		l2, a2 := Rebuild(w.cfg, recs, obs, clk)
		if d := cmp.Diff(a1, a2); d != "" {
			t.Fatalf("actions differ:\n%s", d)
		}
		s1 := l1.Snapshot()
		if d := cmp.Diff(s1, l2.Snapshot()); d != "" {
			t.Fatalf("ledgers differ:\n%s", d)
		}
		l3, a3 := Rebuild(w.cfg, l1.Records(), obs, clk)
		if d := cmp.Diff(s1, l3.Snapshot()); d != "" {
			t.Fatalf("rebuild of rebuild differs:\n%s", d)
		}
		for _, a := range a3 {
			if a.Kind == RecoveryLost || a.Kind == RecoveryRequeue || a.Kind == RecoveryDrop {
				t.Fatalf("second rebuild still changes %s: %s", a.ID, a.Kind)
			}
		}
	})
}
