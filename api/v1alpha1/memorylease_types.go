package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// OwnerKind is the kind of requester a MemoryLease belongs to.
// +kubebuilder:validation:Enum=Pod;External;Reconciler;Probe
type OwnerKind string

// Owner kinds.
const (
	// OwnerKindPod is a pod scheduled through the extender.
	OwnerKindPod OwnerKind = "Pod"
	// OwnerKindExternal is a facade client outside Kubernetes.
	OwnerKindExternal OwnerKind = "External"
	// OwnerKindReconciler is an internal reservation of the reconciler.
	OwnerKindReconciler OwnerKind = "Reconciler"
	// OwnerKindProbe is a lease created by `hvmc check --probe-resize`.
	OwnerKindProbe OwnerKind = "Probe"
)

// LeasePhase is the state of a MemoryLease (DESIGN §3.3).
// +kubebuilder:validation:Enum=Pending;Reserved;Resizing;Granted;Released;Failed
type LeasePhase string

// Lease phases.
const (
	LeasePending  LeasePhase = "Pending"
	LeaseReserved LeasePhase = "Reserved"
	LeaseResizing LeasePhase = "Resizing"
	LeaseGranted  LeasePhase = "Granted"
	LeaseReleased LeasePhase = "Released"
	LeaseFailed   LeasePhase = "Failed"
)

// Reasons for a Failed lease that the spec names (DESIGN §3.3, §3.5).
const (
	// ReasonTimeout: the deadline passed before the lease was granted.
	ReasonTimeout = "Timeout"
	// ReasonLost: after a restart the VM no longer covers a granted lease.
	ReasonLost = "Lost"
)

// MemoryLeaseSpec is a request for memory on one VM of the host.
//
// spec is immutable except ttl, deadline, candidateVMs (while the lease is
// Pending) and a one-time set of vm. The immutability rules below name the
// immutable fields explicitly; a new spec field must be added to them.
//
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.vm) || (has(self.vm) && self.vm == oldSelf.vm)",message="vm can be set once and is immutable afterwards"
// +kubebuilder:validation:XValidation:rule="quantity(string(self.bytes)).compareTo(quantity(string(oldSelf.bytes))) == 0 && self.priorityClass == oldSelf.priorityClass && self.owner == oldSelf.owner && has(self.client) == has(oldSelf.client) && (!has(self.client) || self.client == oldSelf.client) && has(self.idempotencyKey) == has(oldSelf.idempotencyKey) && (!has(self.idempotencyKey) || self.idempotencyKey == oldSelf.idempotencyKey)",message="bytes, priorityClass, owner, client and idempotencyKey are immutable"
// +kubebuilder:validation:XValidation:rule="has(self.vm) || self.owner.kind == 'Pod'",message="vm is required unless owner.kind is Pod"
// +kubebuilder:validation:XValidation:rule="!has(self.ttl) || self.owner.kind == 'External'",message="ttl is only allowed for owner.kind External"
type MemoryLeaseSpec struct {
	// vm is the Hyper-V name of the VM the memory is leased on. Required for
	// every owner kind except Pod; pod leases get it once, at bind, from the
	// chosen node.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	VM string `json:"vm,omitempty"`

	// candidateVMs are the VMs of the filter's node list (pod leases). They
	// decide head-of-line blocking in the queue (DESIGN §3.4). Mutable only
	// while the lease is Pending.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=253
	CandidateVMs []string `json:"candidateVMs,omitempty"`

	// bytes is the leased amount of memory.
	// +required
	// +kubebuilder:validation:XValidation:rule="quantity(string(self)).isGreaterThan(quantity('0'))",message="must be positive"
	Bytes resource.Quantity `json:"bytes"`

	// priorityClass orders the queue: higher first, then arrival.
	// +optional
	// +kubebuilder:default=0
	PriorityClass int32 `json:"priorityClass"`

	// owner identifies the requester.
	// +required
	Owner LeaseOwner `json:"owner"`

	// client is the facade client name (External leases).
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Client string `json:"client,omitempty"`

	// idempotencyKey is the facade client's Idempotency-Key (External leases).
	// +optional
	// +kubebuilder:validation:MaxLength=253
	IdempotencyKey string `json:"idempotencyKey,omitempty"`

	// ttl is the lifetime of an External lease after its last heartbeat.
	// +optional
	TTL *metav1.Duration `json:"ttl,omitempty"`

	// deadline fails the lease if it is not granted by then.
	// +optional
	Deadline *metav1.Time `json:"deadline,omitempty"`
}

// LeaseOwner identifies who holds a lease. Pod leases are also
// owner-referenced to the pod.
type LeaseOwner struct {
	// kind of the requester.
	// +required
	Kind OwnerKind `json:"kind"`
	// namespace of the owner (Pod).
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace,omitempty"`
	// name of the owner.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name,omitempty"`
	// uid of the owner (Pod).
	// +optional
	UID types.UID `json:"uid,omitempty"`
}

// MemoryLeaseStatus is the lease's progress through the state machine.
type MemoryLeaseStatus struct {
	// phase of the lease.
	// +optional
	Phase LeasePhase `json:"phase,omitempty"`
	// reason is a CamelCase reason for the current phase, e.g. Timeout.
	// +optional
	Reason string `json:"reason,omitempty"`
	// message is a human-readable explanation of the current phase.
	// +optional
	Message string `json:"message,omitempty"`
	// enqueuedAt is the lease's queue position (arrival time).
	// +optional
	EnqueuedAt *metav1.Time `json:"enqueuedAt,omitempty"`
	// grantedAt is when the lease became Granted.
	// +optional
	GrantedAt *metav1.Time `json:"grantedAt,omitempty"`
	// expiresAt is when an External lease expires without a heartbeat.
	// +optional
	ExpiresAt *metav1.Time `json:"expiresAt,omitempty"`
	// assignedBefore is the VM's assignment before the grant's resize.
	// +optional
	AssignedBefore *resource.Quantity `json:"assignedBefore,omitempty"`
	// assignedAfter is the VM's assignment after the grant's resize.
	// +optional
	AssignedAfter *resource.Quantity `json:"assignedAfter,omitempty"`
}

// MemoryLease is a lease of memory on a VM of the host. It is the single
// source of truth for the ledger (DESIGN §3.5).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories=hvmc
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="VM",type=string,JSONPath=`.spec.vm`
// +kubebuilder:printcolumn:name="Bytes",type=string,JSONPath=`.spec.bytes`
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.owner.kind`
// +kubebuilder:printcolumn:name="Priority",type=integer,JSONPath=`.spec.priorityClass`,priority=1
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.reason`,priority=1
// +kubebuilder:printcolumn:name="Expires",type=date,JSONPath=`.status.expiresAt`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.status) || !has(oldSelf.status.phase) || oldSelf.status.phase == 'Pending' || (has(self.spec.candidateVMs) == has(oldSelf.spec.candidateVMs) && (!has(self.spec.candidateVMs) || self.spec.candidateVMs == oldSelf.spec.candidateVMs))",message="candidateVMs can only change while the lease is Pending",fieldPath=".spec.candidateVMs"
type MemoryLease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the request.
	// +required
	Spec MemoryLeaseSpec `json:"spec"`

	// status is written by the coordinator.
	// +optional
	Status MemoryLeaseStatus `json:"status,omitempty"`
}

// MemoryLeaseList is a list of MemoryLease.
//
// +kubebuilder:object:root=true
type MemoryLeaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MemoryLease `json:"items"`
}
