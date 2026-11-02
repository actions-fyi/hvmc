package fake_test

import (
	"context"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
	"github.com/matfax/hvmc/internal/fake"
)

const (
	gib = int64(fake.GiB)
	mib = int64(fake.MiB)
)

// t0 is the fake clock's start.
var t0 = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// env is a Sim from testdata/scenario.yaml with the host and every guest
// served over bufconn.
type env struct {
	t    *testing.T
	sim  *fake.Sim
	clk  *fake.ManualClock
	host agentv1.HostAgentClient
	// conns by agent name.
	conns map[string]*grpc.ClientConn
}

func loadTestScenario(t *testing.T) *fake.Scenario {
	t.Helper()
	sc, err := fake.LoadScenario("testdata/scenario.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// newEnv starts the test scenario, after edit (may be nil) has changed it.
func newEnv(t *testing.T, edit func(*fake.Scenario), opts ...fake.Option) *env {
	t.Helper()
	sc := loadTestScenario(t)
	if edit != nil {
		edit(sc)
	}
	clk := fake.NewManualClock(t0)
	sim, err := fake.New(sc, append([]fake.Option{fake.WithClock(clk)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, sim: sim, clk: clk, conns: map[string]*grpc.ClientConn{}}
	e.conns[sc.Host.Name] = e.dial(sim.ServeHost(fake.Bufconn()))
	for _, g := range sc.Guests {
		srv, err := sim.ServeGuest(g.Name, fake.Bufconn())
		if err != nil {
			t.Fatal(err)
		}
		e.conns[g.Name] = e.dial(srv)
	}
	e.host = agentv1.NewHostAgentClient(e.conns[sc.Host.Name])
	return e
}

func (e *env) dial(srv *fake.Server) *grpc.ClientConn {
	e.t.Helper()
	e.t.Cleanup(srv.Stop)
	conn, err := srv.Dial()
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func (e *env) guest(name string) agentv1.GuestAgentClient {
	return agentv1.NewGuestAgentClient(e.conns[name])
}

func (e *env) certs(agent string) agentv1.CertificateServiceClient {
	return agentv1.NewCertificateServiceClient(e.conns[agent])
}

func (e *env) health(agent string) healthpb.HealthClient {
	return healthpb.NewHealthClient(e.conns[agent])
}

// vm lists the VMs over gRPC and returns one.
func (e *env) vm(ctx context.Context, name string) *agentv1.VM {
	e.t.Helper()
	resp, err := e.host.ListVMs(ctx, &agentv1.ListVMsRequest{})
	if err != nil {
		e.t.Fatalf("ListVMs: %v", err)
	}
	for _, v := range resp.GetSnapshot().GetVms() {
		if v.GetName() == name {
			return v
		}
	}
	e.t.Fatalf("VM %s not listed", name)
	return nil
}

func (e *env) free(ctx context.Context) int64 {
	e.t.Helper()
	resp, err := e.host.GetHost(ctx, &agentv1.GetHostRequest{})
	if err != nil {
		e.t.Fatalf("GetHost: %v", err)
	}
	return resp.GetHost().GetFreeBytes()
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

// testCtx is a context that bounds a test against deadlocks; tests never
// rely on it expiring.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// shortCtx is a context whose deadline the caller expects to hit (hangs).
func shortCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("got %v (%v), want %v", got, err, want)
	}
}

// resizeDetails extracts the ResizeResult attached to an error status.
func resizeDetails(t *testing.T, err error) *agentv1.ResizeResult {
	t.Helper()
	for _, d := range status.Convert(err).Details() {
		if r, ok := d.(*agentv1.ResizeResult); ok {
			return r
		}
	}
	t.Fatalf("no ResizeResult in %v", err)
	return nil
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ptr[T any](v T) *T { return &v }

func timestamp(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }
