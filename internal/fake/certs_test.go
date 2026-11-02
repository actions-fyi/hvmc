package fake_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"

	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
	"github.com/matfax/hvmc/internal/fake"
)

// testCA is a throwaway certificate authority.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             t0.Add(-time.Hour),
		NotAfter:              t0.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

// sign issues a certificate for the CSR's key and names.
func (ca *testCA) sign(t *testing.T, csrDER []byte, serial int64, usage x509.ExtKeyUsage) []byte {
	t.Helper()
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      csr.Subject,
		DNSNames:     csr.DNSNames,
		IPAddresses:  csr.IPAddresses,
		NotBefore:    t0.Add(-time.Minute),
		NotAfter:     t0.Add(time.Duration(serial) * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func TestCertificateRotation(t *testing.T) {
	ca := newTestCA(t)
	e := newEnv(t, nil, fake.WithCertificateAuthority(ca.pool))
	ctx := testCtx(t)
	certs := e.certs("hyperv-1")

	rot, err := certs.RotateCertificate(ctx, &agentv1.RotateCertificateRequest{})
	e.must(err)
	csr, err := x509.ParseCertificateRequest(rot.GetCsrDer())
	e.must(err)
	e.must(csr.CheckSignature())
	if _, ok := csr.PublicKey.(*ecdsa.PublicKey); !ok || csr.Subject.CommonName != "hyperv-1" ||
		!slices.Equal(csr.DNSNames, []string{"hyperv-1.example.invalid"}) || len(csr.IPAddresses) != 1 || !csr.IPAddresses[0].Equal(net.ParseIP("192.0.2.10")) {
		t.Errorf("CSR = CN %q, DNS %v, IP %v, key %T", csr.Subject.CommonName, csr.DNSNames, csr.IPAddresses, csr.PublicKey)
	}
	st, err := e.sim.Certificate("hyperv-1")
	e.must(err)
	if !st.PendingKey || st.Current != nil {
		t.Errorf("status after rotate = %+v", st)
	}

	leaf := ca.sign(t, rot.GetCsrDer(), 2, x509.ExtKeyUsageServerAuth)
	inst, err := certs.InstallCertificate(ctx, &agentv1.InstallCertificateRequest{CertificateChainDer: [][]byte{leaf}})
	e.must(err)
	if got := inst.GetNotAfter().AsTime(); !got.Equal(t0.Add(2 * time.Hour)) {
		t.Errorf("not_after = %v", got)
	}
	st, err = e.sim.Certificate("hyperv-1")
	e.must(err)
	if st.PendingKey || st.Installs != 1 || st.Current == nil || st.Current.SerialNumber.Int64() != 2 {
		t.Errorf("status after install = %+v", st)
	}
	pair, err := e.sim.ServingCertificate("hyperv-1")
	e.must(err)
	if pair.Leaf.SerialNumber.Int64() != 2 {
		t.Errorf("serving pair is serial %v", pair.Leaf.SerialNumber)
	}

	// A guest's CSR takes its CN from the scenario.
	rot, err = e.certs("vm-a").RotateCertificate(ctx, &agentv1.RotateCertificateRequest{})
	e.must(err)
	csr, err = x509.ParseCertificateRequest(rot.GetCsrDer())
	e.must(err)
	if csr.Subject.CommonName != "vm-a.example.invalid" {
		t.Errorf("guest CSR CN = %q", csr.Subject.CommonName)
	}
}

func TestInstallCertificateErrors(t *testing.T) {
	ca := newTestCA(t)
	other := newTestCA(t)
	e := newEnv(t, nil, fake.WithCertificateAuthority(ca.pool))
	ctx := testCtx(t)
	certs := e.certs("hyperv-1")

	install := func(chain ...[]byte) error {
		_, err := certs.InstallCertificate(ctx, &agentv1.InstallCertificateRequest{CertificateChainDer: chain})
		return err
	}
	rot1, err := certs.RotateCertificate(ctx, &agentv1.RotateCertificateRequest{})
	e.must(err)
	// No pending key matches a certificate issued before the next rotation.
	wantCode(t, install(), codes.InvalidArgument)
	wantCode(t, install([]byte("not DER")), codes.InvalidArgument)
	wantCode(t, install(ca.sign(t, rot1.GetCsrDer(), 3, x509.ExtKeyUsageClientAuth)), codes.InvalidArgument)
	wantCode(t, install(other.sign(t, rot1.GetCsrDer(), 3, x509.ExtKeyUsageServerAuth)), codes.InvalidArgument)

	rot2, err := certs.RotateCertificate(ctx, &agentv1.RotateCertificateRequest{})
	e.must(err)
	// Only the newest pending key can be installed.
	wantCode(t, install(ca.sign(t, rot1.GetCsrDer(), 3, x509.ExtKeyUsageServerAuth)), codes.FailedPrecondition)
	e.must(install(ca.sign(t, rot2.GetCsrDer(), 4, x509.ExtKeyUsageServerAuth)))
	// No pending key after a successful install.
	wantCode(t, install(ca.sign(t, rot2.GetCsrDer(), 5, x509.ExtKeyUsageServerAuth)), codes.FailedPrecondition)

	// Without a CA every well-formed serving chain for the key is accepted.
	e.sim.SetCertificateAuthority(nil)
	rot3, err := certs.RotateCertificate(ctx, &agentv1.RotateCertificateRequest{})
	e.must(err)
	e.must(install(other.sign(t, rot3.GetCsrDer(), 6, x509.ExtKeyUsageServerAuth)))
}

func TestCertificateFailures(t *testing.T) {
	ca := newTestCA(t)
	e := newEnv(t, func(sc *fake.Scenario) {
		sc.Host.Certificate.FailRotate = 1
		sc.Host.Certificate.FailInstall = 1
	})
	ctx := testCtx(t)
	certs := e.certs("hyperv-1")

	_, err := certs.RotateCertificate(ctx, &agentv1.RotateCertificateRequest{})
	wantCode(t, err, codes.Internal)
	rot, err := certs.RotateCertificate(ctx, &agentv1.RotateCertificateRequest{})
	e.must(err)

	first := ca.sign(t, rot.GetCsrDer(), 7, x509.ExtKeyUsageServerAuth)
	_, err = certs.InstallCertificate(ctx, &agentv1.InstallCertificateRequest{CertificateChainDer: [][]byte{first}})
	wantCode(t, err, codes.Internal)
	st, err := e.sim.Certificate("hyperv-1")
	e.must(err)
	if st.Current != nil || !st.PendingKey || st.Installs != 0 {
		t.Errorf("after failed install = %+v, want the old state with the pending key kept", st)
	}
	if left := e.sim.Scenario().Host.Certificate; left.FailRotate != 0 || left.FailInstall != 0 {
		t.Errorf("failure counters not counted down: %+v", left)
	}
	// Retrying the same certificate succeeds.
	_, err = certs.InstallCertificate(ctx, &agentv1.InstallCertificateRequest{CertificateChainDer: [][]byte{first}})
	e.must(err)

	// Failures can be scripted at runtime too.
	e.must(e.sim.UpdateGuest("vm-a", func(g *fake.GuestSpec) { g.Certificate.FailInstall = 1 }))
	gc := e.certs("vm-a")
	rot, err = gc.RotateCertificate(ctx, &agentv1.RotateCertificateRequest{})
	e.must(err)
	_, err = gc.InstallCertificate(ctx, &agentv1.InstallCertificateRequest{CertificateChainDer: [][]byte{ca.sign(t, rot.GetCsrDer(), 8, x509.ExtKeyUsageServerAuth)}})
	wantCode(t, err, codes.Internal)
}

// TestRotationOverTLS serves the host agent with TLS whose certificate
// comes from the Sim: after an install, new connections see the new leaf.
func TestRotationOverTLS(t *testing.T) {
	ca := newTestCA(t)
	clk := fake.NewManualClock(t0)
	sim, err := fake.New(loadTestScenario(t), fake.WithClock(clk), fake.WithCertificateAuthority(ca.pool))
	if err != nil {
		t.Fatal(err)
	}
	// Enrollment certificate, issued outside the agent API.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "hyperv-1"}, DNSNames: []string{"hyperv-1.example.invalid"},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := sim.SetServingCertificate("hyperv-1", tls.Certificate{Certificate: [][]byte{ca.sign(t, csr, 10, x509.ExtKeyUsageServerAuth)}, PrivateKey: key}); err != nil {
		t.Fatal(err)
	}

	srv := sim.ServeHost(fake.Bufconn(), grpc.Creds(credentials.NewTLS(&tls.Config{
		MinVersion:     tls.VersionTLS13,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return sim.ServingCertificate("hyperv-1") },
	})))
	t.Cleanup(srv.Stop)
	serial := func() int64 {
		t.Helper()
		var seen int64
		conn, err := srv.Dial(grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS13,
			RootCAs:    ca.pool,
			ServerName: "hyperv-1.example.invalid",
			Time:       func() time.Time { return t0 },
			VerifyConnection: func(cs tls.ConnectionState) error {
				seen = cs.PeerCertificates[0].SerialNumber.Int64()
				return nil
			},
		})))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		if _, err := agentv1.NewHostAgentClient(conn).GetHost(testCtx(t), &agentv1.GetHostRequest{}); err != nil {
			t.Fatal(err)
		}
		return seen
	}
	if got := serial(); got != 10 {
		t.Fatalf("serving serial %d, want the enrollment certificate", got)
	}

	conn, err := srv.Dial(grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: ca.pool, ServerName: "hyperv-1.example.invalid",
		Time: func() time.Time { return t0 },
	})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	certs := agentv1.NewCertificateServiceClient(conn)
	rot, err := certs.RotateCertificate(testCtx(t), &agentv1.RotateCertificateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := certs.InstallCertificate(testCtx(t), &agentv1.InstallCertificateRequest{CertificateChainDer: [][]byte{ca.sign(t, rot.GetCsrDer(), 11, x509.ExtKeyUsageServerAuth)}}); err != nil {
		t.Fatal(err)
	}
	if got := serial(); got != 11 {
		t.Errorf("serving serial %d after rotation, want 11", got)
	}
}
