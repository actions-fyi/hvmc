package svc

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/matfax/hvmc/internal/agent"
)

// TestWindowsLabelsOutput: with the Windows labels, install and uninstall
// print exactly what they printed before the labels were the platform's.
func TestWindowsLabelsOutput(t *testing.T) {
	e := newInstallEnv(t, http.StatusOK)
	p := newFakePlatform()
	if err := Install(testCtx(t), p, e.options(agent.RoleHost, "y\n")); err != nil {
		t.Fatalf("Install: %v\n%s", err, e.out)
	}
	steps := fmt.Sprintf(`Installed C:\Program Files\hvmc\hvmc.exe.
Registered the Event Log source hvmc-agent.
Configured service hvmc-agent (NT SERVICE\hvmc-agent, delayed automatic start).
Added NT SERVICE\hvmc-agent to Hyper-V Administrators (S-1-5-32-578).
Allowed inbound TCP 9443 and TCP %d from any source in the firewall.
Starting the service...
`, e.readyz.port)
	if !strings.Contains(e.out.String(), steps) {
		t.Errorf("install output lacks the steps\n%s\nin\n%s", steps, e.out)
	}

	var out strings.Builder
	if err := Uninstall(testCtx(t), p, UninstallOptions{Paths: e.paths, Out: &out}); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`Stopped the service.
Removed NT SERVICE\hvmc-agent from Hyper-V Administrators.
Removed the firewall rules (group hvmc-agent).
Removed service hvmc-agent.
Removed the Event Log source hvmc-agent.
Removed C:\Program Files\hvmc\hvmc.exe.
Kept the configuration, keys and state (%s, %s); remove them with --remove-state.
`, e.paths.Config, e.paths.State)
	if diff := cmp.Diff(want, out.String()); diff != "" {
		t.Errorf("uninstall output (-want +got):\n%s", diff)
	}
}

// TestEmptyLabelsSkipSteps: a platform without an Event Log or Hyper-V
// Administrators leaves their labels empty, and their methods are not
// called.
func TestEmptyLabelsSkipSteps(t *testing.T) {
	e := newInstallEnv(t, http.StatusOK)
	p := newFakePlatform()
	p.labels = &Labels{
		ConfigureService: "Configured unit u", RemoveService: "Removed unit u",
		Firewall: "in ufw", RemoveFirewallRules: "Removed the ufw rules", Logs: "journalctl -u hvmc-agent",
	}
	if err := Install(testCtx(t), p, e.options(agent.RoleGuest, "")); err != nil {
		t.Fatalf("Install: %v\n%s", err, e.out)
	}
	if diff := cmp.Diff([]string{"InstallBinary", "ConfigureService", "SetFirewallRules", "StartService"}, p.recorded()); diff != "" {
		t.Errorf("install calls (-want +got):\n%s", diff)
	}
	for _, s := range []string{"Configured unit u.\n", "in ufw.\n"} {
		if !strings.Contains(e.out.String(), s) {
			t.Errorf("install output lacks %q:\n%s", s, e.out)
		}
	}
	p.calls = nil
	var out strings.Builder
	if err := Uninstall(testCtx(t), p, UninstallOptions{Paths: e.paths, Out: &out}); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"StopService", "RemoveFirewallRules", "RemoveService", "RemoveBinary"}, p.recorded()); diff != "" {
		t.Errorf("uninstall calls (-want +got):\n%s", diff)
	}
	if strings.Contains(out.String(), "Event Log") || strings.Contains(out.String(), "Hyper-V") {
		t.Errorf("uninstall printed a skipped step:\n%s", out.String())
	}
}
