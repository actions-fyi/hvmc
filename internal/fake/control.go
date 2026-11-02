package fake

import (
	"errors"
	"fmt"
	"slices"
	"time"

	agentv1 "github.com/matfax/hvmc/internal/agentapi/hvmc/agent/v1"
)

// This file is the runtime control API: Go methods that change or read the
// simulated state while servers run, for fault injection in tests, the
// coordinator's devFakeAgents mode and e2e. Every change is validated as a
// whole; an invalid change leaves the state untouched. Changes wake
// WatchVMs / WatchMemory streams and update health immediately.

// Update applies fn to a copy of the current scenario and makes the result
// the new state if it validates. Effects of the change:
//   - setting an agent's Stale freezes its observations now (changing Stale
//     re-freezes), clearing it thaws them;
//   - a changed VM assignment moves linked guests' totals (grows after
//     TotalLag);
//   - health follows NotServing, VMMSDown, SourceDown and VM state.
//
// fn runs with the Sim locked and must not call methods of the Sim.
func (s *Sim) Update(fn func(*Scenario)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.sc.clone()
	fn(next)
	return s.replaceLocked(next)
}

// Apply replaces the whole state with sc (copied), as if loaded anew, while
// servers and their streams keep running. Key material of agents is kept.
func (s *Sim) Apply(sc *Scenario) error {
	if sc == nil {
		return errors.New("fake: nil scenario")
	}
	return s.Update(func(cur *Scenario) { *cur = *sc.clone() })
}

// Scenario returns a copy of the current state, including the effects of
// RPCs (resizes, guest totals, pending policy, failure counters left).
func (s *Sim) Scenario() *Scenario {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sc.clone()
}

// UpdateHost changes the host spec (memory, behaviours).
func (s *Sim) UpdateHost(fn func(*HostSpec)) error {
	return s.Update(func(sc *Scenario) { fn(&sc.Host) })
}

// UpdateVM changes one VM (state, assignment, shrink floor, ...).
func (s *Sim) UpdateVM(name string, fn func(*VMSpec)) error {
	var found bool
	err := s.Update(func(sc *Scenario) {
		if v := sc.vm(name); v != nil {
			found = true
			fn(v)
		}
	})
	if !found {
		return fmt.Errorf("fake: no VM named %q", name)
	}
	return err
}

// UpdateGuest changes one guest (used memory, lag, behaviours, ...).
func (s *Sim) UpdateGuest(name string, fn func(*GuestSpec)) error {
	var found bool
	err := s.Update(func(sc *Scenario) {
		if g := sc.guest(name); g != nil {
			found = true
			fn(g)
		}
	})
	if !found {
		return errNoAgent(name)
	}
	return err
}

// updateAgent changes the behaviours of the host or a guest agent.
func (s *Sim) updateAgent(agent string, fn func(*AgentBehaviour)) error {
	var found bool
	err := s.Update(func(sc *Scenario) {
		switch {
		case agent == sc.Host.Name:
			found = true
			fn(&sc.Host.AgentBehaviour)
		case sc.guest(agent) != nil:
			found = true
			fn(&sc.guest(agent).AgentBehaviour)
		}
	})
	if !found {
		return errNoAgent(agent)
	}
	return err
}

// SetVMMSDown simulates the Hyper-V management service going down or
// coming back.
func (s *Sim) SetVMMSDown(down bool) error {
	return s.UpdateHost(func(h *HostSpec) { h.VMMSDown = down })
}

// SetNotServing makes an agent's health report NOT_SERVING (true) or follow
// its other behaviours again (false).
func (s *Sim) SetNotServing(agent string, notServing bool) error {
	return s.updateAgent(agent, func(b *AgentBehaviour) { b.NotServing = notServing })
}

// SetHang makes the listed methods of an agent ("*" for all) hang until the
// caller's deadline; no methods clears it.
func (s *Sim) SetHang(agent string, methods ...string) error {
	return s.updateAgent(agent, func(b *AgentBehaviour) { b.Hang = slices.Clone(methods) })
}

// SetStale freezes an agent's observations now, backdated by age.
func (s *Sim) SetStale(agent string, age time.Duration) error {
	return s.updateAgent(agent, func(b *AgentBehaviour) { b.Stale = &StaleSpec{Age: Duration(age)} })
}

// ClearStale lets an agent observe fresh values again.
func (s *Sim) ClearStale(agent string) error {
	return s.updateAgent(agent, func(b *AgentBehaviour) { b.Stale = nil })
}

// ActivatePendingPolicy makes the pending policy active, as
// `hvmc agent policy sync` does on the host.
func (s *Sim) ActivatePendingPolicy() error {
	var none bool
	err := s.Update(func(sc *Scenario) {
		if sc.PendingPolicy == nil {
			none = true
			return
		}
		sc.Policy, sc.PendingPolicy = sc.PendingPolicy, nil
	})
	if none {
		return errors.New("fake: no pending policy")
	}
	return err
}

// PolicyDigests returns the digests the host agent currently reports for
// its active and pending policy.
func (s *Sim) PolicyDigests() (active, pending string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.liveHostLocked(s.clock.Now())
	return h.GetPolicyDigest(), h.GetPendingPolicyDigest()
}

// Host returns the live host observation (ignoring Stale and VMMSDown).
func (s *Sim) Host() *agentv1.Host {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.liveHostLocked(s.clock.Now())
}

// VM returns the live state of one VM (ignoring Stale and VMMSDown).
func (s *Sim) VM(name string) (*agentv1.VM, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.sc.vm(name)
	if v == nil {
		return nil, false
	}
	return vmMessage(v, s.clock.Now()), true
}

// GuestMemory returns the live reading of one guest (ignoring Stale and
// reachability).
func (s *Sim) GuestMemory(name string) (*agentv1.GuestMemory, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.sc.guest(name)
	if g == nil {
		return nil, false
	}
	return s.liveMemoryLocked(g, s.clock.Now()), true
}

// CallCount returns how often method (e.g. "ProposePolicy") was called on
// the named agent, including failed calls.
func (s *Sim) CallCount(agent, method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[agent+"/"+method]
}
