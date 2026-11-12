// envtest runs in the Linux CI job only (DESIGN §12); controller-runtime's
// envtest process package does not build for Windows.

//go:build !windows

package v1alpha1_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/matfax/hvmc/api/v1alpha1"
)

// k8sClient talks to the envtest API server started by TestMain; nil when
// KUBEBUILDER_ASSETS is not set.
var k8sClient client.Client

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		return m.Run() // tests skip or fail in requireEnvtest
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest: %v\n", err)
		return 1
	}
	defer func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
		}
	}()
	scheme := k8sruntime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "scheme: %v\n", err)
		return 1
	}
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "client: %v\n", err)
		return 1
	}
	return m.Run()
}

// requireEnvtest skips the test when no envtest binaries are available,
// except in CI (CI=true), where envtest is mandatory; `make test` provides
// the binaries.
func requireEnvtest(t *testing.T) {
	t.Helper()
	if k8sClient != nil {
		return
	}
	msg := "KUBEBUILDER_ASSETS is not set; run `make test` (or `make envtest`) to download the envtest binaries"
	if os.Getenv("CI") == "true" {
		t.Fatal(msg)
	}
	t.Skip(msg)
}

func q(s string) resource.Quantity { return resource.MustParse(s) }

func qp(s string) *resource.Quantity {
	v := resource.MustParse(s)
	return &v
}

// validHostConfig is the HostConfig sample of DESIGN §5.
func validHostConfig() *v1alpha1.HostConfig {
	return &v1alpha1.HostConfig{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.HostConfigName},
		Spec: v1alpha1.HostConfigSpec{
			Capacity:      qp("0"),
			Reserve:       qp("4Gi"),
			DefaultMargin: qp("2Gi"),
			Step:          qp("256Mi"),
			DMAccounting:  v1alpha1.DMAccountingMaximum,
			StaleAfter:    &metav1.Duration{Duration: 30 * time.Second},
			Agents: []v1alpha1.AgentSpec{{
				Name: "host", Role: v1alpha1.AgentRoleHost, DNSName: "hyperv-01.example.com", Port: 9443,
			}},
			VMs: []v1alpha1.VMSpec{
				{
					Name: "vm-a", Node: "vm-a", GuestOS: v1alpha1.GuestOSLinux, Mode: v1alpha1.VMModeFixed,
					Base: q("3Gi"), Margin: qp("2Gi"),
				},
				{
					Name: "vm-b", Node: "vm-b", GuestOS: v1alpha1.GuestOSWindows, Mode: v1alpha1.VMModeStatic,
					Base: q("4Gi"),
					Policy: &v1alpha1.VMPolicy{
						Min: q("6Gi"), Max: q("24Gi"), Grow: new(true), Shrink: new(true),
						ShrinkHysteresis: qp("1Gi"),
						Dwell:            &metav1.Duration{Duration: 5 * time.Minute},
						Cooldown:         &metav1.Duration{Duration: 60 * time.Second},
					},
				},
				{
					Name: "vm-c", GuestOS: v1alpha1.GuestOSWindows, Mode: v1alpha1.VMModeStatic, Base: q("3Gi"),
					GuestAgent: &v1alpha1.GuestAgentSpec{DNSName: "vm-c.example.com", Port: 9443},
				},
				{
					Name: "vm-probe", GuestOS: v1alpha1.GuestOSWindows, Mode: v1alpha1.VMModeStatic, Base: q("2Gi"),
					Probe:      true,
					GuestAgent: &v1alpha1.GuestAgentSpec{DNSName: "vm-probe.example.com", Port: 9443},
				},
			},
		},
	}
}

// withVM returns a HostConfig whose only VM is v.
func withVM(v v1alpha1.VMSpec) *v1alpha1.HostConfig {
	hc := validHostConfig()
	hc.Spec.VMs = []v1alpha1.VMSpec{v}
	return hc
}

func windowsVM(mode v1alpha1.VMMode) v1alpha1.VMSpec {
	return v1alpha1.VMSpec{Name: "vm-a", GuestOS: v1alpha1.GuestOSWindows, Mode: mode, Base: q("4Gi")}
}

func policy(minimum, maximum string) *v1alpha1.VMPolicy {
	return &v1alpha1.VMPolicy{Min: q(minimum), Max: q(maximum)}
}

// wantInvalid checks that err is an Invalid error mentioning want, or nil
// when want is empty.
func wantInvalid(t *testing.T, err error, want string) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Fatalf("want accepted, got %v", err)
	case want == "":
	case err == nil:
		t.Fatalf("want rejected with %q, got accepted", want)
	case !apierrors.IsInvalid(err):
		t.Fatalf("want Invalid error with %q, got %v", want, err)
	case !strings.Contains(err.Error(), want):
		t.Fatalf("want error containing %q, got %v", want, err)
	}
}

func TestHostConfigValidation(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		obj  func() *v1alpha1.HostConfig
		want string // substring of the rejection; empty = accepted
	}{
		// name must be "default"
		{name: "name default", obj: validHostConfig},
		{name: "name other", want: "HostConfig must be named 'default'", obj: func() *v1alpha1.HostConfig {
			hc := validHostConfig()
			hc.Name = "other"
			return hc
		}},

		// guestOS linux => mode fixed
		{name: "linux fixed", obj: func() *v1alpha1.HostConfig {
			return withVM(v1alpha1.VMSpec{Name: "vm-a", GuestOS: v1alpha1.GuestOSLinux, Mode: v1alpha1.VMModeFixed, Base: q("3Gi")})
		}},
		{name: "windows fixed", obj: func() *v1alpha1.HostConfig { return withVM(windowsVM(v1alpha1.VMModeFixed)) }},
		{name: "linux static", want: "guestOS linux requires mode fixed", obj: func() *v1alpha1.HostConfig {
			return withVM(v1alpha1.VMSpec{Name: "vm-a", GuestOS: v1alpha1.GuestOSLinux, Mode: v1alpha1.VMModeStatic, Base: q("3Gi")})
		}},
		{name: "linux dynamic", want: "guestOS linux requires mode fixed", obj: func() *v1alpha1.HostConfig {
			return withVM(v1alpha1.VMSpec{Name: "vm-a", GuestOS: v1alpha1.GuestOSLinux, Mode: v1alpha1.VMModeDynamic, Base: q("3Gi")})
		}},

		// policy only for static|dynamic
		{name: "policy static", obj: func() *v1alpha1.HostConfig {
			v := windowsVM(v1alpha1.VMModeStatic)
			v.Policy = policy("6Gi", "24Gi")
			return withVM(v)
		}},
		{name: "policy dynamic", obj: func() *v1alpha1.HostConfig {
			v := windowsVM(v1alpha1.VMModeDynamic)
			v.Policy = policy("6Gi", "24Gi")
			return withVM(v)
		}},
		{name: "policy fixed", want: "policy is only allowed for mode static or dynamic", obj: func() *v1alpha1.HostConfig {
			v := windowsVM(v1alpha1.VMModeFixed)
			v.Policy = policy("6Gi", "24Gi")
			return withVM(v)
		}},

		// policy.min <= policy.max
		{name: "policy min equals max", obj: func() *v1alpha1.HostConfig {
			v := windowsVM(v1alpha1.VMModeStatic)
			v.Policy = policy("8Gi", "8192Mi")
			return withVM(v)
		}},
		{name: "policy min above max", want: "min must not be greater than max", obj: func() *v1alpha1.HostConfig {
			v := windowsVM(v1alpha1.VMModeStatic)
			v.Policy = policy("8194Mi", "8Gi")
			return withVM(v)
		}},

		// probe only for windows/static without node
		{name: "probe windows static", obj: func() *v1alpha1.HostConfig {
			v := windowsVM(v1alpha1.VMModeStatic)
			v.Probe = true
			return withVM(v)
		}},
		{name: "probe with node", want: "probe requires guestOS windows, mode static and no node", obj: func() *v1alpha1.HostConfig {
			v := windowsVM(v1alpha1.VMModeStatic)
			v.Probe, v.Node = true, "vm-a"
			return withVM(v)
		}},
		{name: "probe dynamic", want: "probe requires guestOS windows, mode static and no node", obj: func() *v1alpha1.HostConfig {
			v := windowsVM(v1alpha1.VMModeDynamic)
			v.Probe = true
			return withVM(v)
		}},
		{name: "probe linux", want: "probe requires guestOS windows, mode static and no node", obj: func() *v1alpha1.HostConfig {
			v := v1alpha1.VMSpec{Name: "vm-a", GuestOS: v1alpha1.GuestOSLinux, Mode: v1alpha1.VMModeFixed, Base: q("3Gi"), Probe: true}
			return withVM(v)
		}},

		// positive (or, where 0 is meaningful, non-negative) quantities
		{name: "capacity zero", obj: func() *v1alpha1.HostConfig {
			hc := validHostConfig()
			hc.Spec.Capacity = qp("0")
			return hc
		}},
		{name: "capacity negative", want: `spec.capacity: Invalid value: "-1Gi": must not be negative`, obj: func() *v1alpha1.HostConfig {
			hc := validHostConfig()
			hc.Spec.Capacity = qp("-1Gi")
			return hc
		}},
		{name: "reserve zero", obj: func() *v1alpha1.HostConfig {
			hc := validHostConfig()
			hc.Spec.Reserve = qp("0")
			return hc
		}},
		{name: "reserve negative", want: `spec.reserve: Invalid value: "-4Gi": must not be negative`, obj: func() *v1alpha1.HostConfig {
			hc := validHostConfig()
			hc.Spec.Reserve = qp("-4Gi")
			return hc
		}},
		{name: "defaultMargin negative", want: `spec.defaultMargin: Invalid value: "-1": must not be negative`, obj: func() *v1alpha1.HostConfig {
			hc := validHostConfig()
			hc.Spec.DefaultMargin = qp("-1")
			return hc
		}},
		{name: "step zero", want: `spec.step: Invalid value: "0": must be positive`, obj: func() *v1alpha1.HostConfig {
			hc := validHostConfig()
			hc.Spec.Step = qp("0")
			return hc
		}},
		{name: "base zero", want: `spec.vms[0].base: Invalid value: "0": must be positive`, obj: func() *v1alpha1.HostConfig {
			v := windowsVM(v1alpha1.VMModeStatic)
			v.Base = q("0")
			return withVM(v)
		}},
		{name: "margin negative", want: `spec.vms[0].margin: Invalid value: "-512Mi": must not be negative`, obj: func() *v1alpha1.HostConfig {
			v := windowsVM(v1alpha1.VMModeStatic)
			v.Margin = qp("-512Mi")
			return withVM(v)
		}},
		{name: "policy min zero", want: `spec.vms[0].policy.min: Invalid value: "0": must be positive`, obj: func() *v1alpha1.HostConfig {
			v := windowsVM(v1alpha1.VMModeStatic)
			v.Policy = policy("0", "8Gi")
			return withVM(v)
		}},
		{name: "shrinkHysteresis negative", want: `spec.vms[0].policy.shrinkHysteresis: Invalid value: "-1Gi": must not be negative`, obj: func() *v1alpha1.HostConfig {
			v := windowsVM(v1alpha1.VMModeStatic)
			v.Policy = policy("4Gi", "8Gi")
			v.Policy.ShrinkHysteresis = qp("-1Gi")
			return withVM(v)
		}},

		// unique VM and agent names (listType=map)
		{name: "duplicate vm", want: `spec.vms[1]: Duplicate value: {"name":"vm-a"}`, obj: func() *v1alpha1.HostConfig {
			hc := withVM(windowsVM(v1alpha1.VMModeStatic))
			hc.Spec.VMs = append(hc.Spec.VMs, windowsVM(v1alpha1.VMModeDynamic))
			return hc
		}},
		{name: "duplicate agent", want: `spec.agents[1]: Duplicate value: {"name":"host"}`, obj: func() *v1alpha1.HostConfig {
			hc := validHostConfig()
			hc.Spec.Agents = append(hc.Spec.Agents, v1alpha1.AgentSpec{Name: "host", DNSName: "hyperv-02.example.com"})
			return hc
		}},

		// VM and agent names are DNS-1123 labels
		{name: "vm name label", obj: func() *v1alpha1.HostConfig {
			v := windowsVM(v1alpha1.VMModeStatic)
			v.Name = "docker-host-1"
			return withVM(v)
		}},
		{name: "vm name uppercase", want: "spec.vms[0].name: Invalid value", obj: func() *v1alpha1.HostConfig {
			v := windowsVM(v1alpha1.VMModeStatic)
			v.Name = "Docker-Host-1"
			return withVM(v)
		}},
		{name: "vm name with space", want: "spec.vms[0].name: Invalid value", obj: func() *v1alpha1.HostConfig {
			v := windowsVM(v1alpha1.VMModeStatic)
			v.Name = "docker host"
			return withVM(v)
		}},
		{name: "vm name too long", want: "spec.vms[0].name: Too long", obj: func() *v1alpha1.HostConfig {
			v := windowsVM(v1alpha1.VMModeStatic)
			v.Name = strings.Repeat("a", 64)
			return withVM(v)
		}},
		{name: "agent name with dot", want: "spec.agents[0].name: Invalid value", obj: func() *v1alpha1.HostConfig {
			hc := validHostConfig()
			hc.Spec.Agents[0].Name = "hyperv-01.example.com"
			return hc
		}},
		{name: "agent name trailing dash", want: "spec.agents[0].name: Invalid value", obj: func() *v1alpha1.HostConfig {
			hc := validHostConfig()
			hc.Spec.Agents[0].Name = "host-"
			return hc
		}},

		// enums
		{name: "guestOS unknown", want: "spec.vms[0].guestOS: Unsupported value", obj: func() *v1alpha1.HostConfig {
			v := windowsVM(v1alpha1.VMModeStatic)
			v.GuestOS = "plan9"
			return withVM(v)
		}},
		{name: "mode unknown", want: "spec.vms[0].mode: Unsupported value", obj: func() *v1alpha1.HostConfig {
			return withVM(windowsVM("elastic"))
		}},
		{name: "dmAccounting unknown", want: "spec.dmAccounting: Unsupported value", obj: func() *v1alpha1.HostConfig {
			hc := validHostConfig()
			hc.Spec.DMAccounting = "minimum"
			return hc
		}},
		{name: "agent role guest", want: "spec.agents[0].role: Unsupported value", obj: func() *v1alpha1.HostConfig {
			hc := validHostConfig()
			hc.Spec.Agents[0].Role = "guest"
			return hc
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := tc.obj()
			err := k8sClient.Create(ctx, obj)
			if err == nil {
				t.Cleanup(func() { deleteObject(ctx, t, obj) })
			}
			wantInvalid(t, err, tc.want)
		})
	}
}

func TestHostConfigDefaults(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()

	hc := &v1alpha1.HostConfig{
		ObjectMeta: metav1.ObjectMeta{Name: v1alpha1.HostConfigName},
		Spec: v1alpha1.HostConfigSpec{
			Agents: []v1alpha1.AgentSpec{{Name: "host", DNSName: "hyperv-01.example.com"}},
			VMs: []v1alpha1.VMSpec{{
				Name: "vm-b", GuestOS: v1alpha1.GuestOSWindows, Mode: v1alpha1.VMModeStatic, Base: q("4Gi"),
				Policy:     policy("6Gi", "24Gi"),
				GuestAgent: &v1alpha1.GuestAgentSpec{DNSName: "vm-b.example.com"},
			}},
		},
	}
	if err := k8sClient.Create(ctx, hc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deleteObject(ctx, t, hc) })

	got := &v1alpha1.HostConfig{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(hc), got); err != nil {
		t.Fatal(err)
	}
	want := v1alpha1.HostConfigSpec{
		Capacity:      qp("0"),
		Reserve:       qp("4Gi"),
		DefaultMargin: qp("2Gi"),
		Step:          qp("256Mi"),
		DMAccounting:  v1alpha1.DMAccountingMaximum,
		StaleAfter:    &metav1.Duration{Duration: 30 * time.Second},
		Reconciler:    &v1alpha1.ReconcilerSpec{PriorityOverQueue: new(true)},
		Agents: []v1alpha1.AgentSpec{{
			Name: "host", Role: v1alpha1.AgentRoleHost, DNSName: "hyperv-01.example.com", Port: 9443,
		}},
		VMs: []v1alpha1.VMSpec{{
			Name: "vm-b", GuestOS: v1alpha1.GuestOSWindows, Mode: v1alpha1.VMModeStatic, Base: q("4Gi"),
			Policy: &v1alpha1.VMPolicy{
				Min: q("6Gi"), Max: q("24Gi"), Grow: new(true), Shrink: new(true),
				ShrinkHysteresis: qp("1Gi"),
				Dwell:            &metav1.Duration{Duration: 5 * time.Minute},
				Cooldown:         &metav1.Duration{Duration: 60 * time.Second},
			},
			GuestAgent: &v1alpha1.GuestAgentSpec{DNSName: "vm-b.example.com", Port: 9443},
		}},
	}
	if diff := cmp.Diff(want, got.Spec, cmp.Comparer(func(a, b resource.Quantity) bool { return a.Cmp(b) == 0 })); diff != "" {
		t.Errorf("defaulted spec (-want +got):\n%s", diff)
	}
}

func validLease(name string, kind v1alpha1.OwnerKind) *v1alpha1.MemoryLease {
	ml := &v1alpha1.MemoryLease{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.MemoryLeaseSpec{
			Bytes: q("6Gi"),
			Owner: v1alpha1.LeaseOwner{Kind: kind, Name: "example"},
		},
	}
	switch kind {
	case v1alpha1.OwnerKindPod:
		ml.Spec.Owner.Namespace, ml.Spec.Owner.UID = "default", types.UID("00000000-0000-0000-0000-000000000001")
		ml.Spec.CandidateVMs = []string{"vm-a", "vm-b"}
	case v1alpha1.OwnerKindExternal:
		ml.Spec.VM, ml.Spec.Client, ml.Spec.IdempotencyKey = "vm-c", "orchestrator", "key-1"
		ml.Spec.TTL = &metav1.Duration{Duration: 2 * time.Hour}
	default:
		ml.Spec.VM = "vm-c"
	}
	return ml
}

func TestMemoryLeaseCreateValidation(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		obj  func() *v1alpha1.MemoryLease
		want string
	}{
		// owner.kind enum
		{name: "owner Pod", obj: func() *v1alpha1.MemoryLease { return validLease("kind-pod", v1alpha1.OwnerKindPod) }},
		{name: "owner External", obj: func() *v1alpha1.MemoryLease { return validLease("kind-ext", v1alpha1.OwnerKindExternal) }},
		{name: "owner Reconciler", obj: func() *v1alpha1.MemoryLease { return validLease("kind-rec", v1alpha1.OwnerKindReconciler) }},
		{name: "owner Probe", obj: func() *v1alpha1.MemoryLease { return validLease("kind-probe", v1alpha1.OwnerKindProbe) }},
		{name: "owner Node", want: "spec.owner.kind: Unsupported value", obj: func() *v1alpha1.MemoryLease {
			ml := validLease("kind-node", v1alpha1.OwnerKindProbe)
			ml.Spec.Owner.Kind = "Node"
			return ml
		}},

		// bytes positive
		{name: "bytes zero", want: `spec.bytes: Invalid value: "0": must be positive`, obj: func() *v1alpha1.MemoryLease {
			ml := validLease("bytes-zero", v1alpha1.OwnerKindPod)
			ml.Spec.Bytes = q("0")
			return ml
		}},
		{name: "bytes negative", want: `spec.bytes: Invalid value: "-1Gi": must be positive`, obj: func() *v1alpha1.MemoryLease {
			ml := validLease("bytes-neg", v1alpha1.OwnerKindPod)
			ml.Spec.Bytes = q("-1Gi")
			return ml
		}},

		// vm required unless owner.kind Pod (DESIGN §5)
		{name: "pod without vm", obj: func() *v1alpha1.MemoryLease { return validLease("vm-pod", v1alpha1.OwnerKindPod) }},
		{name: "external without vm", want: "vm is required unless owner.kind is Pod", obj: func() *v1alpha1.MemoryLease {
			ml := validLease("vm-ext", v1alpha1.OwnerKindExternal)
			ml.Spec.VM = ""
			return ml
		}},

		// ttl only for External (DESIGN §5)
		{name: "pod with ttl", want: "ttl is only allowed for owner.kind External", obj: func() *v1alpha1.MemoryLease {
			ml := validLease("ttl-pod", v1alpha1.OwnerKindPod)
			ml.Spec.TTL = &metav1.Duration{Duration: time.Hour}
			return ml
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := tc.obj()
			err := k8sClient.Create(ctx, obj)
			if err == nil {
				t.Cleanup(func() { deleteObject(ctx, t, obj) })
			}
			wantInvalid(t, err, tc.want)
		})
	}
}

func TestMemoryLeaseUpdateValidation(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()

	const (
		immutable  = "bytes, priorityClass, owner, client and idempotencyKey are immutable"
		vmOnce     = "vm can be set once and is immutable afterwards"
		candidates = "spec.candidateVMs: Invalid value: candidateVMs can only change while the lease is Pending"
	)
	for i, tc := range []struct {
		name  string
		kind  v1alpha1.OwnerKind
		phase v1alpha1.LeasePhase // status.phase before the update; empty = no status
		patch string              // JSON merge patch applied to the lease
		want  string
	}{
		// vm settable once
		{name: "set vm on pod lease", kind: v1alpha1.OwnerKindPod, phase: v1alpha1.LeasePending, patch: `{"spec":{"vm":"vm-a"}}`},
		{name: "change vm", kind: v1alpha1.OwnerKindExternal, phase: v1alpha1.LeasePending, patch: `{"spec":{"vm":"vm-a"}}`, want: vmOnce},
		{name: "remove vm", kind: v1alpha1.OwnerKindReconciler, phase: v1alpha1.LeasePending, patch: `{"spec":{"vm":null}}`, want: vmOnce},
		{name: "same vm", kind: v1alpha1.OwnerKindExternal, phase: v1alpha1.LeaseGranted, patch: `{"spec":{"vm":"vm-c"}}`},

		// immutable fields
		{name: "change bytes", kind: v1alpha1.OwnerKindExternal, phase: v1alpha1.LeasePending, patch: `{"spec":{"bytes":"8Gi"}}`, want: immutable},
		{name: "bytes same value other form", kind: v1alpha1.OwnerKindExternal, phase: v1alpha1.LeaseGranted, patch: `{"spec":{"bytes":"6144Mi"}}`},
		{name: "bytes same value as integer", kind: v1alpha1.OwnerKindExternal, phase: v1alpha1.LeaseGranted, patch: `{"spec":{"bytes":6442450944}}`},
		{name: "change priorityClass", kind: v1alpha1.OwnerKindPod, phase: v1alpha1.LeasePending, patch: `{"spec":{"priorityClass":5}}`, want: immutable},
		{name: "change owner name", kind: v1alpha1.OwnerKindPod, phase: v1alpha1.LeasePending, patch: `{"spec":{"owner":{"name":"other"}}}`, want: immutable},
		{name: "change owner kind", kind: v1alpha1.OwnerKindReconciler, phase: v1alpha1.LeasePending, patch: `{"spec":{"owner":{"kind":"Probe"}}}`, want: immutable},
		{name: "change client", kind: v1alpha1.OwnerKindExternal, phase: v1alpha1.LeasePending, patch: `{"spec":{"client":"other"}}`, want: immutable},
		{name: "remove idempotencyKey", kind: v1alpha1.OwnerKindExternal, phase: v1alpha1.LeasePending, patch: `{"spec":{"idempotencyKey":null}}`, want: immutable},
		{name: "add idempotencyKey", kind: v1alpha1.OwnerKindProbe, phase: v1alpha1.LeasePending, patch: `{"spec":{"idempotencyKey":"key-2"}}`, want: immutable},

		// ttl and deadline stay mutable
		{name: "change ttl when granted", kind: v1alpha1.OwnerKindExternal, phase: v1alpha1.LeaseGranted, patch: `{"spec":{"ttl":"30m"}}`},
		{name: "set deadline", kind: v1alpha1.OwnerKindPod, phase: v1alpha1.LeasePending, patch: `{"spec":{"deadline":"2026-11-01T12:00:00Z"}}`},
		{name: "change deadline when granted", kind: v1alpha1.OwnerKindProbe, phase: v1alpha1.LeaseGranted, patch: `{"spec":{"deadline":"2026-11-01T12:00:00Z"}}`},

		// candidateVMs only while Pending
		{name: "candidates while pending", kind: v1alpha1.OwnerKindPod, phase: v1alpha1.LeasePending, patch: `{"spec":{"candidateVMs":["vm-b"]}}`},
		{name: "candidates without status", kind: v1alpha1.OwnerKindPod, patch: `{"spec":{"candidateVMs":["vm-b"]}}`},
		{name: "candidates reordered when granted", kind: v1alpha1.OwnerKindPod, phase: v1alpha1.LeaseGranted, patch: `{"spec":{"candidateVMs":["vm-b","vm-a"]}}`},
		{name: "candidates while reserved", kind: v1alpha1.OwnerKindPod, phase: v1alpha1.LeaseReserved, patch: `{"spec":{"candidateVMs":["vm-b"]}}`, want: candidates},
		{name: "candidates removed when granted", kind: v1alpha1.OwnerKindPod, phase: v1alpha1.LeaseGranted, patch: `{"spec":{"candidateVMs":null}}`, want: candidates},
		{name: "candidates added when granted", kind: v1alpha1.OwnerKindProbe, phase: v1alpha1.LeaseGranted, patch: `{"spec":{"candidateVMs":["vm-c"]}}`, want: candidates},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ml := validLease(fmt.Sprintf("update-%d", i), tc.kind)
			if err := k8sClient.Create(ctx, ml); err != nil {
				t.Fatalf("create: %v", err)
			}
			t.Cleanup(func() { deleteObject(ctx, t, ml) })
			if tc.phase != "" {
				ml.Status.Phase = tc.phase
				if err := k8sClient.Status().Update(ctx, ml); err != nil {
					t.Fatalf("set status: %v", err)
				}
			}
			err := k8sClient.Patch(ctx, ml, client.RawPatch(types.MergePatchType, []byte(tc.patch)))
			wantInvalid(t, err, tc.want)
		})
	}
}

// TestMemoryLeaseStatusUpdate checks that status writes are not blocked by
// the spec rules.
func TestMemoryLeaseStatusUpdate(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()

	ml := validLease("status-update", v1alpha1.OwnerKindExternal)
	if err := k8sClient.Create(ctx, ml); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deleteObject(ctx, t, ml) })
	for _, p := range []v1alpha1.LeasePhase{v1alpha1.LeasePending, v1alpha1.LeaseReserved, v1alpha1.LeaseGranted, v1alpha1.LeaseReleased} {
		ml.Status.Phase = p
		if err := k8sClient.Status().Update(ctx, ml); err != nil {
			t.Fatalf("status %s: %v", p, err)
		}
	}
	ml.Status.Phase = "Done"
	wantInvalid(t, k8sClient.Status().Update(ctx, ml), "status.phase: Unsupported value")
}

func deleteObject(ctx context.Context, t *testing.T, obj client.Object) {
	t.Helper()
	if err := k8sClient.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("delete %s: %v", obj.GetName(), err)
	}
}
