// Package ledger is hvmc's RAM ledger for one Hyper-V host (DESIGN §3): the
// budget math, the request queue and the lease state machine.
//
// The package is pure: no I/O, no Kubernetes types, time only through Clock.
// One mutex guards all state; every method returns without blocking on
// anything but that mutex. Resizes follow reserve → act → commit (§3.2): the
// ledger provides the transitions (Reserve, MarkResizing, Commit, Rollback,
// ReserveShrink, CommitShrink); the grant executor does the I/O between them.
package ledger

import (
	"slices"
	"strings"
	"sync"
	"time"
)

// AgentState is the health tracker's state of an agent (DESIGN §4.8).
type AgentState string

// Agent states. Agents never reported are AgentUnknown, which is not Ready.
const (
	AgentUnknown     AgentState = "Unknown"
	AgentReady       AgentState = "Ready"
	AgentDegraded    AgentState = "Degraded"
	AgentUnreachable AgentState = "Unreachable"
)

// Observation carries data from the agents. Any field may be nil/empty; each
// value carries its own ObservedAt and older values never replace newer ones.
type Observation struct {
	Host *HostObservation
	VMs  []VMObservation
	// CompleteVMList marks VMs as the host's complete VM list (ListVMs or a
	// WatchVMs snapshot): host data of VMs not in it is dropped.
	CompleteVMList bool
	Guest          *GuestObservation
}

// HostObservation is the host agent's GetHost result.
type HostObservation struct {
	Total      int64
	Free       int64
	ObservedAt time.Time
}

// VMObservation is one VM as reported by the host agent (managed or not).
type VMObservation struct {
	Name     VMName
	Assigned int64
	Startup  int64
	// Dynamic is set when Dynamic Memory is enabled on the VM.
	Dynamic    *DynamicMemory
	ObservedAt time.Time
}

// DynamicMemory are a VM's Dynamic Memory settings.
type DynamicMemory struct {
	Minimum  int64
	Maximum  int64
	Buffer   int64
	Priority int
}

// GuestObservation is a guest agent's GetMemory result.
type GuestObservation struct {
	VM              VMName
	Total           int64
	Available       int64
	CommitLimit     int64
	CommitAvailable int64
	ObservedAt      time.Time
}

// Ledger is the RAM ledger. Create it with New or Rebuild.
type Ledger struct {
	mu     sync.Mutex
	clk    Clock
	cfg    Config
	vmCfg  map[VMName]VMConfig
	host   hostState
	debts  []freeDebt
	vms    map[VMName]*vmState
	agents map[AgentName]AgentState
	leases map[RequestID]*lease
	seq    uint64
	resSeq uint64
	subs   map[uint64]*subscriber
	subSeq uint64
}

type hostState struct {
	seen       bool
	total      int64
	free       int64
	observedAt time.Time
}

// freeDebt is growth of assigned memory the ledger knows about but the last
// host observation (hostFree) may not reflect yet.
type freeDebt struct {
	bytes int64
	at    time.Time
}

type vmState struct {
	hostSeen   bool
	assigned   int64
	startup    int64
	dynamic    *DynamicMemory
	observedAt time.Time
	guest      *GuestObservation
	// hold keeps a possibly applied grow counted until the VM is observed
	// after holdSince.
	holdTarget int64
	holdSince  time.Time
	shrink     *ShrinkReservation
}

func (v *vmState) addHold(target int64, now time.Time) {
	v.holdTarget = max(v.holdTarget, target)
	v.holdSince = now
}

// New returns an empty ledger. A nil clock means SystemClock. Until the host
// agent is reported Ready (SetAgentState) and host data is observed, the
// ledger is frozen.
func New(cfg Config, clk Clock) *Ledger {
	if clk == nil {
		clk = SystemClock{}
	}
	l := &Ledger{
		clk:    clk,
		vms:    map[VMName]*vmState{},
		agents: map[AgentName]AgentState{},
		leases: map[RequestID]*lease{},
		subs:   map[uint64]*subscriber{},
	}
	l.configure(cfg)
	return l
}

// Configure applies a new configuration (HostConfig change). Leases are kept;
// leases on VMs that are no longer managed stay as they are but cannot be
// granted anew.
func (l *Ledger) Configure(cfg Config) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.configure(cfg)
	l.refresh("")
}

func (l *Ledger) configure(cfg Config) {
	l.cfg = cfg.normalize()
	l.vmCfg = make(map[VMName]VMConfig, len(l.cfg.VMs))
	for _, vm := range l.cfg.VMs {
		l.vmCfg[vm.Name] = vm
	}
}

// SetAgentState records an agent's health state. The host agent not Ready
// freezes the ledger; a guest agent not Ready makes its VM Unknown (§4.8).
func (l *Ledger) SetAgentState(agent AgentName, s AgentState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.agents[agent] = s
	l.refresh("")
}

func (l *Ledger) agentState(a AgentName) AgentState {
	if s, ok := l.agents[a]; ok {
		return s
	}
	return AgentUnknown
}

// vm returns the state of a VM, creating it.
func (l *Ledger) vm(name VMName) *vmState {
	v, ok := l.vms[name]
	if !ok {
		v = &vmState{}
		l.vms[name] = v
	}
	return v
}

// Observe applies agent data.
func (l *Ledger) Observe(o Observation) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.observe(o)
	l.refresh("")
}

func (l *Ledger) observe(o Observation) {
	if h := o.Host; h != nil && (!l.host.seen || !h.ObservedAt.Before(l.host.observedAt)) {
		l.host = hostState{seen: true, total: h.Total, free: h.Free, observedAt: h.ObservedAt}
		// Only strictly older growth is surely reflected in this observation.
		l.debts = slices.DeleteFunc(l.debts, func(d freeDebt) bool { return d.at.Before(h.ObservedAt) })
	}
	listed := make(map[VMName]bool, len(o.VMs))
	for _, vo := range o.VMs {
		listed[vo.Name] = true
		l.observeVM(vo.Name, vo.Assigned, vo.Startup, vo.Dynamic, vo.ObservedAt)
	}
	if o.CompleteVMList {
		for name, v := range l.vms {
			if listed[name] {
				continue
			}
			v.hostSeen, v.assigned, v.startup, v.dynamic = false, 0, 0, nil
			if v.guest == nil && v.shrink == nil && v.holdTarget == 0 {
				delete(l.vms, name)
			}
		}
	}
	if g := o.Guest; g != nil {
		v := l.vm(g.VM)
		if v.guest == nil || !g.ObservedAt.Before(v.guest.ObservedAt) {
			cp := *g
			v.guest = &cp
		}
	}
}

// observeVM applies host-side data of one VM unless newer data is present.
func (l *Ledger) observeVM(name VMName, assigned, startup int64, dyn *DynamicMemory, at time.Time) {
	v := l.vm(name)
	if v.hostSeen && at.Before(v.observedAt) {
		return
	}
	if v.hostSeen && assigned > v.assigned && (!l.host.seen || !l.host.observedAt.After(at)) {
		l.debts = append(l.debts, freeDebt{bytes: assigned - v.assigned, at: at})
	}
	v.hostSeen = true
	v.assigned, v.startup, v.observedAt = assigned, startup, at
	if dyn != nil {
		cp := *dyn
		v.dynamic = &cp
	} else {
		v.dynamic = nil
	}
	if v.holdTarget > 0 && at.After(v.holdSince) {
		v.holdTarget, v.holdSince = 0, time.Time{}
	}
}

// ResizeOutcome is what the host reported after a resize (ResizeResult) or a
// fresh re-read. A zero ObservedAt means "no data" (e.g. no resize was
// needed); the ledger then keeps its current view.
type ResizeOutcome struct {
	AssignedBefore int64
	AssignedAfter  int64
	StartupBefore  int64
	StartupAfter   int64
	// MaximumAfter is the Dynamic Memory maximum after SetDynamicLimits, 0 if
	// unchanged or not a DM VM.
	MaximumAfter int64
	ObservedAt   time.Time
}

func (l *Ledger) applyOutcome(name VMName, out ResizeOutcome) {
	if out.ObservedAt.IsZero() {
		return
	}
	v := l.vm(name)
	dyn := v.dynamic
	if dyn != nil && out.MaximumAfter > 0 {
		cp := *dyn
		cp.Maximum = out.MaximumAfter
		dyn = &cp
	}
	l.observeVM(name, out.AssignedAfter, out.StartupAfter, dyn, out.ObservedAt)
}

// VMState is the gating state of a managed VM.
type VMState string

// VM states.
const (
	// VMReady: fresh host and guest data, guest agent Ready.
	VMReady VMState = "Ready"
	// VMUnknown: guest agent not Ready or guest data missing/stale; only
	// grants without resize (delta == 0) are allowed (§4.8).
	VMUnknown VMState = "Unknown"
	// VMMissing: no fresh host data for the VM; nothing is granted on it.
	VMMissing VMState = "Missing"
	// VMUnmanaged: reported by the host but not in Config.VMs.
	VMUnmanaged VMState = "Unmanaged"
)

// Snapshot is a consistent read-only view of the ledger.
type Snapshot struct {
	At             time.Time
	Frozen         bool
	HostAgentState AgentState
	Capacity       int64
	Reserve        int64
	// HostFree is the host agent's last reported free memory.
	HostFree int64
	// Committed is Σ committed(vm) over all VMs.
	Committed int64
	// InFlight is memory being added by in-flight grows, plus growth the last
	// host observation does not reflect yet; it is subtracted from HostFree.
	InFlight int64
	// Pool is min(capacity − reserve − Σ committed, hostFree − inFlight −
	// reserve). It is negative when observed usage already exceeds the budget.
	Pool    int64
	VMs     []VMView
	Leases  []LeaseView
	Shrinks []ShrinkReservation
	Agents  map[AgentName]AgentState
}

// VMView is one VM's quantities (§3.1).
type VMView struct {
	Name       VMName
	Managed    bool
	Mode       Mode
	GuestOS    GuestOS
	State      VMState
	Assigned   int64
	Startup    int64
	Dynamic    *DynamicMemory
	ObservedAt time.Time
	UsedKnown  bool
	Used       int64
	Available  int64
	Base       int64
	Margin     int64
	Leased     int64
	EffBase    int64
	Floor      int64
	Headroom   int64
	Committed  int64
}

// LeaseView is one non-terminal request.
type LeaseView struct {
	Request   Request
	Phase     Phase
	Reason    Reason
	Position  Position
	Grantable bool
	GrantedAt time.Time
	// Reservation is set while Reserved or Resizing.
	Reservation *Reservation
}

// Snapshot returns the current state. Leases are listed Pending first in queue
// order, then the others by ID.
func (l *Ledger) Snapshot() Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.compute()
	s := Snapshot{
		At: c.now, Frozen: c.frozen, HostAgentState: l.agentState(l.cfg.HostAgent),
		Capacity: c.capacity, Reserve: l.cfg.Reserve, HostFree: l.host.free,
		Committed: c.committed, InFlight: c.inflight + c.debt, Pool: c.pool,
		Agents: make(map[AgentName]AgentState, len(l.agents)),
	}
	for a, st := range l.agents {
		s.Agents[a] = st
	}
	for _, name := range c.names {
		q := c.vms[name]
		s.VMs = append(s.VMs, q.view(name))
		if v := l.vms[name]; v != nil && v.shrink != nil {
			s.Shrinks = append(s.Shrinks, *v.shrink)
		}
	}
	sim := l.newSim(c)
	for i, le := range sim.queue {
		ok := sim.advance(le)
		s.Leases = append(s.Leases, l.leaseView(le, Position(i+1), ok))
	}
	var rest []*lease
	for _, le := range l.leases {
		if le.phase != PhasePending {
			rest = append(rest, le)
		}
	}
	slices.SortFunc(rest, func(a, b *lease) int { return strings.Compare(string(a.req.ID), string(b.req.ID)) })
	for _, le := range rest {
		s.Leases = append(s.Leases, l.leaseView(le, 0, false))
	}
	return s
}

func (l *Ledger) leaseView(le *lease, pos Position, grantable bool) LeaseView {
	v := LeaseView{
		Request: le.req.clone(), Phase: le.phase, Reason: le.reason,
		Position: pos, Grantable: grantable, GrantedAt: le.grantedAt,
	}
	if le.phase.inFlight() {
		r := le.res
		v.Reservation = &r
	}
	return v
}

// Lease returns the view of one non-terminal request.
func (l *Ledger) Lease(id RequestID) (LeaseView, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	le, ok := l.leases[id]
	if !ok {
		return LeaseView{}, false
	}
	pos, grantable := Position(0), false
	if le.phase == PhasePending {
		sim := l.newSim(l.compute())
		for i, x := range sim.queue {
			ok := sim.advance(x)
			if x == le {
				pos, grantable = Position(i+1), ok
				break
			}
		}
	}
	return l.leaseView(le, pos, grantable), true
}

// LeaseRecord is a persisted lease (a MemoryLease) as input to Rebuild.
type LeaseRecord struct {
	Request   Request
	Phase     Phase
	GrantedAt time.Time
}

// Records returns every non-terminal lease as a LeaseRecord, ordered by
// EnqueuedAt then ID. Rebuild(cfg, l.Records(), …) reproduces the ledger.
func (l *Ledger) Records() []LeaseRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]LeaseRecord, 0, len(l.leases))
	for _, le := range l.leases {
		out = append(out, LeaseRecord{Request: le.req.clone(), Phase: le.phase, GrantedAt: le.grantedAt})
	}
	slices.SortFunc(out, compareRecords)
	return out
}

func compareRecords(a, b LeaseRecord) int {
	if c := a.Request.EnqueuedAt.Compare(b.Request.EnqueuedAt); c != 0 {
		return c
	}
	return strings.Compare(string(a.Request.ID), string(b.Request.ID))
}
