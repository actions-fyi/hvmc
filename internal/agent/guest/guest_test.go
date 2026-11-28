package guest_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/matfax/hvmc/internal/agent"
	"github.com/matfax/hvmc/internal/agent/guest"
	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
	"github.com/matfax/hvmc/internal/fake"
)

var t0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

const gib = int64(1) << 30

// testClock adapts fake.ManualClock to agent.Clock.
type testClock struct{ *fake.ManualClock }

func (c testClock) NewTimer(d time.Duration) agent.Timer { return c.ManualClock.NewTimer(d) }

func (c testClock) AfterFunc(d time.Duration, f func()) agent.Timer {
	return c.ManualClock.AfterFunc(d, f)
}

// waitTimers waits until n timers are pending on the clock.
func (c testClock) waitTimers(t *testing.T, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.BlockUntil(ctx, n); err != nil {
		t.Fatalf("waiting for %d timers: %v", n, err)
	}
}

// source is a scriptable memory source.
type source struct {
	mu  sync.Mutex
	r   guest.Reading
	err error
}

func (s *source) read() (guest.Reading, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.r, s.err
}

func (s *source) set(r guest.Reading, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.r, s.err = r, err
}

// env is a guest service served over bufconn.
type env struct {
	t      *testing.T
	clk    testClock
	src    *source
	svc    *guest.Service
	ready  *agent.Readiness
	client agentv1.GuestAgentClient
}

func newEnv(t *testing.T, os agentv1.OperatingSystem) *env {
	t.Helper()
	e := &env{t: t, clk: testClock{fake.NewManualClock(t0)}, src: &source{r: guest.Reading{Total: 4 * gib, Available: 3 * gib}}}
	e.svc = guest.NewWithSource(e.src.read, os)
	log := slog.New(slog.DiscardHandler)
	e.ready = agent.NewReadiness(e.clk, log)
	srv := grpc.NewServer()
	if err := e.svc.Register(srv, agent.Deps{Clock: e.clk, Log: log, Readiness: e.ready, Version: "v1.2.3"}); err != nil {
		t.Fatal(err)
	}
	e.client = agentv1.NewGuestAgentClient(serve(t, srv))
	return e
}

// serve starts srv on bufconn and returns a client connection.
func serve(t *testing.T, srv *grpc.Server) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// run starts the poller and returns a function that stops it.
func (e *env) run() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.svc.Run(ctx) }()
	e.t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			e.t.Errorf("Run: %v", err)
		}
	})
}

func ctxFor(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func memory(total, avail int64, at time.Time) *agentv1.GuestMemory {
	return &agentv1.GuestMemory{TotalBytes: total, AvailableBytes: avail, ObservedAt: timestamppb.New(at)}
}

func TestServiceName(t *testing.T) {
	if got := guest.New().ServiceName(); got != "hvmc.agent.v1.GuestAgent" {
		t.Fatalf("ServiceName = %q", got)
	}
}

func TestGetMemory(t *testing.T) {
	e := newEnv(t, agentv1.OperatingSystem_OPERATING_SYSTEM_LINUX)
	e.clk.Advance(3 * time.Second)
	got, err := e.client.GetMemory(ctxFor(t), &agentv1.GetMemoryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	want := &agentv1.GetMemoryResponse{
		Memory: memory(4*gib, 3*gib, t0.Add(3*time.Second)),
		Agent:  &agentv1.AgentInfo{Version: "v1.2.3", Role: agentv1.AgentRole_AGENT_ROLE_GUEST, OperatingSystem: agentv1.OperatingSystem_OPERATING_SYSTEM_LINUX},
	}
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("GetMemory (-want +got):\n%s", diff)
	}
}

func TestGetMemoryWindowsCommit(t *testing.T) {
	e := newEnv(t, agentv1.OperatingSystem_OPERATING_SYSTEM_WINDOWS)
	limit, avail := 6*gib, 2*gib
	e.src.set(guest.Reading{Total: 4 * gib, Available: gib, CommitLimit: &limit, CommitAvailable: &avail}, nil)
	got, err := e.client.GetMemory(ctxFor(t), &agentv1.GetMemoryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	want := memory(4*gib, gib, t0)
	want.CommitLimitBytes, want.CommitAvailableBytes = proto.Int64(limit), proto.Int64(avail)
	if diff := cmp.Diff(want, got.GetMemory(), protocmp.Transform()); diff != "" {
		t.Errorf("memory (-want +got):\n%s", diff)
	}
	if got.GetAgent().GetOperatingSystem() != agentv1.OperatingSystem_OPERATING_SYSTEM_WINDOWS {
		t.Errorf("OS = %v", got.GetAgent().GetOperatingSystem())
	}
}

func TestReadiness(t *testing.T) {
	e := newEnv(t, agentv1.OperatingSystem_OPERATING_SYSTEM_LINUX)
	check := func() error {
		rep := e.ready.Evaluate(context.Background())
		for _, c := range rep.Checks {
			if c.Name == guest.ReadinessCheck {
				return c.Err
			}
		}
		t.Fatalf("check %q not registered: %+v", guest.ReadinessCheck, rep.Checks)
		return nil
	}
	if err := check(); err == nil {
		t.Fatal("ready before the first read")
	}
	e.run()
	e.clk.waitTimers(t, 1) // first poll done
	if err := check(); err != nil {
		t.Fatalf("not ready after a read: %v", err)
	}
	e.src.set(guest.Reading{}, errors.New("meminfo gone"))
	e.clk.Advance(guest.PollInterval)
	e.clk.waitTimers(t, 1)
	if err := check(); err == nil || !strings.Contains(err.Error(), "meminfo gone") {
		t.Fatalf("readiness with a failing source: %v", err)
	}
}

func TestReadinessStale(t *testing.T) {
	e := newEnv(t, agentv1.OperatingSystem_OPERATING_SYSTEM_LINUX)
	if _, err := e.client.GetMemory(ctxFor(t), &agentv1.GetMemoryRequest{}); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(2 * guest.PollInterval)
	if rep := e.ready.Evaluate(context.Background()); !rep.Ready {
		t.Fatalf("not ready at 2 × PollInterval: %+v", rep.Checks)
	}
	e.clk.Advance(time.Millisecond)
	if rep := e.ready.Evaluate(context.Background()); rep.Ready {
		t.Fatal("ready with a stale reading")
	}
}

func TestUnreadableSource(t *testing.T) {
	e := newEnv(t, agentv1.OperatingSystem_OPERATING_SYSTEM_LINUX)
	e.src.set(guest.Reading{}, errors.New("meminfo gone"))
	_, err := e.client.GetMemory(ctxFor(t), &agentv1.GetMemoryRequest{})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("GetMemory: %v, want UNAVAILABLE", err)
	}
	stream, err := e.client.WatchMemory(ctxFor(t), &agentv1.WatchMemoryRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.Unavailable {
		t.Fatalf("WatchMemory: %v, want UNAVAILABLE", err)
	}
}

// watch opens WatchMemory and returns its messages on a channel.
func (e *env) watch(ctx context.Context) <-chan *agentv1.GuestMemory {
	e.t.Helper()
	stream, err := e.client.WatchMemory(ctx, &agentv1.WatchMemoryRequest{})
	if err != nil {
		e.t.Fatal(err)
	}
	out := make(chan *agentv1.GuestMemory, 16)
	go func() {
		defer close(out)
		for {
			m, err := stream.Recv()
			if err != nil {
				return
			}
			out <- m.GetMemory()
		}
	}()
	return out
}

func recv(t *testing.T, ch <-chan *agentv1.GuestMemory) *agentv1.GuestMemory {
	t.Helper()
	select {
	case m, ok := <-ch:
		if !ok {
			t.Fatal("stream ended")
		}
		return m
	case <-time.After(10 * time.Second):
		t.Fatal("no message from WatchMemory")
		return nil
	}
}

func requireMemory(t *testing.T, got, want *agentv1.GuestMemory) {
	t.Helper()
	if diff := cmp.Diff(want, got, protocmp.Transform()); diff != "" {
		t.Errorf("WatchMemory message (-want +got):\n%s", diff)
	}
}

func TestWatchMemoryHeartbeat(t *testing.T) {
	e := newEnv(t, agentv1.OperatingSystem_OPERATING_SYSTEM_LINUX)
	e.run()
	ch := e.watch(ctxFor(t))
	requireMemory(t, recv(t, ch), memory(4*gib, 3*gib, t0))
	e.clk.waitTimers(t, 2) // poller and watch timer
	// Polls without a change send nothing; the next message is the
	// heartbeat at WatchInterval.
	for range int(guest.WatchInterval / guest.PollInterval) {
		e.clk.Advance(guest.PollInterval)
		e.clk.waitTimers(t, 2)
	}
	requireMemory(t, recv(t, ch), memory(4*gib, 3*gib, t0.Add(guest.WatchInterval)))
}

func TestWatchMemoryChange(t *testing.T) {
	e := newEnv(t, agentv1.OperatingSystem_OPERATING_SYSTEM_LINUX)
	e.run()
	ch := e.watch(ctxFor(t))
	requireMemory(t, recv(t, ch), memory(4*gib, 3*gib, t0))
	e.clk.waitTimers(t, 2)
	e.src.set(guest.Reading{Total: 6 * gib, Available: 5 * gib}, nil)
	e.clk.Advance(guest.PollInterval)
	requireMemory(t, recv(t, ch), memory(6*gib, 5*gib, t0.Add(guest.PollInterval)))

	// A failing poll ends the stream at once, before the next heartbeat.
	e.clk.waitTimers(t, 2)
	e.src.set(guest.Reading{}, errors.New("gone"))
	e.clk.Advance(guest.PollInterval)
	if m, ok := <-ch; ok {
		t.Fatalf("stream delivered %v from a failing source", m)
	}
}

func TestWatchMemoryEndsWithContext(t *testing.T) {
	e := newEnv(t, agentv1.OperatingSystem_OPERATING_SYSTEM_LINUX)
	ctx, cancel := context.WithCancel(ctxFor(t))
	ch := e.watch(ctx)
	recv(t, ch)
	cancel()
	for range ch {
	}
}

func TestRegisterRequiresDeps(t *testing.T) {
	if err := guest.New().Register(grpc.NewServer(), agent.Deps{}); err == nil {
		t.Fatal("Register without clock and readiness succeeded")
	}
}

// TestFakeParity checks that the guest agent and the fake guest in
// internal/fake answer an unreadable memory source with the same codes.
func TestFakeParity(t *testing.T) {
	clk := fake.NewManualClock(t0)
	sc := &fake.Scenario{
		Host:   fake.HostSpec{Name: "host", Total: fake.Bytes(64 * gib)},
		Guests: []fake.GuestSpec{{Name: "g", Total: fake.Bytes(4 * gib), SourceDown: true}},
	}
	sim, err := fake.New(sc, fake.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	if err := sim.RegisterGuest(srv, "g"); err != nil {
		t.Fatal(err)
	}
	fakeClient := agentv1.NewGuestAgentClient(serve(t, srv))

	e := newEnv(t, agentv1.OperatingSystem_OPERATING_SYSTEM_LINUX)
	e.src.set(guest.Reading{}, errors.New("gone"))

	for name, c := range map[string]agentv1.GuestAgentClient{"fake": fakeClient, "guest": e.client} {
		_, getErr := c.GetMemory(ctxFor(t), &agentv1.GetMemoryRequest{})
		stream, err := c.WatchMemory(ctxFor(t), &agentv1.WatchMemoryRequest{})
		if err != nil {
			t.Fatal(err)
		}
		_, watchErr := stream.Recv()
		if status.Code(getErr) != codes.Unavailable || status.Code(watchErr) != codes.Unavailable {
			t.Errorf("%s: GetMemory %v, WatchMemory %v; want UNAVAILABLE for both", name, getErr, watchErr)
		}
	}
}
