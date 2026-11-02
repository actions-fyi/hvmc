package pki_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/matfax/hvmc/internal/pki"
)

func TestGenerateCA(t *testing.T) {
	clk := newFakeClock()
	ca := newTestCA(t, clk)
	c := ca.Certificate()

	pub, ok := c.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		t.Fatalf("CA key: got %T, want ECDSA P-256", c.PublicKey)
	}
	if !c.BasicConstraintsValid || !c.IsCA || c.MaxPathLen != 0 || !c.MaxPathLenZero {
		t.Errorf("basic constraints: IsCA=%v valid=%v maxPathLen=%d zero=%v", c.IsCA, c.BasicConstraintsValid, c.MaxPathLen, c.MaxPathLenZero)
	}
	if want := x509.KeyUsageCertSign | x509.KeyUsageCRLSign; c.KeyUsage != want {
		t.Errorf("key usage = %v, want %v", c.KeyUsage, want)
	}
	if len(c.ExtKeyUsage) != 0 {
		t.Errorf("CA must not restrict extended key usage, got %v", c.ExtKeyUsage)
	}
	if want := t0.AddDate(10, 0, 0); !c.NotAfter.Equal(want) {
		t.Errorf("NotAfter = %v, want %v (10 years)", c.NotAfter, want)
	}
	if c.NotBefore.After(t0) {
		t.Errorf("NotBefore %v is after issuance time %v", c.NotBefore, t0)
	}
	if c.Subject.CommonName != pki.CACommonName {
		t.Errorf("CN = %q", c.Subject.CommonName)
	}
	if err := c.CheckSignatureFrom(c); err != nil {
		t.Errorf("not self-signed: %v", err)
	}
	if len(c.SubjectKeyId) == 0 {
		t.Error("missing subject key identifier")
	}
}

func TestParseCARoundTrip(t *testing.T) {
	clk := newFakeClock()
	ca := newTestCA(t, clk)
	keyPEM, err := ca.KeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := pki.ParseCA(ca.CertPEM(), keyPEM, pki.WithClock(clk))
	if err != nil {
		t.Fatalf("ParseCA: %v", err)
	}
	if loaded.Fingerprint() != ca.Fingerprint() {
		t.Error("fingerprint changed after round trip")
	}
	// The loaded CA signs certificates that verify against the original.
	kp, err := loaded.IssueClient()
	if err != nil {
		t.Fatal(err)
	}
	verifyChain(t, ca, kp.Certificate, t0, x509.ExtKeyUsageClientAuth)
}

func TestParseCAKeyFormats(t *testing.T) {
	clk := newFakeClock()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	certPEM := selfSigned(t, key, true, x509.KeyUsageCertSign)
	sec1, _ := x509.MarshalECPrivateKey(key)
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(key)
	for name, block := range map[string]*pem.Block{
		"sec1":  {Type: "EC PRIVATE KEY", Bytes: sec1},
		"pkcs8": {Type: "PRIVATE KEY", Bytes: pkcs8},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := pki.ParseCA(certPEM, pem.EncodeToMemory(block), pki.WithClock(clk)); err != nil {
				t.Fatalf("ParseCA: %v", err)
			}
		})
	}
}

func TestParseCARejects(t *testing.T) {
	clk := newFakeClock()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	keyPEM := pkcs8PEM(t, key)
	expired := newFakeClock()
	expired.Advance(11 * 365 * 24 * time.Hour)
	tests := []struct {
		name    string
		cert    []byte
		key     []byte
		clock   pki.Clock
		wantErr string
	}{
		{"not a CA", selfSigned(t, key, false, x509.KeyUsageDigitalSignature), keyPEM, clk, "basic constraints"},
		{"no cert sign usage", selfSigned(t, key, true, x509.KeyUsageDigitalSignature), keyPEM, clk, "key usage"},
		{"key mismatch", selfSigned(t, key, true, x509.KeyUsageCertSign), pkcs8PEM(t, other), clk, "does not match"},
		{"expired", selfSigned(t, key, true, x509.KeyUsageCertSign), keyPEM, expired, "not valid at"},
		{"no certificate", nil, keyPEM, clk, "no PEM certificate"},
		{"no key", selfSigned(t, key, true, x509.KeyUsageCertSign), nil, clk, "no PEM private key"},
		{"garbage key", selfSigned(t, key, true, x509.KeyUsageCertSign), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("x")}), clk, "parse private key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := pki.ParseCA(tt.cert, tt.key, pki.WithClock(tt.clock))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ParseCA error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestFingerprint(t *testing.T) {
	ca := newTestCA(t, newFakeClock())
	sum := sha256.Sum256(ca.Certificate().Raw)
	if got, want := ca.Fingerprint(), hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("Fingerprint = %s, want %s", got, want)
	}
	if len(ca.Fingerprint()) != 64 || strings.ToLower(ca.Fingerprint()) != ca.Fingerprint() {
		t.Fatalf("fingerprint %q is not 64 lowercase hex digits", ca.Fingerprint())
	}
}

func selfSigned(t *testing.T, key *ecdsa.PrivateKey, isCA bool, ku x509.KeyUsage) []byte {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "operator-ca"},
		NotBefore:             t0.Add(-time.Hour),
		NotAfter:              t0.AddDate(5, 0, 0),
		KeyUsage:              ku,
		BasicConstraintsValid: true,
		IsCA:                  isCA,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func pkcs8PEM(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}
