//go:build linux && servicetesthooks

package svc

// Test-only hooks for the systemd round trip (service_linux_test.go). This
// file is compiled only into the hvmc binary the round trip builds with the
// servicetesthooks tag; no other build can hang or hold back its readiness,
// whatever its environment.
//
//   - HVMC_SERVICETEST_HANG=1 hangs the main loop at its third tick, after
//     the agent has reported ready, so the systemd watchdog restarts it.
//   - HVMC_SERVICETEST_HOLD_READY=<file> withholds readiness from systemd
//     while the file exists, so the test sees the unit still activating
//     while /readyz already reports ready.

import (
	"os"
	"sync"
)

func init() {
	testHooks = servicetestHooks
}

func servicetestHooks(h Hooks) Hooks {
	t := &testHookState{inner: h, hang: os.Getenv("HVMC_SERVICETEST_HANG") == "1", hold: os.Getenv("HVMC_SERVICETEST_HOLD_READY")}
	return Hooks{OnTick: t.tick, OnReadyChange: t.readyChange}
}

type testHookState struct {
	inner Hooks
	hang  bool
	hold  string

	mu      sync.Mutex
	ticks   int
	pending *bool
}

func (t *testHookState) held() bool {
	if t.hold == "" {
		return false
	}
	_, err := os.Stat(t.hold)
	return err == nil
}

func (t *testHookState) readyChange(ready bool) {
	if t.held() {
		t.mu.Lock()
		t.pending = &ready
		t.mu.Unlock()
		return
	}
	t.inner.OnReadyChange(ready)
}

func (t *testHookState) tick() {
	t.mu.Lock()
	t.ticks++
	hang := t.hang && t.ticks >= 3
	pending := t.pending
	if pending != nil && !t.held() {
		t.pending = nil
	} else {
		pending = nil
	}
	t.mu.Unlock()
	if pending != nil {
		t.inner.OnReadyChange(*pending)
	}
	if hang {
		select {} // the hung main loop: no tick, no WATCHDOG=1
	}
	t.inner.OnTick()
}
