package fake

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Clock is the only source of time of a Sim. Every simulated delay (grow
// latency, guest total lag, the agent's cmdlet timeout, the WatchMemory
// interval) runs on it, so tests drive the fakes deterministically with a
// ManualClock and never sleep. It is a superset of ledger.Clock.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// NewTimer returns a timer that delivers the time on C once d has passed.
	NewTimer(d time.Duration) Timer
	// AfterFunc calls f once d has passed. The returned Timer's C is nil.
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a pending event on a Clock.
type Timer interface {
	// C delivers the fire time; nil for AfterFunc timers.
	C() <-chan time.Time
	// Stop prevents the timer from firing. It reports whether the call
	// stopped the timer (false when it had already fired or been stopped).
	Stop() bool
}

// RealClock returns a Clock backed by the time package.
func RealClock() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

func (realClock) AfterFunc(d time.Duration, f func()) Timer { return realTimer{time.AfterFunc(d, f)} }

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time { return r.t.C }
func (r realTimer) Stop() bool          { return r.t.Stop() }

// ManualClock is a manually driven Clock. Timers fire only from Advance or
// Set: channel timers receive their fire time, AfterFunc callbacks run
// synchronously on the caller's goroutine, in fire-time order, so the effect
// of a simulated delay is visible as soon as Advance returns. ManualClock is
// safe for concurrent use.
type ManualClock struct {
	mu      sync.Mutex
	now     time.Time
	seq     uint64
	timers  []*fakeTimer
	changed chan struct{} // closed and replaced whenever timers change
}

// NewManualClock returns a ManualClock reading t until it is moved.
func NewManualClock(t time.Time) *ManualClock {
	return &ManualClock{now: t, changed: make(chan struct{})}
}

type fakeTimer struct {
	clk  *ManualClock
	at   time.Time
	seq  uint64
	ch   chan time.Time
	f    func()
	done bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Stop() bool {
	c := t.clk
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.done {
		return false
	}
	t.done = true
	c.removeLocked(t)
	return true
}

// Now returns the current fake time.
func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// NewTimer returns a timer firing once the clock has moved d past now.
func (c *ManualClock) NewTimer(d time.Duration) Timer {
	return c.add(d, make(chan time.Time, 1), nil)
}

// AfterFunc calls f from Advance or Set once the clock has moved d past now.
func (c *ManualClock) AfterFunc(d time.Duration, f func()) Timer {
	return c.add(d, nil, f)
}

func (c *ManualClock) add(d time.Duration, ch chan time.Time, f func()) *fakeTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	t := &fakeTimer{clk: c, at: c.now.Add(d), seq: c.seq, ch: ch, f: f}
	c.timers = append(c.timers, t)
	c.signalLocked()
	return t
}

func (c *ManualClock) removeLocked(t *fakeTimer) {
	for i, x := range c.timers {
		if x == t {
			c.timers = append(c.timers[:i], c.timers[i+1:]...)
			break
		}
	}
	c.signalLocked()
}

func (c *ManualClock) signalLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// Advance moves the clock forward by d, firing every timer due on the way,
// and returns the new time.
func (c *ManualClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	c.Set(target)
	return target
}

// Set moves the clock to t, firing every timer due at or before t in order.
// Moving backwards fires nothing.
func (c *ManualClock) Set(t time.Time) {
	for {
		c.mu.Lock()
		next := c.nextDueLocked(t)
		if next == nil {
			if t.After(c.now) {
				c.now = t
			}
			c.mu.Unlock()
			return
		}
		next.done = true
		c.removeLocked(next)
		if next.at.After(c.now) {
			c.now = next.at
		}
		fired := c.now
		c.mu.Unlock()
		if next.f != nil {
			next.f()
		} else {
			next.ch <- fired // buffered, never blocks: a timer fires once
		}
	}
}

func (c *ManualClock) nextDueLocked(t time.Time) *fakeTimer {
	due := make([]*fakeTimer, 0, len(c.timers))
	for _, x := range c.timers {
		if !x.at.After(t) {
			due = append(due, x)
		}
	}
	if len(due) == 0 {
		return nil
	}
	sort.Slice(due, func(i, j int) bool {
		if !due[i].at.Equal(due[j].at) {
			return due[i].at.Before(due[j].at)
		}
		return due[i].seq < due[j].seq
	})
	return due[0]
}

// Timers returns the number of pending (not yet fired or stopped) timers.
func (c *ManualClock) Timers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// BlockUntil waits until at least n timers are pending, so a test can be
// sure a handler is waiting on the clock before it calls Advance. It returns
// ctx's error if ctx ends first.
func (c *ManualClock) BlockUntil(ctx context.Context, n int) error {
	for {
		c.mu.Lock()
		if len(c.timers) >= n {
			c.mu.Unlock()
			return nil
		}
		ch := c.changed
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
