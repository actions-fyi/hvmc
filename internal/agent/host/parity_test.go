package host

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
	"github.com/matfax/hvmc/internal/fake"
	"github.com/matfax/hvmc/internal/policy"
)

// Behaviour parity with the fake host agent (internal/fake), the reference
// for error codes and check order: every case runs against both, each on
// a fresh host, and must end with the same code.

// scenarioFrom builds the fake agent's scenario for a fixture and policy.
func scenarioFrom(f *fixture, pol policy.Policy) *fake.Scenario {
	sc := &fake.Scenario{
		Host: fake.HostSpec{
			Name: f.ComputerName, Total: fake.Bytes(f.TotalBytes), Overhead: fake.Bytes(f.OverheadBytes),
			VMMSDown: f.VMMS != "Running",
		},
		Policy: &fake.PolicySpec{},
	}
	for _, v := range pol.VMs {
		sc.Policy.VMs = append(sc.Policy.VMs, fake.VMPolicySpec{Name: v.Name, Fixed: v.Fixed, Min: fake.Bytes(v.Min), Max: fake.Bytes(v.Max)})
	}
	for _, v := range f.VMs {
		spec := fake.VMSpec{
			Name: v.Name, ID: v.ID, State: fake.HyperVState(v.State),
			Startup: fake.Bytes(v.StartupBytes), Assigned: ptr(fake.Bytes(v.AssignedBytes)),
			StartupDrift: v.StartupDrift,
		}
		if v.ShrinkFloorBytes != nil {
			spec.ShrinkFloor = ptr(fake.Bytes(*v.ShrinkFloorBytes))
		}
		if v.DynamicMemoryEnabled {
			spec.DynamicMemory = &fake.DynamicMemorySpec{
				Minimum: fake.Bytes(v.MinimumBytes), Maximum: fake.Bytes(v.MaximumBytes), Buffer: v.Buffer, Priority: v.Priority,
			}
		}
		sc.VMs = append(sc.VMs, spec)
	}
	return sc
}

func fakeHostClient(t *testing.T, sc *fake.Scenario) agentv1.HostAgentClient {
	t.Helper()
	sim, err := fake.New(sc)
	if err != nil {
		t.Fatal(err)
	}
	srv := sim.ServeHost(fake.Bufconn())
	t.Cleanup(srv.Stop)
	conn, err := srv.Dial()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return agentv1.NewHostAgentClient(conn)
}

// parityPolicy leaves windows-off-1 out, for the not-listed refusals.
func parityPolicy() policy.Policy {
	return policy.Policy{VMs: []policy.VM{
		{Name: "windows-node-1", Min: 4 * gib, Max: 24 * gib},
		{Name: "windows-dm-1", Min: 1 * gib, Max: 16 * gib},
		{Name: "linux-node-1", Fixed: true},
		{Name: "windows-saved-1", Min: 2 * gib, Max: 16 * gib},
	}}
}

func ctxBackground() context.Context { return context.Background() }

type parityCase struct {
	name    string
	fixture string
	policy  *policy.Policy
	call    func(agentv1.HostAgentClient) error
	want    codes.Code
}

func startup(vm string, n int64) func(agentv1.HostAgentClient) error {
	return func(c agentv1.HostAgentClient) error {
		_, err := c.SetStartup(ctxBackground(), &agentv1.SetStartupRequest{VmName: vm, StartupBytes: n})
		return err
	}
}

func limits(vm string, minimum, maximum *int64) func(agentv1.HostAgentClient) error {
	return func(c agentv1.HostAgentClient) error {
		_, err := c.SetDynamicLimits(ctxBackground(), &agentv1.SetDynamicLimitsRequest{VmName: vm, MinimumBytes: minimum, MaximumBytes: maximum})
		return err
	}
}

func parityCases() []parityCase {
	unbounded := policy.Policy{VMs: []policy.VM{{Name: "windows-node-1", Min: gib, Max: policy.Unbounded}}}
	proposed := policy.Policy{VMs: []policy.VM{{Name: "windows-node-1", Min: gib, Max: 8 * gib}}}
	return []parityCase{
		{name: "startup: no name", call: startup("", 8*gib), want: codes.InvalidArgument},
		{name: "startup: misaligned", call: startup("windows-node-1", 3*mib), want: codes.InvalidArgument},
		{name: "startup: not positive", call: startup("windows-node-1", 0), want: codes.InvalidArgument},
		{name: "startup: no such VM", call: startup("nope", 8*gib), want: codes.NotFound},
		{name: "startup: not listed", call: startup("windows-off-1", 8*gib), want: codes.PermissionDenied},
		{name: "startup: fixed VM", call: startup("linux-node-1", 10*gib), want: codes.FailedPrecondition},
		{name: "startup: DM VM", call: startup("windows-dm-1", 4*gib), want: codes.FailedPrecondition},
		{name: "startup: saved VM", call: startup("windows-saved-1", 6*gib), want: codes.FailedPrecondition},
		{name: "startup: saved VM out of bounds", call: startup("windows-saved-1", 30*gib), want: codes.FailedPrecondition},
		{name: "startup: above max", call: startup("windows-node-1", 30*gib), want: codes.PermissionDenied},
		{name: "startup: below min", call: startup("windows-node-1", 2*gib), want: codes.PermissionDenied},
		{name: "startup: grow", call: startup("windows-node-1", 10*gib), want: codes.OK},
		{name: "startup: shrink", call: startup("windows-node-1", 6*gib), want: codes.OK},
		{name: "startup: same value", call: startup("windows-node-1", 8*gib), want: codes.OK},
		{name: "startup: host out of memory", policy: &unbounded, call: startup("windows-node-1", 50*gib), want: codes.ResourceExhausted},
		{name: "startup: refused shrink", fixture: "refused-shrink", call: startup("windows-node-1", 4*gib), want: codes.Aborted},
		{name: "startup: partial shrink", fixture: "partial-shrink", call: startup("windows-node-1", 4*gib), want: codes.Aborted},
		{name: "startup: vmms down", fixture: "vmms-down", call: startup("windows-node-1", 10*gib), want: codes.Unavailable},
		{name: "startup: vmms down, invalid", fixture: "vmms-down", call: startup("windows-node-1", 1), want: codes.InvalidArgument},

		{name: "limits: no name", call: limits("", nil, ptr(8*gib)), want: codes.InvalidArgument},
		{name: "limits: nothing set", call: limits("windows-dm-1", nil, nil), want: codes.InvalidArgument},
		{name: "limits: misaligned", call: limits("windows-dm-1", nil, ptr(gib+1)), want: codes.InvalidArgument},
		{name: "limits: min above max", call: limits("windows-dm-1", ptr(4*gib), ptr(2*gib)), want: codes.InvalidArgument},
		{name: "limits: new min above current max", call: limits("windows-dm-1", ptr(10*gib), nil), want: codes.InvalidArgument},
		{name: "limits: no such VM", call: limits("nope", nil, ptr(8*gib)), want: codes.NotFound},
		{name: "limits: not listed", call: limits("windows-off-1", nil, ptr(8*gib)), want: codes.PermissionDenied},
		{name: "limits: fixed VM", call: limits("linux-node-1", nil, ptr(16*gib)), want: codes.FailedPrecondition},
		{name: "limits: static VM", call: limits("windows-node-1", nil, ptr(16*gib)), want: codes.FailedPrecondition},
		{name: "limits: max lowered", call: limits("windows-dm-1", nil, ptr(4*gib)), want: codes.FailedPrecondition},
		{name: "limits: min raised", call: limits("windows-dm-1", ptr(3*gib), nil), want: codes.FailedPrecondition},
		{name: "limits: max above policy", call: limits("windows-dm-1", nil, ptr(20*gib)), want: codes.PermissionDenied},
		{name: "limits: min below policy", call: limits("windows-dm-1", ptr(512*mib), nil), want: codes.PermissionDenied},
		{name: "limits: direction ranks before bounds", call: limits("windows-dm-1", ptr(512*mib), ptr(4*gib)), want: codes.FailedPrecondition},
		{name: "limits: only the set limit is checked", call: limits("windows-dm-1", nil, ptr(12*gib)), want: codes.OK},
		{name: "limits: both", call: limits("windows-dm-1", ptr(1*gib), ptr(16*gib)), want: codes.OK},
		{name: "limits: vmms down", fixture: "vmms-down", call: limits("windows-dm-1", nil, ptr(12*gib)), want: codes.Unavailable},

		{name: "GetHost: vmms down", fixture: "vmms-down", call: func(c agentv1.HostAgentClient) error {
			_, err := c.GetHost(ctxBackground(), &agentv1.GetHostRequest{})
			return err
		}, want: codes.Unavailable},
		{name: "ListVMs: vmms down", fixture: "vmms-down", call: func(c agentv1.HostAgentClient) error {
			_, err := c.ListVMs(ctxBackground(), &agentv1.ListVMsRequest{})
			return err
		}, want: codes.Unavailable},
		{name: "WatchVMs: vmms down", fixture: "vmms-down", call: func(c agentv1.HostAgentClient) error {
			s, err := c.WatchVMs(ctxBackground(), &agentv1.WatchVMsRequest{})
			if err == nil {
				_, err = s.Recv()
			}
			return err
		}, want: codes.Unavailable},
		{name: "ProposePolicy: no policy", call: func(c agentv1.HostAgentClient) error {
			_, err := c.ProposePolicy(ctxBackground(), &agentv1.ProposePolicyRequest{})
			return err
		}, want: codes.InvalidArgument},
		{name: "ProposePolicy: digest mismatch", call: func(c agentv1.HostAgentClient) error {
			_, err := c.ProposePolicy(ctxBackground(), &agentv1.ProposePolicyRequest{Policy: proposed.Proto(), Digest: "sha256:0"})
			return err
		}, want: codes.InvalidArgument},
		{name: "ProposePolicy", call: func(c agentv1.HostAgentClient) error {
			_, err := c.ProposePolicy(ctxBackground(), &agentv1.ProposePolicyRequest{Policy: proposed.Proto(), Digest: proposed.Digest()})
			return err
		}, want: codes.OK},
	}
}

func TestParityWithFake(t *testing.T) {
	for _, tc := range parityCases() {
		t.Run(tc.name, func(t *testing.T) { runParity(t, tc) })
	}
}

func runParity(t *testing.T, tc parityCase) {
	name := tc.fixture
	if name == "" {
		name = "host"
	}
	pol := parityPolicy()
	if tc.policy != nil {
		pol = *tc.policy
	}
	ref := fakeHostClient(t, scenarioFrom(loadFixture(t, name), pol))
	e := newTestEnv(t, newFakeHost(loadFixture(t, name)), pol, func(o *envOptions) { o.pollInterval = PollInterval })
	if name == "vmms-down" {
		// The poll has seen vmms down before WatchVMs connects.
		poll := e.svc.srv.poll
		waitFor(t, "first poll", func() <-chan struct{} { _, ch, _ := poll.current(); return ch }, func() bool {
			_, _, err := poll.current()
			return workerKind(err) == kindVMMSDown
		})
	}
	refErr, gotErr := tc.call(ref), tc.call(e.client)
	if status.Code(refErr) != tc.want {
		t.Errorf("fake agent: %v, want %s (case is wrong)", refErr, tc.want)
	}
	if status.Code(gotErr) != status.Code(refErr) {
		t.Errorf("host agent: %v; fake agent: %v", gotErr, refErr)
	}
}
