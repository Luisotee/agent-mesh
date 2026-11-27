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
	if err := w.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		a, err := tx.GetAgent(id)
		if err != nil {
			return err
		}
		a.State = v1.AgentState_AGENT_STATE_WORKING
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
