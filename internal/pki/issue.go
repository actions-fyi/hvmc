package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"
)

// Leaf certificate lifetime and renewal point (DESIGN §4.6).
const (
	// LeafValidity is the lifetime of every leaf certificate.
	LeafValidity = 30 * 24 * time.Hour
	// CoordinatorCommonName is the subject CN of the coordinator's client
	// certificate; agents list it in allowedClients.
	CoordinatorCommonName = "hvmc-coordinator"
)

// ErrInvalidCSR is returned for a CSR that cannot be signed.
var ErrInvalidCSR = errors.New("invalid certificate signing request")

// Identity is the subject a leaf certificate is issued for.
type Identity struct {
	// CommonName is the subject CN.
	CommonName string
	// DNSNames are the DNS subject alternative names.
	DNSNames []string
	// IPAddresses are the IP subject alternative names.
	IPAddresses []net.IP
}

// usage selects the extended key usage of a leaf.
type usage int

const (
	usageClient usage = iota
	usageServer
)

func (u usage) extKeyUsage() []x509.ExtKeyUsage {
	if u == usageClient {
		return []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	return []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
}

// KeyPair is a leaf certificate with the private key generated for it.
type KeyPair struct {
	// Certificate is the parsed leaf; Certificate.Raw is its DER encoding.
	Certificate *x509.Certificate
	// Key is the leaf's private key.
	Key *ecdsa.PrivateKey
	// CA is the issuing CA certificate.
	CA *x509.Certificate
}

// TLSCertificate returns the pair as a tls.Certificate whose chain is the
// leaf followed by the CA certificate.
func (kp *KeyPair) TLSCertificate() *tls.Certificate {
	return &tls.Certificate{
		Certificate: [][]byte{kp.Certificate.Raw, kp.CA.Raw},
		PrivateKey:  kp.Key,
		Leaf:        kp.Certificate,
	}
}

// CertPEM returns the leaf followed by the CA certificate, PEM-encoded.
func (kp *KeyPair) CertPEM() []byte {
	return append(EncodeCertificatePEM(kp.Certificate), EncodeCertificatePEM(kp.CA)...)
}

// KeyPEM returns the private key, PEM-encoded as PKCS #8.
func (kp *KeyPair) KeyPEM() ([]byte, error) { return encodePrivateKeyPEM(kp.Key) }

// RenewAt returns the point after which a certificate is due for renewal:
// two thirds into its lifetime.
func RenewAt(cert *x509.Certificate) time.Time {
	lifetime := cert.NotAfter.Sub(cert.NotBefore)
	return cert.NotBefore.Add(lifetime * 2 / 3)
}

// IssueClient issues the coordinator's client certificate
// (CN hvmc-coordinator, client authentication only) with a fresh key.
func (ca *CA) IssueClient() (*KeyPair, error) {
	return ca.issueKeyPair(Identity{CommonName: CoordinatorCommonName}, usageClient)
}

// IssueServing issues a serving certificate (server authentication only)
// with a fresh key for the given identity. The identity must name at least
// one DNS name or IP address.
func (ca *CA) IssueServing(id Identity) (*KeyPair, error) {
	return ca.issueKeyPair(id, usageServer)
}

// SignCSR signs an agent's PKCS #10 CSR (DER) as a serving certificate. The
// CSR must carry a valid self-signature by an ECDSA P-256 key. Only its
// public key is used: subject and SANs come from id, never from the CSR, so
// a renewal after an address change carries the names HostConfig declares.
func (ca *CA) SignCSR(csrDER []byte, id Identity) (*x509.Certificate, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidCSR, err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("%w: signature: %w", ErrInvalidCSR, err)
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("%w: public key must be ECDSA P-256", ErrInvalidCSR)
	}
	return ca.sign(pub, id, usageServer)
}

func (ca *CA) issueKeyPair(id Identity, u usage) (*KeyPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key: %w", err)
	}
	cert, err := ca.sign(&key.PublicKey, id, u)
	if err != nil {
		return nil, err
	}
	return &KeyPair{Certificate: cert, Key: key, CA: ca.cert}, nil
}

func (ca *CA) sign(pub crypto.PublicKey, id Identity, u usage) (*x509.Certificate, error) {
	if u == usageServer && len(id.DNSNames) == 0 && len(id.IPAddresses) == 0 {
		return nil, errors.New("serving certificate needs at least one DNS name or IP address")
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := ca.clock.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: id.CommonName},
		DNSNames:              slices.Clone(id.DNSNames),
		IPAddresses:           slices.Clone(id.IPAddresses),
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(LeafValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           u.extKeyUsage(),
		BasicConstraintsValid: true,
	}
	if tmpl.NotAfter.After(ca.cert.NotAfter) {
		tmpl.NotAfter = ca.cert.NotAfter
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, pub, ca.key)
	if err != nil {
		return nil, fmt.Errorf("sign certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse signed certificate: %w", err)
	}
	return cert, nil
}

// CreateCSR returns a DER-encoded CSR for key carrying the given identity.
// The coordinator never needs it; tests and agents use it.
func CreateCSR(key crypto.Signer, id Identity) ([]byte, error) {
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:     pkix.Name{CommonName: id.CommonName},
		DNSNames:    id.DNSNames,
		IPAddresses: id.IPAddresses,
	}, key)
	if err != nil {
		return nil, fmt.Errorf("create CSR: %w", err)
	}
	return der, nil
}
