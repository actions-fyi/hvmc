package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matfax/hvmc/internal/policy"
)

const gib = int64(1) << 30

// policyDirs writes agent.yaml with active as its policy and, if pending
// is set, policy.pending.yaml.
func policyDirs(t *testing.T, active policy.Policy, pending *policy.Policy) (config, state string) {
	t.Helper()
	state = t.TempDir()
	config = filepath.Join(state, "agent.yaml")
	if err := os.WriteFile(config, []byte("name: hyperv-1\nrole: host\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := policy.Save(config, active); err != nil {
		t.Fatal(err)
	}
	if pending != nil {
		if err := policy.Save(filepath.Join(state, "policy.pending.yaml"), *pending); err != nil {
			t.Fatal(err)
		}
	}
	return config, state
}

func runSync(t *testing.T, input, config, state string) (int, string, string) {
	t.Helper()
	e := env{now: func() time.Time { return testNow }, stdin: strings.NewReader(input)}
	return runCLIEnv(t, e, "agent", "policy", "sync", "--config", config, "--state-dir", state)
}

func activePolicy() policy.Policy {
	return policy.Policy{VMs: []policy.VM{{Name: "windows-node-1", Min: 6 * gib, Max: 24 * gib}, {Name: "linux-node-1", Fixed: true}}}
}

func proposedPolicy() policy.Policy {
	return policy.Policy{VMs: []policy.VM{{Name: "windows-node-1", Min: 6 * gib, Max: 32 * gib}, {Name: "docker-host-1", Min: 4 * gib, Max: 16 * gib}}}
}

func TestPolicySyncActivates(t *testing.T) {
	proposed := proposedPolicy()
	config, state := policyDirs(t, activePolicy(), &proposed)
	code, out, errOut := runSync(t, "yes\n", config, state)
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	for _, want := range []string{
		"+ docker-host-1: writable, min 4Gi, max 16Gi",
		"- linux-node-1: read-only (fixed)",
		"~ windows-node-1: max 24Gi -> 32Gi",
		"Activate this policy? [y/N]: ",
		"Activated policy " + proposed.Digest(),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	got, err := policy.Load(config)
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest() != proposed.Digest() {
		t.Errorf("active policy %s, want %s", got.Digest(), proposed.Digest())
	}
	if _, err := os.Stat(filepath.Join(state, "policy.pending.yaml")); !os.IsNotExist(err) {
		t.Errorf("pending file still there: %v", err)
	}
	// Other settings of agent.yaml are kept.
	data, _ := os.ReadFile(config)
	if !strings.Contains(string(data), "name: hyperv-1") {
		t.Errorf("agent.yaml lost its settings:\n%s", data)
	}
}

func TestPolicySyncDeclined(t *testing.T) {
	for _, input := range []string{"n\n", "\n", "", "maybe\n"} {
		proposed := proposedPolicy()
		config, state := policyDirs(t, activePolicy(), &proposed)
		code, out, errOut := runSync(t, input, config, state)
		if code != ExitOK {
			t.Fatalf("input %q: exit %d, stderr %q", input, code, errOut)
		}
		if !strings.Contains(out, "Not activated; the policy stays pending.") {
			t.Errorf("input %q: output %q", input, out)
		}
		got, _ := policy.Load(config)
		if got.Digest() != activePolicy().Digest() {
			t.Errorf("input %q: active policy changed", input)
		}
		if _, err := os.Stat(filepath.Join(state, "policy.pending.yaml")); err != nil {
			t.Errorf("input %q: pending file gone: %v", input, err)
		}
	}
}

func TestPolicySyncNoPending(t *testing.T) {
	config, state := policyDirs(t, activePolicy(), nil)
	code, out, _ := runSync(t, "", config, state)
	if code != ExitOK || out != "No pending policy; the active policy is unchanged.\n" {
		t.Errorf("exit %d, output %q", code, out)
	}
}

func TestPolicySyncSamePolicy(t *testing.T) {
	same := activePolicy()
	config, state := policyDirs(t, activePolicy(), &same)
	code, out, errOut := runSync(t, "", config, state)
	if code != ExitOK || strings.Contains(out, "[y/N]") || !strings.Contains(out, "grants the same writes") {
		t.Errorf("exit %d, stderr %q, output %q", code, errOut, out)
	}
	if _, err := os.Stat(filepath.Join(state, "policy.pending.yaml")); !os.IsNotExist(err) {
		t.Errorf("pending file still there: %v", err)
	}
}

func TestPolicySyncUnreadableActive(t *testing.T) {
	proposed := proposedPolicy()
	config, state := policyDirs(t, activePolicy(), &proposed)
	if err := os.WriteFile(config, []byte("name: hyperv-1\nrole: host\nvms: [{name: x, min: bogus}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runSync(t, "y\n", config, state)
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if !strings.Contains(out, "Warning: the active policy cannot be read") {
		t.Errorf("no warning:\n%s", out)
	}
	if got, err := policy.Load(config); err != nil || got.Digest() != proposed.Digest() {
		t.Errorf("after activation: %v, %v", got, err)
	}
}
