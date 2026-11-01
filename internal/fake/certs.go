package fake

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
)

// certState is an agent's key material.
type certState struct {
	pending  *ecdsa.PrivateKey
	current  *tls.Certificate
	installs int
}

// CertStatus is the certificate state of one agent.
type CertStatus struct {
	// Current is the leaf of the serving pair, nil before the first install
	// or SetServingCertificate.
	Current *x509.Certificate
	// PendingKey reports whether a key from RotateCertificate awaits its
	// certificate.
	PendingKey bool
	// Installs counts successful InstallCertificate calls.
	Installs int
}

// certServer implements agentv1.CertificateServiceServer for one agent.
type certServer struct {
	agentv1.UnimplementedCertificateServiceServer
	sim  *Sim
	name string
}

func (s *Sim) certLocked(agent string) *certState {
	c, ok := s.certs[agent]
	if !ok {
		c = &certState{}
		s.certs[agent] = c
	}
	return c
}

// RotateCertificate implements agentv1.CertificateServiceServer.
func (c *certServer) RotateCertificate(ctx context.Context, _ *agentv1.RotateCertificateRequest) (*agentv1.RotateCertificateResponse, error) {
	s := c.sim
	if err := s.enter(ctx, c.name, "RotateCertificate", false); err != nil {
		return nil, err
	}
	s.mu.Lock()
	b, _, ok := s.agentLocked(c.name)
	if !ok {
		s.mu.Unlock()
		return nil, status.Errorf(codes.Unavailable, "agent %s is gone", c.name)
	}
	if b.Certificate != nil && b.Certificate.FailRotate > 0 {
		b.Certificate.FailRotate--
		s.mu.Unlock()
		return nil, status.Error(codes.Internal, "storing the new private key failed")
	}
	tmpl := s.csrTemplateLocked(c.name, b.Certificate)
	s.mu.Unlock()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate key: %v", err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create CSR: %v", err)
	}
	s.mu.Lock()
	s.certLocked(c.name).pending = key
	s.mu.Unlock()
	return &agentv1.RotateCertificateResponse{CsrDer: csr}, nil
}

// csrTemplateLocked takes subject and SANs from the current certificate, or
// from the scenario before the first one.
func (s *Sim) csrTemplateLocked(agent string, spec *CertSpec) *x509.CertificateRequest {
	if cur := s.certLocked(agent).current; cur != nil && cur.Leaf != nil {
		return &x509.CertificateRequest{
			Subject:     cur.Leaf.Subject,
			DNSNames:    cur.Leaf.DNSNames,
			IPAddresses: cur.Leaf.IPAddresses,
		}
	}
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: agent}}
	if spec != nil {
		if spec.CommonName != "" {
			tmpl.Subject.CommonName = spec.CommonName
		}
		tmpl.DNSNames = slices.Clone(spec.DNSNames)
		for _, ip := range spec.IPAddresses {
			tmpl.IPAddresses = append(tmpl.IPAddresses, net.ParseIP(ip))
		}
	}
	return tmpl
}

// InstallCertificate implements agentv1.CertificateServiceServer.
func (c *certServer) InstallCertificate(ctx context.Context, req *agentv1.InstallCertificateRequest) (*agentv1.InstallCertificateResponse, error) {
	s := c.sim
	if err := s.enter(ctx, c.name, "InstallCertificate", false); err != nil {
		return nil, err
	}
	chainDER := req.GetCertificateChainDer()
	chain, err := parseServingChain(chainDER)
	if err != nil {
		return nil, err
	}
	leaf := chain[0]
	s.mu.Lock()
	ca := s.ca
	s.mu.Unlock()
	if err := verifyChain(chain, ca, s.clock.Now()); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	b, _, ok := s.agentLocked(c.name)
	if !ok {
		return nil, status.Errorf(codes.Unavailable, "agent %s is gone", c.name)
	}
	st := s.certLocked(c.name)
	if st.pending == nil {
		return nil, status.Error(codes.FailedPrecondition, "no pending key; call RotateCertificate first")
	}
	if !st.pending.PublicKey.Equal(leaf.PublicKey) {
		return nil, status.Error(codes.FailedPrecondition, "leaf public key is not the pending key")
	}
	if b.Certificate != nil && b.Certificate.FailInstall > 0 {
		b.Certificate.FailInstall--
		return nil, status.Error(codes.Internal, "storing the certificate failed; the current pair stays active")
	}
	st.current = &tls.Certificate{Certificate: slices.Clone(chainDER), PrivateKey: st.pending, Leaf: leaf}
	st.pending = nil
	st.installs++
	return &agentv1.InstallCertificateResponse{NotAfter: timestamppb.New(leaf.NotAfter)}, nil
}

// parseServingChain parses a DER chain whose leaf must be a serving
// certificate.
func parseServingChain(chainDER [][]byte) ([]*x509.Certificate, error) {
	if len(chainDER) == 0 {
		return nil, status.Error(codes.InvalidArgument, "certificate chain is empty")
	}
	chain := make([]*x509.Certificate, 0, len(chainDER))
	for _, der := range chainDER {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "parse certificate: %v", err)
		}
		chain = append(chain, cert)
	}
	usage := chain[0].ExtKeyUsage
	if !slices.Contains(usage, x509.ExtKeyUsageServerAuth) && !slices.Contains(usage, x509.ExtKeyUsageAny) {
		return nil, status.Error(codes.InvalidArgument, "leaf is not a serving certificate (no serverAuth usage)")
	}
	return chain, nil
}

// verifyChain verifies the chain against the CA pool; a nil pool skips
// verification.
func verifyChain(chain []*x509.Certificate, ca *x509.CertPool, now time.Time) error {
	if ca == nil {
		return nil
	}
	inter := x509.NewCertPool()
	for _, ic := range chain[1:] {
		inter.AddCert(ic)
	}
	if _, err := chain[0].Verify(x509.VerifyOptions{
		Roots: ca, Intermediates: inter, CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return status.Errorf(codes.InvalidArgument, "chain does not verify against the hvmc CA: %v", err)
	}
	return nil
}

// ServingCertificate returns the agent's current serving pair, for use in a
// tls.Config's GetCertificate so that connections after an install see the
// new certificate.
func (s *Sim) ServingCertificate(agent string) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, _, ok := s.agentLocked(agent); !ok {
		return nil, errNoAgent(agent)
	}
	if cur := s.certLocked(agent).current; cur != nil {
		return cur, nil
	}
	return nil, errors.New("fake: agent has no serving certificate")
}

// SetServingCertificate sets the agent's current serving pair (e.g. the
// enrollment certificate). Its leaf supplies subject and SANs of later CSRs.
func (s *Sim) SetServingCertificate(agent string, cert tls.Certificate) error {
	if cert.Leaf == nil {
		if len(cert.Certificate) == 0 {
			return errors.New("fake: certificate has no chain")
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return err
		}
		cert.Leaf = leaf
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, _, ok := s.agentLocked(agent); !ok {
		return errNoAgent(agent)
	}
	s.certLocked(agent).current = &cert
	return nil
}

// SetCertificateAuthority sets (or, with nil, clears) the CA pool
// InstallCertificate verifies chains against.
func (s *Sim) SetCertificateAuthority(pool *x509.CertPool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ca = pool
}

// Certificate returns the certificate state of an agent.
func (s *Sim) Certificate(agent string) (CertStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, _, ok := s.agentLocked(agent); !ok {
		return CertStatus{}, errNoAgent(agent)
	}
	st := s.certLocked(agent)
	out := CertStatus{PendingKey: st.pending != nil, Installs: st.installs}
	if st.current != nil {
		out.Current = st.current.Leaf
	}
	return out, nil
}
