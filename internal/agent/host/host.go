// Package host is the hvmc host agent (DESIGN §4.1–§4.3): the HostAgent
// gRPC service on the Hyper-V host. It drives Hyper-V through one
// supervised PowerShell worker (worker.go, worker.ps1), enforces the
// host's active write policy on every write (enforce.go) and streams VM
// snapshots from a local poll (watch.go). The role-independent parts
// (mTLS, health, audit, policy store) come from internal/agent.
package host

import (
	"context"
	"errors"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/matfax/hvmc/internal/agent"
	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
	"github.com/matfax/hvmc/internal/policy"
)

// Options configure the host agent's service. The zero value runs pwsh
// from PATH with worker.ps1.
type Options struct {
	// Start starts the worker; default PwshStarter, logging the worker's
	// stderr.
	Start Starter
	// Script is sent to every new worker; default worker.ps1. Only used
	// with the default Start.
	Script []byte
	// CallTimeout is the per-call timeout; default DefaultCallTimeout.
	CallTimeout time.Duration
}

// Service is the host role's agent.Service.
type Service struct {
	opts Options
	srv  *server
}

// New returns the host agent's service with default options.
func New() agent.Service { return NewWithOptions(Options{}) }

// NewWithOptions returns the host agent's service.
func NewWithOptions(o Options) *Service { return &Service{opts: o} }

// ServiceName implements agent.Service.
func (s *Service) ServiceName() string { return agentv1.HostAgent_ServiceDesc.ServiceName }

// Register implements agent.Service: it registers HostAgent and the
// readiness checks "vmms", "worker" and "hyperv-read".
func (s *Service) Register(reg grpc.ServiceRegistrar, deps agent.Deps) error {
	if deps.Policy == nil {
		return errors.New("host role needs a policy store")
	}
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With("component", "host")
	start, script := s.opts.Start, s.opts.Script
	if start == nil {
		start = PwshStarter(&logWriter{log: log})
		if script == nil {
			script = workerScript
		}
	}
	restarts := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "hvmc_agent_worker_restarts_total",
		Help: "Exits of the PowerShell worker that led to a restart or to giving up.",
	})
	if deps.Metrics != nil {
		if err := deps.Metrics.Register(restarts); err != nil {
			return err
		}
	}
	sup := NewSupervisor(SupervisorOptions{
		Start: start, Script: script, Clock: deps.Clock, Log: log,
		CallTimeout: s.opts.CallTimeout, OnExit: restarts.Inc,
	})
	s.srv = newServer(sup, deps, log)
	agentv1.RegisterHostAgentServer(reg, s.srv)
	for _, c := range []struct {
		name string
		fn   agent.CheckFunc
	}{
		{"vmms", s.srv.poll.checkVMMS},
		{"worker", sup.Check},
		{"hyperv-read", s.srv.poll.checkRead},
	} {
		if err := deps.Readiness.Register(c.name, c.fn); err != nil {
			return err
		}
	}
	return nil
}

// Run implements agent.Service: it supervises the worker and polls
// Hyper-V until ctx ends. It fails with ErrWorkerFailing when the worker
// keeps exiting.
func (s *Service) Run(ctx context.Context) error {
	if s.srv == nil {
		return errors.New("host service not registered")
	}
	return s.srv.run(ctx)
}

// server implements agentv1.HostAgentServer.
type server struct {
	agentv1.UnimplementedHostAgentServer

	sup      *Supervisor
	hv       *hyperV
	poll     *poller
	store    *agent.PolicyStore
	clk      agent.Clock
	log      *slog.Logger
	version  string
	writeSem chan struct{}
}

func newServer(sup *Supervisor, deps agent.Deps, log *slog.Logger) *server {
	hv := &hyperV{sup: sup, clk: deps.Clock}
	return &server{
		sup: sup, hv: hv, poll: newPoller(hv, sup, deps.Clock),
		store: deps.Policy, clk: deps.Clock, log: log, version: deps.Version,
		writeSem: make(chan struct{}, 1),
	}
}

// run supervises the worker and polls until ctx ends or the supervisor
// gives up.
func (s *server) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	if s.poll.interval > 0 {
		wg.Go(func() { s.poll.run(ctx) })
	}
	err := s.sup.Run(ctx)
	cancel()
	wg.Wait()
	return err
}

// policy returns the active policy every write is checked against.
func (s *server) policy() policy.Policy { return s.store.Active() }

// GetHost implements agentv1.HostAgentServer.
func (s *server) GetHost(ctx context.Context, _ *agentv1.GetHostRequest) (*agentv1.GetHostResponse, error) {
	h, at, err := s.hv.host(ctx)
	if err != nil {
		return nil, toStatus(err)
	}
	pending := ""
	if p, ok := s.store.Pending(); ok {
		pending = p.Digest()
	}
	return &agentv1.GetHostResponse{Host: &agentv1.Host{
		Name:       h.ComputerName,
		TotalBytes: h.TotalKiB * 1024,
		FreeBytes:  h.FreeKiB * 1024,
		Agent: &agentv1.AgentInfo{
			Version:         s.version,
			Role:            agentv1.AgentRole_AGENT_ROLE_HOST,
			OperatingSystem: operatingSystem(runtime.GOOS),
		},
		PolicyDigest:        s.store.ActiveDigest(),
		PendingPolicyDigest: pending,
		ObservedAt:          timestamppb.New(at),
	}}, nil
}

func operatingSystem(goos string) agentv1.OperatingSystem {
	switch goos {
	case "windows":
		return agentv1.OperatingSystem_OPERATING_SYSTEM_WINDOWS
	case "linux":
		return agentv1.OperatingSystem_OPERATING_SYSTEM_LINUX
	default:
		return agentv1.OperatingSystem_OPERATING_SYSTEM_UNSPECIFIED
	}
}

// ListVMs implements agentv1.HostAgentServer. The read also feeds the
// poll's view.
func (s *server) ListVMs(ctx context.Context, _ *agentv1.ListVMsRequest) (*agentv1.ListVMsResponse, error) {
	list, at, err := s.hv.vms(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.poll.record(nil, err)
		}
		return nil, toStatus(err)
	}
	snap := snapshotMessage(list, at)
	s.poll.record(snap, nil)
	return &agentv1.ListVMsResponse{Snapshot: snap}, nil
}

// ProposePolicy implements agentv1.HostAgentServer by delegating to the
// policy store, which only ever stores a pending policy.
func (s *server) ProposePolicy(ctx context.Context, req *agentv1.ProposePolicyRequest) (*agentv1.ProposePolicyResponse, error) {
	return s.store.ProposePolicy(ctx, req)
}
