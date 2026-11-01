package fake

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
)

// GuestWatchInterval is how often WatchMemory sends a reading without a
// change (guest.proto).
const GuestWatchInterval = 5 * time.Second

// guestServer implements agentv1.GuestAgentServer for one guest of a Sim.
type guestServer struct {
	agentv1.UnimplementedGuestAgentServer
	sim  *Sim
	name string
}

// readingLocked returns the guest's memory, or UNAVAILABLE when it cannot be
// read: the guest is gone, its memory source is down or its VM is not
// running (a real coordinator would fail to connect).
func (g *guestServer) readingLocked() (*agentv1.GuestMemory, *agentv1.AgentInfo, error) {
	s := g.sim
	spec := s.sc.guest(g.name)
	switch {
	case spec == nil:
		return nil, nil, status.Errorf(codes.Unavailable, "agent %s is gone", g.name)
	case spec.SourceDown:
		return nil, nil, status.Error(codes.Unavailable, "memory source could not be read")
	case !s.guestVMRunningLocked(spec):
		return nil, nil, status.Errorf(codes.Unavailable, "VM %s is not running", spec.VM)
	}
	m := s.memoryLocked(spec)
	return m, guestAgentInfo(s.version, spec.OS), nil
}

func guestAgentInfo(version, os string) *agentv1.AgentInfo {
	info := &agentv1.AgentInfo{
		Version:         version,
		Role:            agentv1.AgentRole_AGENT_ROLE_GUEST,
		OperatingSystem: agentv1.OperatingSystem_OPERATING_SYSTEM_LINUX,
	}
	if os == OSWindows {
		info.OperatingSystem = agentv1.OperatingSystem_OPERATING_SYSTEM_WINDOWS
	}
	return info
}

// GetMemory implements agentv1.GuestAgentServer.
func (g *guestServer) GetMemory(ctx context.Context, _ *agentv1.GetMemoryRequest) (*agentv1.GetMemoryResponse, error) {
	s := g.sim
	if err := s.enter(ctx, g.name, "GetMemory", false); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, info, err := g.readingLocked()
	if err != nil {
		return nil, err
	}
	return &agentv1.GetMemoryResponse{Memory: m, Agent: info}, nil
}

// WatchMemory implements agentv1.GuestAgentServer: a reading on connect,
// then every GuestWatchInterval on the Sim's clock and whenever total,
// available or the commit figures change. It ends with UNAVAILABLE when the
// guest cannot be read any more.
func (g *guestServer) WatchMemory(_ *agentv1.WatchMemoryRequest, stream grpc.ServerStreamingServer[agentv1.WatchMemoryResponse]) error {
	s := g.sim
	ctx := stream.Context()
	if err := s.enter(ctx, g.name, "WatchMemory", false); err != nil {
		return err
	}
	timer := s.clock.NewTimer(GuestWatchInterval)
	defer func() { timer.Stop() }()
	var last *agentv1.GuestMemory
	tick := false
	for {
		s.mu.Lock()
		m, _, err := g.readingLocked()
		changed := s.changed
		s.mu.Unlock()
		if err != nil {
			return err
		}
		if last == nil || tick || !sameMemory(last, m) {
			if err := stream.Send(&agentv1.WatchMemoryResponse{Memory: m}); err != nil {
				return err
			}
			last = m
		}
		tick = false
		select {
		case <-changed:
		case <-timer.C():
			tick = true
			timer = s.clock.NewTimer(GuestWatchInterval)
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
	}
}

// sameMemory compares two readings ignoring observation times.
func sameMemory(a, b *agentv1.GuestMemory) bool {
	ca, cb := proto.CloneOf(a), proto.CloneOf(b)
	ca.ObservedAt, cb.ObservedAt = nil, nil
	return proto.Equal(ca, cb)
}
