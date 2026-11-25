package daemon_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

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
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "a follow-up"}))
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
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "just make the change"}))
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
	var once atomic.Bool
	daemon.OnLaunched(func(agentID string, die func()) {
		if agentID != a.Id || !once.CompareAndSwap(false, true) {
			return
		}
		// The second request lands while the first one's process starts; then that process dies.
		if _, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: a.Id, Effort: "max"})); err != nil {
			t.Error(err)
		}
		die()
	})
	t.Cleanup(func() { daemon.OnLaunched(nil) })
	if _, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: a.Id, Effort: "low"})); err != nil {
		t.Fatal(err)
	}
	after := w.waitReopened(t, a.Id, before.TerminalId)
	if after.Effort != "max" || after.PendingReopen != nil {
		t.Fatalf("after %v", after)
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
