//go:build linux

package svc

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/coreos/go-systemd/v22/daemon"

	"github.com/matfax/hvmc/internal/agent"
)

// LogsToStderr reports that the agent logs to stderr under the service
// manager: journald captures it (DESIGN §2.1).
const LogsToStderr = true

// IsService reports whether systemd started this process as a Type=notify
// service: it then passes the notification socket in NOTIFY_SOCKET.
func IsService() (bool, error) { return os.Getenv("NOTIFY_SOCKET") != "", nil }

// RunService runs the agent under systemd until SIGTERM or SIGINT; call it
// when IsService reports true. It sends READY=1 once the readiness checks
// pass, STATUS= on every change of readiness, WATCHDOG=1 on every main-loop
// tick once the agent serves (WatchdogSec=30s restarts a hung loop), and
// STOPPING=1 before the drain (DESIGN §4.5, §4.8). It returns the agent's
// failure, so the process exits non-zero and Restart=on-failure applies.
func RunService(log *slog.Logger, _ agent.Clock, build BuildFunc) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	return runNotify(ctx, log, build, sdNotify)
}

// testHooks lets a test-only build (tag servicetesthooks) wrap the hooks;
// every other build leaves them as they are.
var testHooks = func(h Hooks) Hooks { return h }

func sdNotify(state string) error {
	_, err := daemon.SdNotify(false, state)
	return err
}

// runNotify runs the agent built by build until ctx ends, reporting to
// systemd through notify.
func runNotify(ctx context.Context, log *slog.Logger, build BuildFunc, notify func(string) error) error {
	n := &notifier{notify: notify, log: log}
	a, err := build(testHooks(Hooks{OnTick: n.tick, OnReadyChange: n.readyChange}))
	if err != nil {
		return err
	}
	// STOPPING=1 goes out as soon as the stop is requested, while the
	// agent drains, and at the latest when Run returns after the stop:
	// Run may see the cancelled context before the AfterFunc runs.
	var stopping sync.Once
	stoppingNow := func() {
		stopping.Do(func() {
			log.Info("stop requested; draining", "timeout", agent.DrainTimeout)
			n.send(daemon.SdNotifyStopping)
		})
	}
	context.AfterFunc(ctx, stoppingNow)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	select {
	case <-a.Started():
		// The watchdog counts from the start of serving: a slow start is
		// not a hung loop.
		n.arm()
		err = <-done
	case err = <-done:
	}
	if ctx.Err() != nil {
		stoppingNow()
	}
	return err
}

// notifier turns the agent's hooks into sd_notify messages.
type notifier struct {
	notify func(string) error
	log    *slog.Logger

	mu        sync.Mutex
	armed     bool
	readySent bool
}

func (n *notifier) send(state string) {
	if err := n.notify(state); err != nil {
		n.log.Warn("sd_notify failed", "state", state, "error", err)
	}
}

// arm starts the watchdog pings with one now.
func (n *notifier) arm() {
	n.mu.Lock()
	n.armed = true
	n.mu.Unlock()
	n.send(daemon.SdNotifyWatchdog)
}

// tick pings the watchdog once the agent serves.
func (n *notifier) tick() {
	n.mu.Lock()
	armed := n.armed
	n.mu.Unlock()
	if armed {
		n.send(daemon.SdNotifyWatchdog)
	}
}

// readyChange sends READY=1 the first time the agent is ready, so the
// start job completes only then, and the readiness as STATUS= every time.
func (n *notifier) readyChange(ready bool) {
	n.mu.Lock()
	first := ready && !n.readySent
	if first {
		n.readySent = true
	}
	n.mu.Unlock()
	status := "STATUS=ready"
	if !ready {
		status = "STATUS=not ready (see /readyz)"
	}
	if first {
		n.send(daemon.SdNotifyReady + "\n" + status)
		return
	}
	n.send(status)
}
