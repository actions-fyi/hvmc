package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

// fixture is a Hyper-V host as testdata/hyperv describes it (README.md).
type fixture struct {
	ComputerName      string       `json:"computerName"`
	TotalBytes        int64        `json:"totalBytes"`
	OverheadBytes     int64        `json:"overheadBytes"`
	ReportedFreeBytes *int64       `json:"reportedFreeBytes,omitempty"`
	VMMS              string       `json:"vmms"`
	Hang              []string     `json:"hang"`
	VMs               []fixtureVM  `json:"vms"`
	Errors            fixtureCodes `json:"-"`
}

type fixtureVM struct {
	vmRecord
	ShrinkFloorBytes *int64 `json:"shrinkFloorBytes,omitempty"`
	StartupDrift     bool   `json:"startupDrift,omitempty"`
}

// fixtureError is one cmdlet error record of errors.json.
type fixtureError struct {
	Message  string `json:"message"`
	Category string `json:"category"`
	ErrorID  string `json:"errorId"`
	HResult  int64  `json:"hresult"`
}

type fixtureCodes struct {
	VMMSDown      fixtureError `json:"vmmsDown"`
	VMNotFound    fixtureError `json:"vmNotFound"`
	ShrinkRefused fixtureError `json:"shrinkRefused"`
	OutOfMemory   fixtureError `json:"outOfMemory"`
}

func fixtureDir() string { return filepath.Join("testdata", "hyperv") }

func loadFixture(t *testing.T, name string) *fixture {
	t.Helper()
	var f fixture
	readJSON(t, filepath.Join(fixtureDir(), name+".json"), &f)
	readJSON(t, filepath.Join(fixtureDir(), "errors.json"), &f.Errors)
	return &f
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// fakeHost is the shared Hyper-V state behind fake workers; it survives
// worker restarts like a real host.
type fakeHost struct {
	mu sync.Mutex
	f  *fixture

	// Behaviours for supervisor tests.
	crashOn  map[string]bool // op → the worker exits instead of answering
	hangOps  map[string]bool // op → never answers
	failNext int             // next n workers fail to start (startup error)
	starts   int
	hung     chan string // receives the op of each call that hangs
	scripts  [][]byte    // frame 0 of every worker
}

func newFakeHost(f *fixture) *fakeHost {
	return &fakeHost{f: f, crashOn: map[string]bool{}, hangOps: map[string]bool{}, hung: make(chan string, 16)}
}

func (h *fakeHost) startCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.starts
}

func (h *fakeHost) set(fn func(f *fixture)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn(h.f)
}

// errFakeExit ends a fake worker as if the process exited.
var errFakeExit = errors.New("fake worker exit")

// serve speaks the worker protocol on r/w until r ends or the worker
// crashes. expectScript reads frame 0 first.
func (h *fakeHost) serve(r io.Reader, w io.Writer, expectScript bool) error {
	if expectScript {
		script, err := readFrame(r)
		if err != nil {
			return err
		}
		h.mu.Lock()
		h.scripts = append(h.scripts, script)
		h.mu.Unlock()
	}
	if err := h.hello(w); err != nil {
		return err
	}
	for {
		frame, err := readFrame(r)
		if err != nil {
			return err
		}
		var req struct {
			ID   uint64          `json:"id"`
			Op   string          `json:"op"`
			Args json.RawMessage `json:"args"`
		}
		if err := json.Unmarshal(frame, &req); err != nil {
			return err
		}
		resp, hang, err := h.handle(req.ID, req.Op, req.Args)
		if err != nil {
			return err
		}
		if hang {
			h.hung <- req.Op
			_, _ = io.Copy(io.Discard, r) // until the worker is killed
			return io.EOF
		}
		if err := writeJSONFrame(w, resp); err != nil {
			return err
		}
	}
}

// hello answers frame 0, or fails the start as Import-Module would.
func (h *fakeHost) hello(w io.Writer) error {
	h.mu.Lock()
	h.starts++
	fail := h.failNext > 0
	if fail {
		h.failNext--
	}
	h.mu.Unlock()
	if fail {
		_ = writeJSONFrame(w, response{ID: 0, OK: false, Error: &WorkerError{Kind: kindStartup, Message: "Import-Module Hyper-V failed"}})
		return errFakeExit
	}
	return writeJSONFrame(w, response{ID: 0, OK: true, Result: json.RawMessage(`{"pid":1,"fake":true}`)})
}

func writeJSONFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeFrame(w, b)
}

// handle runs one op. It reports a hang (the caller blocks until the
// worker is killed) or a crash (errFakeExit).
func (h *fakeHost) handle(id uint64, op string, args json.RawMessage) (resp response, hang bool, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.crashOn[op]:
		return response{}, false, errFakeExit
	case h.hangOps[op], h.f.VMMS == "Running" && h.cmdletHangs(op):
		return response{}, true, nil
	}
	result, werr := h.opLocked(op, args)
	if werr != nil {
		return response{ID: id, OK: false, Error: werr}, false, nil
	}
	return response{ID: id, OK: true, Result: result}, false, nil
}

// cmdletHangs reports whether op runs a cmdlet listed in the fixture's
// hang.
func (h *fakeHost) cmdletHangs(op string) bool {
	cmdlets := map[string][]string{
		opVMs: {"Get-VM", "Get-VMMemory"}, opSetStartup: {"Get-VM", "Set-VMMemory"},
		opSetDynamic: {"Get-VM", "Set-VMMemory"},
	}
	for _, c := range cmdlets[op] {
		if slices.Contains(h.f.Hang, c) {
			return true
		}
	}
	return false
}

// workerErr classifies a cmdlet error as worker.ps1 does.
func (h *fakeHost) workerErr(e fixtureError) *WorkerError {
	kind := kindFailed
	switch {
	case h.f.VMMS != "Running":
		kind = kindVMMSDown
	case slices.Contains([]int64{-2147024882, -2147024888, -2147023446}, e.HResult):
		kind = kindOutOfMemory
	}
	return &WorkerError{Kind: kind, Message: e.Message, Category: e.Category, ErrorID: e.ErrorID, HResult: e.HResult}
}

func (h *fakeHost) opLocked(op string, args json.RawMessage) (json.RawMessage, *WorkerError) {
	if !slices.Contains([]string{opVMs, opHost, opSetStartup, opSetDynamic}, op) {
		return nil, &WorkerError{Kind: kindProtocol, Message: fmt.Sprintf("unknown op '%s'", op)}
	}
	if h.f.VMMS != "Running" {
		return nil, h.workerErr(h.f.Errors.VMMSDown)
	}
	switch op {
	case opVMs:
		list := make([]vmRecord, 0, len(h.f.VMs))
		for _, v := range h.f.VMs {
			list = append(list, v.vmRecord)
		}
		return mustJSON(list), nil
	case opHost:
		free := h.freeLocked()
		if h.f.ReportedFreeBytes != nil {
			free = *h.f.ReportedFreeBytes
		}
		return mustJSON(hostRecord{ComputerName: h.f.ComputerName, TotalKiB: h.f.TotalBytes / 1024, FreeKiB: free / 1024}), nil
	case opSetStartup:
		var a setStartupArgs
		_ = json.Unmarshal(args, &a)
		v := h.vmLocked(a.ID)
		if v == nil {
			return nil, h.workerErr(h.f.Errors.VMNotFound)
		}
		return mustJSON(struct{}{}), h.setStartupLocked(v, a.StartupBytes)
	default: // opSetDynamic
		var a setDynamicArgs
		_ = json.Unmarshal(args, &a)
		v := h.vmLocked(a.ID)
		if v == nil {
			return nil, h.workerErr(h.f.Errors.VMNotFound)
		}
		if a.MinimumBytes != nil {
			v.MinimumBytes = *a.MinimumBytes
		}
		if a.MaximumBytes != nil {
			v.MaximumBytes = *a.MaximumBytes
		}
		return mustJSON(struct{}{}), nil
	}
}

func (h *fakeHost) vmLocked(id string) *fixtureVM {
	for i := range h.f.VMs {
		if h.f.VMs[i].ID == id {
			return &h.f.VMs[i]
		}
	}
	return nil
}

func (h *fakeHost) freeLocked() int64 {
	free := h.f.TotalBytes - h.f.OverheadBytes
	for _, v := range h.f.VMs {
		free -= v.AssignedBytes
	}
	return free
}

// setStartupLocked mirrors Set-MockStartup in the mock module.
func (h *fakeHost) setStartupLocked(v *fixtureVM, n int64) *WorkerError {
	if v.State != "Running" {
		v.StartupBytes = n
		return nil
	}
	cur := v.AssignedBytes
	if n > cur {
		if n-cur > h.freeLocked() {
			return h.workerErr(h.f.Errors.OutOfMemory)
		}
		v.AssignedBytes, v.StartupBytes = n, n
		return nil
	}
	if v.ShrinkFloorBytes == nil || n >= *v.ShrinkFloorBytes {
		v.AssignedBytes, v.StartupBytes = n, n
		return nil
	}
	up := (*v.ShrinkFloorBytes + alignment - 1) / alignment * alignment
	got := min(cur, up)
	v.AssignedBytes, v.StartupBytes = got, got
	if v.StartupDrift {
		v.StartupBytes = n
	}
	return h.workerErr(h.f.Errors.ShrinkRefused)
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// pipeProcess is an in-memory worker process running a fakeHost.
type pipeProcess struct {
	stdinR  *io.PipeReader
	stdinW  *io.PipeWriter
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	done    chan struct{}
	once    sync.Once
	err     error
}

func (p *pipeProcess) Stdin() io.Writer  { return p.stdinW }
func (p *pipeProcess) Stdout() io.Reader { return p.stdoutR }

func (p *pipeProcess) Kill() error {
	p.exit(errors.New("killed"))
	return nil
}

func (p *pipeProcess) exit(err error) {
	p.once.Do(func() {
		p.err = err
		_ = p.stdinR.CloseWithError(err)
		_ = p.stdoutW.Close()
		close(p.done)
	})
}

func (p *pipeProcess) Wait() error {
	<-p.done
	return p.err
}

// starter returns a Starter running workers on h through pipes.
func (h *fakeHost) starter(expectScript bool) Starter {
	return func(context.Context) (Process, error) {
		inR, inW := io.Pipe()
		outR, outW := io.Pipe()
		p := &pipeProcess{stdinR: inR, stdinW: inW, stdoutR: outR, stdoutW: outW, done: make(chan struct{})}
		go func() {
			err := h.serve(inR, outW, expectScript)
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
				err = nil
			}
			p.exit(err)
		}()
		return p, nil
	}
}
