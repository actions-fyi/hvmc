package svc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/matfax/hvmc/internal/agent"
)

// PollInterval is how often install, uninstall and status re-check the
// service manager and the agent's readiness while they wait.
const PollInterval = time.Second

// waitUntil calls cond now and then every PollInterval on clk until it
// reports done or an error, ctx ends or timeout has passed.
func waitUntil(ctx context.Context, clk agent.Clock, timeout time.Duration, what string, cond func() (bool, error)) error {
	deadline := clk.Now().Add(timeout)
	for {
		done, err := cond()
		if err != nil || done {
			return err
		}
		if !clk.Now().Before(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s", timeout, what)
		}
		t := clk.NewTimer(PollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C():
		}
	}
}

// maxReadyzBody bounds what is read from /readyz.
const maxReadyzBody = 64 << 10

// readiness fetches the agent's /readyz: its body (one line per check) and
// whether it reports ready (200) or not (503).
func readiness(ctx context.Context, client *http.Client, url string) (report string, ready bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReadyzBody))
	if err != nil {
		return "", false, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return string(body), true, nil
	case http.StatusServiceUnavailable:
		return string(body), false, nil
	default:
		return "", false, fmt.Errorf("GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}
}
