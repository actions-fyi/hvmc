package fake

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

// Scenario describes a simulated Hyper-V host, its VMs and guest agents, and
// the scripted behaviours of every agent. It is both the input of New
// (usually loaded from YAML) and the live state of a running Sim: control
// methods change it, Sim.Scenario returns it, and simulated effects (resizes,
// guest totals, failure counters) are written back into it.
//
// Example:
//
//	host:
//	  name: hyperv-1
//	  total: 64Gi
//	  overhead: 4Gi
//	  growLatency: 2s
//	policy:
//	  vms:
//	    - name: linux-node-1
//	      fixed: true
//	    - name: vm-a
//	      min: 4Gi
//	      max: 16Gi
//	vms:
//	  - name: linux-node-1
//	    startup: 8Gi
//	  - name: vm-a
//	    startup: 8Gi
//	    shrinkFloor: 6Gi
//	    startupDrift: true
//	guests:
//	  - name: vm-a
//	    vm: vm-a
//	    os: windows
//	    offset: 256Mi
//	    used: 3Gi
//	    totalLag: 10s
type Scenario struct {
	// Host is the Hyper-V host and its agent.
	Host HostSpec `json:"host"`
	// Policy is the host agent's active write policy; nil means an empty
	// policy (every VM read-only).
	Policy *PolicySpec `json:"policy,omitempty"`
	// PendingPolicy is a proposed policy not yet activated on the host.
	PendingPolicy *PolicySpec `json:"pendingPolicy,omitempty"`
	// VMs are all VMs on the host, managed or not.
	VMs []VMSpec `json:"vms,omitempty"`
	// Guests are the guest agents.
	Guests []GuestSpec `json:"guests,omitempty"`
}

// AgentBehaviour holds the scripted behaviours every agent role supports.
type AgentBehaviour struct {
	// NotServing makes the agent's health service report NOT_SERVING.
	NotServing bool `json:"notServing,omitempty"`
	// Hang lists RPC method names (e.g. "SetStartup", or "*" for all) that
	// block until the caller's deadline instead of answering. On the host,
	// Hyper-V calls also end at the agent's cmdlet timeout.
	Hang []string `json:"hang,omitempty"`
	// Stale freezes the agent's observations: reads keep returning the
	// values captured when Stale was set, with an observed_at that no longer
	// advances.
	Stale *StaleSpec `json:"stale,omitempty"`
	// Certificate describes the agent's serving certificate and scripts
	// rotation failures.
	Certificate *CertSpec `json:"certificate,omitempty"`
}

// StaleSpec freezes an agent's observations.
type StaleSpec struct {
	// Age backdates the frozen observed_at at the moment of freezing.
	Age Duration `json:"age,omitempty"`
}

// CertSpec is an agent's certificate identity and rotation behaviour.
type CertSpec struct {
	// CommonName of the agent certificate; defaults to the agent name.
	CommonName string `json:"commonName,omitempty"`
	// DNSNames are the certificate's DNS SANs.
	DNSNames []string `json:"dnsNames,omitempty"`
	// IPAddresses are the certificate's IP SANs.
	IPAddresses []string `json:"ipAddresses,omitempty"`
	// FailRotate makes the next n RotateCertificate calls fail with
	// INTERNAL. Counts down as calls fail.
	FailRotate int `json:"failRotate,omitempty"`
	// FailInstall makes the next n otherwise valid InstallCertificate calls
	// fail with INTERNAL (storing the pair failed; the current pair and the
	// pending key are kept). Counts down as calls fail.
	FailInstall int `json:"failInstall,omitempty"`
}

// HostSpec is the Hyper-V host and the host agent's behaviours.
type HostSpec struct {
	// Name is the host's computer name; it also names the host agent in
	// control methods.
	Name string `json:"name"`
	// Total is the host's physical memory.
	Total Bytes `json:"total"`
	// Overhead is memory used outside the VMs' assigned memory (host OS, VM
	// worker processes). Free = Total − Overhead − Σ assigned − in-flight
	// grows.
	Overhead Bytes `json:"overhead,omitempty"`
	// GrowLatency is how long a runtime grow (SetStartup above the current
	// assignment) takes before it returns.
	GrowLatency Duration `json:"growLatency,omitempty"`
	// CmdletTimeout is the agent's per-cmdlet timeout; Hyper-V calls that
	// take longer end with DEADLINE_EXCEEDED. Default 30s.
	CmdletTimeout Duration `json:"cmdletTimeout,omitempty"`
	// VMMSDown simulates the Hyper-V management service being down: Hyper-V
	// RPCs fail with UNAVAILABLE, WatchVMs streams end, health reports
	// NOT_SERVING.
	VMMSDown bool `json:"vmmsDown,omitempty"`
	// PolicyDigestSkew makes the agent's digest function disagree with the
	// configured one (a canonicalization difference between versions): the
	// reported digests never match and every proposal is refused with
	// INVALID_ARGUMENT.
	PolicyDigestSkew bool `json:"policyDigestSkew,omitempty"`

	AgentBehaviour `json:",inline"`
}

// PolicySpec is a host write policy in the shape of the agent's local file
// (DESIGN §4.3).
type PolicySpec struct {
	// VMs are the per-VM rules.
	VMs []VMPolicySpec `json:"vms,omitempty"`
}

// VMPolicySpec is the write permission for one VM.
type VMPolicySpec struct {
	// Name is the Hyper-V VM name.
	Name string `json:"name"`
	// Fixed marks the VM read-only.
	Fixed bool `json:"fixed,omitempty"`
	// Min is the lowest value a write may set (startup, or DM minimum).
	Min Bytes `json:"min,omitempty"`
	// Max is the highest value a write may set (startup, or DM maximum).
	Max Bytes `json:"max,omitempty"`
}

// VMSpec is one VM on the simulated host.
type VMSpec struct {
	// Name is the Hyper-V VM name.
	Name string `json:"name"`
	// ID is the VM GUID (lowercase, no braces); derived from the name when
	// empty.
	ID string `json:"id,omitempty"`
	// State is Hyper-V's state string ("Running", "Off", "Saved",
	// "RunningCritical", ...); default "Running".
	State HyperVState `json:"state,omitempty"`
	// Startup is MemoryStartup.
	Startup Bytes `json:"startup"`
	// Assigned is MemoryAssigned; defaults to Startup, or 0 when the VM is
	// Off or Saved.
	Assigned *Bytes `json:"assigned,omitempty"`
	// DynamicMemory enables Dynamic Memory with these settings.
	DynamicMemory *DynamicMemorySpec `json:"dynamicMemory,omitempty"`
	// ShrinkFloor is the lowest assignment the guest gives up memory to. A
	// runtime shrink below it fails with ABORTED: partially (the assignment
	// stops at the floor) or, when the floor is at or above the current
	// assignment, entirely. Nil means every shrink succeeds.
	ShrinkFloor *Bytes `json:"shrinkFloor,omitempty"`
	// StartupDrift makes a refused or partial shrink still set MemoryStartup
	// to the requested value (DESIGN §3.5 repair case). Without it,
	// MemoryStartup follows the resulting assignment.
	StartupDrift bool `json:"startupDrift,omitempty"`
}

// HyperVState is Hyper-V's VM state string, e.g. "Running", "Off",
// "RunningCritical". In YAML an unquoted Off (a YAML 1.1 boolean) is read as
// "Off".
type HyperVState string

// UnmarshalJSON reads a state string, or false as "Off".
func (s *HyperVState) UnmarshalJSON(data []byte) error {
	if string(data) == "false" {
		*s = "Off"
		return nil
	}
	var v string
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("VM state must be a Hyper-V state name: %w", err)
	}
	*s = HyperVState(v)
	return nil
}

// DynamicMemorySpec is a VM's Dynamic Memory configuration.
type DynamicMemorySpec struct {
	// Minimum is MinimumBytes.
	Minimum Bytes `json:"minimum"`
	// Maximum is MaximumBytes.
	Maximum Bytes `json:"maximum"`
	// Buffer is the memory buffer in percent (5–2000); default 20.
	Buffer int32 `json:"buffer,omitempty"`
	// Priority is the memory weight (0–100).
	Priority int32 `json:"priority,omitempty"`
}

// Operating systems of a guest.
const (
	OSLinux   = "linux"
	OSWindows = "windows"
)

// GuestSpec is one guest agent.
type GuestSpec struct {
	// Name names the guest agent in control methods.
	Name string `json:"name"`
	// VM links the guest to a VM on the host: its total follows the VM's
	// assignment (minus Offset, with TotalLag on grows) and it is
	// unreachable while the VM is not running. Empty for a standalone guest.
	VM string `json:"vm,omitempty"`
	// OS is "linux" (default) or "windows"; only Windows reports commit
	// figures.
	OS string `json:"os,omitempty"`
	// Total is the guest's total memory. For a linked guest it defaults to
	// the VM's assignment minus Offset and is then maintained by the Sim.
	Total Bytes `json:"total,omitempty"`
	// Offset is the firmware and kernel reservation: assigned − total.
	Offset Bytes `json:"offset,omitempty"`
	// Used is memory in use; available = max(total − used, 0).
	Used Bytes `json:"used,omitempty"`
	// PageFile is the Windows page file size; commit limit = total +
	// PageFile.
	PageFile Bytes `json:"pageFile,omitempty"`
	// Committed is the Windows commit charge; 0 means equal to Used.
	Committed Bytes `json:"committed,omitempty"`
	// TotalLag is how long after a grow the guest's total still reports the
	// old value.
	TotalLag Duration `json:"totalLag,omitempty"`
	// SourceDown makes the memory source unreadable: GetMemory fails with
	// UNAVAILABLE, WatchMemory streams end, health reports NOT_SERVING.
	SourceDown bool `json:"sourceDown,omitempty"`

	AgentBehaviour `json:",inline"`
}

// ParseScenario reads a scenario from YAML (or JSON) and validates it.
func ParseScenario(data []byte) (*Scenario, error) {
	var sc Scenario
	if err := yaml.UnmarshalStrict(data, &sc); err != nil {
		return nil, fmt.Errorf("parse scenario: %w", err)
	}
	c := sc.clone()
	c.setDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &sc, nil
}

// LoadScenario reads and validates a scenario file.
func LoadScenario(path string) (*Scenario, error) {
	data, err := os.ReadFile(path) //nolint:gosec // reading a caller-chosen scenario file is the point
	if err != nil {
		return nil, err
	}
	return ParseScenario(data)
}

// Marshal writes the scenario as YAML.
func (sc *Scenario) Marshal() ([]byte, error) {
	return yaml.Marshal(sc)
}

// clone deep-copies the scenario.
func (sc *Scenario) clone() *Scenario {
	data, err := json.Marshal(sc)
	if err != nil {
		panic(fmt.Sprintf("fake: marshal scenario: %v", err)) // only plain data types
	}
	var c Scenario
	if err := json.Unmarshal(data, &c); err != nil {
		panic(fmt.Sprintf("fake: unmarshal scenario: %v", err))
	}
	return &c
}

// defaultCmdletTimeout is the host agent's default per-cmdlet timeout
// (DESIGN §4.2).
const defaultCmdletTimeout = Duration(30_000_000_000)

// setDefaults fills derived defaults in place.
func (sc *Scenario) setDefaults() {
	if sc.Host.CmdletTimeout == 0 {
		sc.Host.CmdletTimeout = defaultCmdletTimeout
	}
	for i := range sc.VMs {
		v := &sc.VMs[i]
		if v.State == "" {
			v.State = "Running"
		}
		if v.ID == "" {
			v.ID = vmID(v.Name)
		}
		if v.DynamicMemory != nil && v.DynamicMemory.Buffer == 0 {
			v.DynamicMemory.Buffer = 20
		}
		if v.Assigned == nil {
			a := v.Startup
			if !holdsMemory(v.State) {
				a = 0
			}
			v.Assigned = &a
		}
	}
	for i := range sc.Guests {
		g := &sc.Guests[i]
		if g.OS == "" {
			g.OS = OSLinux
		}
		if g.VM != "" && g.Total == 0 {
			if v := sc.vm(g.VM); v != nil {
				g.Total = max(*v.Assigned-g.Offset, 0)
			}
		}
	}
}

// vmID derives a stable GUID-shaped ID from a VM name.
func vmID(name string) string {
	h := sha256.Sum256([]byte("hvmc-fake-vm/" + name))
	s := hex.EncodeToString(h[:16])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

func (sc *Scenario) vm(name string) *VMSpec {
	for i := range sc.VMs {
		if sc.VMs[i].Name == name {
			return &sc.VMs[i]
		}
	}
	return nil
}

func (sc *Scenario) guest(name string) *GuestSpec {
	for i := range sc.Guests {
		if sc.Guests[i].Name == name {
			return &sc.Guests[i]
		}
	}
	return nil
}

// Host agent and guest agent RPC names usable in AgentBehaviour.Hang.
var (
	hostMethods  = []string{"GetHost", "ListVMs", "WatchVMs", "SetStartup", "SetDynamicLimits", "ProposePolicy", "RotateCertificate", "InstallCertificate"}
	guestMethods = []string{"GetMemory", "WatchMemory", "RotateCertificate", "InstallCertificate"}
)

// Validate checks a defaulted scenario for consistency.
func (sc *Scenario) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	h := sc.Host
	if h.Name == "" {
		add("host.name is required")
	}
	if h.Total <= 0 {
		add("host.total must be positive")
	}
	if h.Overhead < 0 || h.GrowLatency < 0 || h.CmdletTimeout < 0 {
		add("host: overhead, growLatency and cmdletTimeout must not be negative")
	}
	errs = append(errs, h.validate("host", hostMethods)...)
	for _, p := range []struct {
		field string
		p     *PolicySpec
	}{{"policy", sc.Policy}, {"pendingPolicy", sc.PendingPolicy}} {
		if p.p != nil {
			if err := validatePolicy(toProtoPolicy(p.p)); err != nil {
				add("%s: %w", p.field, err)
			}
		}
	}

	var sumAssigned Bytes
	names := map[string]bool{}
	for _, v := range sc.VMs {
		errs = append(errs, v.validate()...)
		if names[v.Name] {
			add("vm %q: duplicate name", v.Name)
		}
		names[v.Name] = true
		if v.Assigned != nil {
			sumAssigned += *v.Assigned
		}
	}
	if h.Total > 0 && sumAssigned+h.Overhead > h.Total {
		add("host: overhead %s plus assigned memory %s exceed total %s", h.Overhead, sumAssigned, h.Total)
	}

	agents := map[string]bool{h.Name: true}
	for _, g := range sc.Guests {
		where := fmt.Sprintf("guest %q", g.Name)
		if g.Name == "" {
			add("guest name is required")
		}
		if agents[g.Name] {
			add("%s: duplicate agent name", where)
		}
		agents[g.Name] = true
		if g.VM != "" && sc.vm(g.VM) == nil {
			add("%s: vm %q does not exist", where, g.VM)
		}
		if g.OS != OSLinux && g.OS != OSWindows {
			add("%s: os must be %q or %q", where, OSLinux, OSWindows)
		}
		if g.Total < 0 || g.Offset < 0 || g.Used < 0 || g.PageFile < 0 || g.Committed < 0 || g.TotalLag < 0 {
			add("%s: quantities and durations must not be negative", where)
		}
		errs = append(errs, g.validate(where, guestMethods)...)
	}
	return errors.Join(errs...)
}

func (a AgentBehaviour) validate(where string, methods []string) []error {
	var errs []error
	for _, m := range a.Hang {
		if m != "*" && !slices.Contains(methods, m) {
			errs = append(errs, fmt.Errorf("%s: hang: unknown method %q (want one of %s or *)", where, m, strings.Join(methods, ", ")))
		}
	}
	if a.Stale != nil && a.Stale.Age < 0 {
		errs = append(errs, fmt.Errorf("%s: stale.age must not be negative", where))
	}
	if c := a.Certificate; c != nil {
		for _, ip := range c.IPAddresses {
			if net.ParseIP(ip) == nil {
				errs = append(errs, fmt.Errorf("%s: certificate: invalid IP address %q", where, ip))
			}
		}
		if c.FailRotate < 0 || c.FailInstall < 0 {
			errs = append(errs, fmt.Errorf("%s: certificate: failure counts must not be negative", where))
		}
	}
	return errs
}

func (v VMSpec) validate() []error {
	var errs []error
	where := fmt.Sprintf("vm %q", v.Name)
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(where+": "+format, args...)) }
	if v.Name == "" {
		add("name is required")
	}
	if !knownState(v.State) {
		add("unknown state %q", v.State)
	}
	if v.Startup <= 0 || v.Startup%alignment != 0 {
		add("startup must be positive and a multiple of 2Mi")
	}
	if v.Assigned != nil {
		if *v.Assigned < 0 {
			add("assigned must not be negative")
		}
		if !holdsMemory(v.State) && *v.Assigned != 0 {
			add("assigned must be 0 in state %s", v.State)
		}
	}
	if v.ShrinkFloor != nil && *v.ShrinkFloor < 0 {
		add("shrinkFloor must not be negative")
	}
	if dm := v.DynamicMemory; dm != nil {
		if dm.Minimum <= 0 || dm.Minimum%alignment != 0 || dm.Maximum%alignment != 0 {
			add("dynamicMemory minimum and maximum must be positive multiples of 2Mi")
		}
		if dm.Minimum > v.Startup || v.Startup > dm.Maximum {
			add("dynamicMemory needs minimum ≤ startup ≤ maximum")
		}
		if dm.Buffer < 5 || dm.Buffer > 2000 {
			add("dynamicMemory.buffer must be within 5–2000")
		}
		if dm.Priority < 0 || dm.Priority > 100 {
			add("dynamicMemory.priority must be within 0–100")
		}
	}
	return errs
}
