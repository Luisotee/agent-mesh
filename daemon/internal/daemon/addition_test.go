package daemon_test

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// setWorking marks the agent as in a turn, as its hooks do while it works.
func (w *reopenWorld) setWorking(t *testing.T, id string) {
	t.Helper()
	w.setState(t, id, v1.AgentState_AGENT_STATE_WORKING)
}

func (w *reopenWorld) setState(t *testing.T, id string, state v1.AgentState) {
	t.Helper()
	if err := w.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		a, err := tx.GetAgent(id)
		if err != nil {
			return err
		}
		a.State = state
		return tx.PutAgent(a)
	}); err != nil {
		t.Fatal(err)
	}
}

// A `Direct task` sent to a planner that is still making its plan (a turn running, no plan or question yet) is an
// addition to that plan: it ends no phase and reopens nothing (ui-overhaul lost its plan phase to one). After the
// plan, or when its turn has ended, it is the parent's choice to skip the plan, as before.
func TestADirectTaskAddedToAPlannerStillPlanningChangesNoPhase(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	p := w.spawnUnder(t, asLead, "planner", &v1.SpawnAgentRequest{Tier: "strong", Plan: true})
	w.waitState(p.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.setWorking(t, p.Id)
	res, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: p.Id,
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "Direct task (no plan): also cover the cache"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.Message == nil {
		t.Fatal("the addition was not stored")
	}
	got := w.agent(p.Id)
	if got.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING || !got.DenyEdits || got.PendingReopen != nil {
		t.Fatalf("an addition changed the planner: phase %v deny %v reopen %v", got.Phase, got.DenyEdits, got.PendingReopen)
	}
	// The planner is told why its task said "no plan" and it is still planning.
	msgs, err := w.d.Store().Read(ctx).Inbox(p.Id, true)
	if err != nil {
		t.Fatal(err)
	}
	noted := false
	for _, m := range msgs {
		noted = noted || (m.Type == v1.MessageType_MESSAGE_TYPE_INFO && strings.Contains(m.Body, "still planning"))
	}
	if !noted {
		t.Errorf("the planner was not told its direct task is an addition: %v", msgs)
	}
	// Its plan is in: now a direct task is the parent skipping the rest of the planning.
	readInbox(t, w.as(w.token(t, p.Id)))
	w.report(t, got, v1.MessageType_MESSAGE_TYPE_PLAN)
	w.setWorking(t, p.Id)
	if _, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: p.Id,
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "Direct task (no plan): just make the change"})); err != nil {
		t.Fatal(err)
	}
	if got := w.agent(p.Id); got.Phase != v1.AgentPhase_AGENT_PHASE_UNSPECIFIED || got.PendingReopen == nil || got.PendingReopen.DenyEdits {
		t.Fatalf("a direct task after the plan: phase %v reopen %v", got.Phase, got.PendingReopen)
	}
}

// A planner that is starting, held at a permission prompt, or idle with a follow-up task unread, has not begun its plan either: a direct task is an
// addition to it too, not the end of its plan phase.
func TestADirectTaskToAPlannerThatHasNotReadItsTaskIsAnAddition(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	for i, state := range []v1.AgentState{v1.AgentState_AGENT_STATE_STARTING, v1.AgentState_AGENT_STATE_NEEDS_INPUT, v1.AgentState_AGENT_STATE_IDLE} {
		p := w.spawnUnder(t, asLead, "planner-"+string(rune('a'+i)), &v1.SpawnAgentRequest{Tier: "strong", Plan: true})
		w.waitState(p.Id, v1.AgentState_AGENT_STATE_IDLE)
		if state != v1.AgentState_AGENT_STATE_IDLE { // starting, or held at a permission prompt mid-plan
			w.setState(t, p.Id, state)
		} else {
			// Idle with a follow-up it has not read (the fake never runs `mesh inbox`): the plan it makes is not begun.
			if _, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: p.Id,
				Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "plan the cache first"})); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: p.Id,
			Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "Direct task (no plan): also cover the cache"})); err != nil {
			t.Fatal(err)
		}
		if got := w.agent(p.Id); got.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING || !got.DenyEdits || got.PendingReopen != nil {
			t.Errorf("%v: an addition changed the planner: phase %v deny %v reopen %v", state, got.Phase, got.DenyEdits, got.PendingReopen)
		}
	}
}

// Approving a planner that has sent no plan is the parent skipping its plan: allowed, and the answer says so. An
// agent with no plan phase has nothing to skip.
func TestApprovingAPlannerWithNoPlanSkipsItsPlan(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	p := w.spawnUnder(t, asLead, "planner", &v1.SpawnAgentRequest{Tier: "strong", Plan: true})
	w.waitState(p.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.setWorking(t, p.Id)
	ap, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: p.Id, Tier: "mid"}))
	if err != nil {
		t.Fatalf("approve before the plan: %v", err)
	}
	if body := ap.Msg.Message.Body; !strings.Contains(body, "plan phase is skipped") || !strings.HasPrefix(body, "approved") {
		t.Errorf("the answer does not say the plan is skipped: %q", body)
	}
	if got := w.agent(p.Id); got.Phase != v1.AgentPhase_AGENT_PHASE_EXECUTING || got.ExecTier != "mid" {
		t.Errorf("after the skip: phase %v tier %q", got.Phase, got.ExecTier)
	}
	// No plan phase, no plan: nothing to approve.
	plain := w.spawnUnder(t, asLead, "plain", &v1.SpawnAgentRequest{Tier: "mid"})
	w.waitState(plain.Id, v1.AgentState_AGENT_STATE_IDLE)
	if _, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: plain.Id})); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("approve an agent with no plan phase: %v", err)
	}
}
