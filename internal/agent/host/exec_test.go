package host

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
)

// helperEnv makes the test binary act as a fake worker process (TestMain)
// on the named fixture; helperHangEnv lists ops it never answers.
const (
	helperEnv     = "HVMC_TEST_FAKE_WORKER"
	helperHangEnv = "HVMC_TEST_FAKE_WORKER_HANG"
)

func TestMain(m *testing.M) {
	if name := os.Getenv(helperEnv); name != "" {
		os.Exit(runHelperWorker(name))
	}
	os.Exit(m.Run())
}

// runHelperWorker speaks the worker protocol on stdin and stdout, reading
// the script frame first like worker.ps1's bootstrap.
func runHelperWorker(name string) int {
	t := &testing.T{}
	h := newFakeHost(loadFixture(t, name))
	for _, op := range strings.Split(os.Getenv(helperHangEnv), ",") {
		if op != "" {
			h.hangOps[op] = true
		}
	}
	go func() {
		for op := range h.hung {
			_, _ = os.Stderr.WriteString("hung " + op + "\n")
		}
	}()
	if err := h.serve(os.Stdin, os.Stdout, true); err != nil && !errors.Is(err, io.EOF) {
		return 3
	}
	return 0
}

// helperStarter starts the test binary as a worker process.
func helperStarter(t *testing.T, fixture string, hang ...string) (Starter, *notifyBuffer) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &notifyBuffer{changed: make(chan struct{})}
	return func(context.Context) (Process, error) {
		cmd := exec.Command(exe, "-test.run=^$")
		cmd.Env = append(os.Environ(), helperEnv+"="+fixture, helperHangEnv+"="+strings.Join(hang, ","))
		return StartCommand(cmd, stderr)
	}, stderr
}

// notifyBuffer collects a worker's stderr and signals every write.
type notifyBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	changed chan struct{}
}

func (b *notifyBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, err := b.buf.Write(p)
	close(b.changed)
	b.changed = make(chan struct{})
	return n, err
}

func (b *notifyBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *notifyBuffer) wait(t *testing.T, text string) {
	t.Helper()
	waitFor(t, "worker stderr "+strconv.Quote(text), func() <-chan struct{} {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.changed
	}, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return strings.Contains(b.buf.String(), text)
	})
}

func TestWorkerProcess(t *testing.T) {
	start, stderr := helperStarter(t, "partial-shrink", opSetDynamic)
	clk := newTestClock()
	e := newTestEnv(t, nil, testPolicy(), func(o *envOptions) {
		o.start, o.script, o.clk = start, []byte("# script"), &clk
	})
	ctx := testCtx(t)
	v := e.vm("windows-node-1")
	if v.GetAssignedBytes() != 8*gib {
		t.Errorf("assigned %d", v.GetAssignedBytes())
	}
	_, err := e.client.SetStartup(ctx, startupReq("windows-node-1", 4*gib))
	wantCode(t, "partial shrink", err, codes.Aborted)

	// A hung call is killed at the timeout and the process restarted.
	done := make(chan error, 1)
	go func() {
		_, err := e.client.SetDynamicLimits(ctx, limitsReq("windows-dm-1", 12*gib))
		done <- err
	}()
	stderr.wait(t, "hung "+opSetDynamic)
	clk.waitTimer(t) // the hung call's timer
	sup := e.svc.srv.sup
	changed := sup.Changed()
	clk.Advance(DefaultCallTimeout)
	wantCode(t, "hung call", <-done, codes.DeadlineExceeded)
	<-changed
	e.waitWorker()
	// The new process starts from the fixture again.
	if v := e.vm("windows-node-1"); v.GetAssignedBytes() != 8*gib {
		t.Errorf("after the restart: assigned %d", v.GetAssignedBytes())
	}
}
