package daemon_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
)

// plainRoot starts a plain agent (the user's own conversation) in a run of its own, idle, with a client of its token.
func (w *resetWorld) plainRoot(t *testing.T) (*v1.Run, *v1.Agent, agentmeshv1connect.MeshServiceClient) {
	t.Helper()
	run := startRun(t, w.device.Mesh, "")
	res, err := w.device.Mesh.SpawnAgent(context.Background(), connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: gitRepo(t), Slug: "chat", Role: v1.AgentRole_AGENT_ROLE_ROOT, Kind: "claude", Plain: true}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Msg.Agent.Plain {
		t.Fatalf("spawned %v: not plain", res.Msg.Agent)
	}
	a := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	return run, a, w.as(w.token(t, a.Id))
}

func TestAPlainAgentIsARootWithNoTaskAndNoPlan(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	for name, req := range map[string]*v1.SpawnAgentRequest{
		"a worker":  {Role: v1.AgentRole_AGENT_ROLE_WORKER, Tier: "mid"},
		"a task":    {Role: v1.AgentRole_AGENT_ROLE_ROOT, Task: "do it"},
		"a planner": {Role: v1.AgentRole_AGENT_ROLE_ROOT, Plan: true, Tier: "strong"},
	} {
		req.RunId, req.Repo, req.Slug, req.Plain = run.Id, gitRepo(t), "p", true
		_, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(req))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "a plain agent is a root with no task") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTheMeshNeverResetsAPlainAgent(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{Every: 100 * time.Millisecond, Quiet: 300 * time.Millisecond, UserQuiet: 300 * time.Millisecond, Backoff: time.Hour})
	ctx := context.Background()
	spawnLive := func(as agentmeshv1connect.MeshServiceClient) {
		res, err := as.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: gitRepo(t), Slug: "w", Tier: "mid", Task: "Direct task (no plan): work"}))
		if err != nil {
			t.Fatal(err)
		}
		w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	}
	// A plain agent that started a worker of its own anyway, big and quiet: everything the keeper looks for.
	plainRun, plain, asPlain := w.plainRoot(t)
	spawnLive(asPlain)
	w.size(t, w.agent(plain.Id), 200000, hour, 0)
	// A lead in the same state, so the keeper is shown to be at work.
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	spawnLive(asLead)
	w.size(t, w.agent(lead.Id), 200000, hour, 0)
	w.waitReset(t, lead.Id, "done")
	time.Sleep(500 * time.Millisecond)
	if got := w.resetEvents(t, plainRun.Id, plain.Id); len(got) != 0 {
		t.Errorf("the keeper reset a plain agent: %v", got)
	}
	// Nor on request: not by the device, not by itself.
	for who, c := range map[string]agentmeshv1connect.MeshServiceClient{"device": w.device.Mesh, "itself": asPlain} {
		_, err := c.ResetRoot(ctx, connect.NewRequest(&v1.ResetRootRequest{RunId: plainRun.Id}))
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "the mesh never resets it") {
			t.Errorf("ResetRoot by the %s: %v", who, err)
		}
	}
}

func TestAPlainAgentGetsNoContextHintAndNoReminder(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	_, plain, asPlain := w.plainRoot(t)
	// Big and cold: a lead's command would say to reset now.
	w.size(t, w.agent(plain.Id), 200000, five, 10*time.Minute)
	res, err := asPlain.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if h := res.Header().Get(daemon.HintHeader); h != "" {
		t.Errorf("a plain agent's hint: %q", h)
	}
	// A compaction's SessionStart: no "you are the root of run …, run `mesh brief`".
	w.typeInto(w.agent(plain.Id), "/compact\r")
	waitFor(t, "the compaction's turn", func() bool { return strings.Contains(w.screen(t, w.agent(plain.Id)), "/compact") })
	time.Sleep(700 * time.Millisecond)
	if screen := w.screen(t, w.agent(plain.Id)); strings.Contains(screen, "agent-mesh reminder") {
		t.Errorf("a plain agent was reminded:\n%s", screen)
	}
}

func TestAResumedPlainAgentIsNotCompacted(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	_, plain, _ := w.plainRoot(t)
	w.size(t, w.agent(plain.Id), 400000, hour, 0)
	w.typeInto(w.agent(plain.Id), "/exit\r")
	w.waitState(plain.Id, v1.AgentState_AGENT_STATE_EXITED)
	if _, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Agent: plain.Id})); err != nil {
		t.Fatal(err)
	}
	w.waitReopened(t, plain.Id, plain.TerminalId)
	if n := len(w.compactions()); n != 0 {
		t.Errorf("a plain agent compacted on resume: %v", w.compactions())
	}
}
