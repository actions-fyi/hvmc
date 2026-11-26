// Package guest is the guest agent (DESIGN §4.1, §4.4): the GuestAgent
// gRPC service that reports a managed VM's memory as the guest sees it.
// Linux reads MemTotal and MemAvailable from /proc/meminfo; Windows calls
// GlobalMemoryStatusEx.
//
// The service polls its memory source every PollInterval. GetMemory reads
// the source at call time; WatchMemory sends a reading on connect, then
// every WatchInterval and whenever a poll sees a value differ from the
// last one sent. An unreadable source gives UNAVAILABLE, like the fake
// guest in internal/fake, and fails the readiness check.
package guest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/matfax/hvmc/internal/agent"
	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
)

const (
	// PollInterval is how often the service reads its memory source.
	PollInterval = time.Second
	// WatchInterval is how often WatchMemory sends a reading without a
	// change (guest.proto).
	WatchInterval = 5 * time.Second
	// ReadinessCheck is the name of the readiness check: the last
	// successful memory read is at most 2 × PollInterval old (DESIGN §4.8).
	ReadinessCheck = "memory read"
)

// ErrUnsupported is returned by the memory source on an operating system
// without one.
var ErrUnsupported = errors.New("no memory source on this operating system")

// Reading is one reading of a memory source, in bytes. The commit figures
// are set on Windows only.
type Reading struct {
	Total, Available             int64
	CommitLimit, CommitAvailable *int64
}

// Source reads the guest's memory now.
type Source func() (Reading, error)

// Service is the guest role's agent.Service.
type Service struct {
	src Source
	os  agentv1.OperatingSystem

	clk     agent.Clock
	log     *slog.Logger
	version string

	mu      sync.Mutex
	last    *agentv1.GuestMemory // last successful reading
	lastOK  time.Time            // when it was read
	lastErr error                // error of the last read, nil after a success
	changed chan struct{}        // closed and replaced when a reading differs
}

// New returns the guest service with this operating system's memory source.
func New() *Service { return NewWithSource(platformSource, currentOS()) }

// NewWithSource returns the guest service reading src and reporting os in
// AgentInfo.
func NewWithSource(src Source, os agentv1.OperatingSystem) *Service {
	return &Service{src: src, os: os, changed: make(chan struct{}), log: slog.New(slog.DiscardHandler)}
}

func currentOS() agentv1.OperatingSystem {
	switch runtime.GOOS {
	case "linux":
		return agentv1.OperatingSystem_OPERATING_SYSTEM_LINUX
	case "windows":
		return agentv1.OperatingSystem_OPERATING_SYSTEM_WINDOWS
	default:
		return agentv1.OperatingSystem_OPERATING_SYSTEM_UNSPECIFIED
	}
}

// ServiceName implements agent.Service.
func (s *Service) ServiceName() string { return agentv1.GuestAgent_ServiceDesc.ServiceName }

// Register implements agent.Service: it registers GuestAgent and the
// readiness check.
func (s *Service) Register(reg grpc.ServiceRegistrar, deps agent.Deps) error {
	if deps.Clock == nil || deps.Readiness == nil {
		return errors.New("guest: Deps.Clock and Deps.Readiness are required")
	}
	s.clk, s.version = deps.Clock, deps.Version
	if deps.Log != nil {
		s.log = deps.Log
	}
	agentv1.RegisterGuestAgentServer(reg, &server{s: s})
	return deps.Readiness.Register(ReadinessCheck, s.checkFresh)
}

// Run implements agent.Service: it polls the memory source every
// PollInterval until ctx ends.
func (s *Service) Run(ctx context.Context) error {
	for {
		if _, _, err := s.sample(); err != nil {
			s.log.Warn("memory source could not be read", "err", err)
		}
		t := s.clk.NewTimer(PollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C():
		}
	}
}

// sample reads the source, records the result and returns the reading and
// the channel that is closed when a later reading differs. A failed read
// returns UNAVAILABLE.
func (s *Service) sample() (*agentv1.GuestMemory, <-chan struct{}, error) {
	r, err := s.src()
	now := s.clk.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if s.lastErr == nil {
			// Wake open streams so they end now, not at their next
			// heartbeat.
			s.notifyLocked()
		}
		s.lastErr = err
		return nil, s.changed, unavailable(err)
	}
	m := r.proto(now)
	if s.last != nil && !sameMemory(s.last, m) {
		s.notifyLocked()
	}
	s.last, s.lastOK, s.lastErr = m, now, nil
	return proto.CloneOf(m), s.changed, nil
}

// notifyLocked wakes everyone waiting on the current changed channel.
func (s *Service) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// latest returns the last reading the poller recorded, or UNAVAILABLE if
// its last read failed.
func (s *Service) latest() (*agentv1.GuestMemory, <-chan struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastErr != nil || s.last == nil {
		return nil, s.changed, unavailable(s.lastErr)
	}
	return proto.CloneOf(s.last), s.changed, nil
}

// checkFresh is the readiness check. It only reads recorded state.
func (s *Service) checkFresh(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.lastErr != nil:
		return fmt.Errorf("memory source could not be read: %w", s.lastErr)
	case s.lastOK.IsZero():
		return errors.New("no successful memory read yet")
	}
	if age := s.clk.Now().Sub(s.lastOK); age > 2*PollInterval {
		return fmt.Errorf("last successful memory read is %s old (limit %s)", age, 2*PollInterval)
	}
	return nil
}

func (s *Service) agentInfo() *agentv1.AgentInfo {
	return &agentv1.AgentInfo{Version: s.version, Role: agentv1.AgentRole_AGENT_ROLE_GUEST, OperatingSystem: s.os}
}

func unavailable(err error) error {
	if err == nil {
		return status.Error(codes.Unavailable, "memory source has not been read yet")
	}
	return status.Errorf(codes.Unavailable, "memory source could not be read: %v", err)
}

func (r Reading) proto(at time.Time) *agentv1.GuestMemory {
	m := &agentv1.GuestMemory{TotalBytes: r.Total, AvailableBytes: r.Available, ObservedAt: timestamppb.New(at)}
	if r.CommitLimit != nil {
		m.CommitLimitBytes = proto.Int64(*r.CommitLimit)
	}
	if r.CommitAvailable != nil {
		m.CommitAvailableBytes = proto.Int64(*r.CommitAvailable)
	}
	return m
}

// sameMemory compares two readings ignoring observation times.
func sameMemory(a, b *agentv1.GuestMemory) bool {
	ca, cb := proto.CloneOf(a), proto.CloneOf(b)
	ca.ObservedAt, cb.ObservedAt = nil, nil
	return proto.Equal(ca, cb)
}

// server implements agentv1.GuestAgentServer.
type server struct {
	agentv1.UnimplementedGuestAgentServer
	s *Service
}

// GetMemory implements agentv1.GuestAgentServer: it reads the source now.
func (g *server) GetMemory(context.Context, *agentv1.GetMemoryRequest) (*agentv1.GetMemoryResponse, error) {
	m, _, err := g.s.sample()
	if err != nil {
		return nil, err
	}
	return &agentv1.GetMemoryResponse{Memory: m, Agent: g.s.agentInfo()}, nil
}

// WatchMemory implements agentv1.GuestAgentServer: a fresh reading on
// connect and every WatchInterval, and the poller's reading whenever it
// differs from the last one sent. The stream ends with UNAVAILABLE when the
// source cannot be read, and when its context ends (the client left or the
// agent is draining).
func (g *server) WatchMemory(_ *agentv1.WatchMemoryRequest, stream grpc.ServerStreamingServer[agentv1.WatchMemoryResponse]) error {
	ctx := stream.Context()
	m, changed, err := g.s.sample()
	if err != nil {
		return err
	}
	if err := stream.Send(&agentv1.WatchMemoryResponse{Memory: m}); err != nil {
		return err
	}
	last := m
	timer := g.s.clk.NewTimer(WatchInterval)
	defer func() { timer.Stop() }()
	for {
		tick := false
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-changed:
			m, changed, err = g.s.latest()
		case <-timer.C():
			tick = true
			timer = g.s.clk.NewTimer(WatchInterval)
			m, changed, err = g.s.sample()
		}
		if err != nil {
			return err
		}
		if !tick && sameMemory(last, m) {
			continue
		}
		if err := stream.Send(&agentv1.WatchMemoryResponse{Memory: m}); err != nil {
			return err
		}
		last = m
	}
}
