package ledger

import (
	"errors"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

// testConfig has one VM per mode plus a policy-bounded static VM.
func testConfig() Config {
	cfg := DefaultConfig()
	cfg.VMs = []VMConfig{
		{Name: "vm-a", GuestOS: GuestWindows, Mode: ModeStatic, Base: 4 * Gi},
		{Name: "vm-b", GuestOS: GuestWindows, Mode: ModeStatic, Base: 4 * Gi, Policy: &Policy{Min: 6 * Gi, Max: 24 * Gi}},
		{Name: "linux-node-1", Node: "linux-node-1", GuestOS: GuestLinux, Mode: ModeFixed, Base: 3 * Gi},
		{Name: "vm-dm", GuestOS: GuestWindows, Mode: ModeDynamic, Base: 2 * Gi},
	}
	return cfg
}

type fixture struct {
	t   testing.TB
	clk *FakeClock
	cfg Config
	l   *Ledger
}

func newFixture(t testing.TB, cfg Config) *fixture {
	clk := NewFakeClock(t0)
	return &fixture{t: t, clk: clk, cfg: cfg, l: New(cfg, clk)}
}

// standard returns the fixture most tests use:
//
//	host 64Gi total, 12Gi free, reserve 4Gi
//	vm-a   static  assigned 8Gi, used 2Gi → floor 6Gi, headroom 2Gi, committed 8Gi
//	vm-b   static  assigned 8Gi, used 2Gi → floor 6Gi, headroom 2Gi, committed 8Gi
//	linux  fixed   assigned 16Gi, used 5Gi → floor 7Gi, headroom 9Gi, committed 16Gi
//	vm-dm  dynamic assigned 4Gi, max 12Gi, used 1Gi → floor 4Gi, headroom 0, committed 12Gi
//	pool = min(64−4−44, 12−4) = 8Gi
func standard(t testing.TB) *fixture {
	f := newFixture(t, testConfig())
	f.host(12 * Gi)
	f.vm("vm-a", 8*Gi, 2*Gi)
	f.vm("vm-b", 8*Gi, 2*Gi)
	f.vm("linux-node-1", 16*Gi, 5*Gi)
	f.dm("vm-dm", 4*Gi, 12*Gi, 1*Gi)
	f.ready()
	return f
}

func (f *fixture) now() time.Time { return f.clk.Now() }

// host observes the host: 64Gi total, free as given.
func (f *fixture) host(free int64) {
	f.l.Observe(Observation{Host: &HostObservation{Total: 64 * Gi, Free: free, ObservedAt: f.now()}})
}

// vm observes a static VM; used < 0 means no guest data.
func (f *fixture) vm(name VMName, assigned, used int64) {
	f.l.Observe(Observation{VMs: []VMObservation{{Name: name, Assigned: assigned, Startup: assigned, ObservedAt: f.now()}}})
	f.guest(name, assigned, used)
}

func (f *fixture) dm(name VMName, assigned, maximum, used int64) {
	f.l.Observe(Observation{VMs: []VMObservation{{
		Name: name, Assigned: assigned, Startup: assigned, ObservedAt: f.now(),
		Dynamic: &DynamicMemory{Minimum: 1 * Gi, Maximum: maximum},
	}}})
	f.guest(name, assigned, used)
}

func (f *fixture) guest(name VMName, total, used int64) {
	if used < 0 {
		return
	}
	f.l.Observe(Observation{Guest: &GuestObservation{VM: name, Total: total, Available: total - used, ObservedAt: f.now()}})
}

func (f *fixture) ready() {
	f.l.SetAgentState(f.cfg.HostAgent, AgentReady)
	if f.cfg.HostAgent == "" {
		f.l.SetAgentState(DefaultHostAgent, AgentReady)
	}
	for _, vm := range f.cfg.VMs {
		a := vm.GuestAgent
		if a == "" {
			a = AgentName(vm.Name)
		}
		f.l.SetAgentState(a, AgentReady)
	}
}

func (f *fixture) enqueue(r Request) Position {
	f.t.Helper()
	pos, err := f.l.Enqueue(r)
	if err != nil {
		f.t.Fatalf("Enqueue(%s): %v", r.ID, err)
	}
	return pos
}

func (f *fixture) reserve(id RequestID, vm VMName) Reservation {
	f.t.Helper()
	res, err := f.l.Reserve(id, vm)
	if err != nil {
		f.t.Fatalf("Reserve(%s, %s): %v", id, vm, err)
	}
	return res
}

// grant runs the whole grow path with an outcome that matches the target.
func (f *fixture) grant(id RequestID, vm VMName) {
	f.t.Helper()
	res := f.reserve(id, vm)
	out := ResizeOutcome{}
	if res.Delta > 0 {
		if err := f.l.MarkResizing(res); err != nil {
			f.t.Fatalf("MarkResizing(%s): %v", id, err)
		}
		out = ResizeOutcome{AssignedBefore: res.AssignedBefore, AssignedAfter: res.Target, StartupAfter: res.Target, ObservedAt: f.now()}
	}
	if err := f.l.Commit(res, out); err != nil {
		f.t.Fatalf("Commit(%s): %v", id, err)
	}
}

func (f *fixture) vmView(name VMName) VMView {
	f.t.Helper()
	for _, v := range f.l.Snapshot().VMs {
		if v.Name == name {
			return v
		}
	}
	f.t.Fatalf("no VM %s in snapshot", name)
	return VMView{}
}

func (f *fixture) phase(id RequestID) Phase {
	lv, ok := f.l.Lease(id)
	if !ok {
		return ""
	}
	return lv.Phase
}

func ext(id RequestID, vm VMName, bytes int64) Request {
	return Request{ID: id, Kind: KindExternal, VM: vm, Bytes: bytes}
}

func pod(id RequestID, bytes int64, cands ...VMName) Request {
	return Request{ID: id, Kind: KindPod, Bytes: bytes, Candidates: cands}
}

func reasonOf(err error) Reason {
	var nf *NotFitError
	if errors.As(err, &nf) {
		return nf.Fit.Reason
	}
	return ""
}
