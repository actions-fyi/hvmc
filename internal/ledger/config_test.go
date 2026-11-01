package ledger

import (
	"strings"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if c.Reserve != 4*Gi || c.DefaultMargin != 2*Gi || c.Step != 256*Mi || c.StaleAfter != 30*time.Second ||
		c.DMAccounting != DMAccountingMaximum || !c.PriorityOverQueue || c.HostAgent != "host" {
		t.Errorf("DefaultConfig = %+v", c)
	}
	if err := testConfig().Validate(); err != nil {
		t.Errorf("testConfig invalid: %v", err)
	}
}

func TestConfigValidate(t *testing.T) {
	win := func(mod func(*VMConfig)) Config {
		c := DefaultConfig()
		vm := VMConfig{Name: "vm-a", GuestOS: GuestWindows, Mode: ModeStatic, Base: Gi}
		mod(&vm)
		c.VMs = []VMConfig{vm}
		return c
	}
	for _, tc := range []struct {
		name string
		cfg  Config
		want string // substring of the error, "" for valid
	}{
		{"valid", win(func(*VMConfig) {}), ""},
		{"negative reserve", Config{Reserve: -1}, "must not be negative"},
		{"bad dmAccounting", Config{DMAccounting: "peak"}, "dmAccounting"},
		{"empty name", win(func(v *VMConfig) { v.Name = "" }), "empty name"},
		{"bad mode", win(func(v *VMConfig) { v.Mode = "elastic" }), "mode"},
		{"linux must be fixed", win(func(v *VMConfig) { v.GuestOS = GuestLinux }), "linux guests"},
		{"bad guestOS", win(func(v *VMConfig) { v.GuestOS = "plan9" }), "guestOS"},
		{"negative margin", win(func(v *VMConfig) { v.Margin = ptr(-Gi) }), "must not be negative"},
		{"policy on fixed", win(func(v *VMConfig) { v.Mode = ModeFixed; v.Policy = &Policy{} }), "policy is only"},
		{"policy min > max", win(func(v *VMConfig) { v.Policy = &Policy{Min: 2 * Gi, Max: Gi} }), "min ≤ max"},
		{"probe with node", win(func(v *VMConfig) { v.Probe = true; v.Node = "n" }), "probe"},
		{"probe valid", win(func(v *VMConfig) { v.Probe = true }), ""},
	} {
		err := tc.cfg.Validate()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: unexpected error %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: error %v, want %q", tc.name, err, tc.want)
		}
	}
	dup := DefaultConfig()
	dup.VMs = []VMConfig{{Name: "a", GuestOS: GuestWindows, Mode: ModeStatic}, {Name: "a", GuestOS: GuestWindows, Mode: ModeStatic}}
	if err := dup.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate: %v", err)
	}
}

func TestConfigNormalize(t *testing.T) {
	in := Config{VMs: []VMConfig{{Name: "vm-a", Margin: ptr(Gi), Policy: &Policy{Max: 8 * Gi}}}}
	out := in.normalize()
	if out.Step != DefaultStep || out.StaleAfter != DefaultStaleAfter || out.DMAccounting != DMAccountingMaximum || out.HostAgent != DefaultHostAgent {
		t.Errorf("defaults not applied: %+v", out)
	}
	if out.VMs[0].GuestAgent != "vm-a" {
		t.Errorf("guest agent default %q", out.VMs[0].GuestAgent)
	}
	*in.VMs[0].Margin = 5
	in.VMs[0].Policy.Max = 5
	if *out.VMs[0].Margin != Gi || out.VMs[0].Policy.Max != 8*Gi {
		t.Errorf("normalize aliases the input")
	}
	if out.margin(out.VMs[0]) != Gi || out.margin(VMConfig{}) != 0 {
		t.Errorf("margin lookup")
	}
}

func TestClocks(t *testing.T) {
	if d := time.Since(SystemClock{}.Now()); d < 0 || d > time.Minute {
		t.Errorf("SystemClock off by %v", d)
	}
	c := NewFakeClock(t0)
	if !c.Now().Equal(t0) {
		t.Errorf("fake clock start")
	}
	if got := c.Advance(time.Second); !got.Equal(t0.Add(time.Second)) || !c.Now().Equal(got) {
		t.Errorf("Advance = %v", got)
	}
	c.Set(t0)
	if !c.Now().Equal(t0) {
		t.Errorf("Set")
	}
	// New with a nil clock uses the system clock.
	if s := New(Config{}, nil).Snapshot(); time.Since(s.At) > time.Minute || !s.Frozen {
		t.Errorf("nil clock snapshot %+v", s)
	}
}
