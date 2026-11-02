package fake

import (
	"context"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/test/bufconn"

	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
)

// bufconnSize is the buffer of in-memory listeners.
const bufconnSize = 1 << 20

// Bufconn returns an in-memory listener for ServeHost or ServeGuest.
func Bufconn() *bufconn.Listener { return bufconn.Listen(bufconnSize) }

// Server is one running fake agent.
type Server struct {
	grpc *grpc.Server
	lis  net.Listener
}

// RegisterHost registers the host agent's services on srv: HostAgent,
// CertificateService and grpc.health.v1.Health. GuestAgent stays
// unregistered, so it answers UNIMPLEMENTED like a real host agent.
func (s *Sim) RegisterHost(srv grpc.ServiceRegistrar) {
	s.mu.Lock()
	name := s.sc.Host.Name
	s.mu.Unlock()
	agentv1.RegisterHostAgentServer(srv, &hostServer{sim: s, name: name})
	agentv1.RegisterCertificateServiceServer(srv, &certServer{sim: s, name: name})
	healthpb.RegisterHealthServer(srv, s.healthFor(name))
}

// RegisterGuest registers the services of the named guest agent on srv:
// GuestAgent, CertificateService and grpc.health.v1.Health.
func (s *Sim) RegisterGuest(srv grpc.ServiceRegistrar, name string) error {
	s.mu.Lock()
	ok := s.sc.guest(name) != nil
	s.mu.Unlock()
	if !ok {
		return errNoAgent(name)
	}
	agentv1.RegisterGuestAgentServer(srv, &guestServer{sim: s, name: name})
	agentv1.RegisterCertificateServiceServer(srv, &certServer{sim: s, name: name})
	healthpb.RegisterHealthServer(srv, s.healthFor(name))
	return nil
}

// ServeHost serves the host agent on lis (Bufconn() or a real listener)
// until Stop. opts are passed to grpc.NewServer, e.g. grpc.Creds for mTLS.
func (s *Sim) ServeHost(lis net.Listener, opts ...grpc.ServerOption) *Server {
	srv := grpc.NewServer(opts...)
	s.RegisterHost(srv)
	return start(srv, lis)
}

// ServeGuest serves the named guest agent on lis until Stop.
func (s *Sim) ServeGuest(name string, lis net.Listener, opts ...grpc.ServerOption) (*Server, error) {
	srv := grpc.NewServer(opts...)
	if err := s.RegisterGuest(srv, name); err != nil {
		return nil, err
	}
	return start(srv, lis), nil
}

func start(srv *grpc.Server, lis net.Listener) *Server {
	go func() { _ = srv.Serve(lis) }() // returns once Stop closes the listener
	return &Server{grpc: srv, lis: lis}
}

// Addr returns the listener's address.
func (srv *Server) Addr() net.Addr { return srv.lis.Addr() }

// Stop closes the listener and every connection; open streams end with
// UNAVAILABLE on the client.
func (srv *Server) Stop() { srv.grpc.Stop() }

// Dial returns a client connection to the server: through memory for a
// Bufconn listener, to Addr otherwise. It uses insecure transport
// credentials unless opts carry others.
func (srv *Server) Dial(opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	all := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	target := "passthrough:///" + srv.lis.Addr().String()
	if bl, ok := srv.lis.(*bufconn.Listener); ok {
		target = "passthrough:///bufconn"
		all = append(all, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return bl.DialContext(ctx)
		}))
	}
	return grpc.NewClient(target, append(all, opts...)...)
}
