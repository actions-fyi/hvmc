package fake

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
)

// hostServer implements agentv1.HostAgentServer on a Sim.
//
// Write checks run in this order, each failing with the code of the error
// model (common.proto): request shape (INVALID_ARGUMENT), hang, vmms
// (UNAVAILABLE), VM lookup (NOT_FOUND), policy membership
// (PERMISSION_DENIED), fixed VM and VM mode/state (FAILED_PRECONDITION), DM
// direction (FAILED_PRECONDITION), policy bounds (PERMISSION_DENIED), host
// free memory (RESOURCE_EXHAUSTED), guest refusal (ABORTED).
type hostServer struct {
	agentv1.UnimplementedHostAgentServer
	sim  *Sim
	name string
}

func (h *hostServer) unavailableLocked() error {
	if h.sim.sc.Host.VMMSDown {
		return status.Error(codes.Unavailable, "Hyper-V Virtual Machine Management service (vmms) is not running")
	}
	return nil
}

// GetHost implements agentv1.HostAgentServer.
func (h *hostServer) GetHost(ctx context.Context, _ *agentv1.GetHostRequest) (*agentv1.GetHostResponse, error) {
	s := h.sim
	if err := s.enter(ctx, h.name, "GetHost", true); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := h.unavailableLocked(); err != nil {
		return nil, err
	}
	return &agentv1.GetHostResponse{Host: s.hostLocked()}, nil
}

// ListVMs implements agentv1.HostAgentServer.
func (h *hostServer) ListVMs(ctx context.Context, _ *agentv1.ListVMsRequest) (*agentv1.ListVMsResponse, error) {
	s := h.sim
	if err := s.enter(ctx, h.name, "ListVMs", true); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := h.unavailableLocked(); err != nil {
		return nil, err
	}
	snap, _ := s.snapshotLocked()
	return &agentv1.ListVMsResponse{Snapshot: snap}, nil
}

// WatchVMs implements agentv1.HostAgentServer: a snapshot on connect, then
// one whenever a VM changes (observed_at alone does not count) or the host
// turns stale or fresh again. It ends with UNAVAILABLE when vmms goes down.
func (h *hostServer) WatchVMs(_ *agentv1.WatchVMsRequest, stream grpc.ServerStreamingServer[agentv1.WatchVMsResponse]) error {
	s := h.sim
	ctx := stream.Context()
	if err := s.enter(ctx, h.name, "WatchVMs", false); err != nil {
		return err
	}
	var last *agentv1.VMSnapshot
	lastFrozen := false
	for {
		s.mu.Lock()
		if err := h.unavailableLocked(); err != nil {
			s.mu.Unlock()
			return err
		}
		if s.sc.Host.Name != h.name {
			s.mu.Unlock()
			return status.Errorf(codes.Unavailable, "agent %s is gone", h.name)
		}
		snap, frozen := s.snapshotLocked()
		changed := s.changed
		s.mu.Unlock()

		if last == nil || frozen != lastFrozen || !sameVMs(last, snap) {
			if err := stream.Send(&agentv1.WatchVMsResponse{Snapshot: snap}); err != nil {
				return err
			}
			last, lastFrozen = snap, frozen
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
	}
}

// sameVMs compares two snapshots ignoring observation times.
func sameVMs(a, b *agentv1.VMSnapshot) bool {
	strip := func(s *agentv1.VMSnapshot) *agentv1.VMSnapshot {
		c := proto.CloneOf(s)
		c.ObservedAt = nil
		for _, v := range c.GetVms() {
			v.ObservedAt = nil
		}
		return c
	}
	return proto.Equal(strip(a), strip(b))
}

func checkAligned(field string, v int64) error {
	if v <= 0 || Bytes(v)%alignment != 0 {
		return status.Errorf(codes.InvalidArgument, "%s must be positive and a multiple of 2 MiB, got %d", field, v)
	}
	return nil
}

// acquireWrite serializes writes like the agent's single PowerShell worker.
func (s *Sim) acquireWrite(ctx context.Context) error {
	select {
	case s.writeSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
}

func (s *Sim) releaseWrite() { <-s.writeSem }

// writableLocked looks up a VM for a write and applies the policy checks
// shared by both write RPCs.
func (h *hostServer) writableLocked(name string) (*VMSpec, VMPolicySpec, error) {
	s := h.sim
	if err := h.unavailableLocked(); err != nil {
		return nil, VMPolicySpec{}, err
	}
	v := s.sc.vm(name)
	if v == nil {
		return nil, VMPolicySpec{}, status.Errorf(codes.NotFound, "VM %q does not exist on host %s", name, h.name)
	}
	rule, ok := s.sc.Policy.rule(name)
	if !ok {
		return nil, VMPolicySpec{}, status.Errorf(codes.PermissionDenied, "VM %q is not in the active policy", name)
	}
	if rule.Fixed {
		return nil, VMPolicySpec{}, status.Errorf(codes.FailedPrecondition, "VM %q is fixed (read-only to the agent)", name)
	}
	return v, rule, nil
}

// SetStartup implements agentv1.HostAgentServer.
func (h *hostServer) SetStartup(ctx context.Context, req *agentv1.SetStartupRequest) (*agentv1.SetStartupResponse, error) {
	s := h.sim
	if err := validateStartupRequest(req); err != nil {
		return nil, err
	}
	if err := s.enter(ctx, h.name, "SetStartup", true); err != nil {
		return nil, err
	}
	if err := s.acquireWrite(ctx); err != nil {
		return nil, err
	}
	result, grow, err := h.applyStartup(req.GetVmName(), Bytes(req.GetStartupBytes()))
	if grow != nil {
		// growLater releases the write lock once the grow has landed.
		return h.awaitGrow(ctx, grow)
	}
	s.releaseWrite()
	if err != nil {
		return nil, err
	}
	return &agentv1.SetStartupResponse{Result: result}, nil
}

func validateStartupRequest(req *agentv1.SetStartupRequest) error {
	if req.GetVmName() == "" {
		return status.Error(codes.InvalidArgument, "vm_name is required")
	}
	return checkAligned("startup_bytes", req.GetStartupBytes())
}

// pendingGrow is a grow that lands after GrowLatency.
type pendingGrow struct {
	name    string
	before  *agentv1.VM
	done    <-chan struct{}
	latency time.Duration
	timeout time.Duration
}

// applyStartup checks and applies a SetStartup under the lock. It returns
// the result of an immediate change, or a pendingGrow for a grow that takes
// GrowLatency.
func (h *hostServer) applyStartup(name string, n Bytes) (*agentv1.ResizeResult, *pendingGrow, error) {
	s := h.sim
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := h.startupTargetLocked(name, n)
	if err != nil {
		return nil, nil, err
	}
	before := vmMessage(v, s.clock.Now())
	cur := *v.Assigned
	switch {
	case coarseState(v.State) == agentv1.VMState_VM_STATE_OFF || n == cur:
		// Configuration only (VM off), or the assignment already matches:
		// this also repairs a drifted MemoryStartup.
		v.Startup = n
	case n < cur:
		if err := h.shrinkLocked(v, n, before); err != nil {
			return nil, nil, err
		}
	default:
		if delta := n - cur; delta > s.freeLocked() {
			return nil, nil, statusWithResult(codes.ResourceExhausted, before, before,
				"host has %s free, grow of VM %q needs %s", s.freeLocked(), name, delta)
		}
		if latency := s.sc.Host.GrowLatency.D(); latency > 0 {
			return nil, &pendingGrow{
				name: name, before: before, done: h.growLater(name, n, n-cur),
				latency: latency, timeout: s.sc.Host.CmdletTimeout.D(),
			}, nil
		}
		*v.Assigned, v.Startup = n, n
	}
	after := vmMessage(v, s.clock.Now())
	s.changedLocked()
	return &agentv1.ResizeResult{Before: before, After: after}, nil, nil
}

// startupTargetLocked returns the VM a SetStartup may change, or the error
// that refuses it.
func (h *hostServer) startupTargetLocked(name string, n Bytes) (*VMSpec, error) {
	v, rule, err := h.writableLocked(name)
	if err != nil {
		return nil, err
	}
	if v.DynamicMemory != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "VM %q uses Dynamic Memory; use SetDynamicLimits", name)
	}
	if state := coarseState(v.State); state != agentv1.VMState_VM_STATE_RUNNING && state != agentv1.VMState_VM_STATE_OFF {
		return nil, status.Errorf(codes.FailedPrecondition, "VM %q is %s; memory can only be changed while running or off", name, v.State)
	}
	if n < rule.Min || n > rule.Max {
		return nil, status.Errorf(codes.PermissionDenied, "startup %s for VM %q is outside the policy's [%s, %s]", n, name, rule.Min, rule.Max)
	}
	return v, nil
}

// shrinkLocked shrinks a running VM to n. Below the VM's shrink floor the
// guest gives up memory only down to the floor: the partial (or no) change
// is applied and ABORTED returned, with MemoryStartup at the requested value
// if the VM drifts.
func (h *hostServer) shrinkLocked(v *VMSpec, n Bytes, before *agentv1.VM) error {
	s := h.sim
	f := v.ShrinkFloor
	if f == nil || n >= *f {
		*v.Assigned, v.Startup = n, n
		return nil
	}
	got := min(*v.Assigned, alignUp(*f))
	*v.Assigned = got
	v.Startup = got
	if v.StartupDrift {
		v.Startup = n
	}
	after := vmMessage(v, s.clock.Now())
	s.changedLocked()
	return statusWithResult(codes.Aborted, before, after,
		"guest released memory only down to %s of the requested %s", got, n)
}

// awaitGrow waits for an in-flight grow. A grow slower than the cmdlet
// timeout still completes in the background, so the caller gets
// DEADLINE_EXCEEDED and an unknown outcome.
func (h *hostServer) awaitGrow(ctx context.Context, g *pendingGrow) (*agentv1.SetStartupResponse, error) {
	s := h.sim
	if g.latency > g.timeout {
		return nil, s.wait(ctx, nil, g.timeout)
	}
	if err := s.wait(ctx, g.done, -1); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	after := g.before
	if v := s.sc.vm(g.name); v != nil {
		after = vmMessage(v, s.clock.Now())
	}
	return &agentv1.SetStartupResponse{Result: &agentv1.ResizeResult{Before: g.before, After: after}}, nil
}

// growLater reserves delta of host memory for an in-flight grow and applies
// the grow once GrowLatency has passed on the clock. It releases the write
// lock when done. Called with s.mu held.
func (h *hostServer) growLater(name string, n, delta Bytes) <-chan struct{} {
	s := h.sim
	done := make(chan struct{})
	s.inflight += delta
	s.clock.AfterFunc(s.sc.Host.GrowLatency.D(), func() {
		s.mu.Lock()
		s.inflight -= delta
		if v := s.sc.vm(name); v != nil && v.DynamicMemory == nil {
			v.Startup = n
			if coarseState(v.State) == agentv1.VMState_VM_STATE_RUNNING {
				*v.Assigned = n
			}
		}
		s.changedLocked()
		s.mu.Unlock()
		s.releaseWrite()
		close(done)
	})
	return done
}

// SetDynamicLimits implements agentv1.HostAgentServer.
func (h *hostServer) SetDynamicLimits(ctx context.Context, req *agentv1.SetDynamicLimitsRequest) (*agentv1.SetDynamicLimitsResponse, error) {
	s := h.sim
	if err := validateDynamicLimitsRequest(req); err != nil {
		return nil, err
	}
	if err := s.enter(ctx, h.name, "SetDynamicLimits", true); err != nil {
		return nil, err
	}
	if err := s.acquireWrite(ctx); err != nil {
		return nil, err
	}
	defer s.releaseWrite()

	s.mu.Lock()
	defer s.mu.Unlock()
	name := req.GetVmName()
	v, rule, err := h.writableLocked(name)
	if err != nil {
		return nil, err
	}
	dm := v.DynamicMemory
	if dm == nil {
		return nil, status.Errorf(codes.FailedPrecondition, "VM %q uses static memory; use SetStartup", name)
	}
	newMin, newMax := dm.Minimum, dm.Maximum
	if req.MinimumBytes != nil {
		newMin = Bytes(req.GetMinimumBytes())
	}
	if req.MaximumBytes != nil {
		newMax = Bytes(req.GetMaximumBytes())
	}
	if err := checkDynamicLimits(name, dm, rule, newMin, newMax); err != nil {
		return nil, err
	}
	before := vmMessage(v, s.clock.Now())
	dm.Minimum, dm.Maximum = newMin, newMax
	after := vmMessage(v, s.clock.Now())
	s.changedLocked()
	return &agentv1.SetDynamicLimitsResponse{Result: &agentv1.ResizeResult{Before: before, After: after}}, nil
}

func validateDynamicLimitsRequest(req *agentv1.SetDynamicLimitsRequest) error {
	if req.GetVmName() == "" {
		return status.Error(codes.InvalidArgument, "vm_name is required")
	}
	if req.MinimumBytes == nil && req.MaximumBytes == nil {
		return status.Error(codes.InvalidArgument, "set minimum_bytes, maximum_bytes or both")
	}
	if req.MinimumBytes != nil {
		if err := checkAligned("minimum_bytes", req.GetMinimumBytes()); err != nil {
			return err
		}
	}
	if req.MaximumBytes != nil {
		if err := checkAligned("maximum_bytes", req.GetMaximumBytes()); err != nil {
			return err
		}
	}
	if req.MinimumBytes != nil && req.MaximumBytes != nil && req.GetMinimumBytes() > req.GetMaximumBytes() {
		return status.Error(codes.InvalidArgument, "minimum_bytes is above maximum_bytes")
	}
	return nil
}

// checkDynamicLimits applies the DM direction rules and the policy bounds
// to the requested limits.
func checkDynamicLimits(name string, dm *DynamicMemorySpec, rule VMPolicySpec, newMin, newMax Bytes) error {
	switch {
	case newMin > newMax:
		return status.Errorf(codes.InvalidArgument, "minimum %s would be above maximum %s", newMin, newMax)
	case newMax < dm.Maximum:
		return status.Errorf(codes.FailedPrecondition, "Dynamic Memory maximum of VM %q may only be raised (current %s, requested %s)", name, dm.Maximum, newMax)
	case newMin > dm.Minimum:
		return status.Errorf(codes.FailedPrecondition, "Dynamic Memory minimum of VM %q may only be lowered (current %s, requested %s)", name, dm.Minimum, newMin)
	case newMin < rule.Min:
		return status.Errorf(codes.PermissionDenied, "minimum %s for VM %q is below the policy's min %s", newMin, name, rule.Min)
	case newMax > rule.Max:
		return status.Errorf(codes.PermissionDenied, "maximum %s for VM %q is above the policy's max %s", newMax, name, rule.Max)
	}
	return nil
}

// ProposePolicy implements agentv1.HostAgentServer.
func (h *hostServer) ProposePolicy(ctx context.Context, req *agentv1.ProposePolicyRequest) (*agentv1.ProposePolicyResponse, error) {
	s := h.sim
	if err := s.enter(ctx, h.name, "ProposePolicy", false); err != nil {
		return nil, err
	}
	if err := validatePolicy(req.GetPolicy()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	digest := s.digestLocked(req.GetPolicy())
	if req.GetDigest() != digest {
		return nil, status.Errorf(codes.InvalidArgument, "digest mismatch: request says %q, agent computes %q", req.GetDigest(), digest)
	}
	active := s.digestLocked(toProtoPolicy(s.sc.Policy))
	resp := &agentv1.ProposePolicyResponse{ActiveDigest: active}
	if digest == active {
		s.sc.PendingPolicy = nil
	} else {
		s.sc.PendingPolicy = fromProtoPolicy(req.GetPolicy())
		resp.PendingDigest = digest
	}
	s.changedLocked()
	return resp, nil
}
