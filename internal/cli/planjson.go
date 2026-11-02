package cli

import (
	"slices"
	"strings"
	"time"

	"github.com/matfax/hvmc/internal/ledger"
)

// planJSON is the output of `hvmc plan --output json`, a stable format for
// scripts. It is mapped from ledger.Snapshot and the queue evaluation, so the
// ledger's Go types can change without changing it. Byte values are integers
// in fields ending in Bytes.
type planJSON struct {
	// At is the ledger's time of the snapshot.
	At    time.Time     `json:"at"`
	Host  hostJSON      `json:"host"`
	VMs   []vmJSON      `json:"vms"`
	Queue []requestJSON `json:"queue"`
	// Agents are the agent states the ledger knows, by name.
	Agents []agentJSON `json:"agents"`
	// Notes are agent calls that failed; their data counts as missing.
	Notes []string `json:"notes,omitempty"`
}

type hostJSON struct {
	HostAgentState string `json:"hostAgentState"`
	Frozen         bool   `json:"frozen"`
	CapacityBytes  int64  `json:"capacityBytes"`
	ReserveBytes   int64  `json:"reserveBytes"`
	FreeBytes      int64  `json:"freeBytes"`
	CommittedBytes int64  `json:"committedBytes"`
	InFlightBytes  int64  `json:"inFlightBytes"`
	PoolBytes      int64  `json:"poolBytes"`
}

type vmJSON struct {
	Name    string `json:"name"`
	Managed bool   `json:"managed"`
	// Mode and GuestOS are empty for unmanaged VMs.
	Mode          string             `json:"mode,omitempty"`
	GuestOS       string             `json:"guestOS,omitempty"`
	State         string             `json:"state"`
	AssignedBytes int64              `json:"assignedBytes"`
	StartupBytes  int64              `json:"startupBytes"`
	DynamicMemory *dynamicMemoryJSON `json:"dynamicMemory,omitempty"`
	// ObservedAt is when the host agent reported the VM; absent if never.
	ObservedAt *time.Time `json:"observedAt,omitempty"`
	// UsedKnown is set when fresh guest data is present; usedBytes and
	// availableBytes are 0 otherwise.
	UsedKnown      bool  `json:"usedKnown"`
	UsedBytes      int64 `json:"usedBytes"`
	AvailableBytes int64 `json:"availableBytes"`
	BaseBytes      int64 `json:"baseBytes"`
	MarginBytes    int64 `json:"marginBytes"`
	LeasedBytes    int64 `json:"leasedBytes"`
	EffBaseBytes   int64 `json:"effBaseBytes"`
	FloorBytes     int64 `json:"floorBytes"`
	HeadroomBytes  int64 `json:"headroomBytes"`
	CommittedBytes int64 `json:"committedBytes"`
}

type dynamicMemoryJSON struct {
	MinimumBytes  int64 `json:"minimumBytes"`
	MaximumBytes  int64 `json:"maximumBytes"`
	BufferPercent int64 `json:"bufferPercent"`
	Priority      int   `json:"priority"`
}

// requestJSON is one Pending request, in queue order.
type requestJSON struct {
	Position      int      `json:"position"`
	ID            string   `json:"id"`
	Kind          string   `json:"kind"`
	VM            string   `json:"vm,omitempty"`
	CandidateVMs  []string `json:"candidateVMs,omitempty"`
	Bytes         int64    `json:"bytes"`
	PriorityClass int32    `json:"priorityClass"`
	// Grantable: the request fits on at least one candidate now.
	Grantable bool `json:"grantable"`
	// BlockingCause lists, for a request that is not grantable, the
	// distinct reasons of its candidates in candidate order, e.g.
	// "PoolShort" or "Queued, NoHeadroom".
	BlockingCause string    `json:"blockingCause,omitempty"`
	Fits          []fitJSON `json:"fits"`
}

// fitJSON is the evaluation of a request on one candidate VM.
type fitJSON struct {
	VM   string `json:"vm"`
	Fits bool   `json:"fits"`
	// DeltaBytes and HeadroomBytes are absent when the ledger did not
	// compute them (frozen, or not a candidate).
	DeltaBytes    *int64 `json:"deltaBytes,omitempty"`
	HeadroomBytes *int64 `json:"headroomBytes,omitempty"`
	PoolBytes     int64  `json:"poolBytes"`
	Reason        string `json:"reason,omitempty"`
}

type agentJSON struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

func toPlanJSON(rep *planReport) planJSON {
	s := rep.Snapshot
	out := planJSON{
		At: s.At,
		Host: hostJSON{
			HostAgentState: string(s.HostAgentState), Frozen: s.Frozen,
			CapacityBytes: s.Capacity, ReserveBytes: s.Reserve, FreeBytes: s.HostFree,
			CommittedBytes: s.Committed, InFlightBytes: s.InFlight, PoolBytes: s.Pool,
		},
		VMs:    make([]vmJSON, 0, len(s.VMs)),
		Queue:  make([]requestJSON, 0, len(rep.Evaluation)),
		Agents: agentsJSON(s.Agents),
		Notes:  rep.Notes,
	}
	for _, v := range s.VMs {
		out.VMs = append(out.VMs, toVMJSON(v))
	}
	reqs := make(map[ledger.RequestID]ledger.Request, len(s.Leases))
	for _, lv := range s.Leases {
		reqs[lv.Request.ID] = lv.Request
	}
	for _, ev := range rep.Evaluation {
		out.Queue = append(out.Queue, toRequestJSON(ev, reqs[ev.ID]))
	}
	return out
}

func toVMJSON(v ledger.VMView) vmJSON {
	out := vmJSON{
		Name: string(v.Name), Managed: v.Managed, Mode: string(v.Mode), GuestOS: string(v.GuestOS),
		State: string(v.State), AssignedBytes: v.Assigned, StartupBytes: v.Startup,
		UsedKnown: v.UsedKnown, UsedBytes: v.Used, AvailableBytes: v.Available,
		BaseBytes: v.Base, MarginBytes: v.Margin, LeasedBytes: v.Leased, EffBaseBytes: v.EffBase,
		FloorBytes: v.Floor, HeadroomBytes: v.Headroom, CommittedBytes: v.Committed,
	}
	if !v.ObservedAt.IsZero() {
		at := v.ObservedAt
		out.ObservedAt = &at
	}
	if d := v.Dynamic; d != nil {
		out.DynamicMemory = &dynamicMemoryJSON{
			MinimumBytes: d.Minimum, MaximumBytes: d.Maximum, BufferPercent: d.Buffer, Priority: d.Priority,
		}
	}
	return out
}

func toRequestJSON(ev requestEvaluation, r ledger.Request) requestJSON {
	out := requestJSON{
		Position: int(ev.Position), ID: string(ev.ID), Kind: string(r.Kind), VM: string(r.VM),
		Bytes: r.Bytes, PriorityClass: r.PriorityClass, Grantable: ev.Grantable,
		Fits: make([]fitJSON, 0, len(ev.Fits)),
	}
	for _, vm := range r.Candidates {
		out.CandidateVMs = append(out.CandidateVMs, string(vm))
	}
	var causes []string
	for _, f := range ev.Fits {
		out.Fits = append(out.Fits, toFitJSON(f))
		if !f.Fits && f.Reason != "" && !slices.Contains(causes, string(f.Reason)) {
			causes = append(causes, string(f.Reason))
		}
	}
	if !ev.Grantable {
		out.BlockingCause = strings.Join(causes, ", ")
		if len(causes) == 0 {
			out.BlockingCause = "NoCandidates"
		}
	}
	return out
}

func toFitJSON(f ledger.Fit) fitJSON {
	out := fitJSON{VM: string(f.VM), Fits: f.Fits, PoolBytes: f.Pool}
	if fitComputed(f) {
		delta, headroom := f.Delta, f.Headroom
		out.DeltaBytes, out.HeadroomBytes = &delta, &headroom
	}
	if !f.Fits {
		out.Reason = string(f.Reason)
	}
	return out
}

func agentsJSON(m map[ledger.AgentName]ledger.AgentState) []agentJSON {
	out := make([]agentJSON, 0, len(m))
	for name, st := range m {
		out = append(out, agentJSON{Name: string(name), State: string(st)})
	}
	slices.SortFunc(out, func(a, b agentJSON) int { return strings.Compare(a.Name, b.Name) })
	return out
}
