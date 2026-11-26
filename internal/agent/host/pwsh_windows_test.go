//go:build windows

package host

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
)

// The real worker.ps1 under pwsh, against the mock Hyper-V module in
// testdata/mock (the GitHub Windows runner has pwsh but no Hyper-V). The
// mock's directory goes first on PSModulePath, so `Import-Module Hyper-V`
// in the worker resolves to it; the hello's module path proves it.

// pwshStarter starts pwsh as the agent does, with the mock module and the
// fixture file at path.
func pwshStarter(t *testing.T, path string) (Starter, *notifyBuffer) {
	t.Helper()
	mock, err := filepath.Abs(filepath.Join("testdata", "mock"))
	if err != nil {
		t.Fatal(err)
	}
	pwsh, err := resolvePwsh(runtime.GOOS, os.Getenv, fileExists, exec.LookPath)
	if err != nil {
		t.Fatalf("pwsh is required for the worker tests on Windows: %v", err)
	}
	stderr := &notifyBuffer{changed: make(chan struct{})}
	return func(context.Context) (Process, error) {
		cmd := pwshCommand(pwsh)
		cmd.Env = append(os.Environ(),
			"PSModulePath="+mock+string(os.PathListSeparator)+os.Getenv("PSModulePath"),
			"HVMC_HYPERV_FIXTURE="+path)
		return StartCommand(cmd, stderr)
	}, stderr
}

// fixturePath returns the absolute path of a scenario in testdata/hyperv.
func fixturePath(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join(fixtureDir(), name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// newPwshEnv runs the host agent service on the real worker.
func newPwshEnv(t *testing.T, path string) (*testEnv, *notifyBuffer) {
	t.Helper()
	start, stderr := pwshStarter(t, path)
	clk := newTestClock()
	e := newTestEnv(t, nil, testPolicy(), func(o *envOptions) {
		o.start, o.script, o.clk = start, workerScript, &clk
	})
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("worker stderr:\n%s", stderr.String())
		}
	})
	return e, stderr
}

func TestPwshWorkerUsesTheMockModule(t *testing.T) {
	e, _ := newPwshEnv(t, fixturePath(t, "host"))
	sup := e.svc.srv.sup
	sup.mu.Lock()
	hello := sup.hello
	sup.mu.Unlock()
	var h struct {
		PID          int64  `json:"pid"`
		PSVersion    string `json:"psVersion"`
		HyperVModule string `json:"hypervModule"`
	}
	if err := json.Unmarshal(hello, &h); err != nil {
		t.Fatalf("hello %s: %v", hello, err)
	}
	mock, _ := filepath.Abs(filepath.Join("testdata", "mock"))
	if !strings.HasPrefix(strings.ToLower(h.HyperVModule), strings.ToLower(mock)) {
		t.Fatalf("worker loaded Hyper-V from %q, want the mock in %q", h.HyperVModule, mock)
	}
	if h.PID <= 0 || !strings.HasPrefix(h.PSVersion, "7.") {
		t.Errorf("hello %+v", h)
	}
}

func TestPwshReads(t *testing.T) {
	e, _ := newPwshEnv(t, fixturePath(t, "host"))
	resp, err := e.client.GetHost(testCtx(t), &agentv1.GetHostRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if h := resp.GetHost(); h.GetName() != "hyperv-1" || h.GetTotalBytes() != 64*gib || h.GetFreeBytes() != 40*gib {
		t.Errorf("GetHost %v", h)
	}
	dm := e.vm("windows-dm-1")
	want := &agentv1.DynamicMemory{Enabled: true, MinimumBytes: 2 * gib, MaximumBytes: 8 * gib, BufferPercent: 20, Priority: 50}
	if got := dm.GetDynamicMemory(); got.GetEnabled() != want.GetEnabled() || got.GetMinimumBytes() != want.GetMinimumBytes() ||
		got.GetMaximumBytes() != want.GetMaximumBytes() || got.GetBufferPercent() != want.GetBufferPercent() || got.GetPriority() != want.GetPriority() {
		t.Errorf("DM %v, want %v", got, want)
	}
	if dm.GetId() != "00000002-0000-4000-8000-000000000002" || dm.GetHypervState() != "Running" || dm.GetAssignedBytes() != 4*gib {
		t.Errorf("DM VM %v", dm)
	}
	if v := e.vm("windows-saved-1"); v.GetState() != agentv1.VMState_VM_STATE_SAVED || v.GetAssignedBytes() != 0 {
		t.Errorf("saved VM %v", v)
	}
}

// writeFixture writes f with errors.json into a new directory.
func writeFixture(t *testing.T, f *fixture) string {
	t.Helper()
	dir := t.TempDir()
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	errs, err := os.ReadFile(filepath.Join(fixtureDir(), "errors.json"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "scenario.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "errors.json"), errs, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPwshListsOneAndNoVMs(t *testing.T) {
	for _, n := range []int{0, 1} {
		f := loadFixture(t, "host")
		f.VMs = f.VMs[:n]
		e, _ := newPwshEnv(t, writeFixture(t, f))
		resp, err := e.client.ListVMs(testCtx(t), &agentv1.ListVMsRequest{})
		if err != nil {
			t.Fatalf("%d VMs: %v", n, err)
		}
		if got := len(resp.GetSnapshot().GetVms()); got != n {
			t.Errorf("%d VMs listed, want %d", got, n)
		}
	}
}

func TestPwshWrites(t *testing.T) {
	e, _ := newPwshEnv(t, fixturePath(t, "host"))
	ctx := testCtx(t)
	resp, err := e.client.SetStartup(ctx, startupReq("windows-node-1", 12*gib))
	if err != nil {
		t.Fatal(err)
	}
	if a := resp.GetResult().GetAfter(); a.GetAssignedBytes() != 12*gib || a.GetStartupBytes() != 12*gib {
		t.Errorf("grow: after %v", a)
	}
	resp, err = e.client.SetStartup(ctx, startupReq("windows-node-1", 6*gib))
	if err != nil {
		t.Fatal(err)
	}
	if a := resp.GetResult().GetAfter(); a.GetAssignedBytes() != 6*gib {
		t.Errorf("shrink: after %v", a)
	}
	resp, err = e.client.SetStartup(ctx, startupReq("windows-off-1", 6*gib))
	if err != nil {
		t.Fatal(err)
	}
	if a := resp.GetResult().GetAfter(); a.GetStartupBytes() != 6*gib || a.GetAssignedBytes() != 0 {
		t.Errorf("off VM: after %v", a)
	}
	lr, err := e.client.SetDynamicLimits(ctx, &agentv1.SetDynamicLimitsRequest{VmName: "windows-dm-1", MinimumBytes: ptr(gib), MaximumBytes: ptr(12 * gib)})
	if err != nil {
		t.Fatal(err)
	}
	if dm := lr.GetResult().GetAfter().GetDynamicMemory(); dm.GetMinimumBytes() != gib || dm.GetMaximumBytes() != 12*gib {
		t.Errorf("limits: after %v", dm)
	}
	lr, err = e.client.SetDynamicLimits(ctx, limitsReq("windows-dm-1", 16*gib))
	if err != nil {
		t.Fatal(err)
	}
	if dm := lr.GetResult().GetAfter().GetDynamicMemory(); dm.GetMinimumBytes() != gib || dm.GetMaximumBytes() != 16*gib {
		t.Errorf("max only: after %v", dm)
	}
}

func TestPwshRefusedShrink(t *testing.T) {
	e, _ := newPwshEnv(t, fixturePath(t, "refused-shrink"))
	_, err := e.client.SetStartup(testCtx(t), startupReq("windows-node-1", 4*gib))
	wantCode(t, "refused shrink", err, codes.Aborted)
	if a := resizeDetails(err).GetAfter(); a.GetAssignedBytes() != 8*gib || a.GetStartupBytes() != 8*gib {
		t.Errorf("after %v", a)
	}
}

func TestPwshPartialShrink(t *testing.T) {
	e, _ := newPwshEnv(t, fixturePath(t, "partial-shrink"))
	_, err := e.client.SetStartup(testCtx(t), startupReq("windows-node-1", 4*gib))
	wantCode(t, "partial shrink", err, codes.Aborted)
	if a := resizeDetails(err).GetAfter(); a.GetAssignedBytes() != 6*gib || a.GetStartupBytes() != 4*gib {
		t.Errorf("after %v", a)
	}
}

func TestPwshOutOfMemory(t *testing.T) {
	e, _ := newPwshEnv(t, fixturePath(t, "out-of-memory"))
	_, err := e.client.SetStartup(testCtx(t), startupReq("windows-node-1", 12*gib))
	wantCode(t, "grow", err, codes.ResourceExhausted)
}

func TestPwshVMMSDown(t *testing.T) {
	e, _ := newPwshEnv(t, fixturePath(t, "vmms-down"))
	ctx := testCtx(t)
	_, err := e.client.GetHost(ctx, &agentv1.GetHostRequest{})
	wantCode(t, "GetHost", err, codes.Unavailable)
	_, err = e.client.ListVMs(ctx, &agentv1.ListVMsRequest{})
	wantCode(t, "ListVMs", err, codes.Unavailable)
	_, err = e.client.SetStartup(ctx, startupReq("windows-node-1", 10*gib))
	wantCode(t, "SetStartup", err, codes.Unavailable)
	var list vmList
	if err := e.svc.srv.sup.Call(ctx, opVMs, nil, &list); workerKind(err) != kindVMMSDown {
		t.Errorf("worker error %v, want kind %s", err, kindVMMSDown)
	}
}

func TestPwshHang(t *testing.T) {
	e, stderr := newPwshEnv(t, fixturePath(t, "hang"))
	sup := e.svc.srv.sup
	done := make(chan error, 1)
	go func() {
		_, err := e.client.SetStartup(testCtx(t), startupReq("windows-node-1", 10*gib))
		done <- err
	}()
	stderr.wait(t, "hung Set-VMMemory")
	e.clk.waitTimer(t) // the hung call's timer
	changed := sup.Changed()
	e.clk.Advance(DefaultCallTimeout)
	wantCode(t, "hung Set-VMMemory", <-done, codes.DeadlineExceeded)
	<-changed
	skipPause(t, sup, e.clk)
	e.waitWorker()
	if v := e.vm("windows-node-1"); v.GetAssignedBytes() != 8*gib {
		t.Errorf("after the restart: %v", v)
	}
}

func TestPwshProtocolErrors(t *testing.T) {
	e, _ := newPwshEnv(t, fixturePath(t, "host"))
	if err := e.svc.srv.sup.Call(testCtx(t), "bogus", nil, nil); workerKind(err) != kindProtocol {
		t.Errorf("unknown op: %v", err)
	}
	// The worker survives a bad request.
	if v := e.vm("windows-node-1"); v.GetAssignedBytes() != 8*gib {
		t.Errorf("after a bad request: %v", v)
	}
}
