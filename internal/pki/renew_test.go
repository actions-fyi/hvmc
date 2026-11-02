package pki_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matfax/hvmc/internal/pki"
)

func TestRenewIfDue(t *testing.T) {
	clk := newFakeClock()
	ca := newTestCA(t, clk)
	r, err := pki.NewReloader(ca.IssueClient, pki.ReloaderConfig{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	first := r.Current()

	clk.Advance(19 * 24 * time.Hour) // before 2/3 of 30 days
	if renewed, err := r.RenewIfDue(); err != nil || renewed {
		t.Fatalf("RenewIfDue at 19 days = %v, %v; want false", renewed, err)
	}
	if r.Current() != first {
		t.Fatal("certificate changed before it was due")
	}

	clk.Advance(24 * time.Hour) // 20 days: past 2/3
	if renewed, err := r.RenewIfDue(); err != nil || !renewed {
		t.Fatalf("RenewIfDue at 20 days = %v, %v; want true", renewed, err)
	}
	second := r.Current()
	if second == first || second.Leaf.SerialNumber.Cmp(first.Leaf.SerialNumber) == 0 {
		t.Fatal("certificate was not replaced")
	}
	if want := clk.Now().Add(pki.LeafValidity); !second.Leaf.NotAfter.Equal(want) {
		t.Fatalf("renewed NotAfter = %v, want %v", second.Leaf.NotAfter, want)
	}
	if got, _ := r.GetClientCertificate(nil); got != second {
		t.Fatal("GetClientCertificate does not return the renewed certificate")
	}
	if got, _ := r.GetCertificate(nil); got != second {
		t.Fatal("GetCertificate does not return the renewed certificate")
	}
}

func TestRenewFailureKeepsCurrent(t *testing.T) {
	clk := newFakeClock()
	ca := newTestCA(t, clk)
	var fail atomic.Bool
	issue := func() (*pki.KeyPair, error) {
		if fail.Load() {
			return nil, errors.New("issuer down")
		}
		return ca.IssueClient()
	}
	r, err := pki.NewReloader(issue, pki.ReloaderConfig{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	first := r.Current()
	fail.Store(true)
	clk.Advance(25 * 24 * time.Hour)
	if _, err := r.RenewIfDue(); err == nil {
		t.Fatal("RenewIfDue succeeded with a failing issuer")
	}
	if r.Current() != first {
		t.Fatal("failed renewal replaced the certificate")
	}
	fail.Store(false)
	if renewed, err := r.RenewIfDue(); err != nil || !renewed {
		t.Fatalf("retry = %v, %v; want renewed", renewed, err)
	}
}

func TestNewReloaderFails(t *testing.T) {
	if _, err := pki.NewReloader(nil, pki.ReloaderConfig{}); err == nil {
		t.Fatal("nil issue function accepted")
	}
	failing := func() (*pki.KeyPair, error) { return nil, errors.New("boom") }
	if _, err := pki.NewReloader(failing, pki.ReloaderConfig{}); err == nil {
		t.Fatal("failing first issuance accepted")
	}
}

// liveServer is an mTLS HTTPS server on one listener for the whole test. Its
// handler answers with the serial number of the client certificate.
type liveServer struct {
	addr string
	srv  *http.Server
}

func startLiveServer(t *testing.T, cfg *tls.Config) *liveServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		// Handshakes aborted when the load generator stops are expected;
		// failures that matter are counted on the client side.
		ErrorLog:  log.New(io.Discard, "", 0),
		TLSConfig: cfg,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprint(w, r.TLS.PeerCertificates[0].SerialNumber.String())
		}),
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	return &liveServer{addr: ln.Addr().String(), srv: srv}
}

// observation is what one fresh connection saw.
type observation struct{ server, client string }

// dial makes one request over a new connection.
func dial(ctx context.Context, cfg *tls.Config, addr string) (observation, error) {
	tr := &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+"/", http.NoBody)
	if err != nil {
		return observation{}, err
	}
	resp, err := (&http.Client{Transport: tr, Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return observation{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return observation{}, err
	}
	return observation{server: resp.TLS.PeerCertificates[0].SerialNumber.String(), client: string(body)}, nil
}

// loadGenerator opens new connections from several goroutines until
// stopped, recording every failure and the latest observation.
type loadGenerator struct {
	ok, failed atomic.Int64
	mu         sync.Mutex
	last       observation
	firstErr   error
	wg         sync.WaitGroup
}

func (g *loadGenerator) run(ctx context.Context, cfg *tls.Config, addr string, workers int) {
	for range workers {
		g.wg.Go(func() {
			for ctx.Err() == nil {
				g.record(dial(ctx, cfg, addr))
			}
		})
	}
}

func (g *loadGenerator) record(o observation, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return // shutdown of the generator itself
		}
		g.failed.Add(1)
		if g.firstErr == nil {
			g.firstErr = err
		}
		return
	}
	g.ok.Add(1)
	g.last = o
}

func (g *loadGenerator) latest() observation {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.last
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestReloaderSwapsLiveTLS proves in-place renewal: one listener and one
// client config stay up while the fake clock passes the renewal point; the
// server and client certificates are swapped and no new connection fails.
func TestReloaderSwapsLiveTLS(t *testing.T) {
	clk := newFakeClock()
	ca := newTestCA(t, clk)
	cfg := pki.ReloaderConfig{Clock: clk, CheckInterval: 2 * time.Millisecond}
	serving, err := pki.NewReloader(func() (*pki.KeyPair, error) {
		return ca.IssueServing(pki.ServiceIdentity(pki.WebhookServiceName, "ns"))
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	clientCert, err := pki.NewReloader(ca.IssueClient, cfg)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = serving.Start(ctx) }()
	go func() { _ = clientCert.Start(ctx) }()

	serverCfg := serving.ServerTLSConfig()
	serverCfg.ClientAuth = tls.RequireAndVerifyClientCert
	serverCfg.ClientCAs = ca.Pool()
	serverCfg.Time = clk.Now
	srv := startLiveServer(t, serverCfg)

	clientCfg := clientCert.ClientTLSConfig(ca.Pool())
	clientCfg.ServerName = "hvmc-webhook.ns.svc"
	clientCfg.Time = clk.Now

	before := observation{
		server: serving.Current().Leaf.SerialNumber.String(),
		client: clientCert.Current().Leaf.SerialNumber.String(),
	}
	var gen loadGenerator
	loadCtx, stopLoad := context.WithCancel(ctx)
	gen.run(loadCtx, clientCfg, srv.addr, 4)
	waitFor(t, "connections with the first certificates", func() bool { return gen.ok.Load() >= 20 })
	if got := gen.latest(); got != before {
		t.Fatalf("before renewal the connection saw %+v, want %+v", got, before)
	}

	clk.Advance(21 * 24 * time.Hour) // past 2/3 of the 30-day lifetime
	waitFor(t, "both certificates renewed on live connections", func() bool {
		o := gen.latest()
		return o.server != before.server && o.client != before.client
	})
	okAfter := gen.ok.Load()
	waitFor(t, "connections after renewal", func() bool { return gen.ok.Load() >= okAfter+20 })
	stopLoad()
	gen.wg.Wait()

	if n := gen.failed.Load(); n != 0 {
		t.Fatalf("%d of %d new connections failed during renewal; first: %v", n, n+gen.ok.Load(), gen.firstErr)
	}
	after := gen.latest()
	if after.server != serving.Current().Leaf.SerialNumber.String() || after.client != clientCert.Current().Leaf.SerialNumber.String() {
		t.Fatalf("connections saw %+v, not the current certificates", after)
	}
}
