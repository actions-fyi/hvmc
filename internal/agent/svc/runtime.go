package svc

import (
	"context"
	"sync"
	"time"

	"github.com/matfax/hvmc/internal/agent"
)

// WatchdogTimeout is how long the main loop may go without a tick before
// the agent counts as hung (DESIGN §4.8; systemd's WatchdogSec=30s).
const WatchdogTimeout = 30 * time.Second

// Agent is what the service runtime runs: an *agent.Agent.
type Agent interface {
	Run(ctx context.Context) error
	Started() <-chan struct{}
}

// BuildFunc builds the agent; onTick goes into agent.Options.OnTick, so
// the runtime notices a hung main loop.
type BuildFunc func(onTick func()) (Agent, error)

// watchdog notices a main loop that stopped ticking.
type watchdog struct {
	clk     agent.Clock
	timeout time.Duration

	mu   sync.Mutex
	last time.Time
}

func newWatchdog(clk agent.Clock, timeout time.Duration) *watchdog {
	return &watchdog{clk: clk, timeout: timeout, last: clk.Now()}
}

// Tick records a main-loop tick.
func (w *watchdog) Tick() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.last = w.clk.Now()
}

// arm records a tick now and runs the watchdog: the agent calls it once it
// serves, so the time to build and start the agent never counts.
func (w *watchdog) arm(ctx context.Context, hung func(age time.Duration)) {
	w.Tick()
	w.run(ctx, hung)
}

// run checks every timeout/2 until ctx ends; when the last tick is older
// than the timeout it calls hung with that age once and returns.
func (w *watchdog) run(ctx context.Context, hung func(age time.Duration)) {
	for {
		t := w.clk.NewTimer(w.timeout / 2)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C():
		}
		w.mu.Lock()
		age := w.clk.Now().Sub(w.last)
		w.mu.Unlock()
		if age > w.timeout {
			hung(age)
			return
		}
	}
}
