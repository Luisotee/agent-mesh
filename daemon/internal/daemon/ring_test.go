package daemon_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
)

func TestRingAgentRingsUnreadRespectsDraftGuardAndHandlesEmptyInbox(t *testing.T) {
	w, log := boxWorld(t, nil, func(c *daemon.AgentConfig) {
		c.DeliveryDelay = 10 * time.Millisecond
	})
	w.set(t, "delivery.confirm_after", 0.2)
	w.set(t, "delivery.ring_tries", 2)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	a := w.spawnUnder(t, asLead, "ringme", &v1.SpawnAgentRequest{About: "test worker"})
	other := w.spawnUnder(t, asLead, "other", &v1.SpawnAgentRequest{About: "test worker"})

	// Sibling cannot ring another agent.
	if _, err := w.as(w.token(t, other.Id)).RingAgent(ctx, connect.NewRequest(&v1.RingAgentRequest{
		Meta: meta(""), Agent: a.Id,
	})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("sibling RingAgent = %v, want PermissionDenied", err)
	}

	// No unread mail -> unread=0, rung=false, no error.
	emptyRes, err := asLead.RingAgent(ctx, connect.NewRequest(&v1.RingAgentRequest{
		Meta: meta("ring-empty"), Agent: a.Name,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if emptyRes.Msg.Unread != 0 || emptyRes.Msg.Rung || emptyRes.Msg.Error != "" {
		t.Fatalf("empty RingAgent = %+v", emptyRes.Msg)
	}

	idleCount := func() int {
		return w.count(t, run.Id, func(ev *v1.Event) bool {
			ch := ev.GetAgentStateChanged()
			return ev.AgentId == a.Id && ch != nil && ch.To == v1.AgentState_AGENT_STATE_IDLE
		})
	}
	waitTurnSettled := func(prevIdle int) {
		waitFor(t, "turn finished back to idle", func() bool {
			return idleCount() > prevIdle && w.agent(a.Id).State == v1.AgentState_AGENT_STATE_IDLE
		})
	}

	// Send a message, then wait until its doorbell turn finishes back to idle; even when delivered, if unread, RingAgent rings again.
	beforeMsgIdle := idleCount()
	msgRes, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{
		Meta: meta(""), To: a.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "please reply",
	}))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "initial doorbell", func() bool { return w.delivered(t, a.Id, msgRes.Msg.Message.Id) })
	waitTurnSettled(beforeMsgIdle)
	beforeCount := len(doorbells(lines(t, log)))
	beforeRingIdle := idleCount()

	ringRes, err := asLead.RingAgent(ctx, connect.NewRequest(&v1.RingAgentRequest{
		Meta: meta("ring-1"), Agent: a.Name,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !ringRes.Msg.Rung || ringRes.Msg.Unread != 1 || ringRes.Msg.Via == "" || ringRes.Msg.Error != "" {
		t.Fatalf("RingAgent = %+v", ringRes.Msg)
	}
	waitFor(t, "manual ring reached fake claude", func() bool {
		return len(doorbells(lines(t, log))) > beforeCount
	})
	waitTurnSettled(beforeRingIdle)

	// Human draft in the box holds RingAgent without typing.
	w.typeInto(a, "half typed draft")
	waitFor(t, "draft on screen", func() bool { return strings.Contains(w.screen(t, a), "half typed draft") })
	heldBefore := len(doorbells(lines(t, log)))
	heldRes, err := asLead.RingAgent(ctx, connect.NewRequest(&v1.RingAgentRequest{
		Meta: meta("ring-draft"), Agent: a.Name,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if heldRes.Msg.Rung || heldRes.Msg.Error != "human_draft" || heldRes.Msg.Unread != 1 {
		t.Fatalf("draft RingAgent = %+v, want human_draft", heldRes.Msg)
	}
	if len(doorbells(lines(t, log))) != heldBefore {
		t.Fatalf("RingAgent typed into a human draft!")
	}
	w.typeInto(a, "\x15") // Ctrl-U clears the draft

	// Stopped agent cannot be rung.
	if _, err := asLead.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{
		Meta: meta(""), Agent: a.Id,
	})); err != nil {
		t.Fatal(err)
	}
	stoppedRes, err := asLead.RingAgent(ctx, connect.NewRequest(&v1.RingAgentRequest{
		Meta: meta("ring-retired"), Agent: a.Name,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if stoppedRes.Msg.Rung || !strings.Contains(stoppedRes.Msg.Error, "retired") {
		t.Fatalf("retired RingAgent = %+v", stoppedRes.Msg)
	}
}
