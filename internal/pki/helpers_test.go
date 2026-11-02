package pki_test

import (
	"crypto/x509"
	"sync"
	"testing"
	"time"

	"github.com/matfax/hvmc/internal/pki"
)

// t0 is the fixed start of every fake clock.
var t0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// fakeClock is a manually driven pki.Clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: t0} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestCA(t *testing.T, clk pki.Clock) *pki.CA {
	t.Helper()
	ca, err := pki.GenerateCA(pki.WithClock(clk))
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	return ca
}

// verifyChain verifies cert against ca at time at for the given usage.
func verifyChain(t *testing.T, ca *pki.CA, cert *x509.Certificate, at time.Time, usage x509.ExtKeyUsage) {
	t.Helper()
	_, err := cert.Verify(x509.VerifyOptions{Roots: ca.Pool(), CurrentTime: at, KeyUsages: []x509.ExtKeyUsage{usage}})
	if err != nil {
		t.Fatalf("certificate does not verify against the CA: %v", err)
	}
}
