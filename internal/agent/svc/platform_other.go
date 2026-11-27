//go:build !windows && !linux

package svc

import (
	"log/slog"

	"github.com/matfax/hvmc/internal/agent"
)

// caseInsensitivePaths is false: paths are case-sensitive.
const caseInsensitivePaths = false

// NewPlatform returns ErrUnsupported: only Windows and Linux (systemd)
// have a service integration.
func NewPlatform(agent.Clock) (Platform, error) { return nil, ErrUnsupported }

// LogsToStderr reports that the agent logs to stderr.
const LogsToStderr = true

// IsService reports false: the agent runs in the foreground.
func IsService() (bool, error) { return false, nil }

// RunService returns ErrUnsupported.
func RunService(*slog.Logger, agent.Clock, BuildFunc) error { return ErrUnsupported }
