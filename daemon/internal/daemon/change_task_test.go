package daemon_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
)

// An asker or reviewer given a change to make becomes the worker for it, in the same conversation (PR "asker
// parity"): its role and its tokens' class change, and the deny list goes by the usual paths (approve, a direct
// task, a resume).

// startAsker starts an agent of a read-only role under a root, idle on its question.
func startAsker(t *testing.T, w *reopenWorld, asLead agentmeshv1connect.MeshServiceClient, role v1.AgentRole, slug string) *v1.Agent {
	t.Helper()
	res, err := asLead.SpawnAgent(context.Background(), connect.NewRequest(&v1.SpawnAgentRequest{Repo: gitRepo(t), Slug: slug,
		Role: role, Tier: "mid", Task: "Question (read-only): where is the thing?"}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	if a.Role != role || !a.DenyEdits {
		t.Fatalf("started %v", a)
	}
	return a
}

// classOf is the class the agent's current token resolves to.
func classOf(t *testing.T, w *reopenWorld, id string) v1.ActorClass {
	t.Helper()
	res, err := w.as(w.token(t, id)).Hello(context.Background(), connect.NewRequest(&v1.HelloRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.Actor.Class
}

func sendTask(t *testing.T, c agentmeshv1connect.MeshServiceClient, to, body string) *v1.SendMessageResponse {
	t.Helper()
	res, err := c.SendMessage(context.Background(), connect.NewRequest(&v1.SendMessageRequest{To: to,
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: body}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg
}

func roleEvents(t *testing.T, w *reopenWorld, run, agent string) []*v1.AgentRoleChanged {
	t.Helper()
	evs, err := w.d.Store().Read(context.Background()).EventsAfter(0, run, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []*v1.AgentRoleChanged
	for _, e := range evs {
		if e.AgentId == agent && e.GetAgentRoleChanged() != nil {
			out = append(out, e.GetAgentRoleChanged())
		}
	}
	return out
}

func TestAnAskerOrReviewerGivenAPlannedChangeBecomesAWorkerPlanningThenExecutes(t *testing.T) {
	for name, role := range map[string]v1.AgentRole{"asker": v1.AgentRole_AGENT_ROLE_ASKER, "reviewer": v1.AgentRole_AGENT_ROLE_REVIEWER} {
		t.Run(name, func(t *testing.T) {
			w := startReopen(t)
			ctx := context.Background()
			run := startRun(t, w.device.Mesh, "")
			_, asLead := w.root(t, run.Id)
			a := startAsker(t, w, asLead, role, "q")
			if classOf(t, w, a.Id) == v1.ActorClass_ACTOR_CLASS_WORKER {
				t.Fatal("started as a worker")
			}
			sid := a.SessionId

			// A read-only follow-up changes nothing.
			sendTask(t, asLead, a.Id, "Question (read-only): and the other thing?")
			if got := w.agent(a.Id); got.Role != role || got.Phase != v1.AgentPhase_AGENT_PHASE_UNSPECIFIED || got.PendingReopen != nil {
				t.Fatalf("after a read-only task %v", got)
			}

			// A planned change: a worker planning under the list it already has, no reopen.
			sendTask(t, asLead, a.Id, "Change the thing: plan it first.")
			got := w.agent(a.Id)
			if got.Role != v1.AgentRole_AGENT_ROLE_WORKER || got.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING || !got.DenyEdits ||
				got.PendingReopen != nil || got.Tier != "mid" {
				t.Fatalf("after a planned change %v", got)
			}
			if c := classOf(t, w, a.Id); c != v1.ActorClass_ACTOR_CLASS_WORKER {
				t.Errorf("the live token's class %v", c)
			}
			if evs := roleEvents(t, w, run.Id, a.Id); len(evs) != 1 || evs[0].From != role || evs[0].To != v1.AgentRole_AGENT_ROLE_WORKER {
				t.Errorf("role events %v", evs)
			}

			// Its plan, approved on an execution tier: reopened without the list, as a worker with a worker's token.
			asA := w.as(w.token(t, a.Id))
			readInbox(t, asA)
			if _, err := asA.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent",
				Type: v1.MessageType_MESSAGE_TYPE_PLAN, Body: "the plan"})); err != nil {
				t.Fatal(err)
			}
			ap, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: a.Id, Tier: "mid"}))
			if err != nil {
				t.Fatal(err)
			}
			if r := ap.Msg.Reopen; r == nil || r.DenyEdits {
				t.Fatalf("reopen %v", ap.Msg.Reopen)
			}
			after := w.waitReopened(t, a.Id, got.TerminalId)
			if after.Role != v1.AgentRole_AGENT_ROLE_WORKER || after.DenyEdits || after.Phase != v1.AgentPhase_AGENT_PHASE_EXECUTING ||
				after.SessionId != sid {
				t.Fatalf("after the reopen %v", after)
			}
			all := w.launches(t, sid)
			if f, deny := flags(all[len(all)-1]); len(all) != 2 || f["--resume"] != sid || deny {
				t.Errorf("launches %v", all)
			}
			if c := classOf(t, w, a.Id); c != v1.ActorClass_ACTOR_CLASS_WORKER {
				t.Errorf("the new token's class %v", c)
			}
		})
	}
}

func TestADirectChangeTaskLiftsAnAskersListByAReopenAndAStoppedOneResumesWithout(t *testing.T) {
	w := startReopen(t)
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)

	live := startAsker(t, w, asLead, v1.AgentRole_AGENT_ROLE_ASKER, "live")
	msg := sendTask(t, asLead, live.Id, "Direct task (no plan): make the change.")
	if got := w.agent(live.Id); got.Role != v1.AgentRole_AGENT_ROLE_WORKER || got.PendingReopen == nil || got.PendingReopen.DenyEdits {
		t.Fatalf("after the task %v", got)
	}
	after := w.waitReopened(t, live.Id, live.TerminalId)
	if after.DenyEdits || after.Phase != v1.AgentPhase_AGENT_PHASE_UNSPECIFIED {
		t.Fatalf("after the reopen %v", after)
	}
	all := w.launches(t, live.SessionId)
	if f, deny := flags(all[len(all)-1]); len(all) != 2 || f["--resume"] != live.SessionId || deny {
		t.Errorf("launches %v", all)
	}
	waitFor(t, "the task delivered", func() bool { return w.delivered(t, live.Id, msg.Message.Id) })

	// A retired one: the task resumes it, without the list.
	gone := startAsker(t, w, asLead, v1.AgentRole_AGENT_ROLE_REVIEWER, "gone")
	if _, err := asLead.RetireAgent(context.Background(), connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: gone.Id})); err != nil {
		t.Fatal(err)
	}
	w.waitState(gone.Id, v1.AgentState_AGENT_STATE_RETIRED)
	res := sendTask(t, asLead, gone.Id, "Direct task: make the change.")
	if res.Resumed == nil || res.Resumed.DenyEdits {
		t.Fatalf("resumed %v (%s)", res.Resumed, res.ResumeError)
	}
	back := w.waitReopened(t, gone.Id, gone.TerminalId)
	if back.Role != v1.AgentRole_AGENT_ROLE_WORKER || back.DenyEdits {
		t.Fatalf("after the resume %v", back)
	}
	all = w.launches(t, gone.SessionId)
	if f, deny := flags(all[len(all)-1]); len(all) != 2 || f["--resume"] != gone.SessionId || deny {
		t.Errorf("launches %v", all)
	}
}

// A refused resume keeps the conversion and the stored task; a direct task has no plan phase and no list, so the
// worker that next resumes does the task with the means to do it (bin/mesh's unresumed_task).
func TestARefusedResumeOfADirectChangeTaskLeavesTheWorkerWithoutItsList(t *testing.T) {
	w := startReopen(t)
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	gone := startAsker(t, w, asLead, v1.AgentRole_AGENT_ROLE_REVIEWER, "gone")
	if _, err := asLead.RetireAgent(context.Background(), connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: gone.Id})); err != nil {
		t.Fatal(err)
	}
	w.waitState(gone.Id, v1.AgentState_AGENT_STATE_RETIRED)
	w.set(t, "caps.agents", 1) // the root alone fills it
	res := sendTask(t, asLead, gone.Id, "Direct task: make the change.")
	if res.Resumed != nil || res.ResumeError == "" {
		t.Fatalf("resumed %v (%q)", res.Resumed, res.ResumeError)
	}
	got := w.agent(gone.Id)
	if got.Role != v1.AgentRole_AGENT_ROLE_WORKER || got.DenyEdits || got.Phase != v1.AgentPhase_AGENT_PHASE_UNSPECIFIED {
		t.Fatalf("after the refusal %v", got)
	}
}

func TestAReadOnlyTaskKeepsTheAskerReadOnlyAndAWorkersAskerNeverChangesRole(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)

	// A worker's own asker: a worker never gets a change worker, so nothing it sends changes the role.
	res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: gitRepo(t), Slug: "wk", Tier: "mid",
		Task: "Direct task (no plan): do it."}))
	if err != nil {
		t.Fatal(err)
	}
	worker := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	asWorker := w.as(w.token(t, worker.Id))
	sub, err := asWorker.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: gitRepo(t), Slug: "sub",
		Role: v1.AgentRole_AGENT_ROLE_ASKER, Tier: "mid", Task: "Question (read-only): where?"}))
	if err != nil {
		t.Fatal(err)
	}
	asker := w.waitState(sub.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	sendTask(t, asWorker, asker.Id, "Question (read-only): and there?")
	// A plain task, even a direct one, is a follow-up for it: it stays read-only, its message stored and unrefused.
	msg := sendTask(t, asWorker, asker.Id, "Direct task: change it.")
	if got := w.agent(asker.Id); got.Role != v1.AgentRole_AGENT_ROLE_ASKER || !got.DenyEdits || got.PendingReopen != nil ||
		got.Phase != v1.AgentPhase_AGENT_PHASE_UNSPECIFIED {
		t.Errorf("after the task %v", got)
	}
	if c := classOf(t, w, asker.Id); c != v1.ActorClass_ACTOR_CLASS_ASKER {
		t.Errorf("class %v", c)
	}
	waitFor(t, "the task delivered", func() bool { return w.delivered(t, asker.Id, msg.Message.Id) })
	if evs := roleEvents(t, w, run.Id, asker.Id); len(evs) != 0 {
		t.Errorf("role events %v", evs)
	}
	// A worker is a worker whatever it is told: a question to it changes no role.
	sendTask(t, asLead, worker.Id, "Question (read-only): and you?")
	if got := w.agent(worker.Id); got.Role != v1.AgentRole_AGENT_ROLE_WORKER {
		t.Errorf("the worker's role %v", got.Role)
	}
}

// An asker started on a named model is pinned to it; a planned change task gives it a plan phase to approve into
// a tier, so it is no longer pinned: a plan it sends after that approval enters the phase again.
func TestAPinnedAskerGivenAPlannedChangeIsNoLongerPinnedAndAFollowUpPlanPlans(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: gitRepo(t), Slug: "named",
		Role: v1.AgentRole_AGENT_ROLE_ASKER, Model: "opus", Task: "Question (read-only): where is the thing?"}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	if !a.Pinned {
		t.Fatalf("started unpinned: %v", a)
	}
	sendTask(t, asLead, a.Id, "Change the thing: plan it first.")
	got := w.agent(a.Id)
	if got.Pinned || got.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING {
		t.Fatalf("after the planned change %v", got)
	}
	asA := w.as(w.token(t, a.Id))
	readInbox(t, asA)
	sendPlan := func(c agentmeshv1connect.MeshServiceClient) {
		t.Helper()
		if _, err := c.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent",
			Type: v1.MessageType_MESSAGE_TYPE_PLAN, Body: "the plan"})); err != nil {
			t.Fatal(err)
		}
	}
	sendPlan(asA)
	if _, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: a.Id, Tier: "mid"})); err != nil {
		t.Fatal(err)
	}
	after := w.waitReopened(t, a.Id, got.TerminalId)
	if after.Phase != v1.AgentPhase_AGENT_PHASE_EXECUTING {
		t.Fatalf("after the approval %v", after)
	}
	asB := w.as(w.token(t, a.Id)) // the reopen gave it a new token
	readInbox(t, asB)
	sendPlan(asB)
	if again := w.agent(a.Id); again.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING {
		t.Errorf("a follow-up plan left the phase %v", again.Phase)
	}
}
