package hostconfig_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/matfax/hvmc/api/v1alpha1"
	"github.com/matfax/hvmc/internal/hostconfig"
	"github.com/matfax/hvmc/internal/ledger"
)

func qty(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func ptr[T any](v T) *T { return &v }

func TestToLedgerConfig(t *testing.T) {
	gi := ledger.Gi
	for _, tc := range []struct {
		name    string
		spec    v1alpha1.HostConfigSpec
		want    ledger.Config
		wantErr string
	}{
		{
			name: "empty spec gets the CRD defaults",
			want: ledger.DefaultConfig(),
		},
		{
			name: "every field",
			spec: v1alpha1.HostConfigSpec{
				Capacity:      qty("96Gi"),
				Reserve:       qty("8Gi"),
				DefaultMargin: qty("1Gi"),
				Step:          qty("512Mi"),
				DMAccounting:  v1alpha1.DMAccountingAssigned,
				StaleAfter:    &metav1.Duration{Duration: time.Minute},
				Reconciler:    &v1alpha1.ReconcilerSpec{PriorityOverQueue: ptr(false)},
				Agents:        []v1alpha1.AgentSpec{{Name: "hyperv", Role: v1alpha1.AgentRoleHost, DNSName: "hyperv.example.com"}},
				VMs: []v1alpha1.VMSpec{
					{Name: "linux-node-1", Node: "linux-node-1", GuestOS: v1alpha1.GuestOSLinux, Mode: v1alpha1.VMModeFixed, Base: *qty("3Gi"), Margin: qty("2Gi")},
					{
						Name: "vm-a", GuestOS: v1alpha1.GuestOSWindows, Mode: v1alpha1.VMModeStatic, Base: *qty("4Gi"),
						Policy: &v1alpha1.VMPolicy{Min: *qty("6Gi"), Max: *qty("24Gi"), Grow: ptr(true), ShrinkHysteresis: qty("1Gi")},
					},
					{Name: "vm-dm", GuestOS: v1alpha1.GuestOSWindows, Mode: v1alpha1.VMModeDynamic, Base: *qty("2048Mi")},
					{Name: "probe-1", GuestOS: v1alpha1.GuestOSWindows, Mode: v1alpha1.VMModeStatic, Base: *qty("2Gi"), Probe: true},
				},
			},
			want: ledger.Config{
				Capacity: 96 * gi, Reserve: 8 * gi, DefaultMargin: gi, Step: 512 * ledger.Mi,
				DMAccounting: ledger.DMAccountingAssigned, StaleAfter: time.Minute, PriorityOverQueue: false,
				HostAgent: "hyperv",
				VMs: []ledger.VMConfig{
					{Name: "linux-node-1", Node: "linux-node-1", GuestOS: ledger.GuestLinux, Mode: ledger.ModeFixed, Base: 3 * gi, Margin: ptr(2 * gi)},
					{Name: "vm-a", GuestOS: ledger.GuestWindows, Mode: ledger.ModeStatic, Base: 4 * gi, Policy: &ledger.Policy{Min: 6 * gi, Max: 24 * gi}},
					{Name: "vm-dm", GuestOS: ledger.GuestWindows, Mode: ledger.ModeDynamic, Base: 2 * gi},
					{Name: "probe-1", GuestOS: ledger.GuestWindows, Mode: ledger.ModeStatic, Base: 2 * gi, Probe: true},
				},
			},
		},
		{
			name: "zero quantities stay zero",
			spec: v1alpha1.HostConfigSpec{Capacity: qty("0"), Reserve: qty("0"), DefaultMargin: qty("0")},
			want: func() ledger.Config {
				c := ledger.DefaultConfig()
				c.Reserve, c.DefaultMargin = 0, 0
				return c
			}(),
		},
		{
			name: "agent without role is the host agent",
			spec: v1alpha1.HostConfigSpec{Agents: []v1alpha1.AgentSpec{{Name: "hv", DNSName: "hv.example.com"}}},
			want: func() ledger.Config {
				c := ledger.DefaultConfig()
				c.HostAgent = "hv"
				return c
			}(),
		},
		{
			name:    "two host agents",
			spec:    v1alpha1.HostConfigSpec{Agents: []v1alpha1.AgentSpec{{Name: "a"}, {Name: "b", Role: v1alpha1.AgentRoleHost}}},
			wantErr: "2 host agents",
		},
		{
			name:    "fractional bytes",
			spec:    v1alpha1.HostConfigSpec{VMs: []v1alpha1.VMSpec{{Name: "vm-a", GuestOS: "windows", Mode: "static", Base: *qty("100m")}}},
			wantErr: "spec.vms[0].base: 100m is not a whole number of bytes",
		},
		{
			name:    "zero step",
			spec:    v1alpha1.HostConfigSpec{Step: qty("0")},
			wantErr: "spec.step must be positive",
		},
		{
			name:    "zero staleAfter",
			spec:    v1alpha1.HostConfigSpec{StaleAfter: &metav1.Duration{}},
			wantErr: "spec.staleAfter must be positive",
		},
		{
			name:    "negative reserve",
			spec:    v1alpha1.HostConfigSpec{Reserve: qty("-1Gi")},
			wantErr: "must not be negative",
		},
		{
			name:    "linux guest must be fixed",
			spec:    v1alpha1.HostConfigSpec{VMs: []v1alpha1.VMSpec{{Name: "linux-node-1", GuestOS: "linux", Mode: "static", Base: *qty("3Gi")}}},
			wantErr: "linux guests must use mode fixed",
		},
		{
			name:    "unknown dmAccounting",
			spec:    v1alpha1.HostConfigSpec{DMAccounting: "average"},
			wantErr: `dmAccounting "average"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := hostconfig.ToLedgerConfig(&v1alpha1.HostConfig{Spec: tc.spec})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("config (-want +got):\n%s", diff)
			}
		})
	}
}

func TestToLedgerConfigNil(t *testing.T) {
	if _, err := hostconfig.ToLedgerConfig(nil); err == nil {
		t.Fatal("want an error for a nil HostConfig")
	}
}

const manifest = `apiVersion: hvmc.fax.fyi/v1alpha1
kind: HostConfig
metadata:
  name: default
spec:
  reserve: 8Gi
  agents:
    - name: host
      dnsName: hyperv-01.example.com
  vms:
    - name: vm-a
      guestOS: windows
      mode: static
      base: 4Gi
      policy: {min: 6Gi, max: 24Gi, dwell: 5m}
`

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		data    string
		wantErr string
	}{
		{name: "manifest", data: manifest},
		{name: "without apiVersion and kind", data: "spec:\n  reserve: 8Gi\n"},
		{name: "JSON", data: `{"kind":"HostConfig","spec":{"reserve":"8Gi"}}`},
		{name: "wrong kind", data: "kind: MemoryLease\n", wantErr: `kind "MemoryLease"`},
		{name: "wrong apiVersion", data: "apiVersion: hvmc.fax.fyi/v1\nkind: HostConfig\n", wantErr: `apiVersion "hvmc.fax.fyi/v1"`},
		{name: "unknown field", data: "spec:\n  reserved: 8Gi\n", wantErr: "unknown field"},
		{name: "bad quantity", data: "spec:\n  reserve: lots\n", wantErr: "parse HostConfig"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hc, err := hostconfig.Parse([]byte(tc.data))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := hc.Spec.Reserve.String(); got != "8Gi" {
				t.Errorf("reserve = %s, want 8Gi", got)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hostconfig.yaml")
	if err := os.WriteFile(path, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	hc, err := hostconfig.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := hostconfig.ToLedgerConfig(hc)
	if err != nil {
		t.Fatal(err)
	}
	want := ledger.DefaultConfig()
	want.Reserve = 8 * ledger.Gi
	want.VMs = []ledger.VMConfig{{
		Name: "vm-a", GuestOS: ledger.GuestWindows, Mode: ledger.ModeStatic, Base: 4 * ledger.Gi,
		Policy: &ledger.Policy{Min: 6 * ledger.Gi, Max: 24 * ledger.Gi},
	}}
	if diff := cmp.Diff(want, cfg); diff != "" {
		t.Errorf("config (-want +got):\n%s", diff)
	}
	if _, err := hostconfig.Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("want an error for a missing file")
	}
}
