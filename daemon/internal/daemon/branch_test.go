package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// A worker that moves to a branch for its next PR is shown, checked and counted on that branch, not on the one it
// was spawned on (`mesh check` said it "does not exist" once the first was merged and deleted).
func TestAWorkerThatMovedBranchIsShownOnTheBranchItIsOn(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	repo := gitRepo(t)
	gitIn(t, repo, "remote", "add", "origin", repo)
	gitIn(t, repo, "fetch", "origin")
	gitIn(t, repo, "remote", "set-head", "origin", "main")
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	worker := w.spawn(run.Id, repo, "fix", "")
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	first, _ := w.device.Mesh.GetAgent(ctx, connect.NewRequest(&v1.GetAgentRequest{Agent: worker.Id}))
	wt, spawned := first.Msg.Agent.Worktree, first.Msg.Agent.Branch
	os.WriteFile(filepath.Join(wt, "one.txt"), []byte("1\n"), 0o644)
	gitIn(t, wt, "add", ".")
	gitIn(t, wt, "commit", "-m", "the first PR's work")

	// Its first PR merged and its branch is deleted; it starts the next from main.
	next := "mesh/" + run.Id + "/fix-2"
	gitIn(t, wt, "checkout", "-b", next, "origin/main")
	gitIn(t, repo, "branch", "-D", spawned)
	os.WriteFile(filepath.Join(wt, "two.txt"), []byte("2\n"), 0o644)
	gitIn(t, wt, "add", ".")
	gitIn(t, wt, "commit", "-m", "the next PR's work")

	res, err := asLead.CheckAgent(ctx, connect.NewRequest(&v1.CheckAgentRequest{Agent: worker.Name}))
	if err != nil {
		t.Fatal(err)
	}
	text := res.Msg.Text
	for _, want := range []string{"branch " + next, "commits past the base: 1", "the next PR's work", "two.txt"} {
		if !strings.Contains(text, want) {
			t.Errorf("the check lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "does not exist") || strings.Contains(text, "first PR's work") {
		t.Errorf("the check is of the branch it left:\n%s", text)
	}
	list, err := asLead.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range list.Msg.Agents {
		if a.Id == worker.Id && a.Branch != next {
			t.Errorf("agents lists branch %q, want %q", a.Branch, next)
		}
	}
	if got, _ := asLead.GetAgent(ctx, connect.NewRequest(&v1.GetAgentRequest{Agent: worker.Id})); got.Msg.Agent.Branch != next {
		t.Errorf("get lists branch %q, want %q", got.Msg.Agent.Branch, next)
	}
	// What changed (the archive guard, the app) counts the next branch's commits only.
	ch, err := asLead.GetAgentChanges(ctx, connect.NewRequest(&v1.GetAgentChangesRequest{Agent: worker.Id}))
	if err != nil || !ch.Msg.Available || ch.Msg.Commits != 1 {
		t.Errorf("changes %v, %v", ch, err)
	}
}
