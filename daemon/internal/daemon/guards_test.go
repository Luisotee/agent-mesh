package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/proc"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
	"github.com/Luisotee/agent-mesh/daemon/internal/termclient"
	"github.com/Luisotee/agent-mesh/daemon/internal/testutil/shortdir"
)

type fakeSys struct {
	mu          sync.Mutex
	table       []proc.Info
	tables      [][]proc.Info
	tableReads  int
	tableErr    error
	machineCPUs []int
	taskIDs     func(pid int) []int
	affinities  map[int][]int
	setErr      error
	pins        []pinCall
	signals     []sigCall
	alive       map[pidKey]bool
	scopes      [][]string
	scopeErr    error
}

type pidKey struct {
	pid   int
	start uint64
}

type pinCall struct {
	tid  int
	cpus []int
}

type sigCall struct {
	pid   int
	start uint64
	sig   syscall.Signal
}

func newFakeSys() *fakeSys {
	return &fakeSys{
		machineCPUs: []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11},
		affinities:  map[int][]int{},
		alive:       map[pidKey]bool{},
	}
}

func (f *fakeSys) ops() *daemon.SystemOps {
	return &daemon.SystemOps{
		ProcTable: func() ([]proc.Info, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.tableReads++
			if f.tableErr != nil {
				return nil, f.tableErr
			}
			if len(f.tables) > 0 {
				t := f.tables[0]
				if len(f.tables) > 1 {
					f.tables = f.tables[1:]
				}
				return append([]proc.Info(nil), t...), nil
			}
			return append([]proc.Info(nil), f.table...), nil
		},
		TaskIDs: func(pid int) []int {
			f.mu.Lock()
			fn := f.taskIDs
			f.mu.Unlock()
			if fn != nil {
				return fn(pid)
			}
			return []int{pid}
		},
		MachineCPUs: func() []int {
			f.mu.Lock()
			defer f.mu.Unlock()
			return append([]int(nil), f.machineCPUs...)
		},
		GetAffinity: func(pid int) ([]int, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if c, ok := f.affinities[pid]; ok {
				return append([]int(nil), c...), nil
			}
			return append([]int(nil), f.machineCPUs...), nil
		},
		SetAffinity: func(tid int, cpus []int) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.setErr != nil {
				return f.setErr
			}
			f.pins = append(f.pins, pinCall{tid: tid, cpus: append([]int(nil), cpus...)})
			return nil
		},
		SignalProc: func(pid int, startTicks uint64, sig syscall.Signal) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.signals = append(f.signals, sigCall{pid: pid, start: startTicks, sig: sig})
			return nil
		},
		ProcAlive: func(pid int, startTicks uint64) bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.alive[pidKey{pid: pid, start: startTicks}]
		},
		RunScope: func(_ context.Context, argv []string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.scopes = append(f.scopes, append([]string(nil), argv...))
			return f.scopeErr
		},
		Sleep: func(time.Duration) {},
	}
}

func (f *fakeSys) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pins = nil
	f.signals = nil
	f.scopes = nil
	f.tableReads = 0
}

func TestCPUWindowRotationLeastHeldAndReusedPID(t *testing.T) {
	fs := newFakeSys()
	sys := fs.ops()
	w := startReopen(t, func(c *daemon.AgentConfig) { c.Sys = sys })
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	repo := gitRepo(t)

	spawnAndRecord := func(slug string) (*v1.Agent, []int) {
		fs.resetCalls()
		a := w.spawn(run.Id, repo, slug, "")
		w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
		terms := w.terminals(t)
		var term *termv1.Terminal
		for _, tm := range terms {
			if tm.Id == a.TerminalId {
				term = tm
			}
		}
		if term == nil {
			t.Fatalf("no terminal for %s", a.Name)
		}
		fs.mu.Lock()
		fs.alive[pidKey{pid: int(term.Pid), start: term.PidStartTicks}] = true
		pins := append([]pinCall(nil), fs.pins...)
		fs.mu.Unlock()
		if len(pins) == 0 {
			t.Fatalf("%s was not pinned", slug)
		}
		return a, pins[0].cpus
	}

	// Four spawns take consecutive quarters of 12 cores: 0-2, 3-5, 6-8, 9-11.
	_, c1 := spawnAndRecord("one")
	a2, c2 := spawnAndRecord("two")
	_, c3 := spawnAndRecord("three")
	_, c4 := spawnAndRecord("four")
	if got := fmt.Sprint([][]int{c1, c2, c3, c4}); got != "[[0 1 2] [3 4 5] [6 7 8] [9 10 11]]" {
		t.Fatalf("initial 4 windows = %s", got)
	}

	// Retire a2 (or simulate its PID gone / reused with a different start time): cores 3-5 become least-held.
	terms := w.terminals(t)
	for _, tm := range terms {
		if tm.Id == a2.TerminalId {
			fs.mu.Lock()
			delete(fs.alive, pidKey{pid: int(tm.Pid), start: tm.PidStartTicks})
			fs.mu.Unlock()
		}
	}
	if _, err := w.device.Mesh.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: a2.Id})); err != nil {
		t.Fatal(err)
	}
	_, c5 := spawnAndRecord("five")
	if !slices.Equal(c5, []int{3, 4, 5}) {
		t.Errorf("after retire/reused-pid of 3-5 holder, picked %v, want [3 4 5]", c5)
	}
	// Now every window is held once: ties break forward from where the last ended (6-8).
	_, c6 := spawnAndRecord("six")
	if !slices.Equal(c6, []int{6, 7, 8}) {
		t.Errorf("forward tie-break picked %v, want [6 7 8]", c6)
	}
}

func TestConfinementPinsDescendantsAndThreadsAndSkipsRoots(t *testing.T) {
	fs := newFakeSys()
	var taskPasses int
	fs.taskIDs = func(pid int) []int {
		switch pid {
		case 40:
			taskPasses++
			if taskPasses == 1 {
				return []int{40, 41}
			}
			// New thread spawned after pass 1 is caught on pass 2.
			return []int{40, 41, 42}
		case 50:
			return []int{50}
		case 60:
			return []int{60, 61}
		default:
			return []int{pid}
		}
	}
	fs.table = []proc.Info{
		{PID: 50, PPID: 40},
		{PID: 60, PPID: 50},
		{PID: 70, PPID: 1},
	}
	sys := fs.ops()
	w := startReopen(t, func(c *daemon.AgentConfig) { c.Sys = sys })
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")

	// Roots are never confined (neither CPU pin nor systemd scope).
	w.set(t, "worker_memory_high", 2048)
	fs.resetCalls()
	lead, asLead := w.root(t, run.Id)
	if lead.Role != v1.AgentRole_AGENT_ROLE_ROOT {
		t.Fatalf("role = %v", lead.Role)
	}
	fs.mu.Lock()
	if len(fs.pins) != 0 || len(fs.scopes) != 0 || fs.tableReads != 0 {
		t.Errorf("root was confined: pins=%v scopes=%v tableReads=%d", fs.pins, fs.scopes, fs.tableReads)
	}
	fs.mu.Unlock()

	// Unit-level multi-thread/descendant pin + systemd scope test via ConfineAgent helper.
	w.set(t, "worker_cpus", 25)
	w.set(t, "worker_memory_high", 2048)
	fs.resetCalls()
	daemon.ConfineAgentForTest(w.d, ctx, 40, 999, "Proj_Fix.Worker")
	fs.mu.Lock()
	var pinnedTIDs []int
	for _, p := range fs.pins {
		pinnedTIDs = append(pinnedTIDs, p.tid)
		if !slices.Equal(p.cpus, []int{0, 1, 2}) {
			t.Errorf("pinned %d to %v, want [0 1 2]", p.tid, p.cpus)
		}
	}
	slices.Sort(pinnedTIDs)
	if !slices.Equal(pinnedTIDs, []int{40, 41, 42, 50, 60, 61}) {
		t.Errorf("pinned TIDs = %v, want [40 41 42 50 60 61]", pinnedTIDs)
	}
	if len(fs.scopes) != 1 {
		t.Fatalf("scopes = %v", fs.scopes)
	}
	wantScope := []string{
		"busctl", "--user", "call",
		"org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager",
		"StartTransientUnit", "ssa(sv)a(sa(sv))",
		"mesh-proj-fix-worker-40.scope", "fail", "2",
		"PIDs", "au", "3", "40", "50", "60",
		"MemoryHigh", "t", fmt.Sprintf("%d", 2048<<20), "0",
	}
	if !slices.Equal(fs.scopes[0], wantScope) {
		t.Errorf("scope argv = %v\nwant         %v", fs.scopes[0], wantScope)
	}
	fs.mu.Unlock()

	// SetAffinity error (e.g. cpuset forbids cores) drops holder; RunScope failure never fails spawn.
	fs.setErr = errors.New("EINVAL")
	fs.scopeErr = errors.New("no user bus")
	fs.resetCalls()
	a := w.spawnUnder(t, asLead, "scoped", &v1.SpawnAgentRequest{})
	if a.State != v1.AgentState_AGENT_STATE_IDLE {
		t.Errorf("spawn failed on confinement errors: %v", a.State)
	}
	fs.mu.Lock()
	if len(fs.scopes) != 1 {
		t.Errorf("RunScope not called on spawn: %v", fs.scopes)
	}
	fs.mu.Unlock()

	// ApproveAgent reopens the worker into a new terminal and confines the reopened process too.
	fs.setErr = nil
	fs.scopeErr = nil
	planner := w.spawnUnder(t, asLead, "planned", &v1.SpawnAgentRequest{Tier: "strong", Plan: true})
	w.report(t, planner, v1.MessageType_MESSAGE_TYPE_PLAN)
	fs.resetCalls()
	if _, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{
		Meta: meta(""), Agent: planner.Id, Tier: "mid"})); err != nil {
		t.Fatal(err)
	}
	w.waitReopened(t, planner.Id, planner.TerminalId)
	fs.mu.Lock()
	if len(fs.pins) == 0 || len(fs.scopes) != 1 {
		t.Errorf("ApproveAgent reopen not confined: pins=%d scopes=%d", len(fs.pins), len(fs.scopes))
	}
	fs.mu.Unlock()

	// When both worker_cpus (0 or 100) and worker_memory_high (0) are off, /proc is not scanned.
	w.set(t, "worker_cpus", 0)
	w.set(t, "worker_memory_high", 0)
	fs.resetCalls()
	w.spawnUnder(t, asLead, "unconfined", &v1.SpawnAgentRequest{})
	fs.mu.Lock()
	if fs.tableReads != 0 || len(fs.pins) != 0 || len(fs.scopes) != 0 {
		t.Errorf("confined when off: tableReads=%d pins=%v scopes=%v", fs.tableReads, fs.pins, fs.scopes)
	}
	fs.mu.Unlock()

	// Also verify worker_cpus=100 and single-core machines disable pinning.
	w.set(t, "worker_cpus", 100)
	fs.resetCalls()
	daemon.ConfineAgentForTest(w.d, ctx, 40, 999, "w")
	fs.mu.Lock()
	if len(fs.pins) != 0 {
		t.Errorf("worker_cpus=100 pinned: %v", fs.pins)
	}
	fs.mu.Unlock()
	w.set(t, "worker_cpus", 25)
	fs.mu.Lock()
	fs.machineCPUs = []int{0}
	fs.mu.Unlock()
	daemon.ConfineAgentForTest(w.d, ctx, 40, 999, "w")
	fs.mu.Lock()
	if len(fs.pins) != 0 {
		t.Errorf("single-core pinned: %v", fs.pins)
	}
	fs.mu.Unlock()
}

func TestAdoptSeedsCPUHoldersAfterDaemonRestart(t *testing.T) {
	fs := newFakeSys()
	sys := fs.ops()
	w := startReopen(t, func(c *daemon.AgentConfig) { c.Sys = sys })
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	repo := gitRepo(t)
	a := w.spawn(run.Id, repo, "first", "")
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)

	var aPID int
	var aTicks uint64
	for _, tm := range w.terminals(t) {
		if tm.Id == a.TerminalId {
			aPID, aTicks = int(tm.Pid), tm.PidStartTicks
		}
	}
	fs.mu.Lock()
	fs.alive[pidKey{pid: aPID, start: aTicks}] = true
	// Simulate the adopted terminal holding cores [0 1 2].
	fs.affinities[aPID] = []int{0, 1, 2}
	fs.mu.Unlock()

	// Restart the daemon on the same state dir: adopt must restore the CPU holder.
	w.stop()
	fs.resetCalls()
	w.agentsHarness = w.restart(t)

	// Next spawn should see [0 1 2] held and choose [3 4 5] instead of [0 1 2].
	b := w.spawn(run.Id, repo, "second", "")
	w.waitState(b.Id, v1.AgentState_AGENT_STATE_IDLE)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.pins) == 0 || !slices.Equal(fs.pins[0].cpus, []int{3, 4, 5}) {
		t.Errorf("after restart, second agent pinned to %v, want [3 4 5]", fs.pins)
	}
	_ = ctx
}

func TestLeftoverProcessesAndSweepable(t *testing.T) {
	wtRoot := t.TempDir()
	top := filepath.Join(wtRoot, "r1", "shop-api-fix")
	outside := filepath.Join(wtRoot, "elsewhere")
	os.MkdirAll(top, 0o755)
	os.MkdirAll(outside, 0o755)

	mk := func(pid, ppid int, argv, cwd string, tty uint64) proc.Info {
		return proc.Info{PID: pid, PPID: ppid, Argv: strings.Fields(argv), Cwd: cwd, TTY: tty, StartTicks: uint64(pid)}
	}
	table := []proc.Info{
		mk(1278, 1274, "/init", "/", 0),
		mk(1852, 1851, "/usr/local/bin/mesh-term", "/home/u", 0),
		mk(900, 1, "tmux new-session -d", top, 0),                     // tmux server in worktree, pane has tty -> spared
		mk(901, 900, "-bash", "/home/u", 34840),                       // tmux pane with tty
		mk(902, 901, "pnpm dev", top, 34840),                          // child in worktree with tty -> spared
		mk(1288, 1278, "-bash", top, 34816),                           // user's terminal shell with tty -> spared
		mk(1289, 1288, "vim x.go", top, 34816),                        // user's editor -> spared
		mk(14460, 1852, "/bin/bash", top, 0),                          // live terminal shell -> spared
		mk(14649, 14460, "claude --session-id s-other", top, 0),       // live agent -> spared
		mk(14773, 14649, "node /x/playwright-mcp", top, 0),            // live agent's MCP -> spared
		mk(20811, 1278, "/pnpm/exe dev", top, 0),                      // orphaned under /init -> swept
		mk(20834, 20811, "next-server", filepath.Join(top, "app"), 0), // child of orphan -> swept
		mk(30001, 1, "node orphan.js", top, 0),                        // orphaned straight to pid 1 -> swept
		mk(40000, 1278, "node server.js", outside, 0),                 // outside top -> spared
	}
	got := daemon.LeftoverProcessesForTest(top, table)
	var pids []int
	for _, p := range got {
		pids = append(pids, p.PID)
	}
	if !slices.Equal(pids, []int{20811, 20834, 30001}) {
		t.Errorf("leftoverProcesses = %v, want [20811 20834 30001]", pids)
	}

	// Safety checks on sweepable.
	repoDir := t.TempDir()
	rootWt := filepath.Join(wtRoot, "r1", "shop-api-lead")
	for name, a := range map[string]*v1.Agent{
		"root":          {Role: v1.AgentRole_AGENT_ROLE_ROOT, Worktree: top, Repo: repoDir},
		"main checkout": {Role: v1.AgentRole_AGENT_ROLE_WORKER, Worktree: top, Repo: top},
		"outside":       {Role: v1.AgentRole_AGENT_ROLE_WORKER, Worktree: repoDir, Repo: repoDir},
		"shallow":       {Role: v1.AgentRole_AGENT_ROLE_WORKER, Worktree: filepath.Join(wtRoot, "r1"), Repo: repoDir},
		"holds root":    {Role: v1.AgentRole_AGENT_ROLE_WORKER, Worktree: rootWt, Repo: repoDir},
	} {
		if _, ok := daemon.SweepableForTest(wtRoot, a, []string{rootWt}); ok {
			t.Errorf("%s should not be sweepable", name)
		}
	}
	if _, ok := daemon.SweepableForTest(wtRoot, &v1.Agent{Role: v1.AgentRole_AGENT_ROLE_WORKER, Worktree: top, Repo: repoDir}, []string{rootWt}); !ok {
		t.Errorf("valid worker worktree should be sweepable")
	}

	// ownAgent matches a fresh agy (no --conversation flag) and its resumed session, but not another conversation.
	agyAgent := &v1.Agent{Kind: "agy", SessionId: "conv-1"}
	if !daemon.OwnAgentForTest(mk(1, 0, "agy --model gemini-4-neon-high", top, 0), agyAgent) {
		t.Errorf("fresh agy without --conversation should match")
	}
	if !daemon.OwnAgentForTest(mk(1, 0, "agy --conversation conv-1 --model gemini-4-neon-high", top, 0), agyAgent) {
		t.Errorf("resumed agy with matching --conversation should match")
	}
	if daemon.OwnAgentForTest(mk(1, 0, "agy --conversation conv-other --model gemini-4-neon-high", top, 0), agyAgent) {
		t.Errorf("agy with another --conversation must not match")
	}
	if daemon.OwnAgentForTest(mk(1, 0, "claude --session-id other", top, 0), agyAgent) {
		t.Errorf("claude of another session must not match agy agent")
	}
}

func TestRetireAndStopRunSweepLeftoverProcesses(t *testing.T) {
	fs := newFakeSys()
	sys := fs.ops()
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Sys = sys
		c.SweepAgentPoll = time.Millisecond
		c.SweepKillPoll = time.Millisecond
	})
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	a := w.spawnUnder(t, asLead, "swept", &v1.SpawnAgentRequest{})
	b := w.spawnUnder(t, asLead, "stopped", &v1.SpawnAgentRequest{})

	// Simulate a's own agent exiting across 2 polls, then leaving 20811 and stubborn 20834.
	mk := func(pid, ppid int, argv, cwd string) proc.Info {
		return proc.Info{PID: pid, PPID: ppid, Argv: strings.Fields(argv), Cwd: cwd, StartTicks: uint64(pid * 10)}
	}
	initProc := mk(1278, 1274, "/init", "/")
	own := mk(5000, 1278, "claude --resume "+a.SessionId, a.Worktree)
	otherAgent := mk(6000, 1278, "claude --session-id other-session", a.Worktree)
	otherChild := mk(6001, 6000, "node mcp.js", a.Worktree)
	orphan1 := mk(20811, 1278, "/pnpm/exe dev", a.Worktree)
	orphan2 := mk(20834, 20811, "next-server", a.Worktree)
	fs.resetCalls()
	fs.mu.Lock()
	fs.tables = [][]proc.Info{
		{initProc, own, otherAgent, otherChild, orphan1, orphan2},
		{initProc, own, otherAgent, otherChild, orphan1, orphan2},
		{initProc, otherAgent, otherChild, orphan1, orphan2}, // own exited
	}
	// 20834 ignores SIGTERM so SIGKILL is sent.
	fs.alive[pidKey{pid: 20834, start: 208340}] = true
	fs.mu.Unlock()

	if _, err := asLead.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: a.Id})); err != nil {
		t.Fatal(err)
	}
	wantSignals := []sigCall{
		{pid: 20811, start: 208110, sig: syscall.SIGTERM},
		{pid: 20834, start: 208340, sig: syscall.SIGTERM},
		{pid: 20834, start: 208340, sig: syscall.SIGKILL},
	}
	waitFor(t, "async retire sweep signals", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.tableReads == 3 && slices.Equal(fs.signals, wantSignals)
	})

	// A ProcTable error during sweep never fails RetireAgent.
	c := w.spawnUnder(t, asLead, "errproc", &v1.SpawnAgentRequest{})
	fs.resetCalls()
	fs.mu.Lock()
	fs.tableErr = errors.New("no /proc")
	fs.mu.Unlock()
	if _, err := asLead.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: c.Id})); err != nil {
		t.Fatalf("RetireAgent failed on ProcTable error: %v", err)
	}
	waitFor(t, "async error sweep attempted ProcTable", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.tableReads >= 1
	})
	fs.mu.Lock()
	fs.tableErr = nil
	fs.mu.Unlock()

	// On StopRun, already-retired and live non-root agents are swept before worktree removal.
	fs.resetCalls()
	orphanB := mk(30900, 1, "node vite.js", b.Worktree)
	fs.mu.Lock()
	fs.tables = nil
	fs.table = []proc.Info{initProc, orphanB}
	fs.mu.Unlock()
	if _, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta(""), RunId: run.Id})); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	stopSignals := append([]sigCall(nil), fs.signals...)
	fs.mu.Unlock()
	if !slices.Equal(stopSignals, []sigCall{{pid: 30900, start: 309000, sig: syscall.SIGTERM}}) {
		t.Errorf("StopRun signals = %+v, want SIGTERM to 30900", stopSignals)
	}
	if _, err := os.Stat(b.Worktree); !os.IsNotExist(err) {
		t.Errorf("clean worktree %s was not removed after sweep: %v", b.Worktree, err)
	}
}

func TestMemoryGuardEarlyCheckRefreshCancelAndAdoptSeed(t *testing.T) {
	dir := t.TempDir()
	mem := writeProc(t, dir, "meminfo", meminfoText(500, 0, 16000, 4000))
	psi := writeProc(t, dir, "pressure", "some avg10=0.00 avg60=0.00 avg300=0.00 total=0\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")
	fs := newFakeSys()
	sys := fs.ops()
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.MeminfoPath, c.PressurePath = mem, psi
		c.Sys = sys
	})
	ctx := context.Background()
	w.set(t, "min_free_mb", 1000)
	run := startRun(t, w.device.Mesh, "")
	repo := gitRepo(t)

	// 1. Early check in SpawnAgent refuses before creating a worktree or branch, and takes no reservation.
	if _, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		Meta: meta(""), RunId: run.Id, Repo: repo, Slug: "early-fail", Tier: "mid"})); code(err) != connect.CodeResourceExhausted {
		t.Fatalf("expected ResourceExhausted, got %v", err)
	}
	if wt, _ := filepath.Glob(filepath.Join(w.state, "worktrees", run.Id, "*")); len(wt) != 0 {
		t.Errorf("worktree created despite early memory guard failure: %v", wt)
	}
	if out, _ := exec.Command("git", "-C", repo, "branch", "--list", "mesh/*").Output(); len(strings.TrimSpace(string(out))) != 0 {
		t.Errorf("branch created despite early memory guard failure: %s", out)
	}

	// 2. Resume reserves memory, and reopen refreshes the reservation clock at launch; SwitchAgent does not reserve.
	os.WriteFile(mem, []byte(meminfoText(2000, 0, 16000, 4000)), 0o600)
	a := w.spawn(run.Id, repo, "worker", "")
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	if _, err := w.device.Mesh.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: a.Id})); err != nil {
		t.Fatal(err)
	}
	if w.d.Reserved(a.Id) {
		t.Fatalf("retired agent still holds reservation")
	}
	// SwitchAgent on a live agent must not acquire a reservation.
	if _, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Meta: meta(""), Agent: a.Id})); err != nil {
		t.Fatal(err)
	}
	if !w.d.Reserved(a.Id) {
		t.Fatalf("resumed agent did not reserve memory")
	}
	w.waitReopened(t, a.Id, "")
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	// Simulate reservation expiring after idle wait; a SwitchAgent swap stays unreserved.
	w.d.Release(a.Id)
	if _, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: a.Id, Effort: "low"})); err != nil {
		t.Fatal(err)
	}
	w.waitReopened(t, a.Id, "")
	if w.d.Reserved(a.Id) {
		t.Errorf("SwitchAgent swap should not acquire a memory reservation")
	}

	// 3. Reopen cancelled because the run is closed releases its reservation.
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	if _, err := w.device.Mesh.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: a.Id})); err != nil {
		t.Fatal(err)
	}
	w.putAgent(t, a.Id, func(ag *v1.Agent) {
		ag.PendingReopen = &v1.Reopen{Id: "o-closed", Model: "sonnet", Effort: "high", Why: "resumed"}
	})
	w.d.Reserve(a.Id)
	if err := w.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error {
		r, err := tx.GetRun(run.Id)
		if err != nil {
			return err
		}
		r.Status = v1.RunStatus_RUN_STATUS_CLOSED
		return tx.UpdateRun(r)
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "closed-run reopen dropped and reservation released", func() bool {
		return w.agent(a.Id).PendingReopen == nil && !w.d.Reserved(a.Id)
	})

	// 4. Restarting the daemon seeds reservations for live terminals within ReserveFor.
	run2 := startRun(t, w.device.Mesh, "r2")
	live := w.spawn(run2.Id, repo, "live", "")
	w.waitState(live.Id, v1.AgentState_AGENT_STATE_IDLE)
	// Also seed an old terminal started > ReserveFor ago to ensure it is NOT re-reserved.
	tc, err := termclient.New(statedir.Paths{Dir: w.state}.Term())
	if err != nil {
		t.Fatal(err)
	}
	oldTerm, err := tc.API.CreateTerminal(ctx, connect.NewRequest(&termv1.CreateTerminalRequest{
		Argv: []string{"sleep", "30"}, Cwd: repo,
	}))
	if err != nil {
		t.Fatal(err)
	}
	w.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error {
		return tx.PutAgent(&v1.Agent{Id: "a-old", RunId: run2.Id, Name: "old", Role: v1.AgentRole_AGENT_ROLE_WORKER,
			TerminalId: oldTerm.Msg.Terminal.Id, State: v1.AgentState_AGENT_STATE_IDLE, CreatedAt: timestamppb.Now()})
	})

	w.stop()
	w.agentsHarness = w.restart(t)
	if !w.d.Reserved(live.Id) {
		t.Errorf("live terminal younger than ReserveFor was not seeded after restart")
	}
	// Fake an old reservation check via guard: a-old's terminal just started in mesh-term so its StartedAt is now;
	// verify guard.seed skips timestamps >= ReserveFor directly, and refresh only updates existing reservations.
	w.d.Seed("a-too-old", time.Now().Add(-daemon.ReserveFor-time.Second))
	if w.d.Reserved("a-too-old") {
		t.Errorf("terminal older than ReserveFor should not be seeded")
	}
	w.d.Refresh("a-never-reserved")
	if w.d.Reserved("a-never-reserved") {
		t.Errorf("refresh should not create a reservation")
	}
	// Seed("a-near-expiry", ...) check:
	w.d.Seed("a-near-expiry", time.Now().Add(-daemon.ReserveFor+2*time.Second))
	w.d.Refresh("a-near-expiry")
	if !w.d.Reserved("a-near-expiry") {
		t.Errorf("refresh lost existing reservation")
	}
}

func TestRound1ReviewFixes(t *testing.T) {
	// 1. confineAgent ignores caller ctx cancellation (WithoutCancel).
	fs := newFakeSys()
	sys := fs.ops()
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Sys = sys
		c.SweepAgentPoll = time.Millisecond
		c.SweepKillPoll = time.Millisecond
	})
	ctx := context.Background()
	w.set(t, "worker_memory_high", 1024)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	daemon.ConfineAgentForTest(w.d, cctx, 40, 100, "cancelled-ctx")
	fs.mu.Lock()
	if len(fs.pins) == 0 || len(fs.scopes) != 1 {
		t.Errorf("confineAgent with cancelled ctx failed: pins=%d scopes=%d", len(fs.pins), len(fs.scopes))
	}
	fs.mu.Unlock()

	// 2. adoptCPUHolder checks ProcAlive and does not double-count a holder already held.
	fs.resetCalls()
	fs.mu.Lock()
	fs.machineCPUs = []int{0, 1, 2, 3, 4, 5}
	fs.affinities[701] = []int{0, 1}
	fs.affinities[702] = []int{0, 1}
	fs.affinities[703] = []int{2, 3}
	fs.alive[pidKey{pid: 702, start: 200}] = true
	fs.alive[pidKey{pid: 703, start: 300}] = true
	// 701 is dead/reused (alive[701] == false).
	fs.mu.Unlock()
	w.set(t, "worker_cpus", 34) // 2 of 6 cores
	w.set(t, "worker_memory_high", 0)
	worker := &v1.Agent{Role: v1.AgentRole_AGENT_ROLE_WORKER}
	daemon.AdoptCPUHolderForTest(w.d, ctx, worker, 701, 100) // dead: ignored
	daemon.AdoptCPUHolderForTest(w.d, ctx, worker, 702, 200) // live [0,1]
	daemon.AdoptCPUHolderForTest(w.d, ctx, worker, 702, 200) // duplicate: not double-counted
	daemon.AdoptCPUHolderForTest(w.d, ctx, worker, 703, 300) // live [2,3]
	// Also note PID 40 from step 1 was not alive so pickWindow drops it.
	// If 702 had been double-counted, [0,1] load=2 and [2,3] load=1; since it wasn't, [0,1],[2,3] load=1 and [4,5] load=0 -> picks [4,5]!
	// Then put another live holder on [4,5], so all 3 windows have load=1, and verify [0,1] is NOT behind [2,3] (if 702 was double-counted, [2,3] would beat [0,1]).
	fs.mu.Lock()
	fs.affinities[704] = []int{4, 5}
	fs.alive[pidKey{pid: 704, start: 400}] = true
	fs.mu.Unlock()
	daemon.AdoptCPUHolderForTest(w.d, ctx, worker, 704, 400)
	// Add a second holder on [2,3] and [4,5] so [0,1] is strictly least-held only if 702 wasn't double-counted.
	fs.mu.Lock()
	fs.affinities[705] = []int{2, 3}
	fs.affinities[706] = []int{4, 5}
	fs.alive[pidKey{pid: 705, start: 500}] = true
	fs.alive[pidKey{pid: 706, start: 600}] = true
	fs.mu.Unlock()
	daemon.AdoptCPUHolderForTest(w.d, ctx, worker, 705, 500)
	daemon.AdoptCPUHolderForTest(w.d, ctx, worker, 706, 600)
	fs.resetCalls()
	daemon.ConfineAgentForTest(w.d, ctx, 707, 700, "pick")
	fs.mu.Lock()
	if len(fs.pins) == 0 || !slices.Equal(fs.pins[0].cpus, []int{0, 1}) {
		t.Errorf("expected [0 1] to be least-held (702 not double-counted), got %v", fs.pins)
	}
	fs.mu.Unlock()

	// 3. sweepWorktrees waits for a fresh agy launch (no --conversation flag) before sweeping its child.
	run := startRun(t, w.device.Mesh, "agy-sweep")
	repo := gitRepo(t)
	wt := filepath.Join(w.state, "worktrees", run.Id, "shop-api-agy")
	os.MkdirAll(wt, 0o755)
	_ = repo
	agyW := &v1.Agent{Id: "a-agy", RunId: run.Id, Name: "shop-api-agy", Role: v1.AgentRole_AGENT_ROLE_WORKER,
		Kind: "agy", SessionId: "conv-fresh", Worktree: wt, Repo: repo, State: v1.AgentState_AGENT_STATE_RETIRED}
	if err := w.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error { return tx.PutAgent(agyW) }); err != nil {
		t.Fatal(err)
	}
	initP := proc.Info{PID: 1278, PPID: 1274, Argv: []string{"/init"}, Cwd: "/"}
	freshAgy := proc.Info{PID: 8000, PPID: 1278, Argv: []string{"agy", "--model", "gemini-4-neon-high"}, Cwd: wt, StartTicks: 80}
	orphan := proc.Info{PID: 8001, PPID: 1278, Argv: []string{"node", "dev.js"}, Cwd: wt, StartTicks: 81}
	fs.resetCalls()
	fs.mu.Lock()
	fs.tables = [][]proc.Info{{initP, freshAgy, orphan}, {initP, orphan}}
	fs.mu.Unlock()
	if _, err := w.device.Mesh.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: agyW.Id})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "fresh agy async sweep", func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return fs.tableReads == 2 && slices.Equal(fs.signals, []sigCall{{pid: 8001, start: 81, sig: syscall.SIGTERM}})
	})

	// 4. adopt at boot runs finishRetire's sweep asynchronously without blocking daemon startup.
	w2State := shortdir.State(t)
	w2Home := t.TempDir()
	fs2 := newFakeSys()
	sys2 := fs2.ops()
	origTable := sys2.ProcTable
	h := startWithAgents(t, w2State, w2Home, func(c *daemon.AgentConfig) { c.Sys = sys2 })
	t.Cleanup(func() { stopTermHost(t, w2State) })
	run2 := startRun(t, h.device.Mesh, "")
	vict := h.spawn(run2.Id, gitRepo(t), "boot-retire", "")
	h.waitState(vict.Id, v1.AgentState_AGENT_STATE_IDLE)
	h.stop()
	// Mark retired in DB while its terminal still runs on mesh-term (simulating crash mid-retire).
	st, err := store.Open(statedir.Paths{Dir: w2State}.DB())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, nil, "", func(tx *store.Tx) error {
		ag, err := tx.GetAgent(vict.Id)
		if err != nil {
			return err
		}
		ag.State = v1.AgentState_AGENT_STATE_RETIRED
		return tx.PutAgent(ag)
	}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	sweepStarted := make(chan struct{})
	releaseSweep := make(chan struct{})
	var bootOnce sync.Once
	sys2.ProcTable = func() ([]proc.Info, error) {
		bootOnce.Do(func() {
			close(sweepStarted)
			<-releaseSweep
		})
		return origTable()
	}
	fs2.mu.Lock()
	fs2.table = []proc.Info{initP, {PID: 9001, PPID: 1278, Argv: []string{"node", "leftover.js"}, Cwd: vict.Worktree, StartTicks: 91}}
	fs2.mu.Unlock()
	h2 := startWithAgents(t, w2State, w2Home, func(c *daemon.AgentConfig) {
		c.Sys = sys2
		c.SweepAgentPoll = time.Millisecond
		c.SweepKillPoll = time.Millisecond
	})
	// Startup completed (meshd.json written) even though sweep hasn't been released yet.
	select {
	case <-sweepStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("boot sweep did not start")
	}
	if got := h2.agent(vict.Id); got.TerminalId != "" {
		t.Errorf("retired terminal not stopped synchronously at adopt: %q", got.TerminalId)
	}
	close(releaseSweep)
	waitFor(t, "async boot sweep signalled orphan", func() bool {
		fs2.mu.Lock()
		defer fs2.mu.Unlock()
		return slices.Equal(fs2.signals, []sigCall{{pid: 9001, start: 91, sig: syscall.SIGTERM}})
	})
}
