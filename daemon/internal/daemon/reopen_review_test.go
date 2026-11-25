package daemon_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
)

// planned spawns a planner under lead, has it send a plan and approves it onto its execution tier (light), and
// waits until it executes there, settled.
func (w *reopenWorld) approvedOnLight(t *testing.T, lead interface {
	SpawnAgent(context.Context, *connect.Request[v1.SpawnAgentRequest]) (*connect.Response[v1.SpawnAgentResponse], error)
	ApproveAgent(context.Context, *connect.Request[v1.ApproveAgentRequest]) (*connect.Response[v1.ApproveAgentResponse], error)
}, slug string) *v1.Agent {
	t.Helper()
	ctx := context.Background()
	res, err := lead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: gitRepo(t), Slug: slug,
		Tier: "strong", Plan: true, ExecTier: "light"}))
	if err != nil {
		t.Fatal(err)
	}
	before := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	if _, err := w.as(w.token(t, before.Id)).SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent",
		Type: v1.MessageType_MESSAGE_TYPE_PLAN, Body: "the plan"})); err != nil {
		t.Fatal(err)
	}
	ap, err := lead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: before.Id}))
	if err != nil {
		t.Fatal(err)
	}
	after := w.waitReopened(t, before.Id, before.TerminalId)
	waitFor(t, "the approval delivered", func() bool { return w.delivered(t, before.Id, ap.Msg.Message.Id) })
	w.waitState(before.Id, v1.AgentState_AGENT_STATE_IDLE)
	if after.Model != "haiku" || after.Phase != v1.AgentPhase_AGENT_PHASE_EXECUTING || after.ExecTier != "light" {
		t.Fatalf("approved %v", after)
	}
	return w.agent(before.Id)
}

// After a direct task the model is still the execution tier's: `next` walks that chain, never the planning
// tier's (it once jumped to the planning tier's first model, a costlier one).
func TestSwitchNextAfterADirectTaskWalksTheTierTheAgentRunsOn(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	a := w.approvedOnLight(t, asLead, "direct")
	w.typeInto(a, "/exit\r")
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_EXITED)

	res, err := asLead.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Agent: a.Id, Task: "a small fix"}))
	if err != nil {
		t.Fatal(err)
	}
	after := w.waitReopened(t, a.Id, a.TerminalId)
	if after.Model != "haiku" || after.Tier != "light" || after.ExecTier != "" || after.Phase != v1.AgentPhase_AGENT_PHASE_UNSPECIFIED {
		t.Fatalf("after the direct task %v", after)
	}
	waitFor(t, "the task delivered", func() bool { return w.delivered(t, a.Id, res.Msg.Message.Id) })
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	if _, err := asLead.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: a.Id, Next: true})); code(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "light") {
		t.Errorf("next from the end of the light chain moved it (to the planning tier's): %v", err)
	}
}

// A task the parent sends (not resume) moves the phase too: an approved worker given a follow-up that then sends a
// plan can be approved onto a tier again; a planner given a direct task loses its deny list.
func TestATaskSentByTheParentEndsThePlanPhase(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)

	// An executing worker: a follow-up task, then a plan, then approval onto another tier.
	a := w.approvedOnLight(t, asLead, "follow")
	task, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: a.Id,
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "Direct task (no plan): a follow-up"}))
	if err != nil {
		t.Fatal(err)
	}
	got := w.agent(a.Id)
	if got.Phase != v1.AgentPhase_AGENT_PHASE_UNSPECIFIED || got.Tier != "light" || got.ExecTier != "" || got.PendingReopen != nil {
		t.Fatalf("after a direct task %v", got)
	}
	if _, err := asLead.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: a.Id, Next: true})); code(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "light") {
		t.Errorf("next after the follow-up: %v", err)
	}
	waitFor(t, "the follow-up delivered", func() bool { return w.delivered(t, a.Id, task.Msg.Message.Id) })
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	if _, err := w.as(w.token(t, a.Id)).SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent",
		Type: v1.MessageType_MESSAGE_TYPE_PLAN, Body: "a plan for the follow-up"})); err != nil {
		t.Fatal(err)
	}
	if got := w.agent(a.Id); got.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING || got.Tier != "light" {
		t.Fatalf("after its plan %v", got)
	}
	ap, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: a.Id, Tier: "strong"}))
	if err != nil || ap.Msg.Reopen == nil || ap.Msg.Reopen.Model != "opus" {
		t.Fatalf("approving the follow-up's plan onto a tier: %v, %v", ap, err)
	}
	w.waitReopened(t, a.Id, a.TerminalId)
	waitFor(t, "the approval delivered", func() bool { return w.delivered(t, a.Id, ap.Msg.Message.Id) })
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)

	// A planner given a direct task is reopened without the planning deny list.
	res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: gitRepo(t), Slug: "planner",
		Tier: "strong", Plan: true}))
	if err != nil {
		t.Fatal(err)
	}
	p := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	if !p.DenyEdits {
		t.Fatalf("a planner %v", p)
	}
	task, err = asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: p.Id,
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "Direct task (no plan): just make the change"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := w.agent(p.Id); got.Phase != v1.AgentPhase_AGENT_PHASE_UNSPECIFIED || got.PendingReopen == nil ||
		got.PendingReopen.DenyEdits || got.PendingReopen.Model != "opus" || got.PendingReopen.Why != "direct task" {
		t.Fatalf("a planner given a direct task %v", got)
	}
	lifted := w.waitReopened(t, p.Id, p.TerminalId)
	if lifted.DenyEdits || lifted.Model != "opus" {
		t.Errorf("after the reopen %v", lifted)
	}
	all := w.launches(t, p.SessionId)
	if _, deny := flags(all[len(all)-1]); len(all) != 2 || deny {
		t.Errorf("launches %v", all)
	}
	waitFor(t, "the task delivered", func() bool { return w.delivered(t, p.Id, task.Msg.Message.Id) })
	w.waitState(p.Id, v1.AgentState_AGENT_STATE_IDLE)
}

// A model named with a tier leaves no tier to walk (the tier only gives the effort), a misspelled tier is
// refused whenever it is given, and a root needs neither (its model is the user's).
func TestANamedModelAlongsideATierAndARootsModel(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	for _, effort := range []string{"", "high"} {
		if _, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{RunId: run.Id, Repo: gitRepo(t),
			Slug: "typo", Model: "opus", Tier: "mdi", Effort: effort})); code(err) != connect.CodeInvalidArgument {
			t.Errorf("a named model with a misspelled tier (effort %q): %v", effort, err)
		}
	}
	res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{RunId: run.Id, Repo: gitRepo(t),
		Slug: "named", Model: "fable", Tier: "light"}))
	if err != nil {
		t.Fatal(err)
	}
	named := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	if named.Tier != "" || !named.Pinned || named.Model != "fable" || named.Effort != "medium" {
		t.Errorf("named %v", named)
	}
	if _, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: named.Id, Next: true})); code(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "named model") {
		t.Errorf("next from a named model: %v", err)
	}

	res, err = w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{RunId: run.Id, Repo: gitRepo(t),
		Slug: "lead", Role: v1.AgentRole_AGENT_ROLE_ROOT}))
	if err != nil {
		t.Fatalf("a root with no tier or model: %v", err)
	}
	root := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	if rootEffort, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{RunId: run.Id, Repo: gitRepo(t),
		Slug: "lead2", Role: v1.AgentRole_AGENT_ROLE_ROOT, Effort: "low"})); err != nil || rootEffort.Msg.Agent.Pinned {
		t.Errorf("a root with an effort only: %v, %v", rootEffort, err)
	} else {
		w.waitState(rootEffort.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	}
	all := w.launches(t, root.SessionId)
	if f, _ := flags(all[0]); len(all) != 1 || f["--model"] != "" || f["--effort"] != "" || root.Model != "" {
		t.Errorf("a root's launch %v, %v", all, root)
	}
}

// A reopen's new process that dies at once, after a newer request replaced the reopen, is not recorded as the
// agent's terminal: the newer reopen runs (it used to stay starting for good).
func TestAReopenWhoseProcessDiesWhileReplacedIsRunAgain(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	a := w.spawn(run.Id, gitRepo(t), "race", "")
	before := w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	events := w.collectEvents(t, run.Id)
	var once atomic.Bool
	daemon.OnLaunched(func(agentID string, die func() bool) {
		if agentID != a.Id || !once.CompareAndSwap(false, true) {
			return
		}
		// The second request lands while the first one's process starts; then that process dies.
		if _, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: a.Id, Effort: "max"})); err != nil {
			t.Error(err)
		}
		if !die() {
			t.Error("the relaunched process's exit was not seen within 10 s")
		}
	})
	t.Cleanup(func() { daemon.OnLaunched(nil) })
	if _, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: a.Id, Effort: "low"})); err != nil {
		t.Fatal(err)
	}
	after := w.waitReopened(t, a.Id, before.TerminalId)
	if after.Effort != "max" || after.PendingReopen != nil {
		t.Fatalf("after %v", after)
	}
	dropped := false
	for _, ev := range events() {
		if d := ev.GetAgentReopenDropped(); d != nil && strings.Contains(d.Reason, "exited before it was recorded") {
			dropped = true
		}
	}
	if !dropped {
		t.Error("the replaced reopen's log has no AgentReopenDropped")
	}
	if n := len(w.launches(t, before.SessionId)); n != 3 {
		t.Errorf("%d launches, want the first, the dead one and the replacement", n)
	}
}

// A plan from a worker that is executing (no task in between) enters the plan phase again, on the tier it runs
// on, so its approval can name an execution tier.
func TestAPlanFromAnExecutingWorkerEntersThePlanPhaseAgain(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	a := w.approvedOnLight(t, asLead, "again")
	if _, err := w.as(w.token(t, a.Id)).SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent",
		Type: v1.MessageType_MESSAGE_TYPE_PLAN, Body: "this needs a plan after all"})); err != nil {
		t.Fatal(err)
	}
	if got := w.agent(a.Id); got.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING || got.Tier != "light" || got.ExecTier != "" {
		t.Fatalf("after its plan %v", got)
	}
	ap, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: a.Id, Tier: "strong"}))
	if err != nil || ap.Msg.Reopen == nil || ap.Msg.Reopen.Model != "opus" {
		t.Fatalf("approving it onto a tier: %v, %v", ap, err)
	}
	w.waitReopened(t, a.Id, a.TerminalId)
	waitFor(t, "the approval delivered", func() bool { return w.delivered(t, a.Id, ap.Msg.Message.Id) })
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
}

// Only a task marked direct ends a plan phase and lifts the deny list: a planned follow-up leaves a planner
// planning, under its list and with its preset execution tier. A device sending it is handled as the parent is.
func TestAPlannedFollowUpKeepsAPlannerPlanningAndADevicesDirectTaskLiftsItsList(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: gitRepo(t), Slug: "planner",
		Tier: "strong", Plan: true, ExecTier: "light"}))
	if err != nil {
		t.Fatal(err)
	}
	p := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	planned, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: p.Id,
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "also cover the cache: plan it first"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := w.agent(p.Id); got.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING || !got.DenyEdits || got.PendingReopen != nil ||
		got.ExecTier != "light" {
		t.Fatalf("a planned follow-up changed the planner: %v", got)
	}
	waitFor(t, "the follow-up delivered", func() bool { return w.delivered(t, p.Id, planned.Msg.Message.Id) })
	w.waitState(p.Id, v1.AgentState_AGENT_STATE_IDLE)
	direct, err := w.device.Mesh.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: p.Id,
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "Direct task (no plan): just make the change"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := w.agent(p.Id); got.Phase != v1.AgentPhase_AGENT_PHASE_UNSPECIFIED || got.PendingReopen == nil || got.PendingReopen.DenyEdits {
		t.Fatalf("a device's direct task: %v", got)
	}
	w.waitReopened(t, p.Id, p.TerminalId)
	waitFor(t, "the task delivered", func() bool { return w.delivered(t, p.Id, direct.Msg.Message.Id) })
	w.waitState(p.Id, v1.AgentState_AGENT_STATE_IDLE)
}

// A resume that plans names its tier or model: the tier an agent has recorded may be the one it executed on.
func TestResumePlanNeedsATierOrAModel(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	a := w.spawn(run.Id, gitRepo(t), "resumed", "")
	before := w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.typeInto(before, "/exit\r")
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_EXITED)
	if _, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Agent: a.Id, Task: "x", Plan: true})); code(err) != connect.CodeInvalidArgument ||
		!strings.Contains(err.Error(), "give a tier") {
		t.Errorf("a planned resume with no tier: %v", err)
	}
}

// A relaunch that exits at once is recorded as the agent's exit; a resume stored just after it (the agent is
// stopped, so it may be) must not make finishReopen record that dead process as the new terminal.
func TestAResumeStoredAfterARelaunchDiedDoesNotRecordTheDeadProcess(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	a := w.spawn(run.Id, gitRepo(t), "diesfirst", "")
	before := w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	events := w.collectEvents(t, run.Id)
	var once atomic.Bool
	daemon.OnLaunched(func(agentID string, die func() bool) {
		if agentID != a.Id || !once.CompareAndSwap(false, true) {
			return
		}
		if !die() { // recorded as the agent's exit
			t.Error("the relaunched process's exit was not seen within 10 s")
		}
		if _, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Agent: a.Id, Effort: "max"})); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(func() { daemon.OnLaunched(nil) })
	if _, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: a.Id, Effort: "low"})); err != nil {
		t.Fatal(err)
	}
	after := w.waitReopened(t, a.Id, before.TerminalId)
	if after.Effort != "max" {
		t.Errorf("after %v", after)
	}
	for _, ev := range events() {
		if r := ev.GetAgentReopened(); r != nil {
			if r.Reopen.Effort == "low" {
				t.Errorf("the dead relaunch was recorded as a reopen: %v", ev)
			}
		}
	}
}

// collectEvents follows a run's events from now; the function it returns stops the stream and gives them.
func (w *reopenWorld) collectEvents(t *testing.T, run string) func() []*v1.Event {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s, err := w.device.Mesh.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{RunId: run}))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []*v1.Event
	done := make(chan struct{})
	go func() {
		defer close(done)
		for s.Receive() {
			if ev := s.Msg().GetEvent(); ev != nil {
				mu.Lock()
				got = append(got, ev)
				mu.Unlock()
			}
		}
	}()
	t.Cleanup(cancel)
	return func() []*v1.Event {
		time.Sleep(200 * time.Millisecond)
		cancel()
		<-done
		mu.Lock()
		defer mu.Unlock()
		return append([]*v1.Event(nil), got...)
	}
}
