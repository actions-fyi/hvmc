package ledger

import (
	"slices"
	"strings"
	"time"
)

// The formulas of DESIGN §3.1, as pure functions.

// quantizeUp rounds x up to a multiple of step; x ≤ 0 yields 0.
func quantizeUp(x, step int64) int64 {
	if x <= 0 {
		return 0
	}
	if step <= 1 {
		return x
	}
	return ((x-1)/step + 1) * step
}

// effBaseOf = max(base, used − leased); used counts only when known.
func effBaseOf(base, used int64, usedKnown bool, leased int64) int64 {
	if !usedKnown {
		return base
	}
	return max(base, used-leased)
}

// floorOf = effBase + leased + margin.
func floorOf(effBase, leased, margin int64) int64 { return effBase + leased + margin }

// committedInput are the inputs of committedOf.
type committedInput struct {
	managed  bool
	mode     Mode
	dmAcct   DMAccounting
	assigned int64
	floor    int64
	// dynamicMax is the DM maximum; only meaningful when dynamic is set.
	dynamic    bool
	dynamicMax int64
	// inflightTarget is the highest assignment an in-flight grow may reach.
	inflightTarget int64
}

// committedOf implements committed(vm). In-flight grow targets are included so
// that a reservation counts in committed before the resize lands (§3.2).
func committedOf(in committedInput) int64 {
	if !in.managed {
		if in.dynamic {
			return in.dynamicMax
		}
		return in.assigned
	}
	switch in.mode {
	case ModeFixed:
		return in.assigned
	case ModeDynamic:
		if in.dmAcct == DMAccountingMaximum && in.dynamic {
			return max(in.dynamicMax, in.assigned, in.inflightTarget)
		}
		return max(in.assigned, in.floor, in.inflightTarget)
	default:
		return max(in.assigned, in.floor, in.inflightTarget)
	}
}

// poolOf = min(capacity − reserve − Σ committed, hostFree − reserve).
func poolOf(capacity, reserve, sumCommitted, hostFree int64) int64 {
	return min(capacity-reserve-sumCommitted, hostFree-reserve)
}

// deltaOf = quantize_up(bytes − max(headroom, 0), step).
func deltaOf(bytes, headroom, step int64) int64 {
	return quantizeUp(bytes-max(headroom, 0), step)
}

// fitsOf = delta == 0 OR pool ≥ delta.
func fitsOf(delta, pool int64) bool { return delta == 0 || pool >= delta }

// vmQ are the computed quantities of one VM at one instant.
type vmQ struct {
	managed       bool
	cfg           VMConfig
	state         VMState
	assigned      int64
	assignedFresh bool
	startup       int64
	dynamic       *DynamicMemory
	observedAt    time.Time
	used          int64
	available     int64
	usedKnown     bool
	margin        int64
	leased        int64
	effBase       int64
	floor         int64
	headroom      int64
	committed     int64
	inflight      int64
	// busy: a grow with a resize, a hold or a shrink is in flight.
	busy bool
}

func (q *vmQ) view(name VMName) VMView {
	v := VMView{
		Name: name, Managed: q.managed, Mode: q.cfg.Mode, GuestOS: q.cfg.GuestOS, State: q.state,
		Assigned: q.assigned, Startup: q.startup, ObservedAt: q.observedAt,
		UsedKnown: q.usedKnown, Used: q.used, Available: q.available,
		Base: q.cfg.Base, Margin: q.margin, Leased: q.leased, EffBase: q.effBase,
		Floor: q.floor, Headroom: q.headroom, Committed: q.committed,
	}
	if q.dynamic != nil {
		d := *q.dynamic
		v.Dynamic = &d
	}
	return v
}

// calc is the ledger's quantities at one instant.
type calc struct {
	now       time.Time
	frozen    bool
	capacity  int64
	committed int64
	inflight  int64
	debt      int64
	pool      int64
	names     []VMName
	vms       map[VMName]*vmQ
}

type vmAgg struct {
	leased int64
	target int64
	busy   bool
}

func (l *Ledger) fresh(now, at time.Time) bool {
	return !at.IsZero() && now.Sub(at) <= l.cfg.StaleAfter
}

// compute derives every quantity from the current state.
func (l *Ledger) compute() *calc {
	now := l.clk.Now()
	c := &calc{now: now, vms: map[VMName]*vmQ{}}
	c.frozen = l.agentState(l.cfg.HostAgent) != AgentReady || !l.host.seen || !l.fresh(now, l.host.observedAt)

	agg := l.aggregateLeases()
	c.names = l.vmNames()
	for _, name := range c.names {
		a := agg[name]
		if a == nil {
			a = &vmAgg{}
		}
		q := l.vmQuantities(name, now, a)
		c.vms[name] = q
		c.committed += q.committed
		c.inflight += q.inflight
	}
	for _, d := range l.debts {
		c.debt += d.bytes
	}
	c.capacity = l.cfg.Capacity
	if c.capacity <= 0 {
		c.capacity = l.host.total
	}
	c.pool = poolOf(c.capacity, l.cfg.Reserve, c.committed, l.host.free-c.inflight-c.debt)
	return c
}

// aggregateLeases sums, per VM, the bytes of leases holding memory and the
// highest target of in-flight growing reservations.
func (l *Ledger) aggregateLeases() map[VMName]*vmAgg {
	agg := map[VMName]*vmAgg{}
	for _, le := range l.leases {
		if !le.phase.holdsMemory() || le.req.VM == "" {
			continue
		}
		a := agg[le.req.VM]
		if a == nil {
			a = &vmAgg{}
			agg[le.req.VM] = a
		}
		a.leased += le.req.Bytes
		if le.phase.inFlight() && le.res.Delta > 0 {
			a.target = max(a.target, le.res.Target)
			a.busy = true
		}
	}
	return agg
}

// vmNames returns the configured VMs plus the unmanaged VMs the host
// reports, sorted by name.
func (l *Ledger) vmNames() []VMName {
	var names []VMName
	for name := range l.vmCfg {
		names = append(names, name)
	}
	for name, v := range l.vms {
		if _, ok := l.vmCfg[name]; !ok && v.hostSeen {
			names = append(names, name)
		}
	}
	slices.SortFunc(names, func(a, b VMName) int { return strings.Compare(string(a), string(b)) })
	return names
}

func (l *Ledger) vmQuantities(name VMName, now time.Time, a *vmAgg) *vmQ {
	v := l.vms[name]
	if v == nil {
		v = &vmState{}
	}
	cfg, managed := l.vmCfg[name]
	q := &vmQ{managed: managed, cfg: cfg, leased: a.leased, busy: a.busy}
	if v.hostSeen {
		q.assigned, q.startup, q.observedAt = v.assigned, v.startup, v.observedAt
		q.dynamic = v.dynamic
		q.assignedFresh = l.fresh(now, v.observedAt)
	}
	if !managed {
		q.state = VMUnmanaged
		q.committed = committedOf(committedInput{
			assigned: q.assigned, dynamic: q.dynamic != nil, dynamicMax: dynMax(q.dynamic),
		})
		return q
	}
	if g := v.guest; g != nil && l.fresh(now, g.ObservedAt) {
		q.usedKnown = true
		q.used = max(g.Total-g.Available, 0)
		q.available = g.Available
	}
	q.margin = l.cfg.margin(cfg)
	q.effBase = effBaseOf(cfg.Base, q.used, q.usedKnown, q.leased)
	q.floor = floorOf(q.effBase, q.leased, q.margin)

	target := a.target
	if v.holdTarget > 0 {
		target = max(target, v.holdTarget)
		q.busy = true
	}
	forHeadroom := q.assigned
	if v.shrink != nil {
		forHeadroom = min(forHeadroom, v.shrink.Target)
		q.busy = true
	}
	q.headroom = forHeadroom - q.floor
	if target > q.assigned {
		q.inflight = target - q.assigned
	}
	q.committed = committedOf(committedInput{
		managed: true, mode: cfg.Mode, dmAcct: l.cfg.DMAccounting,
		assigned: q.assigned, floor: q.floor,
		dynamic: q.dynamic != nil, dynamicMax: dynMax(q.dynamic),
		inflightTarget: target,
	})

	switch {
	case !q.assignedFresh:
		q.state = VMMissing
	case !q.usedKnown || l.agentState(cfg.GuestAgent) != AgentReady:
		q.state = VMUnknown
	default:
		q.state = VMReady
	}
	return q
}

func dynMax(d *DynamicMemory) int64 {
	if d == nil {
		return 0
	}
	return d.Maximum
}
