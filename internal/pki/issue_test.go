package pki_test

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/matfax/hvmc/internal/pki"
)

// checkLeaf asserts the rules every leaf follows: ECDSA P-256 not a CA,
// digital signature only, one extended key usage, 30 days from issuance,
// chained to the CA.
func checkLeaf(t *testing.T, ca *pki.CA, c *x509.Certificate, eku x509.ExtKeyUsage) {
	t.Helper()
	if pub, ok := c.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve != elliptic.P256() {
		t.Errorf("leaf key: got %T, want ECDSA P-256", c.PublicKey)
	}
	if !c.BasicConstraintsValid || c.IsCA {
		t.Errorf("leaf basic constraints: valid=%v IsCA=%v", c.BasicConstraintsValid, c.IsCA)
	}
	if c.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Errorf("key usage = %v, want DigitalSignature only", c.KeyUsage)
	}
	if diff := cmp.Diff([]x509.ExtKeyUsage{eku}, c.ExtKeyUsage); diff != "" {
		t.Errorf("extended key usage (-want +got):\n%s", diff)
	}
	if want := t0.Add(pki.LeafValidity); !c.NotAfter.Equal(want) {
		t.Errorf("NotAfter = %v, want %v (30 days)", c.NotAfter, want)
	}
	if c.NotBefore.After(t0) || t0.Sub(c.NotBefore) > 10*time.Minute {
		t.Errorf("NotBefore = %v, want shortly before %v", c.NotBefore, t0)
	}
	if !equalBytes(c.AuthorityKeyId, ca.Certificate().SubjectKeyId) {
		t.Error("authority key identifier does not match the CA")
	}
	verifyChain(t, ca, c, t0, eku)
}

func equalBytes(a, b []byte) bool { return string(a) == string(b) }

func TestIssueClient(t *testing.T) {
	ca := newTestCA(t, newFakeClock())
	kp, err := ca.IssueClient()
	if err != nil {
		t.Fatal(err)
	}
	c := kp.Certificate
	checkLeaf(t, ca, c, x509.ExtKeyUsageClientAuth)
	if c.Subject.CommonName != "hvmc-coordinator" {
		t.Errorf("CN = %q, want hvmc-coordinator", c.Subject.CommonName)
	}
	if len(c.DNSNames)+len(c.IPAddresses) != 0 {
		t.Errorf("client certificate carries SANs: %v %v", c.DNSNames, c.IPAddresses)
	}
	if !kp.Key.PublicKey.Equal(c.PublicKey) {
		t.Error("key pair: private key does not match certificate")
	}
	// A client certificate must not be usable as a serving certificate.
	if _, err := c.Verify(x509.VerifyOptions{Roots: ca.Pool(), CurrentTime: t0}); err == nil {
		t.Error("client certificate verifies for server authentication")
	}
}

func TestIssueServingForServices(t *testing.T) {
	ca := newTestCA(t, newFakeClock())
	tests := []struct {
		service string
		want    []string
	}{
		{pki.ExtenderServiceName, []string{"hvmc-extender.hvmc-system.svc", "hvmc-extender.hvmc-system.svc.cluster.local"}},
		{pki.WebhookServiceName, []string{"hvmc-webhook.hvmc-system.svc", "hvmc-webhook.hvmc-system.svc.cluster.local"}},
	}
	for _, tt := range tests {
		t.Run(tt.service, func(t *testing.T) {
			kp, err := ca.IssueServing(pki.ServiceIdentity(tt.service, "hvmc-system"))
			if err != nil {
				t.Fatal(err)
			}
			c := kp.Certificate
			checkLeaf(t, ca, c, x509.ExtKeyUsageServerAuth)
			requireDNSNamesOnly(t, c, tt.want)
		})
	}
}

// requireDNSNamesOnly asserts that c carries exactly the DNS SANs want, no
// IP SANs, and verifies for each name.
func requireDNSNamesOnly(t *testing.T, c *x509.Certificate, want []string) {
	t.Helper()
	if diff := cmp.Diff(want, c.DNSNames); diff != "" {
		t.Errorf("DNS SANs (-want +got):\n%s", diff)
	}
	if len(c.IPAddresses) != 0 {
		t.Errorf("unexpected IP SANs %v", c.IPAddresses)
	}
	for _, name := range want {
		if err := c.VerifyHostname(name); err != nil {
			t.Errorf("VerifyHostname(%s): %v", name, err)
		}
	}
}

func TestIssueServingRequiresSAN(t *testing.T) {
	ca := newTestCA(t, newFakeClock())
	if _, err := ca.IssueServing(pki.Identity{CommonName: "x"}); err == nil {
		t.Fatal("serving certificate without SANs was issued")
	}
}

func TestLeafNeverOutlivesCA(t *testing.T) {
	clk := newFakeClock()
	ca := newTestCA(t, clk)
	clk.Advance(10*365*24*time.Hour - 24*time.Hour) // the CA has a few days left
	kp, err := ca.IssueClient()
	if err != nil {
		t.Fatal(err)
	}
	if kp.Certificate.NotAfter.After(ca.Certificate().NotAfter) {
		t.Errorf("leaf NotAfter %v is after CA NotAfter %v", kp.Certificate.NotAfter, ca.Certificate().NotAfter)
	}
}

func TestRenewAt(t *testing.T) {
	ca := newTestCA(t, newFakeClock())
	kp, err := ca.IssueClient()
	if err != nil {
		t.Fatal(err)
	}
	c := kp.Certificate
	got := pki.RenewAt(c)
	lifetime := c.NotAfter.Sub(c.NotBefore)
	if want := c.NotBefore.Add(lifetime * 2 / 3); !got.Equal(want) {
		t.Fatalf("RenewAt = %v, want %v", got, want)
	}
	// About 20 days after issuance for a 30-day leaf.
	if d := got.Sub(t0); d < 19*24*time.Hour || d > 21*24*time.Hour {
		t.Fatalf("RenewAt is %v after issuance, want about 20 days", d)
	}
}

func TestKeyPairTLSCertificate(t *testing.T) {
	ca := newTestCA(t, newFakeClock())
	kp, err := ca.IssueClient()
	if err != nil {
		t.Fatal(err)
	}
	tc := kp.TLSCertificate()
	if len(tc.Certificate) != 2 || !equalBytes(tc.Certificate[0], kp.Certificate.Raw) || !equalBytes(tc.Certificate[1], ca.Certificate().Raw) {
		t.Fatal("TLS chain is not leaf followed by CA")
	}
	if tc.Leaf != kp.Certificate || tc.PrivateKey != kp.Key {
		t.Fatal("TLS certificate does not carry leaf and key")
	}
	cert, err := pki.ParseCertificatePEM(kp.CertPEM())
	if err != nil || !cert.Equal(kp.Certificate) {
		t.Fatalf("CertPEM does not start with the leaf: %v", err)
	}
	if _, err := kp.KeyPEM(); err != nil {
		t.Fatal(err)
	}
}

func TestSignCSRUsesIdentityNotRequestedSANs(t *testing.T) {
	ca := newTestCA(t, newFakeClock())
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	// The agent asks for its old address and an extra name it must not get.
	csr, err := pki.CreateCSR(key, pki.Identity{
		CommonName:  "evil.example.com",
		DNSNames:    []string{"node-1", "evil.example.com"},
		IPAddresses: []net.IP{net.ParseIP("192.0.2.10")},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := pki.Identity{
		CommonName:  "node-1",
		DNSNames:    []string{"node-1"},
		IPAddresses: []net.IP{net.ParseIP("192.0.2.20")},
	}
	c, err := ca.SignCSR(csr, id)
	if err != nil {
		t.Fatalf("SignCSR: %v", err)
	}
	checkLeaf(t, ca, c, x509.ExtKeyUsageServerAuth)
	if !key.PublicKey.Equal(c.PublicKey) {
		t.Error("certificate is not for the CSR's key")
	}
	if c.Subject.CommonName != "node-1" {
		t.Errorf("CN = %q, want node-1", c.Subject.CommonName)
	}
	if diff := cmp.Diff([]string{"node-1"}, c.DNSNames); diff != "" {
		t.Errorf("DNS SANs (-want +got):\n%s", diff)
	}
	if len(c.IPAddresses) != 1 || !c.IPAddresses[0].Equal(net.ParseIP("192.0.2.20")) {
		t.Errorf("IP SANs = %v, want [192.0.2.20]", c.IPAddresses)
	}
	if c.VerifyHostname("evil.example.com") == nil || c.VerifyHostname("192.0.2.10") == nil {
		t.Error("certificate carries a SAN only the CSR requested")
	}
}

func TestSignCSRRejects(t *testing.T) {
	ca := newTestCA(t, newFakeClock())
	id := pki.Identity{CommonName: "a", DNSNames: []string{"a.example.com"}}
	p256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	valid := mustCSR(t, p256, id)
	tampered := append([]byte(nil), valid...)
	tampered[len(tampered)-1] ^= 0xff // last byte of the signature

	tests := map[string][]byte{
		"empty":       nil,
		"garbage":     []byte("not a csr"),
		"tampered":    tampered,
		"P-384 key":   mustCSR(t, p384, id),
		"RSA key":     mustCSR(t, rsaKey, id),
		"certificate": mustIssueDER(t, ca),
	}
	for name, der := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ca.SignCSR(der, id)
			if !errors.Is(err, pki.ErrInvalidCSR) {
				t.Fatalf("SignCSR error = %v, want ErrInvalidCSR", err)
			}
		})
	}
	// A valid CSR still needs a non-empty identity.
	if _, err := ca.SignCSR(valid, pki.Identity{CommonName: "a"}); err == nil {
		t.Fatal("signed a serving certificate without SANs")
	}
}

func mustCSR(t *testing.T, key crypto.Signer, id pki.Identity) []byte {
	t.Helper()
	der, err := pki.CreateCSR(key, id)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func mustIssueDER(t *testing.T, ca *pki.CA) []byte {
	t.Helper()
	kp, err := ca.IssueClient()
	if err != nil {
		t.Fatal(err)
	}
	return kp.Certificate.Raw
}
