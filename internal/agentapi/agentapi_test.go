package agentapi_test

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
)

// TestServiceDescriptors pins the service and method names that the health
// contract, the agents and the coordinator depend on.
func TestServiceDescriptors(t *testing.T) {
	type method struct {
		name            string
		serverStreaming bool
	}
	tests := []struct {
		service string
		desc    grpc.ServiceDesc
		methods []method
	}{
		{
			service: "hvmc.agent.v1.HostAgent",
			desc:    agentv1.HostAgent_ServiceDesc,
			methods: []method{
				{name: "GetHost"},
				{name: "ListVMs"},
				{name: "WatchVMs", serverStreaming: true},
				{name: "SetStartup"},
				{name: "SetDynamicLimits"},
				{name: "ProposePolicy"},
			},
		},
		{
			service: "hvmc.agent.v1.GuestAgent",
			desc:    agentv1.GuestAgent_ServiceDesc,
			methods: []method{
				{name: "GetMemory"},
				{name: "WatchMemory", serverStreaming: true},
			},
		},
		{
			service: "hvmc.agent.v1.CertificateService",
			desc:    agentv1.CertificateService_ServiceDesc,
			methods: []method{
				{name: "RotateCertificate"},
				{name: "InstallCertificate"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.service, func(t *testing.T) {
			if tt.desc.ServiceName != tt.service {
				t.Fatalf("ServiceDesc name = %q, want %q", tt.desc.ServiceName, tt.service)
			}
			d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(tt.service))
			if err != nil {
				t.Fatalf("service not registered: %v", err)
			}
			sd, ok := d.(protoreflect.ServiceDescriptor)
			if !ok {
				t.Fatalf("%s is a %T, not a service", tt.service, d)
			}
			if got, want := sd.Methods().Len(), len(tt.methods); got != want {
				t.Fatalf("%d methods, want %d", got, want)
			}
			for _, m := range tt.methods {
				md := sd.Methods().ByName(protoreflect.Name(m.name))
				if md == nil {
					t.Fatalf("method %s missing", m.name)
				}
				if md.IsStreamingServer() != m.serverStreaming || md.IsStreamingClient() {
					t.Errorf("%s: server streaming = %v, client streaming = %v; want %v, false",
						m.name, md.IsStreamingServer(), md.IsStreamingClient(), m.serverStreaming)
				}
			}
		})
	}
}

func TestMessageRoundTrip(t *testing.T) {
	now := timestamppb.New(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	minBytes, maxBytes := int64(4<<30), int64(32<<30)
	commitLimit, commitAvail := int64(12<<30), int64(5<<30)
	msgs := []proto.Message{
		&agentv1.GetHostResponse{Host: &agentv1.Host{
			Name:         "host-1",
			TotalBytes:   256 << 30,
			FreeBytes:    40 << 30,
			Agent:        &agentv1.AgentInfo{Version: "v0.1.0", Role: agentv1.AgentRole_AGENT_ROLE_HOST, OperatingSystem: agentv1.OperatingSystem_OPERATING_SYSTEM_WINDOWS},
			PolicyDigest: "digest-a",
			ObservedAt:   now,
		}},
		&agentv1.SetDynamicLimitsRequest{VmName: "vm-1", MinimumBytes: &minBytes, MaximumBytes: &maxBytes},
		&agentv1.SetStartupResponse{Result: &agentv1.ResizeResult{
			Before: &agentv1.VM{Name: "vm-1", State: agentv1.VMState_VM_STATE_RUNNING, AssignedBytes: 8 << 30, StartupBytes: 8 << 30, DynamicMemory: &agentv1.DynamicMemory{}, ObservedAt: now},
			After:  &agentv1.VM{Name: "vm-1", State: agentv1.VMState_VM_STATE_RUNNING, AssignedBytes: 10 << 30, StartupBytes: 10 << 30, DynamicMemory: &agentv1.DynamicMemory{}, ObservedAt: now},
		}},
		&agentv1.ProposePolicyRequest{Digest: "digest-b", Policy: &agentv1.Policy{Vms: []*agentv1.VMPolicy{
			{Name: "linux-1", Fixed: true},
			{Name: "windows-1", MinBytes: 6 << 30, MaxBytes: 24 << 30},
		}}},
		&agentv1.GetMemoryResponse{Memory: &agentv1.GuestMemory{
			TotalBytes: 8 << 30, AvailableBytes: 3 << 30,
			CommitLimitBytes: &commitLimit, CommitAvailableBytes: &commitAvail, ObservedAt: now,
		}},
		&agentv1.InstallCertificateRequest{CertificateChainDer: [][]byte{{0x30, 0x82}, {0x30, 0x81}}},
	}
	for _, m := range msgs {
		b, err := proto.Marshal(m)
		if err != nil {
			t.Fatalf("marshal %T: %v", m, err)
		}
		got := m.ProtoReflect().New().Interface()
		if err := proto.Unmarshal(b, got); err != nil {
			t.Fatalf("unmarshal %T: %v", m, err)
		}
		if !proto.Equal(m, got) {
			t.Errorf("%T did not round-trip:\n got %v\nwant %v", m, got, m)
		}
	}

	// Commit fields have presence: a Linux reading leaves them unset.
	linux := &agentv1.GuestMemory{TotalBytes: 1 << 30}
	b, err := proto.Marshal(linux)
	if err != nil {
		t.Fatal(err)
	}
	var back agentv1.GuestMemory
	if err := proto.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.CommitLimitBytes != nil || back.CommitAvailableBytes != nil {
		t.Errorf("commit fields set on a reading without them: %v", &back)
	}
}

type hostServer struct {
	agentv1.UnimplementedHostAgentServer
}

func (hostServer) GetHost(context.Context, *agentv1.GetHostRequest) (*agentv1.GetHostResponse, error) {
	return &agentv1.GetHostResponse{Host: &agentv1.Host{Name: "host", TotalBytes: 64 << 30}}, nil
}

func (hostServer) WatchVMs(_ *agentv1.WatchVMsRequest, stream grpc.ServerStreamingServer[agentv1.WatchVMsResponse]) error {
	return stream.Send(&agentv1.WatchVMsResponse{Snapshot: &agentv1.VMSnapshot{Vms: []*agentv1.VM{{Name: "vm-1"}}}})
}

// TestInProcessServer registers all three services on one server, the way an
// agent and the fake agents do, and calls them through the generated clients.
func TestInProcessServer(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	agentv1.RegisterHostAgentServer(srv, hostServer{})
	agentv1.RegisterGuestAgentServer(srv, agentv1.UnimplementedGuestAgentServer{})
	agentv1.RegisterCertificateServiceServer(srv, agentv1.UnimplementedCertificateServiceServer{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	for _, name := range []string{"hvmc.agent.v1.HostAgent", "hvmc.agent.v1.GuestAgent", "hvmc.agent.v1.CertificateService"} {
		if _, ok := srv.GetServiceInfo()[name]; !ok {
			t.Errorf("service %s not registered", name)
		}
	}

	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	host := agentv1.NewHostAgentClient(conn)
	resp, err := host.GetHost(ctx, &agentv1.GetHostRequest{})
	if err != nil {
		t.Fatalf("GetHost: %v", err)
	}
	if resp.GetHost().GetTotalBytes() != 64<<30 {
		t.Errorf("GetHost total = %d", resp.GetHost().GetTotalBytes())
	}

	stream, err := host.WatchVMs(ctx, &agentv1.WatchVMsRequest{})
	if err != nil {
		t.Fatalf("WatchVMs: %v", err)
	}
	snap, err := stream.Recv()
	if err != nil {
		t.Fatalf("WatchVMs recv: %v", err)
	}
	if got := snap.GetSnapshot().GetVms(); len(got) != 1 || got[0].GetName() != "vm-1" {
		t.Errorf("snapshot VMs = %v", got)
	}

	_, err = host.SetStartup(ctx, &agentv1.SetStartupRequest{VmName: "vm-1", StartupBytes: 8 << 30})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("SetStartup on unimplemented server: %v, want Unimplemented", err)
	}
	_, err = agentv1.NewGuestAgentClient(conn).GetMemory(ctx, &agentv1.GetMemoryRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("GetMemory: %v, want Unimplemented", err)
	}
	_, err = agentv1.NewCertificateServiceClient(conn).RotateCertificate(ctx, &agentv1.RotateCertificateRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("RotateCertificate: %v, want Unimplemented", err)
	}
}
