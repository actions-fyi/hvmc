package host

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/matfax/hvmc/internal/agent"
	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
	"github.com/matfax/hvmc/internal/policy"
)

// Enforcement of the active policy (DESIGN §4.3)
//
// A write is checked in this order, each failing with the code of the
// error model (common.proto); the order matches the fake host agent
// (internal/fake), which is the reference:
//
//  1. request shape (INVALID_ARGUMENT);
//  2. the VM as read from Hyper-V right before the write: UNAVAILABLE
//     (vmms down, worker restarting), NOT_FOUND;
//  3. SetDynamicLimits: the new minimum above the new maximum
//     (INVALID_ARGUMENT);
//  4. the active policy with policy.Policy.Allows, one check per setting
//     written: VM not listed (PERMISSION_DENIED), fixed VM, memory type
//     and DM direction (FAILED_PRECONDITION); then, SetStartup only, the
//     VM's run state (FAILED_PRECONDITION); then the policy's bounds
//     (PERMISSION_DENIED). Every non-bounds refusal ranks before a bounds
//     violation (firstRefusal);
//  5. SetStartup grow of a running VM: host free memory
//     (RESOURCE_EXHAUSTED);
//  6. the write, then the outcome from the VM re-read (classifyStartup).
//
// Every call is audited by the framework's interceptor, refusals
// included; handlers add the VM before and after and log each refusal.

// alignment is the granularity of every memory value (host.proto).
const alignment = 2 << 20

func checkAligned(field string, v int64) error {
	if v <= 0 || v%alignment != 0 {
		return status.Errorf(codes.InvalidArgument, "%s must be positive and a multiple of 2 MiB, got %d", field, v)
	}
	return nil
}

func validateStartupRequest(req *agentv1.SetStartupRequest) error {
	if req.GetVmName() == "" {
		return status.Error(codes.InvalidArgument, "vm_name is required")
	}
	return checkAligned("startup_bytes", req.GetStartupBytes())
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

// policyState is the VM state policy.Policy.Allows needs.
func policyState(v *agentv1.VM) policy.VMState {
	st := policy.VMState{Name: v.GetName()}
	if dm := v.GetDynamicMemory(); dm.GetEnabled() {
		st.DynamicMemory = true
		st.DynamicMinimum, st.DynamicMaximum = dm.GetMinimumBytes(), dm.GetMaximumBytes()
	}
	return st
}

// policyWrite is one setting a write RPC changes.
type policyWrite struct {
	op    policy.Op
	bytes int64
}

// firstRefusal picks the error a write fails with: the first refusal that
// is not a policy bounds violation, else the first bounds violation.
func firstRefusal(errs ...error) error {
	var bounds error
	for _, err := range errs {
		switch {
		case err == nil:
		case !errors.Is(err, policy.ErrOutOfBounds):
			return err
		case bounds == nil:
			bounds = err
		}
	}
	return bounds
}

// allow checks writes to v against the active policy. vmErr is a refusal
// of the VM's state that the policy package does not cover; it ranks
// after the policy's own refusals except its bounds.
func allow(pol policy.Policy, v *agentv1.VM, vmErr error, writes ...policyWrite) error {
	st := policyState(v)
	errs := make([]error, 0, len(writes)+1)
	for _, w := range writes {
		errs = append(errs, pol.Allows(st, w.op, w.bytes))
	}
	return refusalStatus(firstRefusal(append(errs, vmErr)...))
}

// runStateErr refuses SetStartup on a VM that is neither running nor off.
func runStateErr(v *agentv1.VM) error {
	switch v.GetState() {
	case agentv1.VMState_VM_STATE_RUNNING, agentv1.VMState_VM_STATE_OFF:
		return nil
	default:
		return status.Errorf(codes.FailedPrecondition, "VM %q is %s; memory can only be changed while running or off", v.GetName(), v.GetHypervState())
	}
}

// acquireWrite serializes writes: the read-check-write sequence of one
// write must not interleave with another's.
func (s *server) acquireWrite(ctx context.Context) error {
	select {
	case s.writeSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
}

func (s *server) releaseWrite() { <-s.writeSem }

// lookup reads the VM a write is for by its exact name.
func (s *server) lookup(ctx context.Context, name string) (*agentv1.VM, error) {
	list, at, err := s.hv.vms(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	var found []vmRecord
	for _, r := range list {
		if r.Name == name {
			found = append(found, r)
		}
	}
	switch len(found) {
	case 0:
		return nil, status.Errorf(codes.NotFound, "VM %q does not exist on host", name)
	case 1:
		return vmMessage(found[0], at), nil
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "%d VMs are named %q; writes need a unique name", len(found), name)
	}
}

// reread reads the VM with before's ID after a write.
func (s *server) reread(ctx context.Context, before *agentv1.VM) (*agentv1.VM, error) {
	list, at, err := s.hv.vms(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	for _, r := range list {
		if vmMessage(r, at).GetId() == before.GetId() {
			return vmMessage(r, at), nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "VM %q disappeared during the write", before.GetName())
}

// refused logs a refused or failed write and returns err.
func (s *server) refused(ctx context.Context, method, vm string, err error) error {
	s.log.Warn("write refused", "method", method, "vm", vm, "client", agent.ClientCN(ctx),
		"code", status.Code(err).String(), "error", err)
	return err
}

// SetStartup implements agentv1.HostAgentServer.
func (s *server) SetStartup(ctx context.Context, req *agentv1.SetStartupRequest) (*agentv1.SetStartupResponse, error) {
	result, err := s.setStartup(ctx, req)
	if err != nil {
		return nil, s.refused(ctx, "SetStartup", req.GetVmName(), err)
	}
	s.log.Info("startup memory set", "vm", req.GetVmName(), "client", agent.ClientCN(ctx),
		"before", result.GetBefore().GetStartupBytes(), "after", result.GetAfter().GetStartupBytes(),
		"assignedAfter", result.GetAfter().GetAssignedBytes())
	return &agentv1.SetStartupResponse{Result: result}, nil
}

func (s *server) setStartup(ctx context.Context, req *agentv1.SetStartupRequest) (*agentv1.ResizeResult, error) {
	if err := validateStartupRequest(req); err != nil {
		return nil, err
	}
	if err := s.acquireWrite(ctx); err != nil {
		return nil, err
	}
	defer s.releaseWrite()
	n := req.GetStartupBytes()
	before, err := s.lookup(ctx, req.GetVmName())
	if err != nil {
		return nil, err
	}
	agent.SetAuditBefore(ctx, before)
	if err := allow(s.policy(), before, runStateErr(before), policyWrite{policy.OpSetStartup, n}); err != nil {
		return nil, err
	}
	if err := s.checkHostFree(ctx, before, n); err != nil {
		return nil, err
	}
	setErr := s.hv.setStartup(ctx, before.GetId(), n)
	if isSupervisorErr(setErr) {
		// Timeout, worker gone or caller gone: the outcome is unknown and
		// the worker cannot re-read now.
		return nil, setErr
	}
	after, readErr := s.reread(ctx, before)
	if after != nil {
		agent.SetAuditAfter(ctx, after)
	}
	return classifyStartup(before, after, n, setErr, readErr)
}

// isSupervisorErr reports whether err came from the supervisor (a status
// error) rather than from a cmdlet.
func isSupervisorErr(err error) bool {
	if err == nil || workerKind(err) != "" {
		return false
	}
	_, ok := status.FromError(err)
	return ok
}

// checkHostFree refuses a runtime grow larger than the host's free memory.
func (s *server) checkHostFree(ctx context.Context, before *agentv1.VM, n int64) error {
	if before.GetState() != agentv1.VMState_VM_STATE_RUNNING || n <= before.GetAssignedBytes() {
		return nil
	}
	h, _, err := s.hv.host(ctx)
	if err != nil {
		return toStatus(err)
	}
	if delta, free := n-before.GetAssignedBytes(), h.FreeKiB*1024; delta > free {
		return statusWithResult(codes.ResourceExhausted, before, before,
			"host has %d bytes free, grow of VM %q needs %d", free, before.GetName(), delta)
	}
	return nil
}

// classifyStartup decides the outcome of a SetStartup from the VM before
// and after the write. A running VM that kept more than the requested
// size after a shrink refused it, entirely or partly (ABORTED), whether or
// not Set-VMMemory reported an error; a failed grow is RESOURCE_EXHAUSTED
// when the worker saw an out-of-memory error.
func classifyStartup(before, after *agentv1.VM, n int64, setErr, readErr error) (*agentv1.ResizeResult, error) {
	if workerKind(setErr) == kindVMMSDown {
		return nil, toStatus(setErr)
	}
	if readErr != nil {
		if setErr != nil {
			return nil, toStatus(setErr)
		}
		return nil, readErr
	}
	running := before.GetState() == agentv1.VMState_VM_STATE_RUNNING
	switch {
	case running && n < before.GetAssignedBytes() && after.GetAssignedBytes() > n:
		return nil, statusWithResult(codes.Aborted, before, after,
			"guest released memory only down to %d of the requested %d bytes%s", after.GetAssignedBytes(), n, cause(setErr))
	case setErr == nil:
		return &agentv1.ResizeResult{Before: before, After: after}, nil
	case running && n > before.GetAssignedBytes() && workerKind(setErr) == kindOutOfMemory:
		return nil, statusWithResult(codes.ResourceExhausted, before, after,
			"host out of memory for the grow of VM %q: %v", before.GetName(), setErr)
	default:
		return nil, toStatus(setErr)
	}
}

func cause(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf(": %v", err)
}

// SetDynamicLimits implements agentv1.HostAgentServer.
func (s *server) SetDynamicLimits(ctx context.Context, req *agentv1.SetDynamicLimitsRequest) (*agentv1.SetDynamicLimitsResponse, error) {
	result, err := s.setDynamicLimits(ctx, req)
	if err != nil {
		return nil, s.refused(ctx, "SetDynamicLimits", req.GetVmName(), err)
	}
	s.log.Info("Dynamic Memory limits set", "vm", req.GetVmName(), "client", agent.ClientCN(ctx),
		"minimum", result.GetAfter().GetDynamicMemory().GetMinimumBytes(),
		"maximum", result.GetAfter().GetDynamicMemory().GetMaximumBytes())
	return &agentv1.SetDynamicLimitsResponse{Result: result}, nil
}

func (s *server) setDynamicLimits(ctx context.Context, req *agentv1.SetDynamicLimitsRequest) (*agentv1.ResizeResult, error) {
	if err := validateDynamicLimitsRequest(req); err != nil {
		return nil, err
	}
	if err := s.acquireWrite(ctx); err != nil {
		return nil, err
	}
	defer s.releaseWrite()
	before, err := s.lookup(ctx, req.GetVmName())
	if err != nil {
		return nil, err
	}
	agent.SetAuditBefore(ctx, before)
	if err := checkDynamicOrder(before, req); err != nil {
		return nil, err
	}
	if err := allow(s.policy(), before, nil, dynamicWrites(req)...); err != nil {
		return nil, err
	}
	setErr := s.hv.setDynamic(ctx, before.GetId(), req.MinimumBytes, req.MaximumBytes)
	if isSupervisorErr(setErr) {
		return nil, setErr
	}
	after, readErr := s.reread(ctx, before)
	if after != nil {
		agent.SetAuditAfter(ctx, after)
	}
	switch {
	case setErr != nil:
		return nil, toStatus(setErr)
	case readErr != nil:
		return nil, readErr
	}
	return &agentv1.ResizeResult{Before: before, After: after}, nil
}

// dynamicLimits returns the DM limits of v after req.
func dynamicLimits(v *agentv1.VM, req *agentv1.SetDynamicLimitsRequest) (newMin, newMax int64) {
	dm := v.GetDynamicMemory()
	newMin, newMax = dm.GetMinimumBytes(), dm.GetMaximumBytes()
	if req.MinimumBytes != nil {
		newMin = req.GetMinimumBytes()
	}
	if req.MaximumBytes != nil {
		newMax = req.GetMaximumBytes()
	}
	return newMin, newMax
}

// checkDynamicOrder refuses a request that would leave the DM minimum
// above the maximum (INVALID_ARGUMENT). A static-memory VM has no limits
// to compare; the policy check refuses it instead.
func checkDynamicOrder(v *agentv1.VM, req *agentv1.SetDynamicLimitsRequest) error {
	if !v.GetDynamicMemory().GetEnabled() {
		return nil
	}
	if newMin, newMax := dynamicLimits(v, req); newMin > newMax {
		return status.Errorf(codes.InvalidArgument, "minimum %d of VM %q would be above maximum %d", newMin, v.GetName(), newMax)
	}
	return nil
}

// dynamicWrites returns the settings a SetDynamicLimits request changes,
// each checked as its own operation: only the limits the request sets.
func dynamicWrites(req *agentv1.SetDynamicLimitsRequest) []policyWrite {
	var writes []policyWrite
	if req.MinimumBytes != nil {
		writes = append(writes, policyWrite{policy.OpSetDynamicMinimum, req.GetMinimumBytes()})
	}
	if req.MaximumBytes != nil {
		writes = append(writes, policyWrite{policy.OpSetDynamicMaximum, req.GetMaximumBytes()})
	}
	return writes
}
