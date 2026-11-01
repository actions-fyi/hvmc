package fake_test

import (
	"testing"
	"time"

	"github.com/matfax/hvmc/internal/fake"
)

func TestManualClock(t *testing.T) {
	clk := fake.NewManualClock(t0)
	var order []string
	clk.AfterFunc(2*time.Second, func() { order = append(order, "b") })
	clk.AfterFunc(time.Second, func() {
		order = append(order, "a")
		// A timer scheduled by a callback within the advanced window fires
		// in the same Advance.
		clk.AfterFunc(500*time.Millisecond, func() { order = append(order, "a+") })
	})
	stopped := clk.AfterFunc(time.Second, func() { order = append(order, "stopped") })
	ch := clk.NewTimer(3 * time.Second)
	if !stopped.Stop() || stopped.Stop() {
		t.Errorf("Stop should report true once")
	}
	if got := clk.Timers(); got != 3 {
		t.Errorf("Timers = %d, want 3", got)
	}

	if got := clk.Advance(2 * time.Second); !got.Equal(t0.Add(2 * time.Second)) {
		t.Errorf("Advance returned %v", got)
	}
	if want := []string{"a", "a+", "b"}; len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Errorf("fire order = %v, want %v", order, want)
	}
	select {
	case <-ch.C():
		t.Fatal("3s timer fired at 2s")
	default:
	}
	clk.Advance(time.Second)
	if got := <-ch.C(); !got.Equal(t0.Add(3 * time.Second)) {
		t.Errorf("timer fired at %v", got)
	}
	if ch.Stop() {
		t.Errorf("Stop of a fired timer reported true")
	}
	clk.Set(t0) // backwards: no effect on pending timers, time stays
	if !clk.Now().Equal(t0.Add(3 * time.Second)) {
		t.Errorf("Set backwards moved the clock to %v", clk.Now())
	}

	ctx := testCtx(t)
	done := make(chan error, 1)
	go func() { done <- clk.BlockUntil(ctx, 1) }()
	clk.NewTimer(time.Minute)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := clk.BlockUntil(shortCtx(t), 5); err == nil {
		t.Errorf("BlockUntil returned without enough timers")
	}

	// The real clock works too.
	rc := fake.RealClock()
	fired := make(chan struct{})
	rc.AfterFunc(time.Millisecond, func() { close(fired) })
	<-fired
	if rc.Now().IsZero() {
		t.Errorf("real clock reads zero")
	}
	rt := rc.NewTimer(time.Hour)
	if !rt.Stop() {
		t.Errorf("Stop of a real timer reported false")
	}
}
