// Package fake provides in-process fake hvmc agents for tests and for the
// coordinator's devFakeAgents mode. CI never has real Hyper-V hosts or guest
// agents; every test above the wire API talks to these fakes through the
// generated gRPC clients.
//
// A Sim simulates one Hyper-V host with its VMs and guest agents, described
// by a Scenario (usually YAML, see ParseScenario). ServeHost and ServeGuest
// serve the agent wire API (HostAgent, GuestAgent, CertificateService and
// grpc.health.v1.Health) on a Bufconn or real listener; Server.Dial returns
// a client connection. The simulation follows the agent contract in
// proto/hvmc/agent/v1 and DESIGN §4: host free memory reacts to resizes,
// writes are checked against the active policy with the error model's
// codes, linked guests see their VM's assignment, WatchVMs and WatchMemory
// push on change. Policies are validated, digested and enforced by
// internal/policy, as on a real host agent, so tests see the canonical
// digests and policy refusals.
//
// Scriptable behaviours (Scenario fields, changeable at runtime through the
// control methods such as Update, SetVMMSDown or SetStale):
//
//   - grow latency (HostSpec.GrowLatency) and the agent's cmdlet timeout;
//   - guest total lagging behind a grow (GuestSpec.TotalLag);
//   - refused and partial shrink (VMSpec.ShrinkFloor) and MemoryStartup
//     drift after a failed shrink (VMSpec.StartupDrift);
//   - vmms down (UNAVAILABLE) and an unreadable guest memory source;
//   - methods hanging until the caller's deadline (AgentBehaviour.Hang);
//   - stale observations with an old observed_at (AgentBehaviour.Stale);
//   - health NOT_SERVING (AgentBehaviour.NotServing);
//   - policy digest mismatch: an active policy different from the
//     coordinator's, and a digest skew that refuses every proposal
//     (HostSpec.PolicyDigestSkew);
//   - certificate rotation, including failing rotations and installs
//     (CertSpec).
//
// All time is read from an injected Clock; tests use ManualClock and move it
// with Advance instead of sleeping.
package fake
