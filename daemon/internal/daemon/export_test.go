package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1/termv1connect"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/proc"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
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

// Release, Refresh, and Seed expose guard methods for tests.
func (d *Daemon) Release(agentID string) { d.rt.guard.release(agentID) }
func (d *Daemon) Refresh(agentID string) { d.rt.guard.refresh(agentID, d.now()) }
func (d *Daemon) Seed(agentID string, startedAt time.Time) {
	d.rt.guard.seed(agentID, startedAt, d.now())
}

// ConfineAgentForTest exposes confineAgent for tests.
func ConfineAgentForTest(d *Daemon, ctx context.Context, pid int, startTicks uint64, name string) {
	d.confineAgent(ctx, pid, startTicks, name)
}

// LeftoverProcessesForTest exposes leftoverProcesses for tests.
func LeftoverProcessesForTest(top string, table []proc.Info) []proc.Info {
	return leftoverProcesses(top, table)
}

// SweepableForTest exposes sweepable for tests.
func SweepableForTest(worktreesRoot string, a *v1.Agent, rootCwds []string) (string, bool) {
	return sweepable(worktreesRoot, a, rootCwds)
}

// OwnAgentForTest exposes ownAgent for tests.
func OwnAgentForTest(p proc.Info, a *v1.Agent) bool {
	return ownAgent(p, a)
}

// AdoptCPUHolderForTest exposes adoptCPUHolder for tests.
func AdoptCPUHolderForTest(d *Daemon, ctx context.Context, a *v1.Agent, pid int, startTicks uint64) {
	d.adoptCPUHolder(ctx, a, pid, startTicks)
}

// SeedModelNotice saves a model check, fresh now, with new families no tier names (what ModelNotice then says).
func (d *Daemon) SeedModelNotice(ctx context.Context, kind string, families ...string) error {
	return d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		return saveModelCheckState(tx, modelCheckState{At: d.now().Unix(), New: map[string][]string{kind: families}})
	})
}

// flakyTerm is the terminal host's client whose ListTerminals fails while fail is set.
type flakyTerm struct {
	termv1connect.TermHostServiceClient
	fail *atomic.Bool
}

func (f *flakyTerm) ListTerminals(ctx context.Context, req *connect.Request[termv1.ListTerminalsRequest]) (*connect.Response[termv1.ListTerminalsResponse], error) {
	if f.fail.Load() {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("host down"))
	}
	return f.TermHostServiceClient.ListTerminals(ctx, req)
}

// FlakyTerminalList makes the daemon's terminal host fail ListTerminals while fail is set. Call it before any agent runs.
func (d *Daemon) FlakyTerminalList(fail *atomic.Bool) {
	d.rt.term.API = &flakyTerm{d.rt.term.API, fail}
}

// ChecksWord exposes checksWord.
func ChecksWord(rollup []map[string]any) string { return checksWord(rollup) }
