package cli

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
	"github.com/matfax/hvmc/internal/fake"
	"github.com/matfax/hvmc/internal/ledger"
)

// planReport is the result of `hvmc plan`. Its JSON form is the ledger
// snapshot plus the evaluation of every queued request, with the field names
// of the ledger's Go types.
type planReport struct {
	Snapshot   ledger.Snapshot
	Evaluation []requestEvaluation
	// Notes are agent calls that failed; the ledger treats their data as
	// missing.
	Notes []string `json:",omitempty"`
}

// requestEvaluation is ledger.Evaluate for one Pending request on each of
// its candidate VMs.
type requestEvaluation struct {
	ID        ledger.RequestID
	Position  ledger.Position
	Grantable bool
	Fits      []ledger.Fit
}

// buildPlan starts the fake agents of pf, reads them over gRPC into a new
// ledger for cfg, queues pf's requests and evaluates the queue. All clocks
// read now.
func buildPlan(ctx context.Context, cfg ledger.Config, pf *planFile, now time.Time) (*planReport, error) {
	reqs, err := pf.ledgerRequests()
	if err != nil {
		return nil, err
	}
	clk := fake.NewManualClock(now)
	sim, err := fake.New(&pf.Scenario, fake.WithClock(clk))
	if err != nil {
		return nil, fmt.Errorf("scenario: %w", err)
	}
	a, err := startAgents(sim, &pf.Scenario)
	if err != nil {
		return nil, err
	}
	defer a.close()

	led := ledger.New(cfg, clk)
	r := &reader{led: led}
	r.readHost(ctx, a.host, cfg.HostAgent)
	for _, g := range a.guests {
		r.readGuest(ctx, g)
	}
	for _, req := range reqs {
		if _, err := led.Enqueue(req); err != nil {
			return nil, fmt.Errorf("request %s: %w", req.ID, err)
		}
	}
	return evaluate(led, r.notes), nil
}

// evaluate takes the ledger's snapshot and evaluates every Pending request.
func evaluate(led *ledger.Ledger, notes []string) *planReport {
	snap := led.Snapshot()
	rep := &planReport{Snapshot: snap, Notes: notes}
	for _, lv := range snap.Leases {
		if lv.Phase != ledger.PhasePending {
			continue
		}
		rep.Evaluation = append(rep.Evaluation, requestEvaluation{
			ID: lv.Request.ID, Position: lv.Position, Grantable: lv.Grantable,
			Fits: led.Evaluate(lv.Request.ID, nil),
		})
	}
	return rep
}

// agentConn is a client connection to one fake agent.
type agentConn struct {
	name string
	// vm is the VM a guest agent serves.
	vm   ledger.VMName
	conn *grpc.ClientConn
}

// agents are the running fake agents of a plan.
type agents struct {
	servers []*fake.Server
	host    agentConn
	guests  []agentConn
}

// startAgents serves the host and every guest of sc over bufconn and dials
// them.
func startAgents(sim *fake.Sim, sc *fake.Scenario) (*agents, error) {
	a := &agents{}
	conn, err := a.serve(sim.ServeHost(fake.Bufconn()))
	if err != nil {
		a.close()
		return nil, err
	}
	a.host = agentConn{name: sc.Host.Name, conn: conn}
	for _, g := range sc.Guests {
		srv, err := sim.ServeGuest(g.Name, fake.Bufconn())
		if err == nil {
			conn, err = a.serve(srv)
		}
		if err != nil {
			a.close()
			return nil, err
		}
		vm := g.VM
		if vm == "" {
			vm = g.Name
		}
		a.guests = append(a.guests, agentConn{name: g.Name, vm: ledger.VMName(vm), conn: conn})
	}
	return a, nil
}

func (a *agents) serve(srv *fake.Server) (*grpc.ClientConn, error) {
	a.servers = append(a.servers, srv)
	return srv.Dial()
}

func (a *agents) close() {
	for _, c := range append([]agentConn{a.host}, a.guests...) {
		if c.conn != nil {
			_ = c.conn.Close()
		}
	}
	for _, s := range a.servers {
		s.Stop()
	}
}

// reader reads agents into a ledger, as the coordinator's health tracker and
// watchers do, and records failed calls.
type reader struct {
	led   *ledger.Ledger
	notes []string
}

// call runs one unary RPC with callTimeout.
func call[Req, Resp any](ctx context.Context, rpc func(context.Context, Req, ...grpc.CallOption) (Resp, error), req Req) (Resp, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return rpc(ctx, req)
}

func (r *reader) note(agent, method string, err error) {
	st := status.Convert(err)
	r.notes = append(r.notes, fmt.Sprintf("%s: %s: %s: %s", agent, method, st.Code(), st.Message()))
}

// health maps one grpc.health.v1 check to an agent state (DESIGN §4.8):
// SERVING is Ready, any other answer Degraded, no answer Unreachable.
func (r *reader) health(ctx context.Context, a agentConn) ledger.AgentState {
	resp, err := call(ctx, healthpb.NewHealthClient(a.conn).Check, &healthpb.HealthCheckRequest{})
	switch {
	case err != nil:
		r.note(a.name, "Health.Check", err)
		return ledger.AgentUnreachable
	case resp.GetStatus() == healthpb.HealthCheckResponse_SERVING:
		return ledger.AgentReady
	default:
		return ledger.AgentDegraded
	}
}

// readHost reads the host agent's health, host memory and VM list.
func (r *reader) readHost(ctx context.Context, a agentConn, agent ledger.AgentName) {
	r.led.SetAgentState(agent, r.health(ctx, a))
	client := agentv1.NewHostAgentClient(a.conn)
	var obs ledger.Observation
	if resp, err := call(ctx, client.GetHost, &agentv1.GetHostRequest{}); err != nil {
		r.note(a.name, "GetHost", err)
	} else {
		h := resp.GetHost()
		obs.Host = &ledger.HostObservation{
			Total: h.GetTotalBytes(), Free: h.GetFreeBytes(), ObservedAt: h.GetObservedAt().AsTime(),
		}
	}
	if resp, err := call(ctx, client.ListVMs, &agentv1.ListVMsRequest{}); err != nil {
		r.note(a.name, "ListVMs", err)
	} else {
		for _, vm := range resp.GetSnapshot().GetVms() {
			obs.VMs = append(obs.VMs, vmObservation(vm))
		}
		obs.CompleteVMList = true
	}
	r.led.Observe(obs)
}

func vmObservation(vm *agentv1.VM) ledger.VMObservation {
	o := ledger.VMObservation{
		Name:       ledger.VMName(vm.GetName()),
		Assigned:   vm.GetAssignedBytes(),
		Startup:    vm.GetStartupBytes(),
		ObservedAt: vm.GetObservedAt().AsTime(),
	}
	if dm := vm.GetDynamicMemory(); dm.GetEnabled() {
		o.Dynamic = &ledger.DynamicMemory{
			Minimum:  dm.GetMinimumBytes(),
			Maximum:  dm.GetMaximumBytes(),
			Buffer:   int64(dm.GetBufferPercent()),
			Priority: int(dm.GetPriority()),
		}
	}
	return o
}

// readGuest reads a guest agent's health and memory; the guest agent of a VM
// carries the VM's name (HostConfig status.agents).
func (r *reader) readGuest(ctx context.Context, a agentConn) {
	r.led.SetAgentState(ledger.AgentName(a.vm), r.health(ctx, a))
	resp, err := call(ctx, agentv1.NewGuestAgentClient(a.conn).GetMemory, &agentv1.GetMemoryRequest{})
	if err != nil {
		r.note(a.name, "GetMemory", err)
		return
	}
	m := resp.GetMemory()
	r.led.Observe(ledger.Observation{Guest: &ledger.GuestObservation{
		VM:              a.vm,
		Total:           m.GetTotalBytes(),
		Available:       m.GetAvailableBytes(),
		CommitLimit:     m.GetCommitLimitBytes(),
		CommitAvailable: m.GetCommitAvailableBytes(),
		ObservedAt:      m.GetObservedAt().AsTime(),
	}})
}
