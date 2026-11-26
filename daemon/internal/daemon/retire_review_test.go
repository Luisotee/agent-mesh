package daemon_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// putAgent changes an agent in the store directly, as a state a race would leave.
func (w *reopenWorld) putAgent(t *testing.T, id string, f func(a *v1.Agent)) {
	t.Helper()
	err := w.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		a, err := tx.GetAgent(id)
		if err != nil {
			return err
		}
		f(a)
		return tx.PutAgent(a)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (w *reopenWorld) liveTerminals(t *testing.T) int {
	n := 0
	for _, term := range w.terminals(t) {
		if !term.Exited {
			n++
		}
	}
	return n
}

func (w *reopenWorld) stopRun(t *testing.T, run string) *v1.StopRunResponse {
	t.Helper()
	res, err := w.device.Mesh.StopRun(context.Background(), connect.NewRequest(&v1.StopRunRequest{Meta: meta(""), RunId: run}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg
}

// A resume stored on a stopped agent and not launched yet is ended by the stop of its run; and a reopen that
// slipped past it still launches nothing in a closed run.
func TestStopRunEndsAStoppedAgentsPendingResumeAndNothingLaunchesInAClosedRun(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	a := w.spawn(run.Id, gitRepo(t), "pending", "")
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	if _, err := w.device.Mesh.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: a.Id})); err != nil {
		t.Fatal(err)
	}
	pend := func(a *v1.Agent) {
		a.PendingReopen = &v1.Reopen{Id: "oraced", Model: "sonnet", Effort: "high", Reason: "test"}
	}
	// The resume is stored while StopRun holds the agent's lock, so it can't launch before the run is closed.
	daemon.OnStopRunLocked(func() { w.putAgent(t, a.Id, pend) })
	t.Cleanup(func() { daemon.OnStopRunLocked(nil) })
	w.stopRun(t, run.Id)
	if got := w.agent(a.Id); got.PendingReopen != nil || got.State != v1.AgentState_AGENT_STATE_RETIRED {
		t.Fatalf("after the stop: %v", got)
	}
	// A request stored after the run closed (nothing but a bug could) is dropped, not launched.
	w.putAgent(t, a.Id, pend)
	waitFor(t, "the request dropped", func() bool { return w.agent(a.Id).PendingReopen == nil })
	time.Sleep(300 * time.Millisecond)
	if got := w.agent(a.Id); got.State != v1.AgentState_AGENT_STATE_RETIRED || got.TerminalId != "" || w.liveTerminals(t) != 0 {
		t.Errorf("a process launched in a closed run: %v", got)
	}
}

// A spawn that was past its run check when the run was stopped is refused: it must not start an agent in it.
func TestASpawnRacingStopRunIsRefusedNotStartedInAClosedRun(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	daemon.OnSpawnPlanned(func() { once.Do(func() { close(reached); <-release }) })
	t.Cleanup(func() { daemon.OnSpawnPlanned(nil) })
	errc := make(chan error, 1)
	go func() {
		_, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Meta: meta(""), RunId: run.Id,
			Repo: gitRepo(t), Slug: "late", Tier: "mid"}))
		errc <- err
	}()
	<-reached
	w.stopRun(t, run.Id)
	close(release)
	if err := <-errc; code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("the spawn: %v", err)
	}
	if n := w.liveTerminals(t); n != 0 {
		t.Errorf("%d processes in a closed run", n)
	}
}

// An agent spawned after StopRun listed the run's agents (and so locked them) is still retired, and its process
// stopped: StopRun locks it too and goes again.
func TestStopRunTakesTheLockOfAnAgentSpawnedWhileItWasLocking(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	stopAtLock, committed, letLaunch := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once1, once2 sync.Once
	daemon.OnStopRunLocked(func() { once1.Do(func() { close(stopAtLock); <-committed }) })
	daemon.OnSpawnCommitted(func(string) { once2.Do(func() { close(committed); <-letLaunch }) })
	t.Cleanup(func() { daemon.OnStopRunLocked(nil); daemon.OnSpawnCommitted(nil) })
	stopped := make(chan *v1.StopRunResponse, 1)
	go func() { stopped <- w.stopRun(t, run.Id) }()
	<-stopAtLock
	spawned := make(chan *v1.Agent, 1)
	go func() {
		res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Meta: meta(""), RunId: run.Id,
			Repo: gitRepo(t), Slug: "new", Tier: "mid"}))
		if err != nil {
			t.Error(err)
			spawned <- nil
			return
		}
		spawned <- res.Msg.Agent
	}()
	<-committed
	time.Sleep(200 * time.Millisecond) // StopRun is now waiting for the new agent's lock
	close(letLaunch)
	a := <-spawned
	out := <-stopped
	if a == nil {
		return
	}
	got := w.agent(a.Id)
	if got.State != v1.AgentState_AGENT_STATE_RETIRED || got.TerminalId != "" || w.liveTerminals(t) != 0 {
		t.Errorf("the late agent: %v", got)
	}
	if len(out.Retired) != 1 || out.Retired[0] != a.Id {
		t.Errorf("retired %v, want %s", out.Retired, a.Id)
	}
}

// A retired agent whose process was not confirmed stopped (its retire failed or its caller gave up) is stopped by
// the reopen before it launches again: one process per conversation.
func TestAReopenStopsTheProcessOfARetiredAgentThatStillHasOne(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	a := w.spawn(run.Id, gitRepo(t), "leftover", "")
	before := w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.putAgent(t, a.Id, func(a *v1.Agent) { a.State = v1.AgentState_AGENT_STATE_RETIRED }) // its terminal is still there
	if _, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Meta: meta(""), Agent: a.Id})); err != nil {
		t.Fatal(err)
	}
	after := w.waitReopened(t, a.Id, before.TerminalId)
	live := 0
	for _, term := range w.terminals(t) {
		if term.Id == before.TerminalId {
			t.Errorf("the old process %s is still on the host (exited %v)", term.Id, term.Exited)
		}
		if !term.Exited {
			live++
		}
	}
	if live != 1 || after.TerminalId == before.TerminalId {
		t.Errorf("%d live processes for one conversation", live)
	}
}

// A branch another branch stacks on, with commits of its own that were never pushed, is kept (the stacked branch
// reaches the same commits, so "no other branch has them" would delete it); an untouched stacked branch goes.
func TestStopRunKeepsAStackedBranchWithItsOwnCommits(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	repo := gitRepo(t)
	a := w.spawn(run.Id, repo, "base", "")
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	gitIn(t, a.Worktree, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "work")
	res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Meta: meta(""), RunId: run.Id,
		Repo: repo, Slug: "stacked", Tier: "mid", Base: a.Branch}))
	if err != nil {
		t.Fatal(err)
	}
	b := res.Msg.Agent
	w.waitState(b.Id, v1.AgentState_AGENT_STATE_IDLE)
	out := w.stopRun(t, run.Id)
	for _, o := range out.Worktrees {
		switch o.AgentId {
		case a.Id:
			if !o.BranchKept || !strings.Contains(gitIn(t, repo, "branch", "--list", a.Branch), a.Branch) {
				t.Errorf("the branch with commits was deleted: %v", o)
			}
		case b.Id:
			if o.BranchKept {
				t.Errorf("the untouched stacked branch was kept: %v", o)
			}
		}
	}
}

// An agent that exits, or is retired, gives its memory reservation back at once, not after ReserveFor.
func TestStoppedAgentsGiveTheirMemoryReservationBack(t *testing.T) {
	for _, how := range []string{"retire", "exit"} {
		t.Run(how, func(t *testing.T) {
			dir := t.TempDir()
			mem := writeProc(t, dir, "meminfo", meminfoText(1500, 0, 16000, 4000))
			w := startReopen(t, func(c *daemon.AgentConfig) { c.MeminfoPath = mem })
			ctx := context.Background()
			w.set(t, "min_free_mb", 1000)
			run := startRun(t, w.device.Mesh, "")
			repo := gitRepo(t)
			spawn := func(slug string) (*v1.Agent, error) {
				res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Meta: meta(""), RunId: run.Id,
					Repo: repo, Slug: slug, Tier: "mid"}))
				if err != nil {
					return nil, err
				}
				return w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE), nil
			}
			var started []*v1.Agent
			for _, slug := range []string{"one", "two"} {
				a, err := spawn(slug)
				if err != nil {
					t.Fatal(err)
				}
				started = append(started, a)
			}
			if _, err := spawn("three"); code(err) != connect.CodeResourceExhausted {
				t.Fatalf("the third: %v", err)
			}
			for _, a := range started {
				if how == "retire" {
					if _, err := w.device.Mesh.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: a.Id})); err != nil {
						t.Fatal(err)
					}
				} else {
					w.typeInto(a, "/exit\r")
					w.waitState(a.Id, v1.AgentState_AGENT_STATE_EXITED)
				}
			}
			// Nothing runs now: a third agent needs no wait for the reservations to lapse.
			a, err := spawn("three")
			if err != nil {
				t.Fatalf("a spawn after both stopped: %v", err)
			}
			w.typeInto(a, "/exit\r")
			w.waitState(a.Id, v1.AgentState_AGENT_STATE_EXITED)
		})
	}
}

// A worktree that was deleted from disk is pruned from git's list, or git refuses to delete its branch.
func TestStopRunDeletesTheUntouchedBranchOfAWorktreeAlreadyGone(t *testing.T) {
	w := startReopen(t)
	run := startRun(t, w.device.Mesh, "")
	repo := gitRepo(t)
	a := w.spawn(run.Id, repo, "gone", "")
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	if err := os.RemoveAll(a.Worktree); err != nil {
		t.Fatal(err)
	}
	out := w.stopRun(t, run.Id)
	if len(out.Worktrees) != 1 || !out.Worktrees[0].WorktreeRemoved || out.Worktrees[0].BranchKept {
		t.Errorf("%v", out.Worktrees)
	}
}

// A message to a stopped agent that has no conversation to resume says so in resume_error (it was empty), plainly.
func TestAMessageToAStoppedAgentWithNoConversationSaysWhyItDidNotResume(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	a := w.spawn(run.Id, gitRepo(t), "nosession", "")
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	if _, err := w.device.Mesh.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: a.Id})); err != nil {
		t.Fatal(err)
	}
	w.putAgent(t, a.Id, func(a *v1.Agent) { a.SessionId = "" })
	res, err := w.device.Mesh.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: a.Id,
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "wake up"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.Resumed != nil || !strings.HasPrefix(res.Msg.ResumeError, a.Name+" never reported a conversation id") {
		t.Errorf("resumed %v, resume_error %q", res.Msg.Resumed, res.Msg.ResumeError)
	}
}

// A replayed retire answers what the first call did, not the agent as it is by now.
func TestAReplayedRetireAnswersWithTheRetiredAgent(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	a := w.spawn(run.Id, gitRepo(t), "replay", "")
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	req := func() *connect.Request[v1.RetireAgentRequest] {
		return connect.NewRequest(&v1.RetireAgentRequest{Meta: meta("retire-once"), Agent: a.Id})
	}
	if _, err := w.device.Mesh.RetireAgent(ctx, req()); err != nil {
		t.Fatal(err)
	}
	msg, err := w.device.Mesh.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: a.Id,
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "back to work"}))
	if err != nil || msg.Msg.Resumed == nil {
		t.Fatalf("%v, %v", msg, err)
	}
	w.waitReopened(t, a.Id, "")
	waitFor(t, "delivered", func() bool { return w.delivered(t, a.Id, msg.Msg.Message.Id) })
	again, err := w.device.Mesh.RetireAgent(ctx, req())
	if err != nil {
		t.Fatal(err)
	}
	if again.Msg.Agent.State != v1.AgentState_AGENT_STATE_RETIRED {
		t.Errorf("the replay answered with %v", again.Msg.Agent.State)
	}
	if got := w.agent(a.Id); got.State == v1.AgentState_AGENT_STATE_RETIRED {
		t.Errorf("the replay retired it again: %v", got.State)
	}
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
}

// Two StopRuns, and an agent spawned after the first listed the run: the first must not take the late agent's
// lock while holding the others', or it waits for the second, which holds the late one and waits for an agent the
// first holds. It lets go of everything and takes the whole set again, in id order.
func TestTwoStopRunsDoNotDeadlockOverAnAgentSpawnedMeanwhile(t *testing.T) {
	w := startReopen(t)
	run := startRun(t, w.device.Mesh, "")
	a := w.spawn(run.Id, gitRepo(t), "first", "")
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	var calls atomic.Int32
	second := make(chan *v1.StopRunResponse, 1)
	daemon.OnStopRunLocked(func() {
		if calls.Add(1) != 1 {
			return
		}
		// The first StopRun holds agent a. A new agent whose id sorts before a's appears, and the second StopRun
		// starts: it locks the new one and waits for a.
		err := w.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
			return tx.PutAgent(&v1.Agent{Id: "!late", RunId: run.Id, Name: "late", State: v1.AgentState_AGENT_STATE_IDLE})
		})
		if err != nil {
			t.Error(err)
		}
		go func() { second <- w.stopRun(t, run.Id) }()
		time.Sleep(300 * time.Millisecond)
	})
	t.Cleanup(func() { daemon.OnStopRunLocked(nil) })
	first := make(chan *v1.StopRunResponse, 1)
	go func() { first <- w.stopRun(t, run.Id) }()
	for i := 0; i < 2; i++ {
		select {
		case <-first:
		case <-second:
		case <-time.After(20 * time.Second):
			t.Fatal("two StopRuns deadlocked")
		}
	}
	if got := w.agent("!late"); got.State != v1.AgentState_AGENT_STATE_RETIRED {
		t.Errorf("the late agent is %v", got.State)
	}
	if got := w.agent(a.Id); got.State != v1.AgentState_AGENT_STATE_RETIRED || w.liveTerminals(t) != 0 {
		t.Errorf("agent a: %v, %d live processes", got.State, w.liveTerminals(t))
	}
}

// A stopped planner's direct task goes through revive alone: one reopen (not one from the direct-task step and
// another from the revive, and not a request that outlives a refused resume).
func TestADirectTaskToAStoppedPlannerWithAReopenPendingRequestsOneReopen(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	a := w.spawnUnder(t, asLead, "plan", &v1.SpawnAgentRequest{Tier: "strong", Plan: true})
	w.report(t, a, v1.MessageType_MESSAGE_TYPE_PLAN)
	if _, err := asLead.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: a.Id})); err != nil {
		t.Fatal(err)
	}
	w.putAgent(t, a.Id, func(a *v1.Agent) {
		a.PendingReopen = &v1.Reopen{Id: "o-earlier", Model: "opus", DenyEdits: true, Why: "resumed"}
	})
	events := w.collectEvents(t, run.Id)
	if _, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: a.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK,
		Body: "Direct task (no plan): make the change"})); err != nil {
		t.Fatal(err)
	}
	got := w.waitReopened(t, a.Id, "")
	if got.Phase != v1.AgentPhase_AGENT_PHASE_UNSPECIFIED || got.DenyEdits {
		t.Errorf("the planner: %v", got)
	}
	requested := 0
	for _, ev := range events() {
		if ev.AgentId == a.Id && ev.GetAgentReopenRequested() != nil {
			requested++
		}
	}
	if requested != 1 {
		t.Errorf("%d reopens requested for one task", requested)
	}
}

// A refused resume leaves a stopped planner as it was: still planning, its list kept, nothing pending. The task
// is stored all the same.
func TestARefusedDirectTaskLeavesAStoppedPlannerPlanning(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	a := w.spawnUnder(t, asLead, "plan", &v1.SpawnAgentRequest{Tier: "strong", Plan: true})
	w.report(t, a, v1.MessageType_MESSAGE_TYPE_PLAN)
	if _, err := asLead.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: a.Id})); err != nil {
		t.Fatal(err)
	}
	w.set(t, "caps.agents", 1) // the root alone fills it
	msg, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: a.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK,
		Body: "Direct task (no plan): make the change"}))
	if err != nil || msg.Msg.Resumed != nil || !strings.Contains(msg.Msg.ResumeError, "caps.agents 1") {
		t.Fatalf("a direct task at the cap: %v, %v", msg, err)
	}
	got := w.agent(a.Id)
	if got.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING || !got.DenyEdits || got.PendingReopen != nil {
		t.Errorf("the refused planner: phase %v, deny %v, pending %v", got.Phase, got.DenyEdits, got.PendingReopen)
	}
}

// A read-only child's "Direct task" to a stopped parent is not a parent's: it can't end the parent's plan phase
// or lift its list, as it can't for a running one.
func TestAChildsDirectTaskDoesNotLiftAStoppedParentsDenyList(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	p := w.spawnUnder(t, asLead, "plan", &v1.SpawnAgentRequest{Tier: "strong", Plan: true})
	asP := w.as(w.token(t, p.Id))
	res, err := asP.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{RunId: run.Id, Repo: gitRepo(t), Slug: "child",
		Role: v1.AgentRole_AGENT_ROLE_ASKER, Tier: "mid"}))
	if err != nil {
		t.Fatal(err)
	}
	child := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.report(t, p, v1.MessageType_MESSAGE_TYPE_PLAN)
	if _, err := asLead.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: p.Id})); err != nil {
		t.Fatal(err)
	}
	if _, err := w.as(w.token(t, child.Id)).SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent",
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "Direct task (no plan): edit the files"})); err != nil {
		t.Fatal(err)
	}
	got := w.waitReopened(t, p.Id, "")
	if got.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING || !got.DenyEdits {
		t.Errorf("the parent after its child's task: phase %v, deny %v", got.Phase, got.DenyEdits)
	}
}

// Stopping an agent whose resume was stored and not launched ends that resume, and with it the reservation the
// resume took: nothing will launch to give it back.
func TestStopAgentGivesBackTheReservationOfAResumeItEnds(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	for name, dead := range map[string]bool{"retired": false, "dead": true} {
		a := w.spawn(run.Id, gitRepo(t), name, "")
		w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
		if _, err := w.device.Mesh.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: a.Id})); err != nil {
			t.Fatal(err)
		}
		// Inject the unlaunched resume and its reservation while StopAgent holds the agent's lock so the background
		// retry loop can't launch it before StopAgent cancels it.
		daemon.OnStopAgentLocked(func() {
			w.putAgent(t, a.Id, func(a *v1.Agent) {
				a.PendingReopen = &v1.Reopen{Id: "o-" + name, Model: "opus", Why: "resumed"}
				if dead { // its process died while the resume was waiting: the exit was left to the reopen
					a.State, a.TerminalId = v1.AgentState_AGENT_STATE_IDLE, ""
				}
			})
			w.d.Reserve(a.Id)
		})
		t.Cleanup(func() { daemon.OnStopAgentLocked(nil) })
		if _, err := w.device.Mesh.StopAgent(ctx, connect.NewRequest(&v1.StopAgentRequest{Meta: meta(""), Agent: a.Id})); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		daemon.OnStopAgentLocked(nil)
		if w.d.Reserved(a.Id) {
			t.Errorf("%s: the reservation of the resume it ended is still held", name)
		}
		if got := w.agent(a.Id); got.PendingReopen != nil {
			t.Errorf("%s: its resume is still pending: %v", name, got.PendingReopen)
		}
	}
}
