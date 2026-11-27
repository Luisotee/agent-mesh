package daemon_test

import (
	"context"
	"os"
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

func TestAPlainAgentsFirstDoorbellNamesTheMessagingRulesNotTheRootCard(t *testing.T) {
	share := t.TempDir()
	w, log := boxWorld(t, map[string]string{}, func(c *daemon.AgentConfig) { c.Share = share })
	run := startRun(t, w.device.Mesh, "")
	res, err := w.device.Mesh.SpawnAgent(context.Background(), connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: gitRepo(t), Slug: "chat", Role: v1.AgentRole_AGENT_ROLE_ROOT, Kind: "claude", Plain: true}))
	if err != nil {
		t.Fatal(err)
	}
	send(t, w.device.Mesh, res.Msg.Agent.Id, v1.MessageType_MESSAGE_TYPE_INFO, "a note from the user")
	waitFor(t, "the doorbell", func() bool { return len(doorbells(lines(t, log))) >= 1 })
	d := doorbells(lines(t, log))[0]
	if !strings.Contains(d, "PROTOCOL.md") || strings.Contains(d, "ROOT_CARD.md") {
		t.Errorf("a plain agent's doorbell %q should name PROTOCOL.md and not ROOT_CARD.md", d)
	}
}

// Each plain agent is a run of its own, so the run's own check of a name would let two of one CLI in a repo share a
// name, and a tab: a second takes -2, a third -3, and a name an agent that was stopped had is free again.
func TestTwoPlainAgentsOfOneRepoAreNamedApart(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	repo := gitRepo(t)
	spawn := func() *v1.Agent {
		t.Helper()
		run := startRun(t, w.device.Mesh, "")
		res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
			RunId: run.Id, Repo: repo, Slug: "claude", Role: v1.AgentRole_AGENT_ROLE_ROOT, Kind: "claude", Plain: true}))
		if err != nil {
			t.Fatal(err)
		}
		return w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	}
	first, second, third := spawn(), spawn(), spawn()
	for i, a := range []*v1.Agent{first, second, third} {
		want := []string{"shop-api-claude", "shop-api-claude-2", "shop-api-claude-3"}[i]
		if a.Name != want || !strings.HasSuffix(a.Branch, "/"+strings.TrimPrefix(want, "shop-api-")) {
			t.Errorf("plain agent %d: name %q branch %q, want %q", i+1, a.Name, a.Branch, want)
		}
	}
	// Stopped, a name is free again; one of another repo never counted.
	if _, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta("stop"), RunId: second.RunId})); err != nil {
		t.Fatal(err)
	}
	if again := spawn(); again.Name != "shop-api-claude-2" {
		t.Errorf("after the second was stopped, a new one is %q, want shop-api-claude-2", again.Name)
	}
	other := startRun(t, w.device.Mesh, "")
	res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: other.Id, Repo: gitRepo(t), Slug: "claude", Role: v1.AgentRole_AGENT_ROLE_ROOT, Kind: "claude", Plain: true}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.Agent.Name != "shop-api-claude" {
		t.Errorf("a plain agent of another repo is %q, want shop-api-claude", res.Msg.Agent.Name)
	}
}

// Stopping a plain agent removes its worktree when it is clean and its branch when what it holds is in the default
// branch; a branch the default has not got stays, as does a worker's branch with commits of its own.
func TestStoppingAPlainAgentDeletesItsMergedBranch(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	branches := func(repo string) string { return runGit(t, repo, "branch", "--list", "mesh/*") }
	for _, tc := range []struct {
		name   string
		merged bool
	}{{"merged", true}, {"not merged", false}} {
		run, a, _ := w.plainRoot(t)
		runGit(t, a.Worktree, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "work")
		// The default branch as the repo last saw it: with the work in it, or where it started.
		onMain := runGit(t, a.Repo, "rev-parse", "main")
		if tc.merged {
			onMain = runGit(t, a.Worktree, "rev-parse", "HEAD")
		}
		runGit(t, a.Repo, "update-ref", "refs/remotes/origin/main", onMain)
		res, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta("stop-" + tc.name), RunId: run.Id}))
		if err != nil {
			t.Fatal(err)
		}
		_, gone := os.Stat(a.Worktree)
		kept := strings.Contains(branches(a.Repo), a.Branch)
		if !os.IsNotExist(gone) {
			t.Errorf("%s: its clean worktree is still there", tc.name)
		}
		if kept == tc.merged {
			t.Errorf("%s: branch kept = %v (outcomes %v)", tc.name, kept, res.Msg.Worktrees)
		}
	}
}
