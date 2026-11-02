package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HostConfigName is the name of the HostConfig singleton.
const HostConfigName = "default"

// GuestOS is the operating system of a managed VM.
// +kubebuilder:validation:Enum=linux;windows
type GuestOS string

// Guest operating systems.
const (
	GuestOSLinux   GuestOS = "linux"
	GuestOSWindows GuestOS = "windows"
)

// VMMode is how hvmc may change a managed VM's memory (DESIGN §3.1).
// +kubebuilder:validation:Enum=fixed;static;dynamic
type VMMode string

// VM modes.
const (
	// VMModeFixed is static memory set at boot and never changed by hvmc.
	// Required for Linux guests.
	VMModeFixed VMMode = "fixed"
	// VMModeStatic is static memory resized at runtime (Windows guests only).
	VMModeStatic VMMode = "static"
	// VMModeDynamic is Hyper-V Dynamic Memory (Windows guests only).
	VMModeDynamic VMMode = "dynamic"
)

// DMAccounting selects what a managed Dynamic Memory VM counts as committed.
// +kubebuilder:validation:Enum=maximum;assigned
type DMAccounting string

// Dynamic Memory accounting modes.
const (
	// DMAccountingMaximum counts MaximumBytes (worst case).
	DMAccountingMaximum DMAccounting = "maximum"
	// DMAccountingAssigned counts max(assigned, floor).
	DMAccountingAssigned DMAccounting = "assigned"
)

// AgentRole is the role of an agent declared in spec.agents.
// +kubebuilder:validation:Enum=host
type AgentRole string

// Agent roles.
const (
	// AgentRoleHost is the host agent on the Hyper-V host. Guest agents are
	// declared per VM (spec.vms[].guestAgent), not in spec.agents.
	AgentRoleHost AgentRole = "host"
)

// AgentState is the coordinator's view of an agent's health (DESIGN §4.8).
type AgentState string

// Agent states.
const (
	AgentStateReady       AgentState = "Ready"
	AgentStateDegraded    AgentState = "Degraded"
	AgentStateUnreachable AgentState = "Unreachable"
)

// VMState is the ledger's view of a managed VM.
type VMState string

// VM states.
const (
	VMStateReady VMState = "Ready"
	// VMStateUnknown means the VM's guest agent is not Ready (DESIGN §4.8).
	VMStateUnknown VMState = "Unknown"
)

// Condition types used in HostConfig status.
const (
	// ConditionReady is set on the HostConfig and on every agent entry.
	ConditionReady = "Ready"
	// ConditionLedgerFrozen is True while the host agent is not Ready.
	ConditionLedgerFrozen = "LedgerFrozen"
	// ConditionCertificateValid is set on every agent entry.
	ConditionCertificateValid = "CertificateValid"
	// ConditionPolicyInSync is set on the host agent entry.
	ConditionPolicyInSync = "PolicyInSync"
)

// HostConfigSpec declares the Hyper-V host, its agents and the VMs the ledger
// manages (DESIGN §5).
type HostConfigSpec struct {
	// capacity is the host memory the ledger may hand out. 0 reads the
	// physical memory reported by the host agent.
	// +optional
	// +kubebuilder:default="0"
	// +kubebuilder:validation:XValidation:rule="!quantity(string(self)).isLessThan(quantity('0'))",message="must not be negative"
	Capacity *resource.Quantity `json:"capacity,omitempty"`

	// reserve is host memory that is never handed out.
	// +optional
	// +kubebuilder:default="4Gi"
	// +kubebuilder:validation:XValidation:rule="!quantity(string(self)).isLessThan(quantity('0'))",message="must not be negative"
	Reserve *resource.Quantity `json:"reserve,omitempty"`

	// defaultMargin is the free memory kept in every managed VM unless the VM
	// sets its own margin. Absolute, never a percentage.
	// +optional
	// +kubebuilder:default="2Gi"
	// +kubebuilder:validation:XValidation:rule="!quantity(string(self)).isLessThan(quantity('0'))",message="must not be negative"
	DefaultMargin *resource.Quantity `json:"defaultMargin,omitempty"`

	// step is the granularity all resize targets are rounded up to.
	// +optional
	// +kubebuilder:default="256Mi"
	// +kubebuilder:validation:XValidation:rule="quantity(string(self)).isGreaterThan(quantity('0'))",message="must be positive"
	Step *resource.Quantity `json:"step,omitempty"`

	// dmAccounting selects what a managed Dynamic Memory VM counts as
	// committed: its MaximumBytes or max(assigned, floor).
	// +optional
	// +kubebuilder:default=maximum
	DMAccounting DMAccounting `json:"dmAccounting,omitempty"`

	// staleAfter is the age after which agent data is treated as missing.
	// +optional
	// +kubebuilder:default="30s"
	StaleAfter *metav1.Duration `json:"staleAfter,omitempty"`

	// reconciler holds settings of the per-VM reconciler (DESIGN §9).
	// +optional
	// +kubebuilder:default={}
	Reconciler *ReconcilerSpec `json:"reconciler,omitempty"`

	// agents declares the host agent. Guest agents are declared per VM.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=16
	Agents []AgentSpec `json:"agents,omitempty"`

	// vms lists every VM the ledger manages. VMs on the host that are not
	// listed are still read for the pool, but never changed.
	// +optional
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=256
	VMs []VMSpec `json:"vms,omitempty"`
}

// ReconcilerSpec holds settings of the reconciler.
type ReconcilerSpec struct {
	// priorityOverQueue queues reconciler grows for running workloads ahead
	// of every waiting request (DESIGN §3.4).
	// +optional
	// +kubebuilder:default=true
	PriorityOverQueue *bool `json:"priorityOverQueue,omitempty"`
}

// AgentSpec declares an agent that is not tied to a VM entry.
type AgentSpec struct {
	// name identifies the agent in its bootstrap token Secret
	// (hvmc-bootstrap-<name>) and in status.agents. A DNS-1123 label, so it
	// is used in object names as is.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// role of the agent.
	// +optional
	// +kubebuilder:default=host
	Role AgentRole `json:"role,omitempty"`

	// dnsName is the agent's DNS name, used for its serving certificate and,
	// unless address is set, to resolve its address.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	DNSName string `json:"dnsName"`

	// address optionally overrides the address resolved from dnsName.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Address string `json:"address,omitempty"`

	// port is the agent's gRPC port.
	// +optional
	// +kubebuilder:default=9443
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
}

// GuestAgentSpec locates the guest agent of a VM.
type GuestAgentSpec struct {
	// dnsName is the guest agent's DNS name. Required for VMs without a
	// node; for node-mapped VMs the address defaults to the node's InternalIP.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	DNSName string `json:"dnsName,omitempty"`

	// address optionally overrides the derived address.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Address string `json:"address,omitempty"`

	// port is the guest agent's gRPC port.
	// +optional
	// +kubebuilder:default=9443
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
}

// Quantities inside spec.vms[] are declared as bounded strings instead of
// the usual int-or-string schema: CEL cost estimation treats every
// int-or-string value as unbounded (request size), so per-item quantity()
// rules on a list would exceed the CRD's validation cost budget.

// VMSpec declares one managed VM. Its memory sizes are written as strings,
// e.g. "4Gi".
//
// +kubebuilder:validation:XValidation:rule="self.guestOS != 'linux' || self.mode == 'fixed'",message="guestOS linux requires mode fixed"
// +kubebuilder:validation:XValidation:rule="!has(self.policy) || self.mode in ['static', 'dynamic']",message="policy is only allowed for mode static or dynamic"
// +kubebuilder:validation:XValidation:rule="!has(self.probe) || !self.probe || (self.guestOS == 'windows' && self.mode == 'static' && !has(self.node))",message="probe requires guestOS windows, mode static and no node"
type VMSpec struct {
	// name is the Hyper-V VM name; it also names the VM's guest agent and
	// its bootstrap token Secret (hvmc-bootstrap-<name>). A DNS-1123 label,
	// so it is used in object names as is.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// node is the Kubernetes node name of the VM. It enables the extender for
	// this VM; without it the VM is a facade-only target.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Node string `json:"node,omitempty"`

	// guestOS is the VM's operating system.
	// +required
	GuestOS GuestOS `json:"guestOS"`

	// mode is how hvmc may change the VM's memory. Linux guests must be fixed.
	// +required
	Mode VMMode `json:"mode"`

	// base is what the VM needs with zero leases (OS, kubelet, container
	// runtime or Docker daemon).
	// +required
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^(\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE](\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))))?$`
	// +kubebuilder:validation:XValidation:rule="quantity(self).isGreaterThan(quantity('0'))",message="must be positive"
	Base resource.Quantity `json:"base"`

	// margin overrides spec.defaultMargin for this VM.
	// +optional
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^(\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE](\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))))?$`
	// +kubebuilder:validation:XValidation:rule="!quantity(self).isLessThan(quantity('0'))",message="must not be negative"
	Margin *resource.Quantity `json:"margin,omitempty"`

	// policy enables the reconciler for a static or dynamic VM. Without it
	// the VM is admission-only.
	// +optional
	Policy *VMPolicy `json:"policy,omitempty"`

	// probe marks a disposable VM used by `hvmc check --probe-resize`. It
	// never carries workloads.
	// +optional
	Probe bool `json:"probe,omitempty"`

	// guestAgent locates the VM's guest agent.
	// +optional
	GuestAgent *GuestAgentSpec `json:"guestAgent,omitempty"`
}

// VMPolicy bounds and tunes the reconciler for one VM (DESIGN §9).
//
// +kubebuilder:validation:XValidation:rule="quantity(self.min).compareTo(quantity(self.max)) <= 0",message="min must not be greater than max"
type VMPolicy struct {
	// min is the lowest assignment the reconciler may shrink to.
	// +required
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^(\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE](\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))))?$`
	// +kubebuilder:validation:XValidation:rule="quantity(self).isGreaterThan(quantity('0'))",message="must be positive"
	Min resource.Quantity `json:"min"`

	// max is the highest assignment the reconciler may grow to.
	// +required
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^(\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE](\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))))?$`
	// +kubebuilder:validation:XValidation:rule="quantity(self).isGreaterThan(quantity('0'))",message="must be positive"
	Max resource.Quantity `json:"max"`

	// grow allows the reconciler to grow the VM.
	// +optional
	// +kubebuilder:default=true
	Grow *bool `json:"grow,omitempty"`

	// shrink allows the reconciler to shrink the VM.
	// +optional
	// +kubebuilder:default=true
	Shrink *bool `json:"shrink,omitempty"`

	// shrinkHysteresis: shrink only when free memory exceeds margin plus
	// this value.
	// +optional
	// +kubebuilder:default="1Gi"
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^(\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))(([KMGTPE]i)|[numkMGTPE]|([eE](\+|-)?(([0-9]+(\.[0-9]*)?)|(\.[0-9]+))))?$`
	// +kubebuilder:validation:XValidation:rule="!quantity(self).isLessThan(quantity('0'))",message="must not be negative"
	ShrinkHysteresis *resource.Quantity `json:"shrinkHysteresis,omitempty"`

	// dwell is how long free memory must stay above the shrink threshold.
	// +optional
	// +kubebuilder:default="5m"
	Dwell *metav1.Duration `json:"dwell,omitempty"`

	// cooldown is the minimum time between two resizes of this VM.
	// +optional
	// +kubebuilder:default="60s"
	Cooldown *metav1.Duration `json:"cooldown,omitempty"`
}

// HostConfigStatus is the observed state of the host, its agents and VMs.
type HostConfigStatus struct {
	// observedGeneration is the spec generation the status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// observedHost is the host as last reported by the host agent.
	// +optional
	ObservedHost *ObservedHost `json:"observedHost,omitempty"`

	// agents reports every declared agent, host and guests.
	// +optional
	// +listType=map
	// +listMapKey=name
	Agents []AgentStatus `json:"agents,omitempty"`

	// vms reports the ledger's view of every managed VM.
	// +optional
	// +listType=map
	// +listMapKey=name
	VMs []VMStatus `json:"vms,omitempty"`

	// conditions: Ready, LedgerFrozen.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ObservedHost is the host's memory as seen by the ledger.
type ObservedHost struct {
	// capacity is the effective capacity used by the ledger.
	// +optional
	Capacity *resource.Quantity `json:"capacity,omitempty"`
	// free is the host's free memory.
	// +optional
	Free *resource.Quantity `json:"free,omitempty"`
	// pool is the memory available for growing VMs (DESIGN §3.1).
	// +optional
	Pool *resource.Quantity `json:"pool,omitempty"`
	// observedAt is when the host agent reported these values.
	// +optional
	ObservedAt *metav1.Time `json:"observedAt,omitempty"`
}

// AgentStatus is the health of one agent.
type AgentStatus struct {
	// name of the agent: a spec.agents name or a VM name for guest agents.
	// +required
	Name string `json:"name"`
	// state: Ready, Degraded or Unreachable.
	// +optional
	State AgentState `json:"state,omitempty"`
	// version reported by the agent.
	// +optional
	Version string `json:"version,omitempty"`
	// lastSeen is the last successful contact.
	// +optional
	LastSeen *metav1.Time `json:"lastSeen,omitempty"`
	// certExpiry is when the agent's serving certificate expires.
	// +optional
	CertExpiry *metav1.Time `json:"certExpiry,omitempty"`
	// conditions: Ready, CertificateValid, PolicyInSync.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// VMStatus is the ledger's view of one managed VM (DESIGN §3.1).
type VMStatus struct {
	// name is the Hyper-V VM name.
	// +required
	Name string `json:"name"`
	// assigned is Hyper-V's MemoryAssigned.
	// +optional
	Assigned *resource.Quantity `json:"assigned,omitempty"`
	// startup is Hyper-V's MemoryStartup.
	// +optional
	Startup *resource.Quantity `json:"startup,omitempty"`
	// floor is the assignment the VM must never go below.
	// +optional
	Floor *resource.Quantity `json:"floor,omitempty"`
	// headroom is assigned − floor; may be negative.
	// +optional
	Headroom *resource.Quantity `json:"headroom,omitempty"`
	// leased is the sum of active leases on the VM.
	// +optional
	Leased *resource.Quantity `json:"leased,omitempty"`
	// used is the guest's total − available.
	// +optional
	Used *resource.Quantity `json:"used,omitempty"`
	// available is the guest's available memory.
	// +optional
	Available *resource.Quantity `json:"available,omitempty"`
	// state: Ready or Unknown.
	// +optional
	State VMState `json:"state,omitempty"`
	// conditions of the VM.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// HostConfig is the singleton (named "default") that configures the Hyper-V
// host, its agents and the VMs hvmc manages.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=hvmc
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Frozen",type=string,JSONPath=`.status.conditions[?(@.type=="LedgerFrozen")].status`
// +kubebuilder:printcolumn:name="Capacity",type=string,JSONPath=`.status.observedHost.capacity`
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.status.observedHost.pool`
// +kubebuilder:printcolumn:name="Reserve",type=string,JSONPath=`.spec.reserve`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default'",message="HostConfig must be named 'default'"
type HostConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the desired configuration.
	// +optional
	// +kubebuilder:default={}
	Spec HostConfigSpec `json:"spec,omitempty"`

	// status is the observed state, written by the coordinator.
	// +optional
	Status HostConfigStatus `json:"status,omitempty"`
}

// HostConfigList is a list of HostConfig.
//
// +kubebuilder:object:root=true
type HostConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HostConfig `json:"items"`
}
