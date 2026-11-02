// Package pki is hvmc's internal certificate authority (DESIGN §4.6): it
// creates or loads the CA, issues the coordinator's client and serving
// certificates and the agents' serving certificates, renews the coordinator's
// certificates in place and publishes the CA certificate for the scheduler and
// the admission webhook. It uses only the standard library's crypto packages.
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// CA validity and subject (DESIGN §4.6).
const (
	// CAValidityYears is the lifetime of a CA created by hvmc.
	CAValidityYears = 10
	// CACommonName is the subject common name of a CA created by hvmc.
	CACommonName = "hvmc-ca"
)

// PEM block types.
const (
	pemCertificate  = "CERTIFICATE"
	pemECPrivateKey = "EC PRIVATE KEY"
	pemPKCS8Key     = "PRIVATE KEY"
	pemRSAKey       = "RSA PRIVATE KEY"
)

// clockSkew backdates NotBefore so that peers with a slightly late clock
// accept a fresh certificate.
const clockSkew = 5 * time.Minute

// Clock is the source of time for issuance and renewal.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// SystemClock reads the wall clock.
var SystemClock Clock = systemClock{}

// CA is a loaded certificate authority: its certificate and signing key.
// It is safe for concurrent use.
type CA struct {
	cert  *x509.Certificate
	key   crypto.Signer
	clock Clock
}

// Option configures a CA.
type Option func(*CA)

// WithClock sets the clock used for validity periods (default SystemClock).
func WithClock(c Clock) Option {
	return func(ca *CA) { ca.clock = c }
}

func newCA(cert *x509.Certificate, key crypto.Signer, opts []Option) *CA {
	ca := &CA{cert: cert, key: key, clock: SystemClock}
	for _, o := range opts {
		o(ca)
	}
	return ca
}

// GenerateCA creates a new self-signed CA: ECDSA P-256, valid for
// CAValidityYears, usable only to sign certificates and CRLs, with no
// intermediate CAs allowed below it.
func GenerateCA(opts ...Option) (*CA, error) {
	ca := newCA(nil, nil, opts)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := ca.clock.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: CACommonName},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.AddDate(CAValidityYears, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("self-sign CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	ca.cert, ca.key = cert, key
	return ca, nil
}

// ParseCA loads a CA from a PEM certificate and a PEM private key (SEC 1,
// PKCS #8 or PKCS #1). It fails unless the certificate is a currently valid
// CA certificate allowed to sign certificates and the key belongs to it.
func ParseCA(certPEM, keyPEM []byte, opts ...Option) (*CA, error) {
	cert, err := ParseCertificatePEM(certPEM)
	if err != nil {
		return nil, err
	}
	key, err := parsePrivateKeyPEM(keyPEM)
	if err != nil {
		return nil, err
	}
	ca := newCA(cert, key, opts)
	if err := ca.validate(); err != nil {
		return nil, err
	}
	return ca, nil
}

func (ca *CA) validate() error {
	c := ca.cert
	if !c.BasicConstraintsValid || !c.IsCA {
		return errors.New("CA certificate: basic constraints do not mark it as a CA")
	}
	if c.KeyUsage != 0 && c.KeyUsage&x509.KeyUsageCertSign == 0 {
		return errors.New("CA certificate: key usage does not allow certificate signing")
	}
	now := ca.clock.Now()
	if now.Before(c.NotBefore) || now.After(c.NotAfter) {
		return fmt.Errorf("CA certificate: not valid at %s (valid %s to %s)",
			now.UTC().Format(time.RFC3339), c.NotBefore.UTC().Format(time.RFC3339), c.NotAfter.UTC().Format(time.RFC3339))
	}
	pub, ok := ca.key.Public().(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(c.PublicKey) {
		return errors.New("CA private key does not match the CA certificate")
	}
	return nil
}

// Certificate returns the CA certificate.
func (ca *CA) Certificate() *x509.Certificate { return ca.cert }

// CertPEM returns the CA certificate, PEM-encoded.
func (ca *CA) CertPEM() []byte { return EncodeCertificatePEM(ca.cert) }

// KeyPEM returns the CA private key, PEM-encoded as PKCS #8.
func (ca *CA) KeyPEM() ([]byte, error) { return encodePrivateKeyPEM(ca.key) }

// Fingerprint returns the SHA-256 fingerprint of the CA certificate.
func (ca *CA) Fingerprint() string { return Fingerprint(ca.cert) }

// Pool returns a certificate pool holding only this CA.
func (ca *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

// Fingerprint returns the SHA-256 digest of a certificate's DER encoding as
// lowercase hex without separators, the form `--ca-sha256` takes.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// EncodeCertificatePEM PEM-encodes a certificate.
func EncodeCertificatePEM(cert *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: pemCertificate, Bytes: cert.Raw})
}

// ParseCertificatePEM parses the first PEM certificate block in data.
func ParseCertificatePEM(data []byte) (*x509.Certificate, error) {
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return nil, errors.New("no PEM certificate found")
		}
		if block.Type != pemCertificate {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse certificate: %w", err)
		}
		return cert, nil
	}
}

func parsePrivateKeyPEM(data []byte) (crypto.Signer, error) {
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return nil, errors.New("no PEM private key found")
		}
		if key, ok, err := parsePrivateKeyBlock(block); ok {
			return key, err
		}
	}
}

// parsePrivateKeyBlock parses a key block; ok is false for other block types.
func parsePrivateKeyBlock(block *pem.Block) (key crypto.Signer, ok bool, err error) {
	var parsed any
	switch block.Type {
	case pemECPrivateKey:
		parsed, err = x509.ParseECPrivateKey(block.Bytes)
	case pemPKCS8Key:
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case pemRSAKey:
		parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		return nil, false, nil
	}
	if err != nil {
		return nil, true, fmt.Errorf("parse private key: %w", err)
	}
	signer, isSigner := parsed.(crypto.Signer)
	if !isSigner {
		return nil, true, fmt.Errorf("private key of type %T cannot sign", parsed)
	}
	return signer, true, nil
}

func encodePrivateKeyPEM(key crypto.Signer) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemPKCS8Key, Bytes: der}), nil
}

// randomSerial returns a random positive 128-bit serial number.
func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate serial number: %w", err)
	}
	return n.Add(n, big.NewInt(1)), nil
}
