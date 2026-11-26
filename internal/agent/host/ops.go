package host

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/matfax/hvmc/internal/agent"
	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
)

// Worker operations (worker.ps1, Invoke-HvmcOp).
const (
	opVMs        = "vms"
	opHost       = "host"
	opSetStartup = "setStartup"
	opSetDynamic = "setDynamic"
)

// vmRecord is one VM as the worker projects it (Get-HvmcVMs).
type vmRecord struct {
	Name                 string `json:"name"`
	ID                   string `json:"id"`
	State                string `json:"state"`
	AssignedBytes        int64  `json:"assignedBytes"`
	StartupBytes         int64  `json:"startupBytes"`
	DynamicMemoryEnabled bool   `json:"dynamicMemoryEnabled"`
	MinimumBytes         int64  `json:"minimumBytes"`
	MaximumBytes         int64  `json:"maximumBytes"`
	Buffer               int32  `json:"buffer"`
	Priority             int32  `json:"priority"`
}

// vmList decodes the worker's VM list. ConvertTo-Json -AsArray always
// writes an array; a bare object is accepted as a list of one in case a
// PowerShell version unrolls it.
type vmList []vmRecord

func (l *vmList) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '{' {
		var one vmRecord
		if err := json.Unmarshal(data, &one); err != nil {
			return err
		}
		*l = vmList{one}
		return nil
	}
	return json.Unmarshal(data, (*[]vmRecord)(l))
}

// hostRecord is the worker's host projection (Get-HvmcHost).
type hostRecord struct {
	ComputerName string `json:"computerName"`
	TotalKiB     int64  `json:"totalVisibleMemoryKiB"`
	FreeKiB      int64  `json:"freePhysicalMemoryKiB"`
}

// setStartupArgs and setDynamicArgs address the VM by ID: Hyper-V names
// are neither unique nor free of wildcard characters.
type setStartupArgs struct {
	ID           string `json:"id"`
	StartupBytes int64  `json:"startupBytes"`
}

type setDynamicArgs struct {
	ID           string `json:"id"`
	MinimumBytes *int64 `json:"minimumBytes"`
	MaximumBytes *int64 `json:"maximumBytes"`
}

// hyperV runs the host agent's Hyper-V operations on the worker. Errors
// are raw (*WorkerError or supervisor status errors); callers map them
// with toStatus.
type hyperV struct {
	sup *Supervisor
	clk agent.Clock
}

// vms reads every VM; the time is when the answer arrived.
func (h *hyperV) vms(ctx context.Context) ([]vmRecord, time.Time, error) {
	var list vmList
	if err := h.sup.Call(ctx, opVMs, nil, &list); err != nil {
		return nil, time.Time{}, err
	}
	return list, h.clk.Now(), nil
}

// host reads the host's name and memory.
func (h *hyperV) host(ctx context.Context) (hostRecord, time.Time, error) {
	var rec hostRecord
	if err := h.sup.Call(ctx, opHost, nil, &rec); err != nil {
		return hostRecord{}, time.Time{}, err
	}
	return rec, h.clk.Now(), nil
}

func (h *hyperV) setStartup(ctx context.Context, id string, n int64) error {
	return h.sup.Call(ctx, opSetStartup, setStartupArgs{ID: id, StartupBytes: n}, nil)
}

func (h *hyperV) setDynamic(ctx context.Context, id string, minimum, maximum *int64) error {
	return h.sup.Call(ctx, opSetDynamic, setDynamicArgs{ID: id, MinimumBytes: minimum, MaximumBytes: maximum}, nil)
}

// vmMessage converts a worker record read at the given time.
func vmMessage(r vmRecord, at time.Time) *agentv1.VM {
	m := &agentv1.VM{
		Name:          r.Name,
		Id:            strings.ToLower(strings.Trim(r.ID, "{}")),
		State:         coarseState(r.State),
		HypervState:   r.State,
		AssignedBytes: r.AssignedBytes,
		StartupBytes:  r.StartupBytes,
		DynamicMemory: &agentv1.DynamicMemory{},
		ObservedAt:    timestamppb.New(at),
	}
	if r.DynamicMemoryEnabled {
		m.DynamicMemory = &agentv1.DynamicMemory{
			Enabled:       true,
			MinimumBytes:  r.MinimumBytes,
			MaximumBytes:  r.MaximumBytes,
			BufferPercent: r.Buffer,
			Priority:      r.Priority,
		}
	}
	return m
}

// snapshotMessage converts a VM list, sorted by name.
func snapshotMessage(list []vmRecord, at time.Time) *agentv1.VMSnapshot {
	snap := &agentv1.VMSnapshot{ObservedAt: timestamppb.New(at)}
	for _, r := range list {
		snap.Vms = append(snap.Vms, vmMessage(r, at))
	}
	slices.SortFunc(snap.Vms, func(a, b *agentv1.VM) int {
		return strings.Compare(a.GetName()+"\x00"+a.GetId(), b.GetName()+"\x00"+b.GetId())
	})
	return snap
}

// coarseState maps Hyper-V's VMState name (stringified enum) to VMState.
func coarseState(s string) agentv1.VMState {
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
