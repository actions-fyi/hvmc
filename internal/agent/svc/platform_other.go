//go:build !windows

package svc

import (
	"log/slog"

	"github.com/matfax/hvmc/internal/agent"
)

// NewPlatform returns ErrUnsupported: only Windows has a service
// integration so far.
func NewPlatform(agent.Clock) (Platform, error) { return nil, ErrUnsupported }

// IsService reports false: outside Windows the agent runs in the
// foreground (systemd needs no handshake to start it).
func IsService() (bool, error) { return false, nil }

// RunService returns ErrUnsupported.
func RunService(*slog.Logger, agent.Clock, BuildFunc) error { return ErrUnsupported }
