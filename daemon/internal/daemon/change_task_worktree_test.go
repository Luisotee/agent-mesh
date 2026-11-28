package daemon_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
)

// A stopped asker or reviewer that a change task makes a worker gets the worktree a worker gets: it is
// prepared as the worker it is about to be, not as the read-only agent it still is when the task arrives.

// StopRun removes a clean worktree; a cross-run follow-up of an asker with a change task must find it again.
func TestACrossRunChangeTaskToAStoppedAskerRecreatesItsWorktree(t *testing.T) {
	ctx := context.Background()
	w := startReopen(t, func(c *daemon.AgentConfig) { c.Usage = &daemon.UsageOps{DisableClaudeNetwork: true} })
	repo := gitRepo(t)
	run1 := startRun(t, w.device.Mesh, "")
	res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker", RunId: run1.Id, Repo: repo,
		Slug: "asker", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_ASKER, Task: "Question (read-only): where is the thing?"}))
	if err != nil {
		t.Fatal(err)
	}
	asker := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	if _, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta(""), RunId: run1.Id})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(asker.Worktree); err == nil {
		t.Fatalf("StopRun kept %s: the test needs it gone", asker.Worktree)
	}
	run2 := startRun(t, w.device.Mesh, "")
	rres, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Meta: meta(""), Agent: asker.Id,
		RunId: run2.Id, Task: "Direct task: make the change."}))
	if err != nil {
		t.Fatalf("ResumeAgent: %v", err)
	}
	created := rres.Msg.Agent
	if created.Role != v1.AgentRole_AGENT_ROLE_WORKER {
		t.Errorf("the follow-up is a %v, not the worker for the task", created.Role)
	}
	if _, err := os.Stat(created.Worktree); err != nil {
		t.Errorf("the follow-up's worktree was not recreated: %v", err)
	}
	if len(rres.Msg.Notes) == 0 || !strings.Contains(rres.Msg.Notes[0], "recreated worktree") {
		t.Errorf("notes %v", rres.Msg.Notes)
	}

	// A read-only follow-up question stays an asker and needs no worktree work.
	run3 := startRun(t, w.device.Mesh, "")
	qres, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Meta: meta(""), Agent: asker.Id,
		RunId: run3.Id, Task: "Question (read-only): and the other?"}))
	if err == nil && qres.Msg.Agent.Role != v1.AgentRole_AGENT_ROLE_ASKER {
		t.Errorf("a read-only task made the follow-up a %v", qres.Msg.Agent.Role)
	}
}

// A reviewer left on the branch of a merged PR, given a change task, moves onto a fresh branch like a worker.
func TestAStoppedReviewerGivenAChangeTaskLeavesAMergedPRBranch(t *testing.T) {
	ctx := context.Background()
	repo, _ := githubBackedRepo(t)
	var ghResp atomic.Value
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Usage = &daemon.UsageOps{
			DisableClaudeNetwork: true,
			RunGH: func(context.Context, string, ...string) ([]byte, error) {
				v, _ := ghResp.Load().(string)
				return []byte(v), nil
			},
		}
	})
	run := startRun(t, w.device.Mesh, "")
	res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker", RunId: run.Id, Repo: repo,
		Slug: "rev", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_REVIEWER, Task: "Review (read-only): the PR."}))
	if err != nil {
		t.Fatal(err)
	}
	rev := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	runGit(t, rev.Worktree, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(rev.Worktree, "f.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, rev.Worktree, "add", "f.txt")
	runGit(t, rev.Worktree, "commit", "-m", "the PR")
	prHead := runGit(t, rev.Worktree, "rev-parse", "HEAD")
	b, _ := json.Marshal([]map[string]any{{"number": 12, "url": "https://github.com/acme/demo/pull/12", "state": "MERGED", "headRefOid": prHead}})
	ghResp.Store(string(b))
	retireStoppedWorker(t, w, rev)

	sres, err := w.device.Mesh.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: rev.Id,
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "Direct task: make the follow-up change."}))
	if err != nil {
		t.Fatal(err)
	}
	if sres.Msg.Resumed == nil {
		t.Fatalf("not resumed: %q", sres.Msg.ResumeError)
	}
	got := w.agent(rev.Id)
	if got.Role != v1.AgentRole_AGENT_ROLE_WORKER || !strings.HasPrefix(got.Branch, "mesh/"+run.Id+"/rev") {
		t.Errorf("role %v on branch %q: want a worker on a fresh branch", got.Role, got.Branch)
	}
	if !strings.Contains(sres.Msg.Message.GetBody(), "PR #12") {
		t.Errorf("task note missing in %q", sres.Msg.Message.GetBody())
	}
}
