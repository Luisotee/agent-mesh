package daemon

// OnLaunched sets reopen's test seam (nil clears it).
func OnLaunched(f func(agentID string, die func() bool)) { launchedHook = f }
