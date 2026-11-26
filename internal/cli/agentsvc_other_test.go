//go:build !windows

package cli

import (
	"strings"
	"testing"

	"github.com/matfax/hvmc/internal/agent/svc"
)

// Only Windows has a service integration so far.
func TestAgentServiceCommandsUnsupported(t *testing.T) {
	for _, cmd := range []string{"install", "uninstall", "status"} {
		code, _, errOut := runCLI(t, "agent", cmd)
		if code != ExitFailure || !strings.Contains(errOut, svc.ErrUnsupported.Error()) {
			t.Errorf("agent %s: exit %d, stderr %q", cmd, code, errOut)
		}
	}
}
