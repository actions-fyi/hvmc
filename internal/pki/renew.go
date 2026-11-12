package pki

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"
)

// DefaultCheckInterval is how often a Reloader checks whether its
// certificate is due for renewal. Checking (instead of sleeping until the
// renewal point) keeps renewal correct across clock jumps and suspends.
const DefaultCheckInterval = time.Minute

// IssueFunc issues a fresh key pair, e.g. CA.IssueClient or a closure over
// CA.IssueServing.
type IssueFunc func() (*KeyPair, error)

// ReloaderConfig configures a Reloader.
type ReloaderConfig struct {
	// Name identifies the certificate in logs, e.g. "webhook".
	Name string
	// Clock decides when renewal is due; nil means SystemClock.
	Clock Clock
	// CheckInterval is the period of the renewal check; zero means
	// DefaultCheckInterval.
	CheckInterval time.Duration
}

// Reloader holds one of the coordinator's certificates and renews it in
// place (DESIGN §4.6): TLS configs read it per handshake through
// GetCertificate or GetClientCertificate, and a renewal swaps it atomically,
// so listeners and clients are never restarted. Handshakes in progress keep
// the certificate they started with; new ones get the renewed one.
type Reloader struct {
	issue    IssueFunc
	name     string
	clock    Clock
	interval time.Duration

	current atomic.Pointer[tls.Certificate]
	renewMu sync.Mutex
}

// NewReloader issues the first certificate and returns a Reloader serving
// it. Start runs the renewal loop.
func NewReloader(issue IssueFunc, cfg ReloaderConfig) (*Reloader, error) {
	if issue == nil {
		return nil, errors.New("reloader: nil issue function")
	}
	r := &Reloader{issue: issue, name: cfg.Name, clock: cfg.Clock, interval: cfg.CheckInterval}
	if r.clock == nil {
		r.clock = SystemClock
	}
	if r.interval <= 0 {
		r.interval = DefaultCheckInterval
	}
	if err := r.Renew(); err != nil {
		return nil, err
	}
	return r, nil
}

// Current returns the certificate handed to new handshakes.
func (r *Reloader) Current() *tls.Certificate { return r.current.Load() }

// GetCertificate implements tls.Config.GetCertificate for servers.
func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.current.Load(), nil
}

// GetClientCertificate implements tls.Config.GetClientCertificate for clients.
func (r *Reloader) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return r.current.Load(), nil
}

// ServerTLSConfig returns a TLS 1.3 server config serving the current
// certificate.
func (r *Reloader) ServerTLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: r.GetCertificate}
}

// ClientTLSConfig returns a TLS 1.3 client config presenting the current
// certificate and trusting roots. Callers set ServerName per peer.
func (r *Reloader) ClientTLSConfig(roots *x509.CertPool) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, GetClientCertificate: r.GetClientCertificate}
}

// Renew issues a new certificate and swaps it in. On failure the current
// certificate stays in use.
func (r *Reloader) Renew() error {
	r.renewMu.Lock()
	defer r.renewMu.Unlock()
	kp, err := r.issue()
	if err != nil {
		return err
	}
	r.current.Store(kp.TLSCertificate())
	return nil
}

// RenewIfDue renews the certificate once two thirds of its lifetime have
// passed and reports whether it did.
func (r *Reloader) RenewIfDue() (bool, error) {
	if r.clock.Now().Before(RenewAt(r.current.Load().Leaf)) {
		return false, nil
	}
	if err := r.Renew(); err != nil {
		return false, err
	}
	return true, nil
}

// Start runs the renewal check every CheckInterval until ctx is done. A
// failed renewal is logged and retried at the next check. It implements
// controller-runtime's manager.Runnable.
func (r *Reloader) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithValues("certificate", r.name)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		renewed, err := r.RenewIfDue()
		if err != nil {
			logger.Error(err, "certificate renewal failed; keeping the current certificate")
			continue
		}
		if renewed {
			logger.Info("certificate renewed", "notAfter", r.current.Load().Leaf.NotAfter)
		}
	}
}

// NeedLeaderElection reports false: every replica serves its own
// certificates (the webhook runs on all replicas, DESIGN §6).
func (r *Reloader) NeedLeaderElection() bool { return false }
