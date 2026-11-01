package ledger

import (
	"errors"
	"fmt"
	"time"
)

// Byte units.
const (
	Ki int64 = 1 << 10
	Mi int64 = 1 << 20
	Gi int64 = 1 << 30
)

// Defaults from DESIGN §3.1, §4.4 and §5.
const (
	DefaultReserve    = 4 * Gi
	DefaultMargin     = 2 * Gi
	DefaultStep       = 256 * Mi
	DefaultStaleAfter = 30 * time.Second
	// DefaultHostAgent is the agent name used for the host agent when
	// Config.HostAgent is empty.
	DefaultHostAgent AgentName = "host"
)

// VMName is the Hyper-V name of a VM.
type VMName string

// AgentName identifies an agent (HostConfig.spec.agents[].name, or a VM's
// guest agent).
type AgentName string

// GuestOS is the operating system inside a VM.
type GuestOS string

// Guest operating systems.
const (
	GuestLinux   GuestOS = "linux"
	GuestWindows GuestOS = "windows"
)

// Mode is how hvmc treats a managed VM's memory (DESIGN §3.1).
type Mode string

// VM modes.
const (
	// ModeFixed is static memory set at boot and never changed by hvmc;
	// leases are admission-only within its headroom.
	ModeFixed Mode = "fixed"
	// ModeStatic is static memory resized at runtime (Windows guests only).
	ModeStatic Mode = "static"
	// ModeDynamic is Hyper-V Dynamic Memory (Windows guests only).
	ModeDynamic Mode = "dynamic"
)

// DMAccounting selects what a managed Dynamic Memory VM commits (DESIGN §3.1).
type DMAccounting string

// Dynamic Memory accounting modes.
const (
	DMAccountingMaximum  DMAccounting = "maximum"
	DMAccountingAssigned DMAccounting = "assigned"
)

// Config is the ledger's view of HostConfig.spec. All quantities are bytes.
// Quantities are taken as given (zero means zero); DefaultConfig returns the
// documented defaults. Step, StaleAfter, DMAccounting and HostAgent fall back
// to their defaults when left zero.
type Config struct {
	// Capacity of the host; 0 means "the host agent's reported total".
	Capacity int64
	// Reserve is kept free on the host at all times.
	Reserve int64
	// DefaultMargin is used for VMs without their own Margin.
	DefaultMargin int64
	// Step is the quantum for every resize target.
	Step int64
	// DMAccounting applies to managed Dynamic Memory VMs.
	DMAccounting DMAccounting
	// StaleAfter is the age after which observed values count as missing.
	StaleAfter time.Duration
	// PriorityOverQueue queues Reconciler requests at priority +∞ (§3.4).
	PriorityOverQueue bool
	// HostAgent is the name of the host agent whose state freezes the ledger.
	HostAgent AgentName
	// VMs are the managed VMs. VMs the host reports but that are not listed
	// here are unmanaged: read for the pool, never granted on.
	VMs []VMConfig
}

// VMConfig is one managed VM (HostConfig.spec.vms[]).
type VMConfig struct {
	Name VMName
	// Node is the Kubernetes node name, empty for facade-only VMs.
	Node    string
	GuestOS GuestOS
	Mode    Mode
	// Base is what the VM needs with zero leases.
	Base int64
	// Margin overrides Config.DefaultMargin when set.
	Margin *int64
	// Policy bounds resizes; nil means no bounds known to the ledger.
	Policy *Policy
	// Probe marks the disposable VM used by `hvmc check --probe-resize`.
	Probe bool
	// GuestAgent names the agent whose readiness gates this VM; empty means
	// the VM name.
	GuestAgent AgentName
}

// Policy holds the resize bounds of a VM (HostConfig.spec.vms[].policy).
// A zero Min or Max means unbounded on that side.
type Policy struct {
	Min int64
	Max int64
}

// DefaultConfig returns a Config with every documented default set and no VMs.
func DefaultConfig() Config {
	return Config{
		Reserve:           DefaultReserve,
		DefaultMargin:     DefaultMargin,
		Step:              DefaultStep,
		DMAccounting:      DMAccountingMaximum,
		StaleAfter:        DefaultStaleAfter,
		PriorityOverQueue: true,
		HostAgent:         DefaultHostAgent,
	}
}

// Validate reports configuration the ledger cannot work with. HostConfig
// validation (CEL) rejects the same combinations at the API edge.
func (c Config) Validate() error {
	var errs []error
	if c.Capacity < 0 || c.Reserve < 0 || c.DefaultMargin < 0 || c.Step < 0 {
		errs = append(errs, errors.New("capacity, reserve, defaultMargin and step must not be negative"))
	}
	switch c.DMAccounting {
	case "", DMAccountingMaximum, DMAccountingAssigned:
	default:
		errs = append(errs, fmt.Errorf("dmAccounting %q: want maximum or assigned", c.DMAccounting))
	}
	seen := make(map[VMName]bool, len(c.VMs))
	for _, vm := range c.VMs {
		errs = append(errs, vm.validate(seen)...)
	}
	return errors.Join(errs...)
}

func (v VMConfig) validate(seen map[VMName]bool) []error {
	p := &vmProblems{name: v.Name}
	if v.Name == "" {
		p.errs = append(p.errs, errors.New("vm with empty name"))
	}
	if seen[v.Name] {
		p.fail("duplicate name")
	}
	seen[v.Name] = true
	v.validateMode(p)
	if v.Base < 0 || (v.Margin != nil && *v.Margin < 0) {
		p.fail("base and margin must not be negative")
	}
	v.validatePolicy(p)
	if v.Probe && (v.GuestOS != GuestWindows || v.Mode != ModeStatic || v.Node != "") {
		p.fail("probe VMs must be windows, static and not mapped to a node")
	}
	return p.errs
}

// vmProblems collects the validation errors of one VM.
type vmProblems struct {
	name VMName
	errs []error
}

// fail records a problem, prefixed with the VM's name.
func (p *vmProblems) fail(format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf("vm %q: "+format, append([]any{p.name}, args...)...))
}

// validateMode checks the mode and guest OS and their combination.
func (v VMConfig) validateMode(p *vmProblems) {
	switch v.Mode {
	case ModeFixed, ModeStatic, ModeDynamic:
	default:
		p.fail("mode %q: want fixed, static or dynamic", v.Mode)
	}
	switch v.GuestOS {
	case GuestLinux:
		if v.Mode != ModeFixed {
			p.fail("linux guests must use mode fixed")
		}
	case GuestWindows:
	default:
		p.fail("guestOS %q: want linux or windows", v.GuestOS)
	}
}

// validatePolicy checks the optional resize policy.
func (v VMConfig) validatePolicy(p *vmProblems) {
	pol := v.Policy
	if pol == nil {
		return
	}
	if v.Mode == ModeFixed {
		p.fail("policy is only allowed for static and dynamic VMs")
	}
	if pol.Min < 0 || pol.Max < 0 || (pol.Max > 0 && pol.Min > pol.Max) {
		p.fail("policy needs 0 ≤ min ≤ max")
	}
}

// normalize returns a deep copy of c with zero-valued Step, StaleAfter,
// DMAccounting and HostAgent replaced by their defaults.
func (c Config) normalize() Config {
	out := c
	if out.Step <= 0 {
		out.Step = DefaultStep
	}
	if out.StaleAfter <= 0 {
		out.StaleAfter = DefaultStaleAfter
	}
	if out.DMAccounting == "" {
		out.DMAccounting = DMAccountingMaximum
	}
	if out.HostAgent == "" {
		out.HostAgent = DefaultHostAgent
	}
	out.VMs = make([]VMConfig, len(c.VMs))
	for i, vm := range c.VMs {
		if vm.Margin != nil {
			m := *vm.Margin
			vm.Margin = &m
		}
		if vm.Policy != nil {
			p := *vm.Policy
			vm.Policy = &p
		}
		if vm.GuestAgent == "" {
			vm.GuestAgent = AgentName(vm.Name)
		}
		out.VMs[i] = vm
	}
	return out
}

// margin returns the VM's effective margin.
func (c Config) margin(vm VMConfig) int64 {
	if vm.Margin != nil {
		return *vm.Margin
	}
	return c.DefaultMargin
}
