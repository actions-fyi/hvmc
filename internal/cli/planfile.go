package cli

import (
	"errors"
	"fmt"
	"os"

	"sigs.k8s.io/yaml"

	"github.com/matfax/hvmc/internal/fake"
	"github.com/matfax/hvmc/internal/ledger"
)

// planFile is the --scenario file of `hvmc plan`: a fake-agent scenario
// (internal/fake.Scenario, the same YAML the fakes read) plus an optional
// list of requests to queue.
type planFile struct {
	fake.Scenario `json:",inline"`
	// Requests are queued in this order, after the agents were read.
	Requests []planRequest `json:"requests,omitempty"`
}

// planRequest is one request in a plan file. Its fields follow
// MemoryLease.spec (DESIGN §5).
type planRequest struct {
	// Name is the request ID (the MemoryLease name); default request-<n>,
	// counted from 1 in file order.
	Name string `json:"name,omitempty"`
	// Kind is the owner kind: Pod, External (default), Reconciler or Probe.
	Kind ledger.Kind `json:"kind,omitempty"`
	// VM is the target VM: required for every kind but Pod; for a Pod it pins
	// the request to that VM.
	VM string `json:"vm,omitempty"`
	// CandidateVMs are a Pod's candidate VMs (its filter's node list).
	CandidateVMs []string `json:"candidateVMs,omitempty"`
	// Bytes is the requested memory, a quantity such as 6Gi.
	Bytes fake.Bytes `json:"bytes"`
	// PriorityClass orders the queue, higher first; default 0.
	PriorityClass int32 `json:"priorityClass,omitempty"`
}

// parsePlanFile reads a plan file from YAML or JSON. Unknown fields are
// rejected; the scenario itself is validated when the fakes start.
func parsePlanFile(data []byte) (*planFile, error) {
	var pf planFile
	if err := yaml.UnmarshalStrict(data, &pf); err != nil {
		return nil, fmt.Errorf("parse scenario: %w", err)
	}
	return &pf, nil
}

func loadPlanFile(path string) (*planFile, error) {
	data, err := os.ReadFile(path) //nolint:gosec // reading a caller-chosen scenario file is the point
	if err != nil {
		return nil, err
	}
	return parsePlanFile(data)
}

// ledgerRequests converts the requests into ledger requests.
func (pf *planFile) ledgerRequests() ([]ledger.Request, error) {
	out := make([]ledger.Request, 0, len(pf.Requests))
	var errs []error
	for i, r := range pf.Requests {
		lr := r.toLedger(i + 1)
		switch {
		case lr.Kind == ledger.KindPod && lr.VM == "" && len(lr.Candidates) == 0:
			errs = append(errs, fmt.Errorf("request %s: kind Pod needs vm or candidateVMs", lr.ID))
		case lr.Kind != ledger.KindPod && len(lr.Candidates) > 0:
			errs = append(errs, fmt.Errorf("request %s: candidateVMs are only for kind Pod, use vm", lr.ID))
		}
		out = append(out, lr)
	}
	return out, errors.Join(errs...)
}

func (r planRequest) toLedger(n int) ledger.Request {
	lr := ledger.Request{
		ID:            ledger.RequestID(r.Name),
		Kind:          r.Kind,
		VM:            ledger.VMName(r.VM),
		Bytes:         int64(r.Bytes),
		PriorityClass: r.PriorityClass,
	}
	if lr.ID == "" {
		lr.ID = ledger.RequestID(fmt.Sprintf("request-%d", n))
	}
	if lr.Kind == "" {
		lr.Kind = ledger.KindExternal
	}
	for _, vm := range r.CandidateVMs {
		lr.Candidates = append(lr.Candidates, ledger.VMName(vm))
	}
	return lr
}

// hyperVAlignment is Hyper-V's memory granularity.
const hyperVAlignment = 2 * ledger.Mi

// derivedPlanFile describes an idle host for a HostConfig without a
// scenario: a host of spec.capacity with no overhead, every managed VM
// running at base + margin (rounded up to step and to Hyper-V's 2 MiB), a
// Dynamic Memory VM with that as its minimum and policy.max (or the same
// size) as its maximum, and a guest agent per VM that uses exactly base.
// Every VM then sits at its floor; there are no requests.
func derivedPlanFile(cfg ledger.Config) (*planFile, error) {
	if cfg.Capacity <= 0 {
		return nil, errors.New("spec.capacity is 0 (read from the host agent): set it or pass --scenario")
	}
	sc := fake.Scenario{Host: fake.HostSpec{Name: string(cfg.HostAgent), Total: fake.Bytes(cfg.Capacity)}}
	for _, vm := range cfg.VMs {
		margin := cfg.DefaultMargin
		if vm.Margin != nil {
			margin = *vm.Margin
		}
		size := roundUp(roundUp(vm.Base+margin, cfg.Step), hyperVAlignment)
		spec := fake.VMSpec{Name: string(vm.Name), Startup: fake.Bytes(size)}
		if vm.Mode == ledger.ModeDynamic {
			maximum := size
			if vm.Policy != nil {
				maximum = max(size, roundUp(vm.Policy.Max, hyperVAlignment))
			}
			spec.DynamicMemory = &fake.DynamicMemorySpec{Minimum: fake.Bytes(size), Maximum: fake.Bytes(maximum)}
		}
		sc.VMs = append(sc.VMs, spec)
		sc.Guests = append(sc.Guests, fake.GuestSpec{
			Name: string(vm.Name), VM: string(vm.Name), OS: string(vm.GuestOS), Used: fake.Bytes(vm.Base),
		})
	}
	return &planFile{Scenario: sc}, nil
}

// roundUp rounds x up to a multiple of step (step ≤ 1: unchanged).
func roundUp(x, step int64) int64 {
	if step <= 1 || x%step == 0 {
		return x
	}
	return (x/step + 1) * step
}
