package host

import (
	"bytes"
	"context"
	_ "embed" // worker.ps1
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/matfax/hvmc/internal/agent"
)

// The PowerShell worker (DESIGN §4.2)
//
// The agent supervises one long-lived `pwsh -NoLogo -NoProfile
// -NonInteractive` process. Both directions carry frames: a 4-byte
// big-endian length followed by that many bytes of UTF-8 JSON.
//
//	agent → worker   frame 0: the worker script (worker.ps1) itself
//	worker → agent   {"id":0,"ok":true,"result":{"pid":…,…}}          hello
//	agent → worker   {"id":n,"op":"vms","args":{…}}
//	worker → agent   {"id":n,"ok":true,"result":…}
//	                 {"id":n,"ok":false,"error":{"kind":…,"message":…}}
//
// pwsh is started with a short plain-text -Command bootstrap that reads
// the script from the first frame and dot-sources it, so no script file is
// written to disk and the command line stays short.
//
// Calls are serialized, each with its own timeout (default 30 s). A worker
// that does not answer in time is killed and restarted; so is one that
// exits or breaks the framing. Restarts back off on the clock: 1 s,
// doubling up to 30 s, back to 1 s once a worker ran for a minute; the
// readiness check "worker" fails meanwhile. After MaxWorkerExits exits
// within WorkerExitWindow the supervisor gives up: Run returns
// ErrWorkerFailing, which stops the agent with a non-zero exit so the
// service manager's recovery restarts it (§4.8). A missing pwsh is not an
// exit: the agent stays up and unready, and pwsh is looked for again every
// minute.

//go:embed worker.ps1
var workerScript []byte

// Supervisor limits.
const (
	// DefaultCallTimeout is the per-call (per-cmdlet) timeout.
	DefaultCallTimeout = 30 * time.Second
	// MaxWorkerExits worker exits within WorkerExitWindow make the
	// supervisor give up.
	MaxWorkerExits = 5
	// WorkerExitWindow is the window MaxWorkerExits counts in.
	WorkerExitWindow = 5 * time.Minute
	// InitialRestartBackoff is the pause before restarting a worker that
	// ended; it doubles with every further exit up to MaxRestartBackoff.
	InitialRestartBackoff = time.Second
	// MaxRestartBackoff caps the restart pause.
	MaxRestartBackoff = 30 * time.Second
	// BackoffResetAfter is how long a worker must have run for the next
	// restart pause to start again at InitialRestartBackoff.
	BackoffResetAfter = time.Minute
	// PwshRetryInterval is how often pwsh is looked for again while it is
	// missing (ErrNoPwsh).
	PwshRetryInterval = time.Minute
	// maxFrame bounds one frame; a longer length prefix is a protocol
	// error (for example stray text on the worker's stdout).
	maxFrame = 16 << 20
)

// ErrWorkerFailing is returned by Supervisor.Run after MaxWorkerExits
// worker exits within WorkerExitWindow.
var ErrWorkerFailing = errors.New("PowerShell worker keeps failing")

// errFrameTooLarge is a length prefix above maxFrame.
var errFrameTooLarge = errors.New("frame too large")

// writeFrame writes one length-prefixed frame.
func writeFrame(w io.Writer, payload []byte) error {
	if len(payload) > maxFrame {
		return fmt.Errorf("%w: %d bytes", errFrameTooLarge, len(payload))
	}
	buf := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(len(payload))) //nolint:gosec // bounded by maxFrame above
	copy(buf[4:], payload)
	_, err := w.Write(buf)
	return err
}

// readFrame reads one length-prefixed frame. It returns io.EOF only at a
// clean frame boundary.
func readFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrame {
		return nil, fmt.Errorf("%w: length prefix %d", errFrameTooLarge, n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return buf, nil
}

// request is one call to the worker.
type request struct {
	ID   uint64 `json:"id"`
	Op   string `json:"op"`
	Args any    `json:"args,omitempty"`
}

// response is the worker's answer to one request.
type response struct {
	ID     uint64          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *WorkerError    `json:"error,omitempty"`
}

// Process is a started worker process.
type Process interface {
	// Stdin is the worker's standard input.
	Stdin() io.Writer
	// Stdout is the worker's standard output.
	Stdout() io.Reader
	// Kill ends the process.
	Kill() error
	// Wait waits for the process to end, after Stdout has been read to the
	// end.
	Wait() error
}

// Starter starts a worker process.
type Starter func(ctx context.Context) (Process, error)

// pwshArgs are pwsh's arguments; the bootstrap follows -Command.
var pwshArgs = []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command"}

// bootstrap is the plain-text command pwsh starts with. It reads frame 0
// (the worker script) from stdin and dot-sources it, defining what
// worker.ps1 relies on: $hvmcIn, $hvmcOut and Read-HvmcExact.
// Console.Out is redirected to stderr so that stray host output cannot
// corrupt the frames on stdout.
//
// It is deliberately readable text rather than -EncodedCommand: base64
// encoded PowerShell is a common EDR, AMSI and Defender ASR detection
// signal, and Hyper-V hosts often run such tools. It is one line without
// double quotes or backslashes, so Windows command-line quoting passes it
// through unchanged. Nothing caller-controlled is ever part of PowerShell
// code: VM names and values travel only as JSON data in frames.
var bootstrap = strings.Join([]string{
	"$ErrorActionPreference = 'Stop'",
	"$hvmcIn = [Console]::OpenStandardInput()",
	"$hvmcOut = [Console]::OpenStandardOutput()",
	"[Console]::SetOut([Console]::Error)",
	"function Read-HvmcExact([int64]$Count) { " +
		"$buf = [byte[]]::new($Count); $off = 0; " +
		"while ($off -lt $Count) { $n = $hvmcIn.Read($buf, $off, $Count - $off); if ($n -le 0) { exit 0 }; $off += $n }; " +
		"return ,$buf }",
	"$hvmcHdr = Read-HvmcExact 4",
	"$hvmcLen = ([int64]$hvmcHdr[0] -shl 24) -bor ([int64]$hvmcHdr[1] -shl 16) -bor ([int64]$hvmcHdr[2] -shl 8) -bor [int64]$hvmcHdr[3]",
	". ([scriptblock]::Create([Text.Encoding]::UTF8.GetString((Read-HvmcExact $hvmcLen))))",
}, "; ")

// ErrNoPwsh means PowerShell 7 was not found. The host agent requires
// PowerShell 7 and never falls back to Windows PowerShell 5.1.
var ErrNoPwsh = errors.New("the host agent requires PowerShell 7 (pwsh), which was not found; Windows PowerShell 5.1 is not supported")

// resolvePwsh returns the absolute path of pwsh: the default installation
// under %ProgramFiles%\PowerShell\7 on Windows if present, else pwsh from
// PATH. A relative result (current directory) is refused.
func resolvePwsh(goos string, getenv func(string) string, exists func(string) bool, lookPath func(string) (string, error)) (string, error) {
	where := "PATH"
	if goos == "windows" {
		if pf := getenv("ProgramFiles"); pf != "" {
			p := filepath.Join(pf, "PowerShell", "7", "pwsh.exe")
			if exists(p) {
				return p, nil
			}
			where = p + " and PATH"
		}
	}
	p, err := lookPath("pwsh")
	if err != nil {
		return "", fmt.Errorf("%w (looked in %s): %w", ErrNoPwsh, where, err)
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%w (looked in %s): %q on PATH is not an absolute path", ErrNoPwsh, where, p)
	}
	return p, nil
}

// FindPwsh returns the absolute path of PowerShell 7 as the agent resolves
// it (DESIGN §4.2), or an error wrapping ErrNoPwsh that says where it
// looked. `hvmc agent install --role host` checks it before changing
// anything (DESIGN §4.5).
func FindPwsh() (string, error) {
	return resolvePwsh(runtime.GOOS, os.Getenv, fileExists, exec.LookPath)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// pwshCommand returns the worker command for pwsh at path: started
// directly (no shell), with fixed arguments.
func pwshCommand(path string) *exec.Cmd {
	// Not CommandContext: the supervisor kills the worker itself.
	return exec.Command(path, append(append([]string(nil), pwshArgs...), bootstrap)...) //nolint:gosec // G204: fixed arguments; path from resolvePwsh
}

// PwshStarter returns a Starter that starts pwsh with the bootstrap; the
// worker's stderr goes to stderr. pwsh is resolved to an absolute path on
// the first start that finds it and never looked up again. Until then every
// start fails with ErrNoPwsh, which the supervisor treats as
// misconfiguration: it retries every PwshRetryInterval and keeps the worker
// readiness check failing.
func PwshStarter(stderr io.Writer) Starter {
	var (
		mu   sync.Mutex
		path string
	)
	return func(context.Context) (Process, error) {
		mu.Lock()
		if path == "" {
			p, err := FindPwsh()
			if err != nil {
				mu.Unlock()
				return nil, err
			}
			path = p
		}
		cmd := pwshCommand(path)
		mu.Unlock()
		return StartCommand(cmd, stderr)
	}
}

// StartCommand starts cmd as a worker process with pipes for stdin and
// stdout.
func StartCommand(cmd *exec.Cmd, stderr io.Writer) (Process, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", cmd.Path, err)
	}
	return &execProcess{cmd: cmd, stdin: stdin, stdout: stdout}, nil
}

type execProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
}

func (p *execProcess) Stdin() io.Writer  { return p.stdin }
func (p *execProcess) Stdout() io.Reader { return p.stdout }
func (p *execProcess) Kill() error       { return p.cmd.Process.Kill() }
func (p *execProcess) Wait() error       { return p.cmd.Wait() }

// logWriter logs every line written to it, for the worker's stderr.
type logWriter struct {
	log *slog.Logger

	mu  sync.Mutex
	buf []byte
}

const maxLogLine = 4096

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emit(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > maxLogLine {
		w.emit(w.buf)
		w.buf = nil
	}
	return len(p), nil
}

func (w *logWriter) emit(line []byte) {
	if len(line) > maxLogLine {
		line = line[:maxLogLine]
	}
	if s := strings.TrimRight(string(line), "\r "); s != "" {
		w.log.Warn("PowerShell worker stderr", "line", s)
	}
}

// worker is one running worker process with its stdout reader.
type worker struct {
	proc Process
	log  *slog.Logger
	resp chan response
	done chan struct{}
	err  error // why the worker ended; set before done is closed
	kill sync.Once
}

func startWorker(p Process, log *slog.Logger) *worker {
	w := &worker{proc: p, log: log, resp: make(chan response, 1), done: make(chan struct{})}
	go w.read()
	return w
}

// read delivers responses until stdout ends or breaks, then reaps the
// process.
func (w *worker) read() {
	err := w.readLoop()
	w.stop()
	werr := w.proc.Wait()
	switch {
	case errors.Is(err, io.EOF) && werr != nil:
		err = fmt.Errorf("worker exited: %w", werr)
	case errors.Is(err, io.EOF):
		err = errors.New("worker exited")
	default:
		err = fmt.Errorf("worker output broken: %w", err)
	}
	w.err = err
	close(w.done)
}

func (w *worker) readLoop() error {
	for {
		frame, err := readFrame(w.proc.Stdout())
		if err != nil {
			return err
		}
		var r response
		if err := json.Unmarshal(frame, &r); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		select {
		case w.resp <- r:
		default:
			w.log.Warn("dropping unexpected worker response", "id", r.ID)
		}
	}
}

// send writes one frame to the worker without blocking the caller: a
// worker that does not read its stdin (the script is larger than a pipe
// buffer) must not block the supervisor beyond the call timeout, which
// kills it and so ends the write. A failed write kills the worker, which
// ends the waiting call with UNAVAILABLE.
func (w *worker) send(frame []byte) {
	go func() {
		if err := writeFrame(w.proc.Stdin(), frame); err != nil {
			w.log.Warn("writing to the PowerShell worker failed; killing it", "error", err)
			w.stop()
		}
	}()
}

// stop kills the process once.
func (w *worker) stop() {
	w.kill.Do(func() { _ = w.proc.Kill() })
}

// SupervisorOptions configure a Supervisor.
type SupervisorOptions struct {
	// Start starts a worker process. Required.
	Start Starter
	// Script is sent as frame 0 to every new worker; nil sends nothing.
	Script []byte
	// Clock times calls and counts exits. Required.
	Clock agent.Clock
	// Log defaults to discarding.
	Log *slog.Logger
	// CallTimeout defaults to DefaultCallTimeout.
	CallTimeout time.Duration
	// OnExit is called after every worker exit that is not a shutdown
	// (metrics).
	OnExit func()
}

// Supervisor runs the PowerShell worker and serializes calls to it.
type Supervisor struct {
	opts   SupervisorOptions
	sem    chan struct{}
	nextID atomic.Uint64

	mu      sync.Mutex
	cur     *worker
	exits   []time.Time
	failed  error
	lastErr error
	hello   json.RawMessage // the current worker's hello
	noPwsh  error           // ErrNoPwsh while pwsh is missing
	pausing time.Duration   // the pause Run is in (restart backoff, pwsh retry); 0 otherwise
	paused  int             // pauses completed, for tests
	changed chan struct{}   // closed and replaced when the state above changes
}

// NewSupervisor returns a supervisor; Run starts the worker.
func NewSupervisor(o SupervisorOptions) *Supervisor {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.CallTimeout <= 0 {
		o.CallTimeout = DefaultCallTimeout
	}
	return &Supervisor{opts: o, sem: make(chan struct{}, 1), changed: make(chan struct{})}
}

// Changed returns a channel that is closed at the next start, exit or
// give-up of the worker.
func (s *Supervisor) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

func (s *Supervisor) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Supervisor) setCurrent(w *worker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur = w
	s.notifyLocked()
}

func (s *Supervisor) current() (*worker, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.failed != nil:
		return nil, status.Errorf(codes.Unavailable, "PowerShell worker unavailable: %v", s.failed)
	case s.noPwsh != nil:
		return nil, status.Errorf(codes.Unavailable, "PowerShell worker unavailable: %v", s.noPwsh)
	case s.cur == nil:
		return nil, status.Error(codes.Unavailable, "PowerShell worker is restarting")
	default:
		return s.cur, nil
	}
}

// worker returns the running worker, nil while none runs.
func (s *Supervisor) worker() *worker {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// Check is the readiness check "worker" (pwsh worker alive).
func (s *Supervisor) Check(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.failed != nil:
		return s.failed
	case s.noPwsh != nil:
		return s.noPwsh
	case s.cur == nil && s.lastErr != nil:
		return fmt.Errorf("PowerShell worker is not running: %w", s.lastErr)
	case s.cur == nil:
		return errors.New("PowerShell worker is not running")
	default:
		return nil
	}
}

// Run starts the worker and restarts it whenever it ends, until ctx ends
// (nil) or the worker exited MaxWorkerExits times within WorkerExitWindow
// (ErrWorkerFailing). Restarts back off from InitialRestartBackoff,
// doubling up to MaxRestartBackoff, and start over once a worker ran for
// BackoffResetAfter. A missing pwsh (ErrNoPwsh) is misconfiguration, not a
// worker exit: Run keeps going, retries every PwshRetryInterval and the
// worker check reports it until pwsh is found.
func (s *Supervisor) Run(ctx context.Context) error {
	backoff := InitialRestartBackoff
	for {
		uptime, err := s.runOnce(ctx)
		next, stop, gaveUp := s.afterRun(ctx, uptime, err, backoff)
		if stop {
			return gaveUp
		}
		backoff = next
	}
}

// afterRun handles the end of one worker run: a stop through ctx, a
// missing pwsh, or an exit that counts toward the give-up rule and is
// followed by the restart pause. It returns the next restart pause,
// whether Run stops, and ErrWorkerFailing when it gives up.
func (s *Supervisor) afterRun(ctx context.Context, uptime time.Duration, err error, backoff time.Duration) (time.Duration, bool, error) {
	select {
	case <-ctx.Done():
		return 0, true, nil
	default:
	}
	if s.setNoPwsh(err) {
		return backoff, !s.pause(ctx, PwshRetryInterval), nil
	}
	if s.opts.OnExit != nil {
		s.opts.OnExit()
	}
	if gaveUp := s.recordExit(err); gaveUp != nil {
		return 0, true, gaveUp
	}
	if uptime >= BackoffResetAfter {
		backoff = InitialRestartBackoff
	}
	if !s.pause(ctx, backoff) {
		return 0, true, nil
	}
	return nextBackoff(backoff), false, nil
}

// nextBackoff doubles a restart pause up to MaxRestartBackoff.
func nextBackoff(d time.Duration) time.Duration { return min(2*d, MaxRestartBackoff) }

// setNoPwsh records whether the last start failed for lack of pwsh and logs
// when that starts and stops being the case. It reports whether pwsh is
// missing.
func (s *Supervisor) setNoPwsh(err error) bool {
	missing := errors.Is(err, ErrNoPwsh)
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case missing && s.noPwsh == nil:
		s.opts.Log.Error("PowerShell 7 not found; the host agent stays unready and looks again periodically",
			"error", err, "retry", PwshRetryInterval)
	case !missing && s.noPwsh != nil:
		s.opts.Log.Info("PowerShell 7 found")
	}
	if missing {
		s.noPwsh = err
	} else {
		s.noPwsh = nil
	}
	s.notifyLocked()
	return missing
}

// pause waits d on the clock; it reports false when ctx ended first.
func (s *Supervisor) pause(ctx context.Context, d time.Duration) bool {
	t := s.opts.Clock.NewTimer(d)
	defer t.Stop()
	s.setPausing(d, false)
	defer s.setPausing(0, true)
	select {
	case <-t.C():
		return true
	case <-ctx.Done():
		return false
	}
}

func (s *Supervisor) setPausing(d time.Duration, done bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pausing = d
	if done {
		s.paused++
	}
	s.notifyLocked()
}

// runOnce starts one worker and waits until it ends or ctx ends. It
// returns how long the worker ran (0 if it never started).
func (s *Supervisor) runOnce(ctx context.Context) (time.Duration, error) {
	w, err := s.spawn(ctx)
	if err != nil {
		return 0, err
	}
	s.setNoPwsh(nil)
	// The uptime counts from before the worker is published, so whoever
	// sees it running also sees its start time.
	started := s.opts.Clock.Now()
	s.setCurrent(w)
	select {
	case <-w.done:
		s.setCurrent(nil)
		return s.opts.Clock.Now().Sub(started), w.err
	case <-ctx.Done():
		s.setCurrent(nil)
		w.stop()
		<-w.done
		return 0, nil
	}
}

// spawn starts a worker, sends the script and waits for its hello.
func (s *Supervisor) spawn(ctx context.Context) (*worker, error) {
	p, err := s.opts.Start(ctx)
	if err != nil {
		return nil, err
	}
	w := startWorker(p, s.opts.Log)
	if s.opts.Script != nil {
		w.send(s.opts.Script)
	}
	r, err := s.await(ctx, w, 0)
	if err == nil && !r.OK {
		err = fmt.Errorf("worker failed to start: %w", r.Error)
	}
	if err != nil {
		w.stop()
		<-w.done
		return nil, err
	}
	s.opts.Log.Info("PowerShell worker started", "hello", string(r.Result))
	s.mu.Lock()
	s.hello = r.Result
	s.mu.Unlock()
	return w, nil
}

// recordExit counts a worker exit and gives up after MaxWorkerExits
// within WorkerExitWindow.
func (s *Supervisor) recordExit(cause error) error {
	now := s.opts.Clock.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]time.Time, 0, len(s.exits)+1)
	for _, t := range s.exits {
		if now.Sub(t) < WorkerExitWindow {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	s.exits = kept
	s.lastErr = cause
	if len(s.exits) < MaxWorkerExits {
		s.opts.Log.Warn("PowerShell worker ended; restarting", "error", cause, "exits", len(s.exits), "window", WorkerExitWindow)
		return nil
	}
	s.failed = fmt.Errorf("%w: %d exits within %s, last: %w", ErrWorkerFailing, len(s.exits), WorkerExitWindow, cause)
	s.opts.Log.Error("PowerShell worker keeps failing; giving up", "error", s.failed)
	s.notifyLocked()
	return s.failed
}

// await waits for the response with the given id, the worker's end or the
// call timeout, which kills the worker. ctx only matters while the worker
// starts: a call that was sent waits for its answer even after the caller
// gave up, so a late answer can never be taken for the next call's.
func (s *Supervisor) await(ctx context.Context, w *worker, id uint64) (response, error) {
	t := s.opts.Clock.NewTimer(s.opts.CallTimeout)
	defer t.Stop()
	var cancel <-chan struct{}
	if id == 0 {
		cancel = ctx.Done()
	}
	for {
		select {
		case r := <-w.resp:
			if r.ID != id {
				s.opts.Log.Warn("ignoring worker response for another call", "id", r.ID, "want", id)
				continue
			}
			return r, nil
		case <-w.done:
			return response{}, status.Errorf(codes.Unavailable, "PowerShell worker ended during the call: %v", w.err)
		case <-t.C():
			s.opts.Log.Warn("PowerShell worker call timed out; killing the worker", "id", id, "timeout", s.opts.CallTimeout)
			w.stop()
			return response{}, status.Errorf(codes.DeadlineExceeded, "Hyper-V call did not finish within %s", s.opts.CallTimeout)
		case <-cancel:
			return response{}, status.FromContextError(ctx.Err()).Err()
		}
	}
}

// Call runs op with args on the worker and decodes its result into
// result (nil: discard). A worker-reported failure is a *WorkerError;
// supervisor failures are status errors: UNAVAILABLE while no worker
// runs or when it ends during the call, DEADLINE_EXCEEDED on the call
// timeout, the caller's context error when ctx ends first.
func (s *Supervisor) Call(ctx context.Context, op string, args, result any) error {
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
	w, err := s.current()
	if err != nil {
		<-s.sem
		return err
	}
	id := s.nextID.Add(1)
	payload, err := json.Marshal(request{ID: id, Op: op, Args: args})
	if err != nil {
		<-s.sem
		return status.Errorf(codes.Internal, "encode %s request: %v", op, err)
	}
	w.send(payload)
	type outcome struct {
		r   response
		err error
	}
	out := make(chan outcome, 1)
	go func() {
		r, err := s.await(ctx, w, id)
		<-s.sem
		out <- outcome{r, err}
	}()
	select {
	case o := <-out:
		return decodeResult(op, o.r, o.err, result)
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
}

func decodeResult(op string, r response, err error, result any) error {
	switch {
	case err != nil:
		return err
	case !r.OK && r.Error == nil:
		return &WorkerError{Kind: kindProtocol, Message: op + ": failure without an error"}
	case !r.OK:
		return r.Error
	case result == nil:
		return nil
	}
	if err := json.Unmarshal(r.Result, result); err != nil {
		return status.Errorf(codes.Internal, "unparsable %s output from the PowerShell worker: %v", op, err)
	}
	return nil
}
