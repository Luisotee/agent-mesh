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

	// Run 1: lead + worker.
	run1 := startRun(t, w.device.Mesh, "")
	root1Res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run1.Id, Repo: repo, Slug: "lead", Role: v1.AgentRole_AGENT_ROLE_ROOT,
	}))
	if err != nil {
		t.Fatal(err)
	}
	root1 := w.waitState(root1Res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)

	w1Res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run1.Id, Repo: repo, Slug: "worker", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_WORKER,
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
	root2Res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run2.Id, Repo: repo, Slug: "lead", Role: v1.AgentRole_AGENT_ROLE_ROOT,
	}))
	if err != nil {
		t.Fatal(err)
	}
	root2 := w.waitState(root2Res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	root2Cli := agentClient(t, w, root2)

	askerRes, err := root2Cli.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
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
}
