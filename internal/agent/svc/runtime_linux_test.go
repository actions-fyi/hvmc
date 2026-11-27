//go:build linux

package svc

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
)

// hookAgent is an Agent that calls its hooks when the test says so.
type hookAgent struct {
	hooks   Hooks
	started chan struct{}
	run     chan error
}

func (a *hookAgent) Run(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case err := <-a.run:
		return err
	}
}

func (a *hookAgent) Started() <-chan struct{} { return a.started }

// notifications records sd_notify messages.
type notifications struct{ ch chan string }

func newNotifications() *notifications { return &notifications{ch: make(chan string, 100)} }

func (n *notifications) notify(s string) error {
	n.ch <- s
	return nil
}

// take returns the next count messages, waiting for them, and fails if
// more are queued.
func (n *notifications) take(t *testing.T, count int) []string {
	t.Helper()
	var m []string
	for range count {
		m = append(m, <-n.ch)
	}
	if len(n.ch) > 0 {
		t.Errorf("unexpected notifications after %q: %d more", m, len(n.ch))
	}
	return m
}

func TestRunNotify(t *testing.T) {
	sent := newNotifications()
	a := &hookAgent{started: make(chan struct{}), run: make(chan error, 1)}
	built := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runNotify(ctx, slog.New(slog.DiscardHandler), func(h Hooks) (Agent, error) {
			a.hooks = h
			close(built)
			return a, nil
		}, sent.notify)
	}()
	<-built

	// Before the agent serves, ticks do not ping the watchdog, and a
	// failing readiness evaluation sends no READY=1.
	a.hooks.OnTick()
	a.hooks.OnReadyChange(false)
	if got := sent.take(t, 1); !slices.Equal(got, []string{"STATUS=not ready (see /readyz)"}) {
		t.Errorf("before serving: %q", got)
	}
	close(a.started)
	if got := sent.take(t, 1); !slices.Equal(got, []string{"WATCHDOG=1"}) {
		t.Errorf("serving: %q, want the first watchdog ping", got)
	}
	a.hooks.OnTick()
	a.hooks.OnReadyChange(true)
	a.hooks.OnReadyChange(false)
	a.hooks.OnReadyChange(true)
	want := []string{"WATCHDOG=1", "READY=1\nSTATUS=ready", "STATUS=not ready (see /readyz)", "STATUS=ready"}
	if got := sent.take(t, len(want)); !slices.Equal(got, want) {
		t.Errorf("running: %q, want %q", got, want)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("stop: %v", err)
	}
	if got := sent.take(t, 1); !slices.Equal(got, []string{"STOPPING=1"}) {
		t.Errorf("stop: %q", got)
	}
}

func TestRunNotifyFailure(t *testing.T) {
	sent := newNotifications()
	boom := errors.New("boom")
	// A build failure is returned, so the process exits non-zero.
	err := runNotify(context.Background(), slog.New(slog.DiscardHandler), func(Hooks) (Agent, error) { return nil, boom }, sent.notify)
	if !errors.Is(err, boom) {
		t.Errorf("build: %v", err)
	}
	// So is a failure before the agent serves, and one after.
	for _, serving := range []bool{false, true} {
		a := &hookAgent{started: make(chan struct{}), run: make(chan error, 1)}
		if serving {
			close(a.started)
		}
		a.run <- boom
		err := runNotify(context.Background(), slog.New(slog.DiscardHandler), func(Hooks) (Agent, error) { return a, nil }, sent.notify)
		if !errors.Is(err, boom) {
			t.Errorf("serving %v: %v", serving, err)
		}
	}
	close(sent.ch)
	for m := range sent.ch {
		if m != "WATCHDOG=1" {
			t.Errorf("sent %q for a failed agent", m)
		}
	}
}
