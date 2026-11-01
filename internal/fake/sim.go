package fake

import (
	"context"
	"crypto/x509"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
)

// Health service names (common.proto).
const (
	HostAgentService  = "hvmc.agent.v1.HostAgent"
	GuestAgentService = "hvmc.agent.v1.GuestAgent"
)

// DefaultAgentVersion is the AgentInfo.version the fakes report unless
// WithAgentVersion is given.
const DefaultAgentVersion = "v0.0.0-fake"

// Sim is a simulated Hyper-V host with its VMs and guest agents. Its gRPC
// servers (ServeHost, ServeGuest) implement the agent wire API on top of one
// shared, mutex-guarded Scenario, so a grow on the host shows up in the
// linked guest and in host free memory. Every method is safe for concurrent
// use, including while servers run.
type Sim struct {
	clock   Clock
	digest  DigestFunc
	version string

	writeSem chan struct{} // the agent serializes Hyper-V writes

	mu       sync.Mutex
	sc       *Scenario
	changed  chan struct{} // closed and replaced on every state change
	inflight Bytes         // memory taken by grows still in progress
	ca       *x509.CertPool
	frozen   map[string]*frozenView
	lags     map[string]*guestLag
	targets  map[string]Bytes // last assignment-derived total per linked guest
	certs    map[string]*certState
	health   map[string]*health.Server
	calls    map[string]int
}

// frozenView is what a stale agent keeps serving.
type frozenView struct {
	stale StaleSpec
	host  *agentv1.Host
	vms   *agentv1.VMSnapshot
	mem   *agentv1.GuestMemory
}

// guestLag is a pending guest total update.
type guestLag struct {
	target Bytes
	timer  Timer
}

// Option configures a Sim.
type Option func(*Sim)

// WithClock sets the clock; default RealClock.
func WithClock(c Clock) Option { return func(s *Sim) { s.clock = c } }

// WithPolicyDigest sets the policy digest function; default
// PlaceholderDigest.
func WithPolicyDigest(f DigestFunc) Option { return func(s *Sim) { s.digest = f } }

// WithAgentVersion sets the version every fake agent reports.
func WithAgentVersion(v string) Option { return func(s *Sim) { s.version = v } }

// WithCertificateAuthority makes InstallCertificate verify chains against
// pool, as a real agent verifies against the hvmc CA. Without it the chain
// is not verified (parsing, serving usage and key match still are).
func WithCertificateAuthority(pool *x509.CertPool) Option {
	return func(s *Sim) { s.ca = pool }
}

// New returns a Sim in the state sc describes. sc is copied; later changes
// to it have no effect.
func New(sc *Scenario, opts ...Option) (*Sim, error) {
	s := &Sim{
		clock:    RealClock(),
		digest:   PlaceholderDigest,
		version:  DefaultAgentVersion,
		writeSem: make(chan struct{}, 1),
		sc:       &Scenario{},
		changed:  make(chan struct{}),
		frozen:   map[string]*frozenView{},
		lags:     map[string]*guestLag{},
		targets:  map[string]Bytes{},
		certs:    map[string]*certState{},
		health:   map[string]*health.Server{},
		calls:    map[string]int{},
	}
	for _, o := range opts {
		o(s)
	}
	if err := s.Apply(sc); err != nil {
		return nil, err
	}
	return s, nil
}

// Clock returns the Sim's clock.
func (s *Sim) Clock() Clock { return s.clock }

// replaceLocked validates next and makes it the current state, applying the
// runtime side effects of the change.
func (s *Sim) replaceLocked(next *Scenario) error {
	next.setDefaults()
	if err := next.Validate(); err != nil {
		return err
	}
	prev := s.sc
	s.sc = next
	s.syncFrozenLocked(prev)
	s.changedLocked()
	return nil
}

// changedLocked propagates a state change: guest totals follow their VMs,
// health follows the behaviours, watchers wake up.
func (s *Sim) changedLocked() {
	s.reconcileGuestsLocked()
	s.syncHealthLocked()
	close(s.changed)
	s.changed = make(chan struct{})
}

// agentLocked returns the behaviours of the named agent and whether it is
// the host agent.
func (s *Sim) agentLocked(name string) (b *AgentBehaviour, isHost bool, ok bool) {
	if name == s.sc.Host.Name {
		return &s.sc.Host.AgentBehaviour, true, true
	}
	if g := s.sc.guest(name); g != nil {
		return &g.AgentBehaviour, false, true
	}
	return nil, false, false
}

func (s *Sim) agentNamesLocked() []string {
	names := []string{s.sc.Host.Name}
	for _, g := range s.sc.Guests {
		names = append(names, g.Name)
	}
	return names
}

// healthFor returns the health server of an agent, creating it on first use.
// It is kept across scenario changes so running servers stay attached.
func (s *Sim) healthFor(name string) *health.Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthLocked(name)
}

func (s *Sim) healthLocked(name string) *health.Server {
	h, ok := s.health[name]
	if !ok {
		h = health.NewServer()
		s.health[name] = h
		s.syncHealthLocked()
	}
	return h
}

// syncHealthLocked derives every agent's serving status from the scenario
// (DESIGN §4.8 readiness): the host is ready unless NotServing or vmms is
// down; a guest unless NotServing, its memory source is down or its VM is
// not running. Agents no longer in the scenario report NOT_SERVING.
func (s *Sim) syncHealthLocked() {
	for name, h := range s.health {
		serving, service := false, GuestAgentService
		switch {
		case name == s.sc.Host.Name:
			service = HostAgentService
			serving = !s.sc.Host.NotServing && !s.sc.Host.VMMSDown
		case s.sc.guest(name) != nil:
			g := s.sc.guest(name)
			serving = !g.NotServing && !g.SourceDown && s.guestVMRunningLocked(g)
		}
		st := healthpb.HealthCheckResponse_NOT_SERVING
		if serving {
			st = healthpb.HealthCheckResponse_SERVING
		}
		h.SetServingStatus("", st)
		h.SetServingStatus(service, st)
	}
}

func (s *Sim) guestVMRunningLocked(g *GuestSpec) bool {
	if g.VM == "" {
		return true
	}
	v := s.sc.vm(g.VM)
	return v != nil && coarseState(v.State) == agentv1.VMState_VM_STATE_RUNNING
}

// reconcileGuestsLocked makes linked guests follow their VM's assignment:
// a lower total shows immediately, a higher one after TotalLag. Only changes
// of the assignment are followed, so an explicitly set Total stays until
// the VM is resized.
func (s *Sim) reconcileGuestsLocked() {
	live := map[string]bool{}
	for i := range s.sc.Guests {
		g := &s.sc.Guests[i]
		v := s.sc.vm(g.VM)
		if g.VM == "" || v == nil {
			continue
		}
		live[g.Name] = true
		s.followAssignmentLocked(g, max(*v.Assigned-g.Offset, 0))
	}
	s.forgetGuestsLocked(live)
}

// followAssignmentLocked moves a linked guest's total towards target when
// target changed since the last reconcile.
func (s *Sim) followAssignmentLocked(g *GuestSpec, target Bytes) {
	last, seen := s.targets[g.Name]
	s.targets[g.Name] = target
	if !seen || last == target {
		return
	}
	if l := s.lags[g.Name]; l != nil {
		l.timer.Stop()
		delete(s.lags, g.Name)
	}
	if target <= g.Total || g.TotalLag <= 0 {
		g.Total = target
		return
	}
	s.lags[g.Name] = s.lagLocked(g.Name, target, g.TotalLag.D())
}

// lagLocked schedules a guest's total to become target after d.
func (s *Sim) lagLocked(name string, target Bytes, d time.Duration) *guestLag {
	l := &guestLag{target: target}
	l.timer = s.clock.AfterFunc(d, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.lags[name] != l {
			return
		}
		delete(s.lags, name)
		if g := s.sc.guest(name); g != nil {
			g.Total = l.target
			s.changedLocked()
		}
	})
	return l
}

// forgetGuestsLocked drops lag and target state of guests no longer linked.
func (s *Sim) forgetGuestsLocked(live map[string]bool) {
	for name, l := range s.lags {
		if !live[name] {
			l.timer.Stop()
			delete(s.lags, name)
		}
	}
	for name := range s.targets {
		if !live[name] {
			delete(s.targets, name)
		}
	}
}

// syncFrozenLocked freezes agents whose Stale was set or changed and thaws
// those whose Stale was cleared.
func (s *Sim) syncFrozenLocked(prev *Scenario) {
	prevStale := map[string]*StaleSpec{prev.Host.Name: prev.Host.Stale}
	for _, g := range prev.Guests {
		prevStale[g.Name] = g.Stale
	}
	for name := range s.frozen {
		if b, _, ok := s.agentLocked(name); !ok || b.Stale == nil {
			delete(s.frozen, name)
		}
	}
	for _, name := range s.agentNamesLocked() {
		b, isHost, _ := s.agentLocked(name)
		if b.Stale == nil {
			continue
		}
		if f := s.frozen[name]; f != nil && f.stale == *b.Stale && prevStale[name] != nil {
			continue
		}
		at := s.clock.Now().Add(-b.Stale.Age.D())
		f := &frozenView{stale: *b.Stale}
		if isHost {
			f.host = s.liveHostLocked(at)
			f.vms = s.liveSnapshotLocked(at)
		} else {
			f.mem = s.liveMemoryLocked(s.sc.guest(name), at)
		}
		s.frozen[name] = f
	}
}

// enter is the common prologue of every RPC: it counts the call, fails
// with UNAVAILABLE for an agent no longer in the scenario and blocks a
// hanging method until ctx ends (or, for host Hyper-V calls, until the
// cmdlet timeout).
func (s *Sim) enter(ctx context.Context, agent, method string, cmdlet bool) error {
	s.mu.Lock()
	s.calls[agent+"/"+method]++
	b, isHost, ok := s.agentLocked(agent)
	if !ok {
		s.mu.Unlock()
		return status.Errorf(codes.Unavailable, "agent %s is gone", agent)
	}
	hangs := slices.Contains(b.Hang, method) || slices.Contains(b.Hang, "*")
	timeout := time.Duration(-1)
	if isHost && cmdlet {
		timeout = s.sc.Host.CmdletTimeout.D()
	}
	s.mu.Unlock()
	if !hangs {
		if err := ctx.Err(); err != nil {
			return status.FromContextError(err).Err()
		}
		return nil
	}
	return s.wait(ctx, nil, timeout)
}

// wait blocks until done is closed (nil), ctx ends (its error) or the
// timeout passes on the clock (DEADLINE_EXCEEDED); a negative timeout never
// passes.
func (s *Sim) wait(ctx context.Context, done <-chan struct{}, timeout time.Duration) error {
	var timeoutC <-chan time.Time
	if timeout >= 0 {
		t := s.clock.NewTimer(timeout)
		defer t.Stop()
		timeoutC = t.C()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	case <-timeoutC:
		return status.Errorf(codes.DeadlineExceeded, "cmdlet did not finish within %s", timeout)
	}
}

// statusWithResult attaches a ResizeResult to an error status.
func statusWithResult(c codes.Code, before, after *agentv1.VM, format string, args ...any) error {
	st := status.Newf(c, format, args...)
	if d, err := st.WithDetails(&agentv1.ResizeResult{Before: before, After: after}); err == nil {
		st = d
	}
	return st.Err()
}

// coarseState maps Hyper-V's state string to VMState.
func coarseState(s HyperVState) agentv1.VMState {
	switch s {
	case "Running":
		return agentv1.VMState_VM_STATE_RUNNING
	case "Off":
		return agentv1.VMState_VM_STATE_OFF
	case "Starting":
		return agentv1.VMState_VM_STATE_STARTING
	case "Stopping", "ForceShutdown":
		return agentv1.VMState_VM_STATE_STOPPING
	case "Saving", "FastSaving":
		return agentv1.VMState_VM_STATE_SAVING
	case "Saved", "FastSaved", "Hibernated":
		return agentv1.VMState_VM_STATE_SAVED
	case "Pausing":
		return agentv1.VMState_VM_STATE_PAUSING
	case "Paused":
		return agentv1.VMState_VM_STATE_PAUSED
	case "Resuming":
		return agentv1.VMState_VM_STATE_RESUMING
	case "Reset", "ForceReboot":
		return agentv1.VMState_VM_STATE_RESETTING
	default:
		return agentv1.VMState_VM_STATE_OTHER
	}
}

func knownState(s HyperVState) bool {
	return s == "Other" || s == "ComponentServicing" || strings.HasSuffix(string(s), "Critical") ||
		coarseState(s) != agentv1.VMState_VM_STATE_OTHER
}

// holdsMemory reports whether a VM in Hyper-V state s has memory assigned.
func holdsMemory(s HyperVState) bool {
	switch strings.TrimSuffix(string(s), "Critical") {
	case "Off", "Saved", "FastSaved", "Hibernated":
		return false
	default:
		return true
	}
}

func alignUp(b Bytes) Bytes {
	return (b + alignment - 1) / alignment * alignment
}

func (s *Sim) freeLocked() Bytes {
	free := s.sc.Host.Total - s.sc.Host.Overhead - s.inflight
	for _, v := range s.sc.VMs {
		free -= *v.Assigned
	}
	return max(free, 0)
}

func (s *Sim) digestLocked(p *agentv1.Policy) string {
	d := s.digest(p)
	if s.sc.Host.PolicyDigestSkew {
		d = "skewed-" + d
	}
	return d
}

func (s *Sim) hostAgentInfo() *agentv1.AgentInfo {
	return &agentv1.AgentInfo{
		Version:         s.version,
		Role:            agentv1.AgentRole_AGENT_ROLE_HOST,
		OperatingSystem: agentv1.OperatingSystem_OPERATING_SYSTEM_WINDOWS,
	}
}

func (s *Sim) liveHostLocked(at time.Time) *agentv1.Host {
	h := &agentv1.Host{
		Name:         s.sc.Host.Name,
		TotalBytes:   int64(s.sc.Host.Total),
		FreeBytes:    int64(s.freeLocked()),
		Agent:        s.hostAgentInfo(),
		PolicyDigest: s.digestLocked(toProtoPolicy(s.sc.Policy)),
		ObservedAt:   timestamppb.New(at),
	}
	if s.sc.PendingPolicy != nil {
		h.PendingPolicyDigest = s.digestLocked(toProtoPolicy(s.sc.PendingPolicy))
	}
	return h
}

// hostLocked returns the host observation, frozen if the host is stale.
// Policy digests are agent state, not observations, and are always current.
func (s *Sim) hostLocked() *agentv1.Host {
	live := s.liveHostLocked(s.clock.Now())
	f := s.frozen[s.sc.Host.Name]
	if f == nil {
		return live
	}
	h := proto.CloneOf(f.host)
	h.PolicyDigest, h.PendingPolicyDigest = live.PolicyDigest, live.PendingPolicyDigest
	return h
}

func vmMessage(v *VMSpec, at time.Time) *agentv1.VM {
	m := &agentv1.VM{
		Name:          v.Name,
		Id:            v.ID,
		State:         coarseState(v.State),
		HypervState:   string(v.State),
		AssignedBytes: int64(*v.Assigned),
		StartupBytes:  int64(v.Startup),
		DynamicMemory: &agentv1.DynamicMemory{},
		ObservedAt:    timestamppb.New(at),
	}
	if dm := v.DynamicMemory; dm != nil {
		m.DynamicMemory = &agentv1.DynamicMemory{
			Enabled:       true,
			MinimumBytes:  int64(dm.Minimum),
			MaximumBytes:  int64(dm.Maximum),
			BufferPercent: dm.Buffer,
			Priority:      dm.Priority,
		}
	}
	return m
}

func (s *Sim) liveSnapshotLocked(at time.Time) *agentv1.VMSnapshot {
	snap := &agentv1.VMSnapshot{ObservedAt: timestamppb.New(at)}
	for i := range s.sc.VMs {
		snap.Vms = append(snap.Vms, vmMessage(&s.sc.VMs[i], at))
	}
	slices.SortFunc(snap.Vms, func(a, b *agentv1.VM) int { return strings.Compare(a.GetName(), b.GetName()) })
	return snap
}

// snapshotLocked returns the VM snapshot, frozen if the host is stale, and
// whether it is frozen.
func (s *Sim) snapshotLocked() (*agentv1.VMSnapshot, bool) {
	if f := s.frozen[s.sc.Host.Name]; f != nil {
		return proto.CloneOf(f.vms), true
	}
	return s.liveSnapshotLocked(s.clock.Now()), false
}

func (s *Sim) liveMemoryLocked(g *GuestSpec, at time.Time) *agentv1.GuestMemory {
	m := &agentv1.GuestMemory{
		TotalBytes:     int64(g.Total),
		AvailableBytes: int64(max(g.Total-g.Used, 0)),
		ObservedAt:     timestamppb.New(at),
	}
	if g.OS == OSWindows {
		committed := g.Committed
		if committed == 0 {
			committed = g.Used
		}
		limit := int64(g.Total + g.PageFile)
		avail := max(limit-int64(committed), 0)
		m.CommitLimitBytes, m.CommitAvailableBytes = &limit, &avail
	}
	return m
}

// memoryLocked returns the guest's reading, frozen if the guest is stale.
func (s *Sim) memoryLocked(g *GuestSpec) *agentv1.GuestMemory {
	if f := s.frozen[g.Name]; f != nil {
		return proto.CloneOf(f.mem)
	}
	return s.liveMemoryLocked(g, s.clock.Now())
}

func errNoAgent(name string) error {
	return fmt.Errorf("fake: no agent named %q", name)
}
