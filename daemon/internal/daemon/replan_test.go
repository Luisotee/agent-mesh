package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
)

// hasPair reports whether argv holds the flag with the value after it.
func hasPair(argv []string, flag, value string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == value {
			return true
		}
	}
	return false
}

// executingWorker spawns a worker that planned on strong and was approved to execute on mid: the starting point of a
// re-plan. It returns the worker as it runs after the approval's reopen.
func (w *reopenWorld) executingWorker(t *testing.T, asLead agentmeshv1connect.MeshServiceClient) *v1.Agent {
	t.Helper()
	ctx := context.Background()
	res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker", Repo: gitRepo(t), Slug: "fix",
		Tier: "strong", Plan: true, Task: "plan the fix"}))
	if err != nil {
		t.Fatal(err)
	}
	worker := res.Msg.Agent
	before := w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	asWorker := w.as(w.token(t, worker.Id))
	readInbox(t, asWorker)
	if _, err := asWorker.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent", Type: v1.MessageType_MESSAGE_TYPE_PLAN, Body: "the plan"})); err != nil {
		t.Fatal(err)
	}
	ap, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: worker.Id, Tier: "mid"}))
	if err != nil {
		t.Fatal(err)
	}
	w.waitReopened(t, worker.Id, before.TerminalId)
	// Its approval rings in the new process: wait for that turn to end before anything else is typed or sent.
	waitFor(t, "the approval delivered", func() bool { return w.delivered(t, worker.Id, ap.Msg.Message.Id) })
	return w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
}

// A task with `plan` sends a worker that is executing back to planning in its own conversation: the plan tier's model,
// the planning deny list and Claude's plan mode on a reopen; approve then moves it back to the tier it was executing on.
func TestATaskWithPlanSendsAnExecutingWorkerBackToPlanning(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	worker := w.executingWorker(t, asLead)
	if worker.Phase != v1.AgentPhase_AGENT_PHASE_EXECUTING || worker.Model != "sonnet" || worker.Tier != "strong" || worker.ExecTier != "mid" {
		t.Fatalf("executing worker %v", worker)
	}

	res, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: worker.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK,
		Body: "Should we move the store to another database? Think it through.", Plan: true}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Msg.Message.Body, "This task is a re-plan") || !strings.Contains(res.Msg.Message.Body, "ExitPlanMode") {
		t.Errorf("the task body: %q", res.Msg.Message.Body)
	}
	after := w.waitReopened(t, worker.Id, worker.TerminalId)
	if after.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING || !after.DenyEdits || after.Model != "opus" || after.Effort != "high" ||
		after.Tier != "strong" || after.ExecTier != "mid" || after.SessionId != worker.SessionId {
		t.Fatalf("after the re-plan %v", after)
	}
	all := w.launches(t, worker.SessionId)
	last := all[len(all)-1]
	if f, deny := flags(last); f["--resume"] != worker.SessionId || f["--model"] != "opus" || !deny || !hasPair(last, "--permission-mode", "plan") {
		t.Errorf("the re-plan's launch %v", last)
	}
	waitFor(t, "the task delivered", func() bool { return w.delivered(t, worker.Id, res.Msg.Message.Id) })

	// Approved again, it goes back to the tier it was executing on, with no tier named and no plan mode.
	asWorker := w.as(w.token(t, worker.Id))
	readInbox(t, asWorker)
	if _, err := asWorker.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent", Type: v1.MessageType_MESSAGE_TYPE_PLAN, Body: "the new plan"})); err != nil {
		t.Fatal(err)
	}
	ap, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: worker.Id}))
	if err != nil {
		t.Fatal(err)
	}
	if r := ap.Msg.Reopen; r == nil || r.Model != "sonnet" || r.DenyEdits {
		t.Fatalf("approve reopen %v", r)
	}
	done := w.waitReopened(t, worker.Id, after.TerminalId)
	if done.Phase != v1.AgentPhase_AGENT_PHASE_EXECUTING || done.DenyEdits || done.ExecTier != "mid" {
		t.Errorf("after approval %v", done)
	}
	all = w.launches(t, worker.SessionId)
	if last := all[len(all)-1]; hasPair(last, "--permission-mode", "plan") {
		t.Errorf("the execution launch is still in plan mode: %v", last)
	}
}

// An effort alone is a step up on the model the worker is on, with no change of model.
func TestAReplanWithAnEffortOnlyKeepsTheModel(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	worker := w.executingWorker(t, asLead)
	if _, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: worker.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK,
		Body: "Think about the edge cases of this change.", Plan: true, Effort: "max"})); err != nil {
		t.Fatal(err)
	}
	after := w.waitReopened(t, worker.Id, worker.TerminalId)
	if after.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING || !after.DenyEdits || after.Model != "sonnet" || after.Effort != "max" || after.ExecTier != "mid" {
		t.Fatalf("after %v", after)
	}
}

// A stopped worker is revived into planning by the same send, and a named tier is the one it plans on.
func TestAReplanTaskToAStoppedWorkerResumesItPlanning(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	worker := w.executingWorker(t, asLead)
	w.typeInto(worker, "/exit\r")
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_EXITED)
	res, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: worker.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK,
		Body: "Design the rewrite and give options.", Plan: true, Tier: "top"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.Resumed == nil || !res.Msg.Resumed.DenyEdits {
		t.Fatalf("resumed %v", res.Msg.Resumed)
	}
	after := w.waitReopened(t, worker.Id, worker.TerminalId)
	if after.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING || !after.DenyEdits || after.Tier != "top" || after.ExecTier != "mid" {
		t.Fatalf("after %v", after)
	}
}

// What a re-plan can't be.
func TestAReplanIsRefusedWhereItMakesNoSense(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	worker := w.executingWorker(t, asLead)
	send := func(c agentmeshv1connect.MeshServiceClient, to string, req *v1.SendMessageRequest) error {
		req.To = to
		_, err := c.SendMessage(ctx, connect.NewRequest(req))
		return err
	}
	task := v1.MessageType_MESSAGE_TYPE_TASK
	for name, tc := range map[string]struct {
		c   agentmeshv1connect.MeshServiceClient
		to  string
		req *v1.SendMessageRequest
		is  connect.Code
	}{
		"a tier without plan":   {asLead, worker.Id, &v1.SendMessageRequest{Type: task, Body: "x", Tier: "strong"}, connect.CodeInvalidArgument},
		"an effort without it":  {asLead, worker.Id, &v1.SendMessageRequest{Type: task, Body: "x", Effort: "high"}, connect.CodeInvalidArgument},
		"not a task":            {asLead, worker.Id, &v1.SendMessageRequest{Type: v1.MessageType_MESSAGE_TYPE_INFO, Body: "x", Plan: true}, connect.CodeInvalidArgument},
		"a direct task":         {asLead, worker.Id, &v1.SendMessageRequest{Type: task, Body: "Direct task (no plan): x", Plan: true}, connect.CodeInvalidArgument},
		"a bad effort":          {asLead, worker.Id, &v1.SendMessageRequest{Type: task, Body: "x", Plan: true, Effort: "huge"}, connect.CodeInvalidArgument},
		"held for another task": {asLead, worker.Id, &v1.SendMessageRequest{Type: task, Body: "x", Plan: true, After: "m1"}, connect.CodeInvalidArgument},
		"a root":                {w.device.Mesh, lead.Id, &v1.SendMessageRequest{Type: task, Body: "x", Plan: true}, connect.CodeInvalidArgument},
	} {
		if err := send(tc.c, tc.to, tc.req); code(err) != tc.is {
			t.Errorf("%s: %v, want %v", name, err, tc.is)
		}
	}
	// Nothing changed on the worker.
	if a := w.agent(worker.Id); a.Phase != v1.AgentPhase_AGENT_PHASE_EXECUTING || a.PendingReopen != nil {
		t.Errorf("a refused re-plan changed the worker: %v", a)
	}
	// The worker can't send itself back to planning: it sends to its parent only.
	if err := send(w.as(w.token(t, worker.Id)), lead.Id, &v1.SendMessageRequest{Type: task, Body: "x", Plan: true}); err == nil {
		t.Errorf("a worker's re-plan task to its parent was accepted")
	}
}

// A planner in Claude's plan mode: its ExitPlanMode is its plan report to the parent (and a file), the hook answers
// with a refusal that says so, and the plan is then approved as any other.
func TestAnExitPlanModeFromAPlannerIsItsPlanReport(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker", Repo: gitRepo(t), Slug: "plan",
		Tier: "strong", Plan: true, Task: "plan the fix"}))
	if err != nil {
		t.Fatal(err)
	}
	worker := res.Msg.Agent
	before := w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	waitFor(t, "its task delivered", func() bool {
		msgs, _ := w.d.Store().Read(ctx).Inbox(worker.Id, true)
		return len(msgs) > 0 && msgs[0].DeliveredAt != nil
	})
	before = w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	readInbox(t, w.as(w.token(t, worker.Id)))
	w.typeInto(before, "/exitplan\r")
	var plan *v1.Message
	waitFor(t, "the plan report", func() bool {
		msgs, _ := w.d.Store().Read(ctx).Inbox(lead.Id, true)
		for _, m := range msgs {
			if m.Type == v1.MessageType_MESSAGE_TYPE_PLAN && m.From.Id == worker.Id {
				plan = m
				return true
			}
		}
		return false
	})
	if !strings.Contains(plan.Body, "Create hello.txt") {
		t.Errorf("the plan report: %q", plan.Body)
	}
	file := filepath.Join(w.state, "runs", run.Id, "plans", worker.Name+".md")
	if b, err := os.ReadFile(file); err != nil || !strings.Contains(string(b), "Create hello.txt") {
		t.Errorf("the plan file %s: %q %v", file, b, err)
	}
	// The prompt was the daemon's to answer: it is not listed for the user.
	if ps, err := w.device.Mesh.ListPermissions(ctx, connect.NewRequest(&v1.ListPermissionsRequest{})); err == nil {
		for _, p := range ps.Msg.Permissions {
			if p.AgentId == worker.Id {
				t.Errorf("the planner's ExitPlanMode was listed as a prompt: %v", p)
			}
		}
	}
	if _, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: worker.Id, Tier: "mid"})); err != nil {
		t.Errorf("approving the reported plan: %v", err)
	}
}
