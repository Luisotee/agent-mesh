package daemon

// OnLaunched sets reopen's test seam (nil clears it).
func OnLaunched(f func(agentID string, die func() bool)) { launchedHook = f }

// OnSpawn* and OnStopRunLocked set SpawnAgent's and StopRun's test seams (nil clears them).
func OnSpawnPlanned(f func())         { spawnPlannedHook = f }
func OnSpawnCommitted(f func(string)) { spawnCommittedHook = f }
func OnStopRunLocked(f func())        { stopRunLockedHook = f }
