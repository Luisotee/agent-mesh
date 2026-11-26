package daemon

import (
	"context"
	"time"
)

// OnLaunched sets reopen's test seam (nil clears it).
func OnLaunched(f func(agentID string, die func() bool)) { launchedHook = f }

// OnSpawn* and OnStopRunLocked set SpawnAgent's and StopRun's test seams (nil clears them).
func OnSpawnPlanned(f func())         { spawnPlannedHook = f }
func OnSpawnCommitted(f func(string)) { spawnCommittedHook = f }
func OnStopRunLocked(f func())        { stopRunLockedHook = f }

// Reserve and Reserved put and read an agent's memory reservation, as a resume that passed the guard leaves it.
func (d *Daemon) Reserve(agentID string) {
	d.rt.guard.mu.Lock()
	defer d.rt.guard.mu.Unlock()
	if d.rt.guard.reserved == nil {
		d.rt.guard.reserved = map[string]time.Time{}
	}
	d.rt.guard.reserved[agentID] = d.now()
}

func (d *Daemon) Reserved(agentID string) bool {
	d.rt.guard.mu.Lock()
	defer d.rt.guard.mu.Unlock()
	_, ok := d.rt.guard.reserved[agentID]
	return ok
}

// SameRelayURL exposes sameRelayURL.
func SameRelayURL(a, b string) bool { return sameRelayURL(a, b) }

// Accountability runs one pass of the accountability loop (nudges and alerts to parents).
func (d *Daemon) Accountability(ctx context.Context) { d.accountability(ctx) }
