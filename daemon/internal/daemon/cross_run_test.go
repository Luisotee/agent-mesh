package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

func agentClient(t *testing.T, w *reopenWorld, a *v1.Agent) agentmeshv1connect.MeshServiceClient {
	t.Helper()
	tok := auth.NewToken()
	if err := w.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		return tx.PutToken(auth.Hash(tok), store.TokenRecord{
			Class: daemon.ClassOf(a.Role), SubjectID: a.Id, Name: a.Name, RunID: a.RunId,
		})
	}); err != nil {
		t.Fatal(err)
	}
	return w.as(tok)
}

func TestRootCrossRunReadsAndFollowUpResume(t *testing.T) {
	ctx := context.Background()
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Usage = &daemon.UsageOps{DisableClaudeNetwork: true}
	})
	repo := gitRepo(t)
	w.userMCPServers(t, "context7", "playwright")

	// Run 1: lead + worker.
	run1 := startRun(t, w.device.Mesh, "")
	root1Res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
		RunId: run1.Id, Repo: repo, Slug: "lead", Role: v1.AgentRole_AGENT_ROLE_ROOT,
	}))
	if err != nil {
		t.Fatal(err)
	}
	root1 := w.waitState(root1Res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)

	w1Res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
		RunId: run1.Id, Repo: repo, Slug: "worker", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_WORKER,
		Mcp: []string{"playwright"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	w1 := w.waitState(w1Res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	sid := w1.SessionId

	// Commit work in run1 so branch stays when StopRun closes run1.
	if err := os.WriteFile(filepath.Join(w1.Worktree, "f.txt"), []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, w1.Worktree, "add", "f.txt")
	runGit(t, w1.Worktree, "commit", "-m", "work in run1")

	if _, err := w.device.Mesh.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{
		Meta: meta(""), To: w1.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "run1 task",
	})); err != nil {
		t.Fatal(err)
	}
	w.waitState(w1.Id, v1.AgentState_AGENT_STATE_IDLE)

	if _, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{
		Meta: meta(""), RunId: run1.Id,
	})); err != nil {
		t.Fatal(err)
	}

	// Run 2: new lead root2 and a read-only asker spawned by root2.
	run2 := startRun(t, w.device.Mesh, "")
	root2Res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
		RunId: run2.Id, Repo: repo, Slug: "lead", Role: v1.AgentRole_AGENT_ROLE_ROOT,
	}))
	if err != nil {
		t.Fatal(err)
	}
	root2 := w.waitState(root2Res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	root2Cli := agentClient(t, w, root2)

	askerRes, err := root2Cli.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
		RunId: run2.Id, Repo: repo, Slug: "asker", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_ASKER,
	}))
	if err != nil {
		t.Fatal(err)
	}
	asker := w.waitState(askerRes.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	askerCli := agentClient(t, w, asker)

	// 1. root2 lists all runs including closed run1; asker only sees run2.
	runsRoot, err := root2Cli.ListRuns(ctx, connect.NewRequest(&v1.ListRunsRequest{All: true}))
	if err != nil || len(runsRoot.Msg.Runs) != 2 {
		t.Fatalf("root ListRuns(--all) = %+v, err=%v", runsRoot, err)
	}
	runsAsker, err := askerCli.ListRuns(ctx, connect.NewRequest(&v1.ListRunsRequest{All: true}))
	if err != nil || len(runsAsker.Msg.Runs) != 1 || runsAsker.Msg.Runs[0].Id != run2.Id {
		t.Fatalf("asker ListRuns = %+v, err=%v", runsAsker, err)
	}

	// 2. root2 reads agents and messages of run1, and GetAgent for w1; asker is denied.
	agentsR1, err := root2Cli.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{RunId: run1.Id}))
	if err != nil || len(agentsR1.Msg.Agents) != 2 {
		t.Fatalf("root ListAgents(run1) = %+v, err=%v", agentsR1, err)
	}
	if _, err := askerCli.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{RunId: run1.Id})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("asker ListAgents(run1): %v", err)
	}
	msgsR1, err := root2Cli.ListMessages(ctx, connect.NewRequest(&v1.ListMessagesRequest{RunId: run1.Id}))
	if err != nil || len(msgsR1.Msg.Messages) == 0 {
		t.Fatalf("root ListMessages(run1) = %+v, err=%v", msgsR1, err)
	}
	// root2 omitting run_id defaults to its own run (run2), and an empty-run ROOT actor is refused.
	if msgsR2, err := root2Cli.ListMessages(ctx, connect.NewRequest(&v1.ListMessagesRequest{})); err != nil || len(msgsR2.Msg.Messages) != 0 {
		t.Errorf("root ListMessages(default=run2) = %+v, err=%v", msgsR2, err)
	}
	emptyRootCtx := auth.WithActor(ctx, &v1.Actor{
		Class: v1.ActorClass_ACTOR_CLASS_ROOT, Id: "a_empty", Name: "lead", RunId: "",
	})
	if _, err := w.d.ListMessages(emptyRootCtx, connect.NewRequest(&v1.ListMessagesRequest{RunId: run1.Id})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("empty-run root ListMessages(run1): %v, want PermissionDenied", err)
	}
	if _, err := w.d.ListMessages(emptyRootCtx, connect.NewRequest(&v1.ListMessagesRequest{})); err == nil {
		t.Errorf("expected empty-run root ListMessages(empty run_id) to fail")
	}
	gotW1, err := root2Cli.GetAgent(ctx, connect.NewRequest(&v1.GetAgentRequest{Agent: w1.Id}))
	if err != nil || gotW1.Msg.Agent.RunId != run1.Id {
		t.Fatalf("root GetAgent(w1) = %+v, err=%v", gotW1, err)
	}
	if _, err := askerCli.GetAgent(ctx, connect.NewRequest(&v1.GetAgentRequest{Agent: w1.Id})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("asker GetAgent(w1): %v", err)
	}

	// 3. root2 cannot resume run1's root1, nor target another run explicitly.
	if _, err := root2Cli.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Meta: meta(""), Agent: root1.Id,
	})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("resuming another run's root: %v", err)
	}
	if _, err := root2Cli.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Meta: meta(""), Agent: w1.Id, RunId: run1.Id,
	})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("resuming into closed run1: %v", err)
	}

	// 4. root2 resumes stopped w1 into run2: new agent in run2, same session & worktree, ParentId = root2.Id.
	rres, err := root2Cli.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Meta: meta(""), Agent: w1.Id, Task: "follow-up from run2",
	}))
	if err != nil {
		t.Fatalf("cross-run ResumeAgent: %v", err)
	}
	w2 := rres.Msg.Agent
	if w2.Id == w1.Id || w2.RunId != run2.Id || w2.ParentId != root2.Id ||
		w2.SessionId != sid || w2.ResumedFromRunId != run1.Id || w2.ResumedFromAgentId != w1.Id {
		t.Fatalf("unexpected cross-run agent: %+v", w2)
	}
	w2 = w.waitState(w2.Id, v1.AgentState_AGENT_STATE_IDLE)
	// The follow-up keeps the servers its spawn named: on the record and in the new launch's MCP file.
	if !slices.Equal(w2.Mcp, []string{"playwright"}) {
		t.Errorf("the cross-run agent's mcp %v", w2.Mcp)
	}
	if got := mcpFileServers(t, filepath.Join(w.state, "agents", w2.Id, "mcp.json")); !slices.Equal(got, []string{"context7", "playwright"}) {
		t.Errorf("the cross-run launch's MCP servers %v", got)
	}

	// 5. One-process-per-conversation: attempting to resume w1 again while w2 runs is refused.
	if _, err := root2Cli.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Meta: meta(""), Agent: w1.Id,
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "already running") {
		t.Errorf("expected same-conversation refusal, got %v", err)
	}

	// 6. StopRun(run1) again must not remove w2's active worktree.
	stopR1Again, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{
		Meta: meta(""), RunId: run1.Id,
	}))
	if err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, wt := range stopR1Again.Msg.Worktrees {
		if wt.AgentId == w1.Id && wt.BranchKept && strings.Contains(wt.Reason, "another run") {
			kept = true
		}
	}
	if !kept {
		t.Errorf("expected StopRun(run1) to keep w2's held worktree, got %+v", stopR1Again.Msg.Worktrees)
	}

	// 7. root2 can retire w2 in run2, while root2 still cannot retire/stop an agent in run1 directly.
	if _, err := root2Cli.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{
		Meta: meta(""), Agent: w2.Id,
	})); err != nil {
		t.Fatalf("RetireAgent(w2): %v", err)
	}
	w.waitState(w2.Id, v1.AgentState_AGENT_STATE_RETIRED)

	// 7b. Even after w2 is retired in open run2, StopRun(run1) must keep w2's worktree while run2 stays open.
	stopR1WhileR2Open, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{
		Meta: meta(""), RunId: run1.Id,
	}))
	if err != nil {
		t.Fatal(err)
	}
	keptRetired := false
	for _, wt := range stopR1WhileR2Open.Msg.Worktrees {
		if wt.AgentId == w1.Id && wt.BranchKept && strings.Contains(wt.Reason, "another") {
			keptRetired = true
		}
	}
	if !keptRetired {
		t.Errorf("expected StopRun(run1) to keep worktree held by retired w2 in open run2, got %+v", stopR1WhileR2Open.Msg.Worktrees)
	}
	if _, err := os.Stat(w2.Worktree); err != nil {
		t.Errorf("StopRun(run1) removed worktree held by open run2: stat=%v", err)
	}

	// 8. StopRun(run2) must remove w2's clean worktree now that run1 is closed and w1 is stopped.
	stopR2, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{
		Meta: meta(""), RunId: run2.Id,
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, wt := range stopR2.Msg.Worktrees {
		if strings.Contains(wt.Reason, "another run") {
			t.Errorf("StopRun(run2) falsely skipped worktree as busy: %+v", wt)
		}
	}
	if _, err := os.Stat(w2.Worktree); !os.IsNotExist(err) {
		t.Errorf("StopRun(run2) leaked follow-up worktree %s: stat=%v", w2.Worktree, err)
	}
}

func TestCrossRunResumeClosedRunsOnlyAndClosedBeforeOpenSearch(t *testing.T) {
	ctx := context.Background()
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Usage = &daemon.UsageOps{DisableClaudeNetwork: true}
	})
	repo := gitRepo(t)

	// Run 1 (closed): worker "repo-worker".
	run1 := startRun(t, w.device.Mesh, "")
	w1Res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
		RunId: run1.Id, Repo: repo, Slug: "worker", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_WORKER,
	}))
	if err != nil {
		t.Fatal(err)
	}
	w1 := w.waitState(w1Res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	if _, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{
		Meta: meta(""), RunId: run1.Id,
	})); err != nil {
		t.Fatal(err)
	}

	// Run 3 (open): another worker also named "repo-worker", stopped/retired inside open run3.
	run3 := startRun(t, w.device.Mesh, "")
	w3Res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
		RunId: run3.Id, Repo: repo, Slug: "worker", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_WORKER,
	}))
	if err != nil {
		t.Fatal(err)
	}
	w3 := w.waitState(w3Res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	w3 = retireStoppedWorker(t, w, w3)

	// Run 2 (open): lead root2.
	run2 := startRun(t, w.device.Mesh, "")
	root2Res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
		RunId: run2.Id, Repo: repo, Slug: "lead", Role: v1.AgentRole_AGENT_ROLE_ROOT,
	}))
	if err != nil {
		t.Fatal(err)
	}
	root2 := w.waitState(root2Res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	root2Cli := agentClient(t, w, root2)

	// Cross-run resume of open run3's worker is refused even when stopped.
	if _, err := root2Cli.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Meta: meta(""), Agent: w3.Id, Task: "hijack open run3 worker",
	})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("resuming open run3's worker: %v, want PermissionDenied", err)
	}

	// Name lookup from root2 searches closed runs before other open runs, resolving "repo-worker" to w1.
	got, err := root2Cli.GetAgent(ctx, connect.NewRequest(&v1.GetAgentRequest{Agent: w1.Name}))
	if err != nil || got.Msg.Agent.Id != w1.Id {
		t.Fatalf("root2 GetAgent(%q) = %+v, err=%v (want closed run1 %s)", w1.Name, got, err, w1.Id)
	}
	rres, err := root2Cli.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Meta: meta(""), Agent: w1.Name, Task: "follow-up on closed run1",
	}))
	if err != nil {
		t.Fatalf("root2 ResumeAgent(%q): %v", w1.Name, err)
	}
	if rres.Msg.Agent.ResumedFromAgentId != w1.Id || rres.Msg.Agent.ResumedFromRunId != run1.Id {
		t.Errorf("resumed %+v, want from closed run1 %s / %s", rres.Msg.Agent, run1.Id, w1.Id)
	}
}
