package host

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/matfax/hvmc/internal/agent"
	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
)

// PollInterval is the period of the local Hyper-V poll behind WatchVMs
// and the readiness checks "vmms" and "hyperv-read" (DESIGN §4.2, §4.8).
const PollInterval = 2 * time.Second

// errNotPolled is the state before the first poll.
var errNotPolled = errors.New("Hyper-V not read yet")

// poller reads every VM every PollInterval, and right after each worker
// start, and publishes a snapshot whenever a VM changed (observed_at
// alone does not count) or the read error changes.
type poller struct {
	hv       *hyperV
	sup      *Supervisor
	clk      agent.Clock
	interval time.Duration // 0 runs no poll loop (tests)

	mu      sync.Mutex
	snap    *agentv1.VMSnapshot
	err     error     // last poll's error; nil after a success
	lastOK  time.Time // last successful read
	vmmsErr error     // nil while vmms is known to run
	changed chan struct{}
}

func newPoller(hv *hyperV, sup *Supervisor, clk agent.Clock) *poller {
	return &poller{
		hv: hv, sup: sup, clk: clk, interval: PollInterval,
		err: errNotPolled, vmmsErr: errNotPolled, changed: make(chan struct{}),
	}
}

// run polls until ctx ends.
func (p *poller) run(ctx context.Context) {
	for {
		before := p.sup.worker()
		polled := before
		if p.poll(ctx) {
			// Read through the worker that is up now; a restart right
			// after the read is picked up by the next interval.
			polled = p.sup.worker()
		}
		if !p.waitNext(ctx, polled) {
			return
		}
	}
}

// waitNext waits for the next poll: the interval, or a worker other than w
// coming up (a fresh snapshot right after a restart). Other supervisor state
// changes do not re-poll, so the poll timer, once armed, stays armed until
// it fires. It reports false when ctx ended.
func (p *poller) waitNext(ctx context.Context, w *worker) bool {
	t := p.clk.NewTimer(p.interval)
	defer t.Stop()
	for {
		changed := p.sup.Changed()
		if cur := p.sup.worker(); cur != nil && cur != w {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-t.C():
			return true
		case <-changed:
		}
	}
}

// poll reads every VM once and records the result; it reports whether
// the read succeeded.
func (p *poller) poll(ctx context.Context) bool {
	list, at, err := p.hv.vms(ctx)
	if ctx.Err() != nil {
		return false
	}
	var snap *agentv1.VMSnapshot
	if err == nil {
		snap = snapshotMessage(list, at)
	}
	p.record(snap, err)
	return err == nil
}

// record stores the result of a VM read (from the poll or ListVMs).
func (p *poller) record(snap *agentv1.VMSnapshot, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil && p.snap == nil && errors.Is(p.err, errNotPolled) && isSupervisorErr(err) {
		// The first worker is not up yet: watchers keep waiting for the
		// first read instead of ending.
		return
	}
	switch {
	case err == nil:
		p.vmmsErr = nil
		p.lastOK = snap.GetObservedAt().AsTime()
	case workerKind(err) == kindVMMSDown:
		p.vmmsErr = fmt.Errorf("vmms is not running: %w", err)
	}
	notify := (err == nil) != (p.err == nil) || (err != nil && err.Error() != p.err.Error())
	if err == nil {
		notify = notify || p.snap == nil || !sameVMs(p.snap, snap)
		p.snap = snap
	}
	p.err = err
	if notify {
		close(p.changed)
		p.changed = make(chan struct{})
	}
}

// current returns the latest snapshot, a channel closed at the next
// published change and the last poll's error.
func (p *poller) current() (*agentv1.VMSnapshot, <-chan struct{}, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snap, p.changed, p.err
}

// checkVMMS is the readiness check "vmms" (vmms service running).
func (p *poller) checkVMMS(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.vmmsErr
}

// checkRead is the readiness check "hyperv-read": last successful Get-VM
// within 2× the poll interval.
func (p *poller) checkRead(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastOK.IsZero() {
		if p.err != nil && !errors.Is(p.err, errNotPolled) {
			return fmt.Errorf("no successful Get-VM yet: %w", p.err)
		}
		return errors.New("no successful Get-VM yet")
	}
	if age := p.clk.Now().Sub(p.lastOK); age > 2*p.interval {
		return fmt.Errorf("last successful Get-VM %s ago (limit %s): %w", age, 2*p.interval, p.errOrNone())
	}
	return nil
}

func (p *poller) errOrNone() error {
	if p.err == nil {
		return errors.New("no error recorded")
	}
	return p.err
}

// sameVMs compares two snapshots ignoring observation times.
func sameVMs(a, b *agentv1.VMSnapshot) bool {
	strip := func(s *agentv1.VMSnapshot) *agentv1.VMSnapshot {
		c := proto.CloneOf(s)
		c.ObservedAt = nil
		for _, v := range c.GetVms() {
			v.ObservedAt = nil
		}
		return c
	}
	return proto.Equal(strip(a), strip(b))
}

// WatchVMs implements agentv1.HostAgentServer: the current snapshot on
// connect, then one whenever a VM changes. The stream ends with
// UNAVAILABLE when reading Hyper-V fails (vmms down, worker restarting),
// and when the call's context ends (drain).
func (s *server) WatchVMs(_ *agentv1.WatchVMsRequest, stream grpc.ServerStreamingServer[agentv1.WatchVMsResponse]) error {
	ctx := stream.Context()
	var last *agentv1.VMSnapshot
	for {
		snap, changed, err := s.poll.current()
		switch {
		case err != nil && !errors.Is(err, errNotPolled):
			return status.Errorf(codes.Unavailable, "lost Hyper-V: %v", status.Convert(toStatus(err)).Message())
		case snap != nil && err == nil && (last == nil || !sameVMs(last, snap)):
			if err := stream.Send(&agentv1.WatchVMsResponse{Snapshot: snap}); err != nil {
				return err
			}
			last = snap
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		}
	}
}
