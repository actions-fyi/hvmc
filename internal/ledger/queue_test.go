package ledger

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

func pendingOrder(l *Ledger) []RequestID {
	var out []RequestID
	for _, lv := range l.Snapshot().Leases {
		if lv.Phase == PhasePending {
			out = append(out, lv.Request.ID)
		}
	}
	return out
}

func TestQueueOrder(t *testing.T) {
	for _, tc := range []struct {
		name string
		poq  bool
		want []RequestID
	}{
		{"reconciler at +inf", true, []RequestID{"rec", "hi", "lo-1", "lo-2", "lo-3"}},
		{"reconciler at its class", false, []RequestID{"hi", "lo-1", "lo-2", "rec", "lo-3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := standard(t)
			f.cfg.PriorityOverQueue = tc.poq
			f.l.Configure(f.cfg)
			// Same instant: arrival order breaks ties.
			f.enqueue(Request{ID: "lo-2", Kind: KindExternal, VM: "vm-a", Bytes: 30 * Gi})
			f.enqueue(Request{ID: "lo-1", Kind: KindExternal, VM: "vm-a", Bytes: Gi, EnqueuedAt: t0.Add(-time.Minute)})
			f.clk.Advance(time.Second)
			f.enqueue(Request{ID: "rec", Kind: KindReconciler, VM: "vm-b", Bytes: Gi})
			f.enqueue(Request{ID: "lo-3", Kind: KindExternal, VM: "vm-b", Bytes: Gi})
			f.enqueue(Request{ID: "hi", Kind: KindExternal, VM: "vm-b", Bytes: Gi, PriorityClass: 5})
			if d := cmp.Diff(tc.want, pendingOrder(f.l)); d != "" {
				t.Errorf("queue order (-want +got):\n%s", d)
			}
			for i, id := range tc.want {
				if lv, _ := f.l.Lease(id); lv.Position != Position(i+1) {
					t.Errorf("%s: position %d, want %d", id, lv.Position, i+1)
				}
			}
		})
	}
}

// TestQueueRule covers §3.4: requests are evaluated in queue order; a waiting
// request that could use the pool (H) lets later requests fit only with
// delta == 0 and no candidate VM in common with it; a waiting request whose
// candidates are all fixed only keeps later requests off those VMs.
func TestQueueRule(t *testing.T) {
	type want struct {
		vm     VMName
		fits   bool
		delta  int64
		reason Reason
	}
	for _, tc := range []struct {
		name  string
		ahead []Request
		req   Request
		want  []want
	}{
		{
			name: "alone, needs resize from pool",
			req:  ext("r", "vm-a", 5*Gi),
			want: []want{{"vm-a", true, 3 * Gi, ""}},
		},
		{
			name:  "behind pool-short head: pool-drawing request waits",
			ahead: []Request{ext("h", "vm-a", 20*Gi)},
			req:   ext("r", "vm-b", 3*Gi),
			want:  []want{{"vm-b", false, 1 * Gi, ReasonQueued}},
		},
		{
			name:  "behind head: delta 0 on a different VM is granted",
			ahead: []Request{ext("h", "vm-a", 20*Gi)},
			req:   ext("r", "vm-b", 1*Gi),
			want:  []want{{"vm-b", true, 0, ""}},
		},
		{
			name:  "behind head: delta 0 on the head's VM waits",
			ahead: []Request{ext("h", "vm-a", 20*Gi)},
			req:   ext("r", "vm-a", 1*Gi),
			want:  []want{{"vm-a", false, 0, ReasonQueued}},
		},
		{
			name:  "behind head pod: overlapping candidate sets wait on every VM",
			ahead: []Request{pod("h", 20*Gi, "vm-a", "vm-b")},
			req:   pod("r", 1*Gi, "vm-b", "linux-node-1"),
			want:  []want{{"vm-b", false, 0, ReasonQueued}, {"linux-node-1", false, 0, ReasonQueued}},
		},
		{
			name:  "behind head pod: disjoint candidates with delta 0 pass",
			ahead: []Request{pod("h", 20*Gi, "vm-a")},
			req:   pod("r", 1*Gi, "vm-b", "linux-node-1"),
			want:  []want{{"vm-b", true, 0, ""}, {"linux-node-1", true, 0, ""}},
		},
		{
			name:  "behind fixed-only head: pool-drawing request on another VM is granted",
			ahead: []Request{pod("h", 20*Gi, "linux-node-1")},
			req:   ext("r", "vm-a", 5*Gi),
			want:  []want{{"vm-a", true, 3 * Gi, ""}},
		},
		{
			name:  "behind fixed-only head: request on its VM waits",
			ahead: []Request{pod("h", 20*Gi, "linux-node-1")},
			req:   pod("r", 1*Gi, "linux-node-1"),
			want:  []want{{"linux-node-1", false, 0, ReasonQueued}},
		},
		{
			name:  "behind fixed-only head: a pod may use its other candidates",
			ahead: []Request{pod("h", 20*Gi, "linux-node-1")},
			req:   pod("r", 3*Gi, "vm-b", "linux-node-1"),
			want:  []want{{"vm-b", true, 1 * Gi, ""}, {"linux-node-1", false, 0, ReasonQueued}},
		},
		{
			name:  "fixed-only head then a waiting pool user: pool draws wait again",
			ahead: []Request{pod("h", 20*Gi, "linux-node-1"), ext("h2", "vm-a", 20*Gi)},
			req:   ext("r", "vm-b", 3*Gi),
			want:  []want{{"vm-b", false, 1 * Gi, ReasonQueued}},
		},
		{
			name:  "waiting requests behind the head lock their candidates too",
			ahead: []Request{ext("h", "vm-a", 20*Gi), ext("q", "vm-b", 3*Gi)},
			req:   ext("r", "vm-b", 1*Gi),
			want:  []want{{"vm-b", false, 0, ReasonQueued}},
		},
		{
			name:  "earlier fitting request uses up the pool",
			ahead: []Request{ext("a", "vm-a", 7*Gi)}, // delta 5Gi, pool left 3Gi
			req:   ext("r", "vm-b", 6*Gi),            // delta 4Gi
			want:  []want{{"vm-b", false, 4 * Gi, ReasonPoolShort}},
		},
		{
			name:  "earlier fitting request uses up the VM's headroom",
			ahead: []Request{ext("a", "vm-a", 2*Gi)},
			req:   ext("r", "vm-a", 1*Gi),
			want:  []want{{"vm-a", true, 1 * Gi, ""}},
		},
		{
			name:  "pod without candidates is skipped, not a head",
			ahead: []Request{pod("p", 20*Gi)},
			req:   ext("r", "vm-a", 5*Gi),
			want:  []want{{"vm-a", true, 3 * Gi, ""}},
		},
		{
			name:  "earlier pod is assumed on its best VM (delta 0, most headroom)",
			ahead: []Request{pod("p", 2*Gi, "vm-a", "linux-node-1")},
			req:   ext("r", "vm-a", 2*Gi),
			want:  []want{{"vm-a", true, 0, ""}},
		},
		{
			name:  "earlier pod needing resize everywhere takes the smallest delta",
			ahead: []Request{pod("p", 5*Gi, "vm-a", "vm-dm")}, // vm-a delta 3Gi, vm-dm 5Gi
			req:   ext("r", "vm-b", 8*Gi),                     // delta 6Gi > 8−3
			want:  []want{{"vm-b", false, 6 * Gi, ReasonPoolShort}},
		},
		{
			name: "fixed VM fits only from headroom",
			req:  pod("r", 10*Gi, "linux-node-1"),
			want: []want{{"linux-node-1", false, 1 * Gi, ReasonNoHeadroom}},
		},
		{
			name: "fixed VM headroom exactly enough",
			req:  pod("r", 9*Gi, "linux-node-1"),
			want: []want{{"linux-node-1", true, 0, ""}},
		},
		{
			name: "policy max bounds the grow",
			req:  ext("r", "vm-b", 19*Gi), // delta 17Gi → 25Gi > 24Gi
			want: []want{{"vm-b", false, 17 * Gi, ReasonExceedsMax}},
		},
		{
			name: "probe always grows at least one step",
			req:  Request{ID: "r", Kind: KindProbe, VM: "vm-a", Bytes: 256 * Mi},
			want: []want{{"vm-a", true, 256 * Mi, ""}},
		},
		{
			name: "unknown and non-candidate VMs",
			req:  ext("r", "vm-a", 1*Gi),
			want: []want{{"vm-a", true, 0, ""}, {"vm-b", false, 0, ReasonNotCandidate}, {"nope", false, 0, ReasonUnknownVM}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := standard(t)
			for _, r := range tc.ahead {
				f.enqueue(r)
			}
			f.clk.Advance(time.Millisecond)
			f.enqueue(tc.req)
			vms := make([]VMName, len(tc.want))
			for i, w := range tc.want {
				vms[i] = w.vm
			}
			got := f.l.Evaluate(tc.req.ID, vms)
			for i, w := range tc.want {
				g := got[i]
				if g.VM != w.vm || g.Fits != w.fits || g.Delta != w.delta || g.Reason != w.reason {
					t.Errorf("fit[%d] = %+v, want %+v", i, g, w)
				}
				if g.Position != Position(len(tc.ahead)+1) {
					t.Errorf("fit[%d] position %d", i, g.Position)
				}
			}
			// Reserve agrees with Evaluate.
			res, err := f.l.Reserve(tc.req.ID, tc.want[0].vm)
			if tc.want[0].fits {
				if err != nil || res.Delta != tc.want[0].delta {
					t.Errorf("Reserve = %+v, %v", res, err)
				}
			} else if reasonOf(err) != tc.want[0].reason {
				t.Errorf("Reserve error %v, want reason %s", err, tc.want[0].reason)
			}
		})
	}
}

func TestEvaluateGating(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mod    func(f *fixture)
		req    Request
		fits   bool
		reason Reason
		is     error
	}{
		{
			name: "host agent not ready freezes",
			mod:  func(f *fixture) { f.l.SetAgentState(DefaultHostAgent, AgentUnreachable) },
			req:  ext("r", "vm-a", 1*Gi), reason: ReasonHostFrozen, is: ErrFrozen,
		},
		{
			name: "guest agent not ready: delta 0 still allowed",
			mod:  func(f *fixture) { f.l.SetAgentState("vm-a", AgentDegraded) },
			req:  ext("r", "vm-a", 1*Gi), fits: true,
		},
		{
			name: "guest agent not ready: resize held",
			mod:  func(f *fixture) { f.l.SetAgentState("vm-a", AgentDegraded) },
			req:  ext("r", "vm-a", 3*Gi), reason: ReasonVMUnknown, is: ErrVMUnknown,
		},
		{
			name: "stale guest data: resize held",
			mod: func(f *fixture) {
				f.clk.Advance(31 * time.Second)
				f.host(12 * Gi)
				f.vm("vm-a", 8*Gi, -1)
			},
			req: ext("r", "vm-a", 3*Gi), reason: ReasonVMUnknown, is: ErrVMUnknown,
		},
		{
			name: "missing host data for the VM: nothing granted",
			mod: func(f *fixture) {
				f.clk.Advance(31 * time.Second)
				f.host(12 * Gi)
			},
			req: ext("r", "vm-a", 1*Gi), reason: ReasonVMDataMissing, is: ErrVMUnknown,
		},
		{
			name: "pool short",
			req:  ext("r", "vm-a", 11*Gi), reason: ReasonPoolShort, is: ErrDoesNotFit,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := standard(t)
			if tc.mod != nil {
				tc.mod(f)
			}
			f.enqueue(tc.req)
			got := f.l.Evaluate(tc.req.ID, nil)
			if len(got) != 1 || got[0].Fits != tc.fits || got[0].Reason != tc.reason {
				t.Fatalf("Evaluate = %+v", got)
			}
			_, err := f.l.Reserve(tc.req.ID, tc.req.VM)
			if tc.fits != (err == nil) || (tc.is != nil && !errors.Is(err, tc.is)) {
				t.Errorf("Reserve error %v, want %v", err, tc.is)
			}
		})
	}
}

func TestEvaluateNonPending(t *testing.T) {
	f := standard(t)
	f.enqueue(ext("r", "vm-a", 1*Gi))
	f.grant("r", "vm-a")
	if got := f.l.Evaluate("r", nil); len(got) != 1 || got[0].Reason != ReasonNotPending {
		t.Errorf("granted: %+v", got)
	}
	if got := f.l.Evaluate("nope", []VMName{"vm-a"}); len(got) != 1 || got[0].Reason != ReasonUnknownRequest {
		t.Errorf("unknown: %+v", got)
	}
	if got := f.l.Evaluate("nope", nil); len(got) != 0 {
		t.Errorf("unknown without candidates: %+v", got)
	}
}

func TestVMBusy(t *testing.T) {
	f := standard(t)
	f.enqueue(ext("a", "vm-a", 3*Gi))
	f.enqueue(ext("b", "vm-a", 3*Gi))
	f.enqueue(ext("c", "vm-a", 256*Mi))
	f.reserve("a", "vm-a") // delta 1Gi, in flight
	if got := f.l.Evaluate("b", nil); got[0].Reason != ReasonVMBusy {
		t.Errorf("second resize while one is in flight: %+v", got)
	}
	// c needs no resize only if headroom is left: a took all of it, so c is
	// behind b (the head) on the same VM.
	if got := f.l.Evaluate("c", nil); got[0].Reason != ReasonQueued {
		t.Errorf("c: %+v", got)
	}
}

func TestDispatchable(t *testing.T) {
	f := standard(t)
	f.enqueue(pod("p1", 1*Gi, "vm-a"))
	f.enqueue(ext("e1", "vm-b", 1*Gi))
	f.enqueue(Request{ID: "r1", Kind: KindReconciler, VM: "vm-a", Bytes: 5 * Gi}) // +∞: first
	f.enqueue(ext("e2", "vm-b", 30*Gi))                                           // head, pool short
	f.enqueue(ext("e3", "linux-node-1", 256*Mi))                                  // behind head, delta 0, other VM
	f.enqueue(ext("e4", "vm-b", 256*Mi))                                          // behind head, same VM
	if d := cmp.Diff([]RequestID{"r1", "e1", "e3"}, f.l.Dispatchable()); d != "" {
		t.Errorf("Dispatchable (-want +got):\n%s", d)
	}
	f.l.SetAgentState(DefaultHostAgent, AgentDegraded)
	if got := f.l.Dispatchable(); got != nil {
		t.Errorf("frozen: %v", got)
	}
}

func TestEnqueueAndCandidates(t *testing.T) {
	f := standard(t)
	for _, tc := range []struct {
		r  Request
		is error
	}{
		{Request{Kind: KindPod, Bytes: 1}, ErrInvalidRequest},
		{Request{ID: "x", Kind: "Other", Bytes: 1}, ErrInvalidRequest},
		{Request{ID: "x", Kind: KindPod}, ErrInvalidRequest},
		{Request{ID: "x", Kind: KindExternal, Bytes: 1}, ErrInvalidRequest},
		{Request{ID: "x", Kind: KindExternal, VM: "nope", Bytes: 1}, ErrUnknownVM},
	} {
		if _, err := f.l.Enqueue(tc.r); !errors.Is(err, tc.is) {
			t.Errorf("Enqueue(%+v) = %v, want %v", tc.r, err, tc.is)
		}
	}
	pos := f.enqueue(Request{ID: "e", Kind: KindExternal, VM: "vm-a", Bytes: Gi, Candidates: []VMName{"vm-b"}})
	if pos != 1 {
		t.Errorf("position %d", pos)
	}
	if lv, _ := f.l.Lease("e"); lv.Request.Candidates != nil || !lv.Request.EnqueuedAt.Equal(t0) {
		t.Errorf("external request kept candidates or lost enqueuedAt: %+v", lv.Request)
	}
	if pos := f.enqueue(pod("p", Gi)); pos != 2 {
		t.Errorf("pod position %d", pos)
	}
	if pos := f.enqueue(pod("p", 99*Gi, "vm-b")); pos != 2 {
		t.Errorf("idempotent enqueue position %d", pos)
	}
	if lv, _ := f.l.Lease("p"); lv.Request.Bytes != Gi || lv.Grantable {
		t.Errorf("idempotent enqueue changed the request: %+v", lv)
	}
	if err := f.l.SetCandidates("p", []VMName{"vm-b"}); err != nil {
		t.Fatal(err)
	}
	if lv, _ := f.l.Lease("p"); !slices.Equal(lv.Request.Candidates, []VMName{"vm-b"}) || !lv.Grantable {
		t.Errorf("candidates not set: %+v", lv)
	}
	if err := f.l.SetCandidates("e", nil); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("SetCandidates on external: %v", err)
	}
	if err := f.l.SetCandidates("nope", nil); !errors.Is(err, ErrUnknownRequest) {
		t.Errorf("SetCandidates unknown: %v", err)
	}
	f.grant("p", "vm-b")
	if pos := f.enqueue(pod("p", Gi)); pos != 0 {
		t.Errorf("enqueue of granted request: position %d", pos)
	}
	if err := f.l.SetCandidates("p", nil); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("SetCandidates after grant: %v", err)
	}
}

func TestPodPinnedAfterRollback(t *testing.T) {
	f := standard(t)
	f.enqueue(pod("p", 3*Gi, "vm-a", "vm-b"))
	if _, err := f.l.Reserve("p", "linux-node-1"); reasonOf(err) != ReasonNotCandidate {
		t.Errorf("reserve on non-candidate: %v", err)
	}
	res := f.reserve("p", "vm-b")
	f.l.Rollback(res, ReasonResizeFailed, true)
	lv, _ := f.l.Lease("p")
	if lv.Phase != PhasePending || lv.Request.VM != "vm-b" || lv.Position != 1 {
		t.Fatalf("after rollback: %+v", lv)
	}
	got := f.l.Evaluate("p", []VMName{"vm-a", "vm-b"})
	if got[0].Reason != ReasonNotCandidate || !got[1].Fits {
		t.Errorf("pinned pod evaluated elsewhere: %+v", got)
	}
}
