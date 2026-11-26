//go:build windows

package svc

import (
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

// TestProbeRunner records what the CI runner offers (temporary; it never
// fails): the Hyper-V Administrators alias, vmms, elevation and pwsh.
func TestProbeRunner(t *testing.T) {
	sid, err := windows.StringToSid("S-1-5-32-578")
	if err != nil {
		t.Fatal(err)
	}
	account, domain, use, err := sid.LookupAccount("")
	t.Logf("S-1-5-32-578: account %q domain %q use %d err %v", account, domain, use, err)
	t.Logf("elevated: %v", windows.GetCurrentProcessToken().IsElevated())
	m, err := mgr.Connect()
	if err != nil {
		t.Logf("mgr.Connect: %v", err)
		return
	}
	defer m.Disconnect()
	s, err := m.OpenService("vmms")
	t.Logf("vmms service: err %v", err)
	if err == nil {
		s.Close()
	}
	p, err := exec.LookPath("pwsh")
	t.Logf("pwsh: %q %v", p, err)
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		"Get-Module -ListAvailable NetSecurity,Hyper-V | Select-Object Name,Version | Format-Table -AutoSize | Out-String").CombinedOutput()
	t.Logf("modules: %s %v", out, err)
}
