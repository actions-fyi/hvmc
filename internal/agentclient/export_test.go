package agentclient

// Tracked is a tracker's record of one agent, for tests that feed it the
// results of calls that were in flight when the agent was removed.
type Tracked = tracked

// TrackedForTest returns the tracker's current record of agent.
func TrackedForTest(t *Tracker, agent string) *Tracked {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.agents[agent]
}

// RecordCheckForTest records a health check result on a, as pollAgent
// does when the check returns.
func RecordCheckForTest(t *Tracker, a *Tracked, err error) {
	t.recordCheck(a, a.ep, HealthUnknown, err)
}

// ObserveGuestForTest records a guest reading on a, as a GetMemory or
// WatchMemory result does.
func ObserveGuestForTest(t *Tracker, a *Tracked, m GuestMemory) { t.observeGuest(a, a.ep, m) }

// ObserveHostForTest records host data and the VM list on a, as GetHost
// and ListVMs results do.
func ObserveHostForTest(t *Tracker, a *Tracked, info HostInfo, vms []VMInfo) {
	t.observeHost(a, info)
	t.observeVMs(a, vmsObservation(vms), vms)
}
