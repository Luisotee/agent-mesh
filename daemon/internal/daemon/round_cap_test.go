package daemon_test

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// A review round counts as one agent against `caps.agents`: its first reviewer has to fit, the others then join
// it. Before, a host one place short started the first slot's reviewer and left the second as "not asked: at the
// limit", which the root saw only in the answers file.
func TestAReviewRoundIsNeverCutShortByTheCap(t *testing.T) {
	w := startReviews(t)
	ctx := context.Background()
	w.pr.push(t, "the feature")
	w.set(t, "caps.agents", 2) // the root fills one place: room for one more agent, and the round has two reviewers

	res := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "claude,agy"})
	if len(res.Agents) != 2 {
		t.Fatalf("started %d reviewers, want both of the round's slots: notes %v", len(res.Agents), res.Notes)
	}
	for _, m := range res.Ask.Members {
		if m.Status != v1.AskMemberStatus_ASK_MEMBER_STATUS_PENDING {
			t.Errorf("member %s is %s (%s), want pending", m.Name, m.Status, m.Line)
		}
	}

	// The cap still holds for everything else: a spawn past it is refused.
	_, err := w.asRoot.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Meta: meta("spawn-extra"), Repo: w.pr.repo, Slug: "extra", Tier: "mid"}))
	if code(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "caps.agents 2") {
		t.Fatalf("a spawn over the cap after the round: %v, want ResourceExhausted naming caps.agents 2", err)
	}
}

// A host with no room for even the first reviewer refuses the round when it is asked, with the cap named, and
// starts nobody.
func TestAReviewRoundOnAFullHostIsRefusedUpFront(t *testing.T) {
	w := startReviews(t)
	w.pr.push(t, "the feature")
	w.set(t, "caps.agents", 1) // the root alone fills it

	_, err := w.asRoot.Ask(context.Background(), connect.NewRequest(&v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "claude,agy"}))
	if err == nil || !strings.Contains(err.Error(), "caps.agents 1") {
		t.Fatalf("a round on a full host: %v, want a refusal naming caps.agents 1", err)
	}
	list, err := w.asRoot.ListAgents(context.Background(), connect.NewRequest(&v1.ListAgentsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range list.Msg.Agents {
		if a.Role == v1.AgentRole_AGENT_ROLE_REVIEWER {
			t.Errorf("reviewer %s was started on a full host", a.Name)
		}
	}
}

// A re-review resumes the earlier round's retired reviewers through SendMessage, which checks the cap inside its
// own write: the second resume must pass it as the first did.
func TestAReReviewResumesTheWholeRoundUnderTheCap(t *testing.T) {
	w := startReviews(t)
	ctx := context.Background()
	w.pr.push(t, "round 1")
	r1 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "claude,agy"})
	if len(r1.Agents) != 2 {
		t.Fatalf("round 1 started %d reviewers, want 2: %v", len(r1.Agents), r1.Notes)
	}
	for _, a := range r1.Agents {
		w.quiet(t, a.Id)
		w.say(t, a.Id, v1.MessageType_MESSAGE_TYPE_DONE, a.Kind+": fine")
	}
	waitFor(t, "round 1 finished", func() bool { return len(w.answers(t)) == 1 })
	for _, a := range r1.Agents {
		if _, err := w.asRoot.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: a.Id})); err != nil {
			t.Fatal(err)
		}
		w.waitState(a.Id, v1.AgentState_AGENT_STATE_RETIRED)
	}

	w.set(t, "caps.agents", 2) // the root and one reviewer
	w.pr.push(t, "round 2")
	r2 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "auto"})
	if len(r2.Agents) != 2 {
		t.Fatalf("the re-review started %d reviewers, want both resumed: notes %v", len(r2.Agents), r2.Notes)
	}
	for _, m := range r2.Ask.Members {
		if m.Status != v1.AskMemberStatus_ASK_MEMBER_STATUS_PENDING || m.How != "resumed" {
			t.Errorf("member %s: %s %q (%s), want pending and resumed", m.Name, m.Status, m.How, m.Line)
		}
	}
}

// Which reviewers the cap refuses must not depend on the order of the round's slots: a re-review whose first
// slot has to be resumed while a later slot's reviewer is still running used to count that running one against
// the cap and refuse the first.
func TestAReReviewAdmitsItsRunningReviewerWhicheverSlotComesFirst(t *testing.T) {
	w := startReviews(t)
	ctx := context.Background()
	w.pr.push(t, "round 1")
	r1 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "claude,agy"})
	if len(r1.Agents) != 2 {
		t.Fatalf("round 1 started %d reviewers, want 2: %v", len(r1.Agents), r1.Notes)
	}
	for _, a := range r1.Agents {
		w.quiet(t, a.Id)
		w.say(t, a.Id, v1.MessageType_MESSAGE_TYPE_DONE, a.Kind+": fine")
	}
	waitFor(t, "round 1 finished", func() bool { return len(w.answers(t)) == 1 })
	// The first slot's reviewer is retired; the second slot's is still running (idle).
	if r1.Agents[0].Kind != "claude" {
		t.Fatalf("the first slot is %s, want claude", r1.Agents[0].Kind)
	}
	if _, err := w.asRoot.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: r1.Agents[0].Id})); err != nil {
		t.Fatal(err)
	}
	w.waitState(r1.Agents[0].Id, v1.AgentState_AGENT_STATE_RETIRED)

	w.set(t, "caps.agents", 2) // the root and one more
	w.pr.push(t, "round 2")
	r2 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "auto"})
	if len(r2.Agents) != 2 {
		t.Fatalf("the re-review started %d reviewers, want both: notes %v", len(r2.Agents), r2.Notes)
	}
	for _, m := range r2.Ask.Members {
		if m.Status != v1.AskMemberStatus_ASK_MEMBER_STATUS_PENDING {
			t.Errorf("member %s is %s (%s), want pending", m.Name, m.Status, m.Line)
		}
	}
}

// A reviewer whose reuse was refused is a member that was not asked: no agent is recorded on it (a retry of the
// ask would count it as started and let the round share the cap with an agent that isn't in it), and the task
// stored with the refusal is marked read, not left for the next resume.
func TestAReReviewsRefusedReuseIsNotAStartedMember(t *testing.T) {
	w := startReviews(t)
	ctx := context.Background()
	w.pr.push(t, "round 1")
	r1 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "claude,agy"})
	for _, a := range r1.Agents {
		w.quiet(t, a.Id)
		w.say(t, a.Id, v1.MessageType_MESSAGE_TYPE_DONE, a.Kind+": fine")
	}
	waitFor(t, "round 1 finished", func() bool { return len(w.answers(t)) == 1 })
	claude := r1.Agents[0]
	if _, err := w.asRoot.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: claude.Id})); err != nil {
		t.Fatal(err)
	}
	w.waitState(claude.Id, v1.AgentState_AGENT_STATE_RETIRED)

	w.seedAgent(w.run.Id, "extra", "repo-two", w.lead.Id, v1.AgentRole_AGENT_ROLE_WORKER)
	w.set(t, "caps.agents", 2) // the root and the extra worker fill it: claude cannot come back, agy still runs
	w.pr.push(t, "round 2")
	r2 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "auto"})
	ask, err := w.d.Store().Read(ctx).GetAsk(r2.Ask.Id)
	if err != nil {
		t.Fatal(err)
	}
	var refused int
	for _, m := range ask.Members {
		if m.Status == v1.AskMemberStatus_ASK_MEMBER_STATUS_NOT_ASKED {
			refused++
			if m.AgentId != "" {
				t.Errorf("the refused member %s records agent %s", m.Name, m.AgentId)
			}
		}
	}
	if refused != 1 {
		t.Fatalf("%d members not asked, want the one refused resume: notes %v", refused, r2.Notes)
	}
	in, err := w.d.Store().Read(ctx).Inbox(claude.Id, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range in {
		if m.Type == v1.MessageType_MESSAGE_TYPE_TASK && m.CreatedAt.AsTime().After(r1.Ask.CreatedAt.AsTime()) {
			t.Errorf("the refused reviewer keeps an unread task of the new round: %q", m.Body)
		}
	}
}
