package daemon_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

func spawnReviewer(t *testing.T, w *reopenWorld, as interface {
	SpawnAgent(context.Context, *connect.Request[v1.SpawnAgentRequest]) (*connect.Response[v1.SpawnAgentResponse], error)
}, slug string) *v1.Agent {
	t.Helper()
	res, err := as.SpawnAgent(context.Background(), connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker", Repo: gitRepo(t), Slug: slug,
		Role: v1.AgentRole_AGENT_ROLE_REVIEWER, Tier: "mid", Task: "Review PR 1."}))
	if err != nil {
		t.Fatal(err)
	}
	return w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
}

func (w *reopenWorld) hasTerminal(t *testing.T, id string) bool {
	for _, tm := range w.terminals(t) {
		if tm.Id == id {
			return true
		}
	}
	return false
}

// The reviewers a worker spawned end with it, and the root may retire any agent of its run (a reviewer's parent is
// the worker, so the root was refused).
func TestRetiringAWorkerRetiresItsReviewersAndTheRootMayRetireAnyAgentOfItsRun(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "fix", &v1.SpawnAgentRequest{About: "test worker"})
	other := w.spawnUnder(t, asLead, "other", &v1.SpawnAgentRequest{About: "test worker"})
	asWorker := w.as(w.token(t, worker.Id))
	rev1 := spawnReviewer(t, w, asWorker, "review-1")
	rev2 := spawnReviewer(t, w, asWorker, "review-2")
	if rev1.ParentId != worker.Id {
		t.Fatalf("the reviewer's parent is %q, want the worker", rev1.ParentId)
	}

	// A sibling still may not; the root may.
	if _, err := w.as(w.token(t, other.Id)).RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: rev1.Id})); code(err) != connect.CodePermissionDenied {
		t.Errorf("a sibling retiring a reviewer: %v", err)
	}
	if _, err := asLead.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: rev1.Id})); err != nil {
		t.Fatalf("the root retiring a worker's reviewer: %v", err)
	}
	if w.hasTerminal(t, rev1.TerminalId) {
		t.Error("the retired reviewer's process still runs")
	}

	// Retiring the worker takes the reviewer it still has, and leaves its sibling.
	if _, err := asLead.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: worker.Id})); err != nil {
		t.Fatal(err)
	}
	got, _ := w.device.Mesh.GetAgent(ctx, connect.NewRequest(&v1.GetAgentRequest{Agent: rev2.Id}))
	if got.Msg.Agent.State != v1.AgentState_AGENT_STATE_RETIRED || w.hasTerminal(t, rev2.TerminalId) {
		t.Errorf("the worker's other reviewer: %v (terminal alive %v)", got.Msg.Agent.State, w.hasTerminal(t, rev2.TerminalId))
	}
	if o, _ := w.device.Mesh.GetAgent(ctx, connect.NewRequest(&v1.GetAgentRequest{Agent: other.Id})); o.Msg.Agent.State == v1.AgentState_AGENT_STATE_RETIRED {
		t.Error("a sibling was retired")
	}
	// A root is no one's to retire, itself included.
	lead, _ := w.device.Mesh.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{RunId: run.Id}))
	for _, a := range lead.Msg.Agents {
		if a.Role == v1.AgentRole_AGENT_ROLE_ROOT {
			if _, err := asLead.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: a.Id})); code(err) != connect.CodePermissionDenied {
				t.Errorf("a root retiring itself: %v", err)
			}
		}
	}
}

// A reviewer ends when its PR merges or closes, once it is idle with nothing unread.
func TestAReviewerIsRetiredWhenItsPRMerges(t *testing.T) {
	w := startPR(t)
	ctx := context.Background()
	const url = "https://github.com/acme/demo/pull/12"
	w.seedAgent(w.run.Id, "r-idle", "demo-rev-a", "a-w1", v1.AgentRole_AGENT_ROLE_REVIEWER)
	w.seedAgent(w.run.Id, "r-busy", "demo-rev-b", "a-w1", v1.AgentRole_AGENT_ROLE_REVIEWER)
	w.edit("r-idle", func(a *v1.Agent) { a.State = v1.AgentState_AGENT_STATE_IDLE })
	w.edit("r-busy", func(a *v1.Agent) { a.State = v1.AgentState_AGENT_STATE_WORKING })
	err := w.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error {
		return tx.PutAsk(&v1.Ask{Id: "k-1", RunId: w.run.Id, Kind: v1.AskKind_ASK_KIND_REVIEW, Status: v1.AskStatus_ASK_STATUS_OPEN,
			Pr: url, Members: []*v1.AskMember{{AgentId: "r-idle", Name: "demo-rev-a"}, {AgentId: "r-busy", Name: "demo-rev-b"}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	state := func(id string) v1.AgentState {
		a, err := w.d.Store().Read(ctx).GetAgent(id)
		if err != nil {
			t.Fatal(err)
		}
		return a.State
	}

	w.setList(ghPR(12, "OPEN", w.branchTip, false))
	w.prs("", true)
	if state("r-idle") == v1.AgentState_AGENT_STATE_RETIRED {
		t.Fatal("a reviewer of an open PR was retired")
	}
	w.setList(ghPR(12, "MERGED", w.branchTip, false))
	w.advance(time.Hour)
	w.prs("", true)
	waitFor(t, "the idle reviewer retired", func() bool { return state("r-idle") == v1.AgentState_AGENT_STATE_RETIRED })
	if state("r-busy") == v1.AgentState_AGENT_STATE_RETIRED {
		t.Error("a reviewer on a turn was retired")
	}
	// Brought back (a resume), it is not retired again at the next read of the PR that is still merged.
	w.edit("r-idle", func(a *v1.Agent) { a.State = v1.AgentState_AGENT_STATE_IDLE })
	w.advance(time.Hour)
	w.prs("", true)
	time.Sleep(300 * time.Millisecond)
	if state("r-idle") != v1.AgentState_AGENT_STATE_IDLE {
		t.Error("a resumed reviewer was retired again by a later read of its merged PR")
	}
}
