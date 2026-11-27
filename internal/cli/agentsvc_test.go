package cli

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/matfax/hvmc/internal/agent"
	"github.com/matfax/hvmc/internal/agent/svc"
)

// stubPlatform is a service integration that refuses to act; methods the
// tests do not reach panic through the nil embedded interface.
type stubPlatform struct {
	svc.Platform
	calls []string
}

func (p *stubPlatform) CheckPrivileges() error {
	p.calls = append(p.calls, "CheckPrivileges")
	return errors.New("run this command as Administrator")
}

func (p *stubPlatform) ServiceState() (svc.State, uint32, error) {
	p.calls = append(p.calls, "ServiceState")
	return svc.NotInstalled, 0, nil
}

func stubEnv(p *stubPlatform) env {
	return env{
		now:         func() time.Time { return testNow },
		newPlatform: func(agent.Clock) (svc.Platform, error) { return p, nil },
	}
}

func TestAgentInstallUsage(t *testing.T) {
	dir := t.TempDir()
	paths := []string{"--config", filepath.Join(dir, "agent.yaml"), "--state-dir", dir}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "--role, --agent, --enroll, --token, --ca-sha256 required"},
		{[]string{"--role", "hypervisor"}, `--role must be host or guest, got "hypervisor"`},
		{[]string{"--role", "host", "--agent", "hyperv-1", "--enroll", "https://hvmc.example.com/v1/enroll", "--token", "t", "--ca-sha256", "00"}, "--ca-sha256"},
	} {
		p := &stubPlatform{}
		code, _, errOut := runCLIEnv(t, stubEnv(p), append(append([]string{"agent", "install"}, tc.args...), paths...)...)
		if code != ExitUsage || !strings.Contains(errOut, tc.want) || !strings.Contains(errOut, "Usage:") {
			t.Errorf("%v: exit %d, stderr %q, want usage error with %q", tc.args, code, errOut, tc.want)
		}
		if len(p.calls) != 0 {
			t.Errorf("%v: platform called: %v", tc.args, p.calls)
		}
	}
}

// TestAgentInstallChecksPrivileges: with valid flags, install checks the
// privileges before it changes anything.
func TestAgentInstallChecksPrivileges(t *testing.T) {
	dir := t.TempDir()
	p := &stubPlatform{}
	code, _, errOut := runCLIEnv(t, stubEnv(p), "agent", "install", "--role", "guest", "--agent", "vm-1",
		"--enroll", "https://hvmc.example.com/v1/enroll", "--token", "t",
		"--ca-sha256", strings.Repeat("ab", 32), "--config", filepath.Join(dir, "agent.yaml"), "--state-dir", dir)
	if code != ExitFailure || !strings.Contains(errOut, "Administrator") || !slices.Equal(p.calls, []string{"CheckPrivileges"}) {
		t.Errorf("exit %d, stderr %q, calls %v", code, errOut, p.calls)
	}
}

func TestAgentUninstallAndStatus(t *testing.T) {
	dir := t.TempDir()
	paths := []string{"--config", filepath.Join(dir, "agent.yaml"), "--state-dir", dir}
	p := &stubPlatform{}
	code, _, errOut := runCLIEnv(t, stubEnv(p), append([]string{"agent", "uninstall", "--remove-state"}, paths...)...)
	if code != ExitFailure || !strings.Contains(errOut, "Administrator") {
		t.Errorf("uninstall: exit %d, stderr %q", code, errOut)
	}
	code, out, errOut := runCLIEnv(t, stubEnv(p), append([]string{"agent", "status"}, paths...)...)
	if code != ExitFailure || !strings.Contains(out, "Service hvmc-agent: not installed") || !strings.Contains(errOut, svc.ErrNotReady.Error()) {
		t.Errorf("status: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

// TestAgentPathsLogsFollowState: on Windows the logs are in the state
// directory, so --state-dir moves them too (a test never writes to
// %ProgramData%\hvmc).
func TestAgentPathsLogsFollowState(t *testing.T) {
	cmd := newAgentRunCommand()
	dir := t.TempDir()
	if err := cmd.Flags().Parse([]string{"--state-dir", dir}); err != nil {
		t.Fatal(err)
	}
	p := agentPaths(cmd)
	if p.State != dir || (p.Logs != "" && p.Logs != filepath.Join(dir, "logs")) {
		t.Errorf("paths %+v, want the logs in %s", p, dir)
	}
	if want := agent.DefaultPaths(); (want.Logs == "") != (p.Logs == "") {
		t.Errorf("logs %q, default %q", p.Logs, want.Logs)
	}
}
