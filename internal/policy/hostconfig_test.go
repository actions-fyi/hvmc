package policy

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/matfax/hvmc/api/v1alpha1"
)

func qp(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func vmPolicy(minimum, maximum string) *v1alpha1.VMPolicy {
	return &v1alpha1.VMPolicy{Min: resource.MustParse(minimum), Max: resource.MustParse(maximum)}
}

// designHostConfig is the HostConfig of DESIGN §5.
func designHostConfig() *v1alpha1.HostConfig {
	return &v1alpha1.HostConfig{Spec: v1alpha1.HostConfigSpec{
		Capacity: qp("0"),
		Reserve:  qp("4Gi"),
		VMs: []v1alpha1.VMSpec{
			{Name: "linux-node-1", Node: "linux-node-1", GuestOS: v1alpha1.GuestOSLinux, Mode: v1alpha1.VMModeFixed, Base: resource.MustParse("3Gi"), Margin: qp("2Gi")},
			{Name: "windows-node-1", Node: "windows-node-1", GuestOS: v1alpha1.GuestOSWindows, Mode: v1alpha1.VMModeStatic, Base: resource.MustParse("4Gi"), Policy: vmPolicy("6Gi", "24Gi")},
			{Name: "docker-host-1", GuestOS: v1alpha1.GuestOSWindows, Mode: v1alpha1.VMModeStatic, Base: resource.MustParse("3Gi")},
			{Name: "probe-1", GuestOS: v1alpha1.GuestOSWindows, Mode: v1alpha1.VMModeStatic, Base: resource.MustParse("2Gi"), Probe: true},
		},
	}}
}

func TestFromHostConfig(t *testing.T) {
	win := func(name string, mode v1alpha1.VMMode, base string, pol *v1alpha1.VMPolicy) v1alpha1.VMSpec {
		return v1alpha1.VMSpec{Name: name, GuestOS: v1alpha1.GuestOSWindows, Mode: mode, Base: resource.MustParse(base), Policy: pol}
	}
	withSpec := func(capacity, reserve *resource.Quantity, vms ...v1alpha1.VMSpec) *v1alpha1.HostConfig {
		return &v1alpha1.HostConfig{Spec: v1alpha1.HostConfigSpec{Capacity: capacity, Reserve: reserve, VMs: vms}}
	}
	observed := func(hc *v1alpha1.HostConfig, capacity *resource.Quantity) *v1alpha1.HostConfig {
		hc.Status.ObservedHost = &v1alpha1.ObservedHost{Capacity: capacity}
		return hc
	}
	tests := []struct {
		name string
		hc   *v1alpha1.HostConfig
		want []VM
	}{
		{name: "nil", hc: nil, want: []VM{}},
		{name: "no VMs", hc: &v1alpha1.HostConfig{}, want: []VM{}},
		{
			name: "DESIGN example",
			hc:   designHostConfig(),
			want: []VM{
				{Name: "linux-node-1", Fixed: true},
				{Name: "windows-node-1", Min: 6 * gi, Max: 24 * gi},
				{Name: "docker-host-1", Min: 3 * gi, Max: Unbounded},
				{Name: "probe-1", Min: 2 * gi, Max: Unbounded},
			},
		},
		{
			name: "linux is fixed whatever the mode",
			hc: withSpec(nil, nil,
				v1alpha1.VMSpec{Name: "l1", GuestOS: v1alpha1.GuestOSLinux, Mode: v1alpha1.VMModeFixed, Base: resource.MustParse("2Gi")},
				v1alpha1.VMSpec{Name: "l2", GuestOS: v1alpha1.GuestOSLinux, Mode: v1alpha1.VMModeStatic, Base: resource.MustParse("2Gi"), Policy: vmPolicy("2Gi", "4Gi")},
			),
			want: []VM{{Name: "l1", Fixed: true}, {Name: "l2", Fixed: true}},
		},
		{
			name: "windows fixed",
			hc:   withSpec(nil, nil, win("w", v1alpha1.VMModeFixed, "4Gi", nil)),
			want: []VM{{Name: "w", Fixed: true}},
		},
		{
			name: "unknown mode is fixed",
			hc:   withSpec(nil, nil, win("w", "", "4Gi", nil), win("x", "balloon", "4Gi", vmPolicy("1Gi", "2Gi"))),
			want: []VM{{Name: "w", Fixed: true}, {Name: "x", Fixed: true}},
		},
		{
			name: "static and dynamic with policy",
			hc: withSpec(qp("64Gi"), qp("4Gi"),
				win("s", v1alpha1.VMModeStatic, "4Gi", vmPolicy("6Gi", "24Gi")),
				win("d", v1alpha1.VMModeDynamic, "2Gi", vmPolicy("2048Mi", "128Gi")),
			),
			want: []VM{{Name: "s", Min: 6 * gi, Max: 24 * gi}, {Name: "d", Min: 2 * gi, Max: 128 * gi}},
		},
		{
			name: "without policy, spec capacity 0 and nothing observed: [base, unbounded]",
			hc:   withSpec(qp("0"), qp("8Gi"), win("s", v1alpha1.VMModeStatic, "4Gi", nil), win("d", v1alpha1.VMModeDynamic, "3Gi", nil)),
			want: []VM{{Name: "s", Min: 4 * gi, Max: Unbounded}, {Name: "d", Min: 3 * gi, Max: Unbounded}},
		},
		{
			name: "without policy and spec capacity set: [base, capacity - reserve]",
			hc:   withSpec(qp("64Gi"), qp("8Gi"), win("s", v1alpha1.VMModeStatic, "4Gi", nil), win("d", v1alpha1.VMModeDynamic, "3Gi", nil)),
			want: []VM{{Name: "s", Min: 4 * gi, Max: 56 * gi}, {Name: "d", Min: 3 * gi, Max: 56 * gi}},
		},
		{
			name: "without policy, spec capacity wins over observed",
			hc:   observed(withSpec(qp("64Gi"), qp("8Gi"), win("s", v1alpha1.VMModeStatic, "4Gi", nil)), qp("128Gi")),
			want: []VM{{Name: "s", Min: 4 * gi, Max: 56 * gi}},
		},
		{
			name: "without policy, spec capacity 0, observed capacity: [base, observed - reserve]",
			hc:   observed(withSpec(qp("0"), qp("8Gi"), win("s", v1alpha1.VMModeStatic, "4Gi", nil), win("d", v1alpha1.VMModeDynamic, "3Gi", nil)), qp("128Gi")),
			want: []VM{{Name: "s", Min: 4 * gi, Max: 120 * gi}, {Name: "d", Min: 3 * gi, Max: 120 * gi}},
		},
		{
			name: "without policy, spec capacity unset, observed capacity",
			hc:   observed(withSpec(nil, qp("8Gi"), win("s", v1alpha1.VMModeStatic, "4Gi", nil)), qp("32Gi")),
			want: []VM{{Name: "s", Min: 4 * gi, Max: 24 * gi}},
		},
		{
			name: "without policy, spec capacity 0, observedHost without capacity: unbounded",
			hc:   observed(withSpec(qp("0"), qp("8Gi"), win("s", v1alpha1.VMModeStatic, "4Gi", nil)), nil),
			want: []VM{{Name: "s", Min: 4 * gi, Max: Unbounded}},
		},
		{
			name: "without policy, spec capacity 0, observed capacity 0: unbounded",
			hc:   observed(withSpec(qp("0"), qp("8Gi"), win("s", v1alpha1.VMModeStatic, "4Gi", nil)), qp("0")),
			want: []VM{{Name: "s", Min: 4 * gi, Max: Unbounded}},
		},
		{
			name: "observed capacity does not affect VMs with a policy or fixed VMs",
			hc: observed(withSpec(qp("0"), qp("8Gi"),
				win("s", v1alpha1.VMModeStatic, "4Gi", vmPolicy("6Gi", "24Gi")),
				v1alpha1.VMSpec{Name: "l", GuestOS: v1alpha1.GuestOSLinux, Mode: v1alpha1.VMModeFixed, Base: resource.MustParse("2Gi")},
			), qp("128Gi")),
			want: []VM{{Name: "s", Min: 6 * gi, Max: 24 * gi}, {Name: "l", Fixed: true}},
		},
		{
			name: "without policy, capacity set, reserve defaulted to 4Gi",
			hc:   withSpec(qp("64Gi"), nil, win("s", v1alpha1.VMModeStatic, "4Gi", nil)),
			want: []VM{{Name: "s", Min: 4 * gi, Max: 60 * gi}},
		},
		{
			name: "without policy, capacity - reserve below base: max = base",
			hc:   withSpec(qp("8Gi"), qp("6Gi"), win("s", v1alpha1.VMModeStatic, "4Gi", nil)),
			want: []VM{{Name: "s", Min: 4 * gi, Max: 4 * gi}},
		},
		{
			name: "fractional bytes round up",
			hc:   withSpec(nil, nil, win("s", v1alpha1.VMModeStatic, "4Gi", &v1alpha1.VMPolicy{Min: resource.MustParse("1500m"), Max: resource.MustParse("2.5")})),
			want: []VM{{Name: "s", Min: 2, Max: 3}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FromHostConfig(tt.hc)
			if diff := cmp.Diff(Policy{VMs: tt.want}, got); diff != "" {
				t.Fatalf("FromHostConfig mismatch (-want +got):\n%s", diff)
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("FromHostConfig result invalid: %v", err)
			}
		})
	}
}

func TestFromHostConfigDigestIgnoresNonPolicyFields(t *testing.T) {
	a := designHostConfig()
	b := designHostConfig()
	b.Spec.DefaultMargin = qp("4Gi")
	b.Spec.Reserve = qp("16Gi") // capacity is 0 and not observed, so reserve bounds nothing
	b.Spec.VMs[0].Base = resource.MustParse("8Gi")
	b.Spec.VMs[1].Base = resource.MustParse("5Gi")
	b.Spec.VMs[1].Policy.Shrink = new(false)
	b.Spec.VMs[1].Policy.Max = resource.MustParse("24576Mi")
	b.Spec.VMs[0], b.Spec.VMs[3] = b.Spec.VMs[3], b.Spec.VMs[0]
	b.Status.VMs = []v1alpha1.VMStatus{{Name: "linux-node-1"}}
	b.Status.ObservedHost = &v1alpha1.ObservedHost{Free: qp("10Gi"), Pool: qp("4Gi")}
	if FromHostConfig(a).Digest() != FromHostConfig(b).Digest() {
		t.Fatalf("digests differ:\n%s\n%s", FromHostConfig(a).Canonical(), FromHostConfig(b).Canonical())
	}
}

func TestFromHostConfigFirstObservationChangesDigest(t *testing.T) {
	hc := designHostConfig()
	before := FromHostConfig(hc)
	hc.Status.ObservedHost = &v1alpha1.ObservedHost{Capacity: qp("128Gi")}
	after := FromHostConfig(hc)
	if before.Digest() == after.Digest() {
		t.Fatal("digest unchanged after the host capacity was observed")
	}
	want := "~ docker-host-1: max unbounded -> 124Gi\n~ probe-1: max unbounded -> 124Gi\n"
	if got := Diff(before, after).String(); got != want {
		t.Fatalf("Diff =\n%s\nwant\n%s", got, want)
	}
}
