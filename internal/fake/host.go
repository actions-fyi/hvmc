package fake

import (
	"context"

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
	name, n := req.GetVmName(), Bytes(req.GetStartupBytes())
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "vm_name is required")
	}
	if err := checkAligned("startup_bytes", req.GetStartupBytes()); err != nil {
		return nil, err
	}
	if err := s.enter(ctx, h.name, "SetStartup", true); err != nil {
		return nil, err
	}
	if err := s.acquireWrite(ctx); err != nil {
		return nil, err
	}
	release := true
	defer func() {
		if release {
			s.releaseWrite()
		}
	}()

	s.mu.Lock()
	v, rule, err := h.writableLocked(name)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if v.DynamicMemory != nil {
		s.mu.Unlock()
		return nil, status.Errorf(codes.FailedPrecondition, "VM %q uses Dynamic Memory; use SetDynamicLimits", name)
	}
	state := coarseState(v.State)
	if state != agentv1.VMState_VM_STATE_RUNNING && state != agentv1.VMState_VM_STATE_OFF {
		s.mu.Unlock()
		return nil, status.Errorf(codes.FailedPrecondition, "VM %q is %s; memory can only be changed while running or off", name, v.State)
	}
	if n < rule.Min || n > rule.Max {
		s.mu.Unlock()
		return nil, status.Errorf(codes.PermissionDenied, "startup %s for VM %q is outside the policy's [%s, %s]", n, name, rule.Min, rule.Max)
	}
	before := vmMessage(v, s.clock.Now())
	cur := *v.Assigned
	switch {
	case state == agentv1.VMState_VM_STATE_OFF || n == cur:
		// Configuration only (VM off), or the assignment already matches:
		// this also repairs a drifted MemoryStartup.
		v.Startup = n
	case n < cur:
		if f := v.ShrinkFloor; f != nil && n < *f {
			got := min(cur, alignUp(*f))
			*v.Assigned = got
			v.Startup = got
			if v.StartupDrift {
				v.Startup = n
			}
			after := vmMessage(v, s.clock.Now())
			s.changedLocked()
			s.mu.Unlock()
			return nil, statusWithResult(codes.Aborted, before, after,
				"guest released memory only down to %s of the requested %s", got, n)
		}
		*v.Assigned, v.Startup = n, n
	default: // grow
		if delta := n - cur; delta > s.freeLocked() {
			s.mu.Unlock()
			return nil, statusWithResult(codes.ResourceExhausted, before, before,
				"host has %s free, grow of VM %q needs %s", s.freeLocked(), name, delta)
		}
		if latency := s.sc.Host.GrowLatency.D(); latency > 0 {
			release = false
			done := h.growLater(name, n, n-cur)
			timeout := s.sc.Host.CmdletTimeout.D()
			s.mu.Unlock()
			if latency > timeout {
				// The cmdlet outlives the agent's timeout; the grow still
				// completes in the background, so the outcome is unknown to
				// the caller.
				return nil, s.wait(ctx, nil, timeout)
			}
			if err := s.wait(ctx, done, -1); err != nil {
				return nil, err
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			after := before
			if v := s.sc.vm(name); v != nil {
				after = vmMessage(v, s.clock.Now())
			}
			return &agentv1.SetStartupResponse{Result: &agentv1.ResizeResult{Before: before, After: after}}, nil
		}
		*v.Assigned, v.Startup = n, n
	}
	after := vmMessage(v, s.clock.Now())
	s.changedLocked()
	s.mu.Unlock()
	return &agentv1.SetStartupResponse{Result: &agentv1.ResizeResult{Before: before, After: after}}, nil
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
	name := req.GetVmName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "vm_name is required")
	}
	if req.MinimumBytes == nil && req.MaximumBytes == nil {
		return nil, status.Error(codes.InvalidArgument, "set minimum_bytes, maximum_bytes or both")
	}
	if req.MinimumBytes != nil {
		if err := checkAligned("minimum_bytes", req.GetMinimumBytes()); err != nil {
			return nil, err
		}
	}
	if req.MaximumBytes != nil {
		if err := checkAligned("maximum_bytes", req.GetMaximumBytes()); err != nil {
			return nil, err
		}
	}
	if req.MinimumBytes != nil && req.MaximumBytes != nil && req.GetMinimumBytes() > req.GetMaximumBytes() {
		return nil, status.Error(codes.InvalidArgument, "minimum_bytes is above maximum_bytes")
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
	switch {
	case newMin > newMax:
		return nil, status.Errorf(codes.InvalidArgument, "minimum %s would be above maximum %s", newMin, newMax)
	case newMax < dm.Maximum:
		return nil, status.Errorf(codes.FailedPrecondition, "Dynamic Memory maximum of VM %q may only be raised (current %s, requested %s)", name, dm.Maximum, newMax)
	case newMin > dm.Minimum:
		return nil, status.Errorf(codes.FailedPrecondition, "Dynamic Memory minimum of VM %q may only be lowered (current %s, requested %s)", name, dm.Minimum, newMin)
	case newMin < rule.Min:
		return nil, status.Errorf(codes.PermissionDenied, "minimum %s for VM %q is below the policy's min %s", newMin, name, rule.Min)
	case newMax > rule.Max:
		return nil, status.Errorf(codes.PermissionDenied, "maximum %s for VM %q is above the policy's max %s", newMax, name, rule.Max)
	}
	before := vmMessage(v, s.clock.Now())
	dm.Minimum, dm.Maximum = newMin, newMax
	after := vmMessage(v, s.clock.Now())
	s.changedLocked()
	return &agentv1.SetDynamicLimitsResponse{Result: &agentv1.ResizeResult{Before: before, After: after}}, nil
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
