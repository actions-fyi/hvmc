//go:build windows

package svc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// windowsPowerShell is Windows PowerShell 5.1, by absolute path.
func windowsPowerShell() string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
}

// runFirewallScript runs one of the fixed firewall scripts with params as
// JSON on stdin.
func runFirewallScript(script string, params firewallParams) error {
	in, err := json.Marshal(params)
	if err != nil {
		return err
	}
	cmd := exec.Command(windowsPowerShell(), "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script) //nolint:gosec // G204: fixed script; values travel as JSON on stdin
	cmd.Stdin = bytes.NewReader(in)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("firewall rules (group %s): %w: %s", params.Group, err, strings.TrimSpace(string(out)))
	}
	return nil
}
