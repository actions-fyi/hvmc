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
// does when the check returns: Serving without err.
func RecordCheckForTest(t *Tracker, a *Tracked, err error) {
	st := HealthServing
	if err != nil {
		st = HealthUnknown
	}
	t.recordCheck(a, a.ep, st, err)
}

// ObserveGuestForTest records a guest reading on a, as a GetMemory or
// WatchMemory result does.
func ObserveGuestForTest(t *Tracker, a *Tracked, m GuestMemory) { t.observeGuest(a, a.ep, m) }

// ObserveHostForTest records host data and the VM list on a, as GetHost
// and ListVMs results do.
func ObserveHostForTest(t *Tracker, a *Tracked, info HostInfo, vms []VMInfo) {
	t.observeHost(a, info)
	t.observeVMs(a, vms)
}
