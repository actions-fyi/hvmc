package host

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
)

func TestFraming(t *testing.T) {
	var buf bytes.Buffer
	for _, msg := range []string{`{"id":1}`, "", strings.Repeat("x", 70000)} {
		if err := writeFrame(&buf, []byte(msg)); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{`{"id":1}`, "", strings.Repeat("x", 70000)} {
		got, err := readFrame(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("frame %.20q, want %.20q", got, want)
		}
	}
	if _, err := readFrame(&buf); !errors.Is(err, io.EOF) {
		t.Errorf("at the end: %v, want EOF", err)
	}
}

func TestFramingErrors(t *testing.T) {
	// A length prefix that is really text (stray output on stdout).
	if _, err := readFrame(strings.NewReader("WARNING: something\n")); !errors.Is(err, errFrameTooLarge) {
		t.Errorf("text as frame: %v, want errFrameTooLarge", err)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 10)
	if _, err := readFrame(bytes.NewReader(append(hdr[:], "abc"...))); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated frame: %v, want ErrUnexpectedEOF", err)
	}
	if _, err := readFrame(bytes.NewReader(hdr[:2])); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated header: %v, want ErrUnexpectedEOF", err)
	}
	if err := writeFrame(io.Discard, make([]byte, maxFrame+1)); !errors.Is(err, errFrameTooLarge) {
		t.Errorf("oversized write: %v", err)
	}
}

func TestBootstrap(t *testing.T) {
	// One plain line without double quotes or backslashes: passes Windows
	// command-line quoting unchanged and is readable in process listings.
	for _, bad := range []string{"\"", "\\", "\n", "\r"} {
		if strings.Contains(bootstrap, bad) {
			t.Errorf("bootstrap contains %q", bad)
		}
	}
	if len(bootstrap) > 1024 {
		t.Errorf("bootstrap is %d characters", len(bootstrap))
	}
	if !strings.Contains(bootstrap, "Read-HvmcExact") || pwshArgs[len(pwshArgs)-1] != "-Command" {
		t.Errorf("bootstrap %q, args %q", bootstrap, pwshArgs)
	}
	if !bytes.Contains(workerScript, []byte("Set-VMMemory")) {
		t.Error("worker.ps1 is not embedded")
	}
}

func TestPwshStarterWithoutPwsh(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("ProgramFiles", t.TempDir())
	start := PwshStarter(io.Discard)
	if _, err := start(context.Background()); !errors.Is(err, ErrNoPwsh) {
		t.Errorf("start without pwsh: %v, want ErrNoPwsh", err)
	}
	// The supervisor gives up and the worker check names the cause.
	sup := NewSupervisor(SupervisorOptions{Start: start, Clock: newTestClock(), Log: testLogger(t)})
	if err := sup.Run(context.Background()); !errors.Is(err, ErrWorkerFailing) {
		t.Fatalf("Run = %v, want ErrWorkerFailing", err)
	}
	if err := sup.Check(context.Background()); !errors.Is(err, ErrNoPwsh) || !strings.Contains(err.Error(), "PowerShell 7") {
		t.Errorf("worker check = %v, want it to name the missing PowerShell 7", err)
	}
}

func TestResolvePwsh(t *testing.T) {
	pf := filepath.Join("C:", "Program Files")
	def := filepath.Join(pf, "PowerShell", "7", "pwsh.exe")
	env := func(k string) string {
		if k == "ProgramFiles" {
			return pf
		}
		return ""
	}
	abs, err := filepath.Abs("pwsh-on-path")
	if err != nil {
		t.Fatal(err)
	}
	onPath := func(string) (string, error) { return abs, nil }
	notOnPath := func(string) (string, error) { return "", errors.New("not found") }
	relative := func(string) (string, error) { return "pwsh", nil }
	yes := func(string) bool { return true }
	no := func(string) bool { return false }

	for _, tc := range []struct {
		name   string
		goos   string
		exists func(string) bool
		look   func(string) (string, error)
		want   string
		err    bool
	}{
		{name: "windows default install", goos: "windows", exists: yes, look: notOnPath, want: def},
		{name: "windows PATH", goos: "windows", exists: no, look: onPath, want: abs},
		{name: "linux PATH", goos: "linux", exists: yes, look: onPath, want: abs},
		{name: "missing", goos: "windows", exists: no, look: notOnPath, err: true},
		{name: "relative", goos: "linux", exists: no, look: relative, err: true},
	} {
		got, err := resolvePwsh(tc.goos, env, tc.exists, tc.look)
		switch {
		case tc.err && !errors.Is(err, ErrNoPwsh):
			t.Errorf("%s: %q, %v; want ErrNoPwsh", tc.name, got, err)
		case tc.err && !strings.Contains(err.Error(), "PowerShell 7") || tc.err && !strings.Contains(err.Error(), "looked in"):
			t.Errorf("%s: message %q does not say what is required and where it looked", tc.name, err)
		case !tc.err && (err != nil || got != tc.want):
			t.Errorf("%s: %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

// supEnv is a supervisor on a fake host, running until the test ends.
type supEnv struct {
	t      *testing.T
	clk    testClock
	host   *fakeHost
	sup    *Supervisor
	runErr chan error
	exits  int
	mu     sync.Mutex
}

func newSupEnv(t *testing.T, h *fakeHost, script []byte) *supEnv {
	t.Helper()
	e := &supEnv{t: t, clk: newTestClock(), host: h, runErr: make(chan error, 1)}
	e.sup = NewSupervisor(SupervisorOptions{
		Start: h.starter(script != nil), Script: script, Clock: e.clk, Log: testLogger(t),
		OnExit: func() { e.mu.Lock(); e.exits++; e.mu.Unlock() },
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { e.runErr <- e.sup.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-e.runErr; err != nil {
			t.Logf("Run: %v", err)
		}
	})
	return e
}

func (e *supEnv) exitCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.exits
}

func slogTo(w io.Writer) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }

// worker returns the current worker, nil while none runs.
func (e *supEnv) worker() *worker {
	e.sup.mu.Lock()
	defer e.sup.mu.Unlock()
	return e.sup.cur
}

// waitNew waits until a worker other than old runs.
func (e *supEnv) waitNew(old *worker) {
	e.t.Helper()
	waitFor(e.t, "worker restart", e.sup.Changed, func() bool { w := e.worker(); return w != nil && w != old })
}

func (e *supEnv) waitUp() {
	e.t.Helper()
	waitFor(e.t, "worker up", e.sup.Changed, func() bool { return e.sup.Check(context.Background()) == nil })
}

func (e *supEnv) vms(ctx context.Context) ([]vmRecord, error) {
	var list vmList
	err := e.sup.Call(ctx, opVMs, nil, &list)
	return list, err
}

func TestSupervisorCall(t *testing.T) {
	h := newFakeHost(loadFixture(t, "host"))
	e := newSupEnv(t, h, []byte("# script"))
	e.waitUp()
	list, err := e.vms(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 5 || list[0].Name != "windows-node-1" || list[0].AssignedBytes != 8*gib {
		t.Errorf("vms = %+v", list)
	}
	h.mu.Lock()
	scripts := h.scripts
	h.mu.Unlock()
	if len(scripts) != 1 || string(scripts[0]) != "# script" {
		t.Errorf("worker got scripts %q", scripts)
	}
	// A worker-reported failure is a *WorkerError.
	h.set(func(f *fixture) { f.VMMS = "Stopped" })
	_, err = e.vms(testCtx(t))
	if workerKind(err) != kindVMMSDown {
		t.Errorf("vmms down: %v", err)
	}
	wantCode(t, "vmms down", toStatus(err), codes.Unavailable)
	// Unknown op.
	if err := e.sup.Call(testCtx(t), "bogus", nil, nil); workerKind(err) != kindProtocol {
		t.Errorf("unknown op: %v", err)
	}
}

func TestSupervisorSerializesCalls(t *testing.T) {
	h := newFakeHost(loadFixture(t, "host"))
	e := newSupEnv(t, h, nil)
	e.waitUp()
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Go(func() {
			var rec hostRecord
			if err := e.sup.Call(testCtx(t), opHost, nil, &rec); err != nil {
				errs <- err
			} else if rec.ComputerName != "hyperv-1" {
				errs <- errors.New("wrong host " + rec.ComputerName)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestSupervisorTimeoutKillsAndRestarts(t *testing.T) {
	h := newFakeHost(loadFixture(t, "host"))
	h.hangOps[opSetStartup] = true
	e := newSupEnv(t, h, nil)
	e.waitUp()
	done := make(chan error, 1)
	go func() { done <- e.sup.Call(context.Background(), opSetStartup, setStartupArgs{ID: "x"}, nil) }()
	<-h.hung
	e.clk.waitTimer(t) // the call's timer
	changed := e.sup.Changed()
	e.clk.Advance(DefaultCallTimeout)
	wantCode(t, "hung call", <-done, codes.DeadlineExceeded)
	<-changed // the killed worker is gone
	e.waitUp()
	if _, err := e.vms(testCtx(t)); err != nil {
		t.Errorf("after the restart: %v", err)
	}
	if h.startCount() != 2 || e.exitCount() != 1 {
		t.Errorf("starts %d, exits %d; want 2, 1", h.startCount(), e.exitCount())
	}
}

func TestSupervisorCallerGivesUp(t *testing.T) {
	h := newFakeHost(loadFixture(t, "host"))
	h.hangOps[opSetStartup] = true
	e := newSupEnv(t, h, nil)
	e.waitUp()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.sup.Call(ctx, opSetStartup, setStartupArgs{ID: "x"}, nil) }()
	<-h.hung
	cancel()
	wantCode(t, "cancelled call", <-done, codes.Canceled)
	// The next call waits behind the hung one: it is still serialized and
	// cannot take the hung call's answer. The timeout frees both.
	next := make(chan error, 1)
	go func() { _, err := e.vms(context.Background()); next <- err }()
	e.clk.waitTimer(t)
	changed := e.sup.Changed()
	e.clk.Advance(DefaultCallTimeout)
	<-changed // the killed worker is gone
	// The next call runs on the dead worker or finds none: UNAVAILABLE,
	// or it succeeds on the new worker.
	if err := <-next; err != nil {
		wantCode(t, "call after a timeout", err, codes.Unavailable)
	}
	e.waitUp()
	if _, err := e.vms(testCtx(t)); err != nil {
		t.Errorf("after the restart: %v", err)
	}
}

func TestSupervisorWorkerCrashDuringCall(t *testing.T) {
	h := newFakeHost(loadFixture(t, "host"))
	h.crashOn[opHost] = true
	e := newSupEnv(t, h, nil)
	e.waitUp()
	old := e.worker()
	err := e.sup.Call(testCtx(t), opHost, nil, nil)
	wantCode(t, "crash", err, codes.Unavailable)
	h.set(func(*fixture) { delete(h.crashOn, opHost) })
	e.waitNew(old)
	if err := e.sup.Call(testCtx(t), opHost, nil, nil); err != nil {
		t.Errorf("after the restart: %v", err)
	}
}

func TestSupervisorGivesUpAfterFiveExitsInFiveMinutes(t *testing.T) {
	h := newFakeHost(loadFixture(t, "host"))
	h.failNext = MaxWorkerExits
	e := newSupEnv(t, h, nil)
	err := <-e.runErr
	e.runErr <- nil // for the cleanup
	if !errors.Is(err, ErrWorkerFailing) {
		t.Fatalf("Run = %v, want ErrWorkerFailing", err)
	}
	if err := e.sup.Check(context.Background()); !errors.Is(err, ErrWorkerFailing) {
		t.Errorf("readiness check = %v, want ErrWorkerFailing", err)
	}
	wantCode(t, "call after giving up", e.sup.Call(testCtx(t), opHost, nil, nil), codes.Unavailable)
	if h.startCount() != MaxWorkerExits {
		t.Errorf("%d starts, want %d", h.startCount(), MaxWorkerExits)
	}
}

func TestSupervisorExitsSpreadOverTimeDoNotGiveUp(t *testing.T) {
	h := newFakeHost(loadFixture(t, "host"))
	h.crashOn[opHost] = true
	e := newSupEnv(t, h, nil)
	// Two exits per window, ten in all: never five within five minutes.
	e.waitUp()
	for i := range 2 * MaxWorkerExits {
		old := e.worker()
		wantCode(t, "crash", e.sup.Call(testCtx(t), opHost, nil, nil), codes.Unavailable)
		e.waitNew(old) // the exit is recorded before the next worker starts
		if i%2 == 1 {
			e.clk.Advance(WorkerExitWindow/2 + time.Second)
		}
	}
	select {
	case err := <-e.runErr:
		t.Fatalf("Run returned %v", err)
	default:
	}
}

func TestSupervisorFailedStartCounts(t *testing.T) {
	h := newFakeHost(loadFixture(t, "host"))
	h.failNext = 2
	e := newSupEnv(t, h, nil)
	e.waitUp()
	if h.startCount() != 3 {
		t.Errorf("%d starts, want 3", h.startCount())
	}
	if err := e.sup.Call(testCtx(t), opHost, nil, nil); err != nil {
		t.Error(err)
	}
}

func TestSupervisorWorkerNotReadingStdin(t *testing.T) {
	h := newFakeHost(loadFixture(t, "host"))
	stall := make(chan struct{})
	t.Cleanup(func() { close(stall) })
	h.stall = stall
	e := newSupEnv(t, h, []byte("# script"))
	<-h.hung
	e.clk.waitTimer(t) // the hello timer runs although the script write blocks
	e.clk.Advance(DefaultCallTimeout)
	e.waitUp() // killed, counted and restarted
	if e.exitCount() != 1 || h.startCount() != 1 {
		t.Errorf("exits %d, started workers %d; want 1, 1", e.exitCount(), h.startCount())
	}
	if _, err := e.vms(testCtx(t)); err != nil {
		t.Errorf("after the restart: %v", err)
	}
}

func TestSupervisorCheckBeforeStart(t *testing.T) {
	sup := NewSupervisor(SupervisorOptions{Start: newFakeHost(loadFixture(t, "host")).starter(false), Clock: newTestClock()})
	if err := sup.Check(context.Background()); err == nil {
		t.Error("worker check passes before Run")
	}
	wantCode(t, "call before Run", sup.Call(testCtx(t), opHost, nil, nil), codes.Unavailable)
}

func TestLogWriter(t *testing.T) {
	var out bytes.Buffer
	w := &logWriter{log: slogTo(&out)}
	_, _ = w.Write([]byte("first line\r\nsecond "))
	_, _ = w.Write([]byte("part\n\n"))
	_, _ = w.Write([]byte(strings.Repeat("y", maxLogLine+10)))
	got := out.String()
	for _, want := range []string{`line="first line"`, `line="second part"`, "yyyy"} {
		if !strings.Contains(got, want) {
			t.Errorf("log %q lacks %q", got, want)
		}
	}
	if strings.Count(got, "\n") != 3 {
		t.Errorf("log has %d records, want 3:\n%s", strings.Count(got, "\n"), got)
	}
}
