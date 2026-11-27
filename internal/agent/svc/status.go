package svc

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/matfax/hvmc/internal/agent"
)

// StatusOptions configure Status.
type StatusOptions struct {
	Paths agent.Paths
	// Clock is the time certificate expiry is measured against.
	Clock agent.Clock
	// HTTP reads /readyz (nil: a client with a short timeout).
	HTTP *http.Client
	Out  io.Writer
}

// Status prints the service's state, the agent's identity, the serving
// certificate's expiry, a host's active and pending policy and the
// readiness checks (DESIGN §4.5). It returns ErrNotReady unless the service
// runs and the agent reports ready, so scripts can test the exit code.
func Status(ctx context.Context, p Platform, o StatusOptions) error {
	if o.Clock == nil {
		o.Clock = agent.SystemClock()
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 5 * time.Second}
	}
	out := &printer{w: o.Out}
	state, pid, err := p.ServiceState()
	switch {
	case err != nil:
		out.f("Service %s: %v\n", ServiceName, err)
	case pid != 0:
		out.f("Service %s: %s (pid %d)\n", ServiceName, state, pid)
	default:
		out.f("Service %s: %s\n", ServiceName, state)
	}
	cfg, err := agent.LoadConfig(o.Paths.Config)
	if err != nil {
		out.f("Configuration: %v\n", err)
		return notReady(out)
	}
	out.f("Agent: %s (role %s), config %s\n", cfg.Name, cfg.Role, o.Paths.Config)
	printCertificate(ctx, o, out)
	if cfg.Role == agent.RoleHost {
		printPolicy(ctx, o.Paths, out)
	}
	if state != Running {
		out.f("Readiness: not checked, the service is not running\n")
		return notReady(out)
	}
	url, err := healthURL(cfg)
	if err != nil {
		out.f("Readiness: %v\n", err)
		return notReady(out)
	}
	report, ready, err := readiness(ctx, o.HTTP, url)
	if err != nil {
		out.f("Readiness: %v\n", err)
		return notReady(out)
	}
	out.f("Readiness (%s):\n%s", url, report)
	if !ready {
		return notReady(out)
	}
	return out.err
}

func notReady(out *printer) error {
	if out.err != nil {
		return out.err
	}
	return ErrNotReady
}

func printCertificate(ctx context.Context, o StatusOptions, out *printer) {
	certs, err := agent.OpenCertStore(o.Paths, o.Clock, slog.New(slog.DiscardHandler))
	if err != nil {
		out.f("Certificate: %v\n", err)
		return
	}
	leaf := certs.Leaf()
	left := leaf.NotAfter.Sub(o.Clock.Now()).Truncate(time.Minute)
	out.f("Certificate: serial %x, valid until %s (%s left)\n", leaf.SerialNumber, leaf.NotAfter.UTC().Format(time.RFC3339), left)
	if err := certs.Check(ctx); err != nil {
		out.f("  warning: %v\n", err)
	}
}

func printPolicy(ctx context.Context, paths agent.Paths, out *printer) {
	store := agent.OpenPolicyStore(paths, slog.New(slog.DiscardHandler))
	if err := store.Check(ctx); err != nil {
		out.f("Policy: %v (every write is refused)\n", err)
	}
	out.f("Active policy:  %s (%d VMs)\n", store.ActiveDigest(), len(store.Active().VMs))
	if pending, ok := store.Pending(); ok {
		out.f("Pending policy: %s; review and activate it with `hvmc agent policy sync`\n", pending.Digest())
	} else {
		out.f("Pending policy: none\n")
	}
}
