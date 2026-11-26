//go:build windows

package svc

import (
	"context"
	"log/slog"
	"math"
	"os"
	"time"

	"golang.org/x/sys/windows/svc"

	"github.com/matfax/hvmc/internal/agent"
)

// exitFailure is the service-specific exit code of a failed run; SCM
// recovery restarts the service after it, because install enables
// recovery actions for stops with an error.
const exitFailure = 1

// accepted are the controls the service handles: stop, shutdown and
// preshutdown all drain the agent (DESIGN §4.5).
const accepted = svc.AcceptStop | svc.AcceptShutdown | svc.AcceptPreShutdown

// IsService reports whether the Service Control Manager started this
// process.
func IsService() (bool, error) { return svc.IsWindowsService() }

// RunService runs the agent under the Service Control Manager; call it
// when IsService reports true. It reports SERVICE_RUNNING once the agent
// serves, drains it on stop, shutdown and preshutdown, and exits non-zero
// when the agent fails or its main loop hangs (WatchdogTimeout), so SCM
// recovery restarts it (DESIGN §4.8).
func RunService(log *slog.Logger, clk agent.Clock, build BuildFunc) error {
	h := &handler{log: log, clk: clk, build: build, exit: os.Exit}
	if err := svc.Run(ServiceName, h); err != nil {
		return err
	}
	return h.err
}

type handler struct {
	log   *slog.Logger
	clk   agent.Clock
	build BuildFunc
	exit  func(code int)
	err   error
}

// Execute implements svc.Handler.
func (h *handler) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending, WaitHint: 30_000}
	wd := newWatchdog(h.clk, WatchdogTimeout)
	a, err := h.build(wd.Tick)
	if err != nil {
		h.fail(err)
		return true, exitFailure
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	go wd.run(ctx, h.hung)
	if err := h.serve(a.Started(), done, cancel, req, status); err != nil {
		h.fail(err)
		return true, exitFailure
	}
	h.log.Info("service stopped")
	return false, 0
}

// serve reports the service's state until the agent's Run returns.
func (h *handler) serve(started <-chan struct{}, done <-chan error, stop func(), req <-chan svc.ChangeRequest, status chan<- svc.Status) error {
	for {
		select {
		case <-started:
			started = nil
			status <- svc.Status{State: svc.Running, Accepts: accepted}
		case err := <-done:
			status <- svc.Status{State: svc.StopPending}
			return err
		case c := <-req:
			h.control(c, stop, status)
		}
	}
}

func (h *handler) control(c svc.ChangeRequest, stop func(), status chan<- svc.Status) {
	switch c.Cmd {
	case svc.Interrogate:
		status <- c.CurrentStatus
	case svc.Stop, svc.Shutdown, svc.PreShutdown:
		h.log.Info("service stop requested; draining", "control", controlName(c.Cmd), "timeout", agent.DrainTimeout)
		status <- svc.Status{State: svc.StopPending, WaitHint: toUint32((agent.DrainTimeout + 5*time.Second).Milliseconds())}
		stop()
	default:
	}
}

func controlName(c svc.Cmd) string {
	switch c {
	case svc.Stop:
		return "stop"
	case svc.Shutdown:
		return "shutdown"
	case svc.PreShutdown:
		return "preshutdown"
	default:
		return "other"
	}
}

func (h *handler) fail(err error) {
	h.err = err
	h.log.Error("service failed; exiting non-zero so recovery restarts it", "error", err)
}

// hung ends the process without a stop report, so SCM counts a failure and
// applies the recovery actions.
func (h *handler) hung(age time.Duration) {
	h.log.Error("main loop hung; exiting so service recovery restarts the agent", "since_last_tick", age, "limit", WatchdogTimeout)
	h.exit(exitFailure)
}

// toUint32 clamps v to the range of uint32 (SCM times and hints).
func toUint32(v int64) uint32 {
	switch {
	case v < 0:
		return 0
	case v > math.MaxUint32:
		return math.MaxUint32
	default:
		return uint32(v)
	}
}
