package daemon_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// ListAgentWaits words every idle agent in one call: a declared wait, an answer owed, a report, nothing; an agent that is
// not idle is left out.
func TestListAgentWaitsSaysWhatEachIdleAgentWaitsOn(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	idle := w.spawnUnder(t, asLead, "idle", &v1.SpawnAgentRequest{})
	declared := w.spawnUnder(t, asLead, "declared", &v1.SpawnAgentRequest{})
	asked := w.spawnUnder(t, asLead, "asked", &v1.SpawnAgentRequest{})
	done := w.spawnUnder(t, asLead, "done", &v1.SpawnAgentRequest{})
	busy := w.spawnUnder(t, asLead, "busy", &v1.SpawnAgentRequest{})
	for _, a := range []*v1.Agent{idle, declared, asked, done, busy} {
		w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	}
	send := func(a *v1.Agent, typ v1.MessageType, body, until string) {
		t.Helper()
		if _, err := w.as(w.token(t, a.Id)).SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent", Type: typ, Body: body, Until: until})); err != nil {
			t.Fatal(err)
		}
	}
	send(declared, v1.MessageType_MESSAGE_TYPE_WAIT, "waiting for the build", "2h")
	send(asked, v1.MessageType_MESSAGE_TYPE_QUESTION, "which port?", "")
	w.report(t, done, v1.MessageType_MESSAGE_TYPE_DONE)
	w.setWorking(t, busy.Id)

	waits := func(run string) map[string]*v1.AgentWait {
		t.Helper()
		res, err := w.device.Mesh.ListAgentWaits(ctx, connect.NewRequest(&v1.ListAgentWaitsRequest{RunId: run}))
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]*v1.AgentWait{}
		for _, r := range res.Msg.Waits {
			out[r.AgentId] = r.Wait
		}
		return out
	}
	for _, run := range []string{"", run.Id} {
		got := waits(run)
		for _, c := range []struct {
			a    *v1.Agent
			kind v1.AgentWaitKind
		}{
			{idle, v1.AgentWaitKind_AGENT_WAIT_KIND_MESSAGE},
			{declared, v1.AgentWaitKind_AGENT_WAIT_KIND_DECLARED},
			{asked, v1.AgentWaitKind_AGENT_WAIT_KIND_ANSWER},
			{done, v1.AgentWaitKind_AGENT_WAIT_KIND_REPORTED},
		} {
			if g := got[c.a.Id]; g == nil || g.Kind != c.kind {
				t.Errorf("run %q: %s waits %v, want kind %v", run, c.a.Name, g, c.kind)
			}
		}
		if g := got[declared.Id]; g != nil && (g.Detail != "waiting for the build" || g.Until == nil) {
			t.Errorf("run %q: the declared wait is %v, want its body and deadline", run, g)
		}
		if g := got[idle.Id]; g != nil && g.From != lead.Name {
			t.Errorf("run %q: an idle worker waits for %q, want its lead %q", run, g.From, lead.Name)
		}
		if _, ok := got[busy.Id]; ok {
			t.Errorf("run %q: a working agent has a wait: %v", run, got[busy.Id])
		}
	}
	// Each row is what GetAgentProgress says for the agent.
	one, err := w.device.Mesh.GetAgentProgress(ctx, connect.NewRequest(&v1.GetAgentProgressRequest{Agent: declared.Id}))
	if err != nil || one.Msg.Wait.Kind != waits("")[declared.Id].Kind {
		t.Errorf("progress says %v (%v), the list %v", one, err, waits("")[declared.Id])
	}
	// A run that does not exist lists nothing; a worker may read its own run only.
	if got := waits("nosuch"); len(got) != 0 {
		t.Errorf("an unknown run lists %v", got)
	}
	if _, err := w.as(w.token(t, idle.Id)).ListAgentWaits(ctx, connect.NewRequest(&v1.ListAgentWaitsRequest{RunId: "other"})); err == nil {
		t.Error("a worker read another run's waits")
	}
}
