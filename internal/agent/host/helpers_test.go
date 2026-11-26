package host

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/matfax/hvmc/internal/agent"
	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
	"github.com/matfax/hvmc/internal/fake"
	"github.com/matfax/hvmc/internal/policy"
)

// t0 is the start time of every manual clock.
var t0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

const (
	mib = int64(1) << 20
	gib = int64(1) << 30
)

// testClock adapts fake.ManualClock to agent.Clock.
type testClock struct{ *fake.ManualClock }

func newTestClock() testClock { return testClock{fake.NewManualClock(t0)} }

func (c testClock) NewTimer(d time.Duration) agent.Timer { return c.ManualClock.NewTimer(d) }

func (c testClock) AfterFunc(d time.Duration, f func()) agent.Timer {
	return c.ManualClock.AfterFunc(d, f)
}

// waitTimer waits until a timer is pending on the clock.
func (c testClock) waitTimer(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.BlockUntil(ctx, 1); err != nil {
		t.Fatalf("waiting for a timer: %v", err)
	}
}

func testLogger(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(string(bytes.TrimRight(p, "\n")))
	return len(p), nil
}

// waitFor waits until cond holds, re-checking whenever changed() fires.
// The deadline is only a guard against a hanging test.
func waitFor(t *testing.T, what string, changed func() <-chan struct{}, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		ch := changed()
		if cond() {
			return
		}
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func testCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// syncBuffer is a goroutine-safe buffer for the audit log.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) entries(t *testing.T) []agent.AuditEntry {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []agent.AuditEntry
	for _, line := range bytes.Split(bytes.TrimSpace(b.buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var e agent.AuditEntry
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("audit line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

// testEnv is a host agent service on a fake or real worker, served over an
// in-memory gRPC connection with the framework's audit interceptor.
type testEnv struct {
	t      *testing.T
	clk    testClock
	svc    *Service
	store  *agent.PolicyStore
	ready  *agent.Readiness
	audit  *syncBuffer
	paths  agent.Paths
	client agentv1.HostAgentClient
	runErr chan error
}

type envOptions struct {
	start        Starter
	script       []byte
	pollInterval time.Duration // 0: no poll loop
	clk          *testClock
}

// newTestEnv starts the service with pol as the active policy. The worker
// runs on h unless the options give another starter.
func newTestEnv(t *testing.T, h *fakeHost, pol policy.Policy, opts ...func(*envOptions)) *testEnv {
	t.Helper()
	o := envOptions{}
	for _, fn := range opts {
		fn(&o)
	}
	if o.start == nil {
		o.start = h.starter(false)
	}
	clk := newTestClock()
	if o.clk != nil {
		clk = *o.clk
	}
	dir := t.TempDir()
	paths := agent.Paths{Config: filepath.Join(dir, "agent.yaml"), State: dir}
	if err := os.WriteFile(paths.Config, []byte("name: hyperv-1\nrole: host\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := policy.Save(paths.Config, pol); err != nil {
		t.Fatal(err)
	}
	log := testLogger(t)
	store := agent.OpenPolicyStore(paths, log)
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	e := &testEnv{
		t: t, clk: clk, store: store, ready: agent.NewReadiness(clk, log), audit: &syncBuffer{},
		paths: paths, runErr: make(chan error, 1),
	}
	audit := agent.NewAudit(e.audit, "hyperv-1", clk, log)
	e.svc = NewWithOptions(Options{Start: o.start, Script: o.script})
	srv := grpc.NewServer(grpc.UnaryInterceptor(audit.UnaryInterceptor))
	deps := agent.Deps{
		Config: agent.Config{Name: "hyperv-1", Role: agent.RoleHost}, Clock: clk, Log: log,
		Readiness: e.ready, Audit: audit, Policy: store, Metrics: prometheus.NewRegistry(), Version: "v1.2.3-test",
	}
	if err := e.svc.Register(srv, deps); err != nil {
		t.Fatal(err)
	}
	e.svc.srv.poll.interval = o.pollInterval
	lis := fake.Bufconn()
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	e.client = agentv1.NewHostAgentClient(conn)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { e.runErr <- e.svc.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-e.runErr:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("Run did not return")
		}
		_ = conn.Close()
		srv.Stop()
	})
	e.waitWorker()
	return e
}

// waitWorker waits until a worker is up.
func (e *testEnv) waitWorker() {
	e.t.Helper()
	sup := e.svc.srv.sup
	waitFor(e.t, "worker up", sup.Changed, func() bool { return sup.Check(context.Background()) == nil })
}

// vm returns the named VM as ListVMs reports it.
func (e *testEnv) vm(name string) *agentv1.VM {
	e.t.Helper()
	resp, err := e.client.ListVMs(testCtx(e.t), &agentv1.ListVMsRequest{})
	if err != nil {
		e.t.Fatalf("ListVMs: %v", err)
	}
	for _, v := range resp.GetSnapshot().GetVms() {
		if v.GetName() == name {
			return v
		}
	}
	e.t.Fatalf("no VM %q", name)
	return nil
}

func wantCode(t *testing.T, what string, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Errorf("%s: code %s (%v), want %s", what, got, err, want)
	}
}

// resizeDetails returns the ResizeResult in a status error's details.
func resizeDetails(err error) *agentv1.ResizeResult {
	for _, d := range status.Convert(err).Details() {
		if r, ok := d.(*agentv1.ResizeResult); ok {
			return r
		}
	}
	return nil
}

// testPolicy is the policy most tests run with, matching the fixtures.
func testPolicy() policy.Policy {
	return policy.Policy{VMs: []policy.VM{
		{Name: "windows-node-1", Min: 4 * gib, Max: 24 * gib},
		{Name: "windows-dm-1", Min: 1 * gib, Max: 16 * gib},
		{Name: "linux-node-1", Fixed: true},
		{Name: "windows-off-1", Min: 2 * gib, Max: 16 * gib},
		{Name: "windows-saved-1", Min: 2 * gib, Max: 16 * gib},
	}}
}

func ptr[T any](v T) *T { return &v }

func startupReq(vm string, n int64) *agentv1.SetStartupRequest {
	return &agentv1.SetStartupRequest{VmName: vm, StartupBytes: n}
}

func limitsReq(vm string, maximum int64) *agentv1.SetDynamicLimitsRequest {
	return &agentv1.SetDynamicLimitsRequest{VmName: vm, MaximumBytes: &maximum}
}
