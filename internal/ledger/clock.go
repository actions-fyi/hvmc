package ledger

import (
	"sync"
	"time"
)

// Clock is the only source of time for the ledger. Production code passes
// SystemClock; tests pass a FakeClock.
type Clock interface {
	Now() time.Time
}

// SystemClock reads the wall clock.
type SystemClock struct{}

// Now returns time.Now().
func (SystemClock) Now() time.Time { return time.Now() }

// FakeClock is a manually driven Clock for tests. It is safe for concurrent use.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewFakeClock returns a FakeClock that reads t until it is moved.
func NewFakeClock(t time.Time) *FakeClock { return &FakeClock{now: t} }

// Now returns the current fake time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Set moves the clock to t.
func (c *FakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// Advance moves the clock forward by d and returns the new time.
func (c *FakeClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}
