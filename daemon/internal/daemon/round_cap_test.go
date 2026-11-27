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
	w.set(t, "review.wait_room", 0)

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

// With review.wait_room a round on a full host waits, its request open, for a place, and starts when one frees (the
// cap raised here: a retire does the same), with a note on how long it waited.
func TestAReviewRoundOnAFullHostWaitsForRoom(t *testing.T) {
	old := daemon.SetRoomPoll(100 * time.Millisecond)
	t.Cleanup(func() { daemon.SetRoomPoll(old) })
	w := startReviews(t)
	w.pr.push(t, "the feature")
	w.set(t, "caps.agents", 1)
	w.set(t, "review.wait_room", 30)

	type result struct {
		res *connect.Response[v1.AskResponse]
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := w.asRoot.Ask(context.Background(), connect.NewRequest(&v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "claude,agy"}))
		done <- result{res, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("the round did not wait for room: %v", r.err)
	case <-time.After(700 * time.Millisecond):
	}
	if asks, _ := w.asRoot.ListAsks(context.Background(), connect.NewRequest(&v1.ListAsksRequest{})); asks != nil && len(asks.Msg.Asks) != 0 {
		t.Errorf("a round waiting for room is stored: %v", asks.Msg.Asks)
	}
	w.set(t, "caps.agents", 3) // a place frees
	r := <-done
	if r.err != nil {
		t.Fatalf("the round after room came: %v", r.err)
	}
	if len(r.res.Msg.Agents) != 2 {
		t.Errorf("started %d reviewers: %v", len(r.res.Msg.Agents), r.res.Msg.Notes)
	}
	noted := false
	for _, n := range r.res.Msg.Notes {
		noted = noted || strings.Contains(n, "waited") && strings.Contains(n, "for room")
	}
	if !noted {
		t.Errorf("no note that it waited for room: %v", r.res.Msg.Notes)
	}
}

// The wait ends: past review.wait_room the round is refused as before, with the cap named and nobody started.
func TestAReviewRoundThatNeverGetsRoomIsRefusedAfterWaitRoom(t *testing.T) {
	old := daemon.SetRoomPoll(100 * time.Millisecond)
	t.Cleanup(func() { daemon.SetRoomPoll(old) })
	w := startReviews(t)
	w.pr.push(t, "the feature")
	w.set(t, "caps.agents", 1)
	w.set(t, "review.wait_room", 1)

	began := time.Now()
	_, err := w.asRoot.Ask(context.Background(), connect.NewRequest(&v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "claude,agy"}))
	if err == nil || !strings.Contains(err.Error(), "caps.agents 1") {
		t.Fatalf("a round that never got room: %v, want the refusal naming caps.agents 1", err)
	}
	if waited := time.Since(began); waited < time.Second {
		t.Errorf("refused after %s, before review.wait_room", waited)
	}
}

// A round whose every slot has a reviewer running to reuse starts no process, so a host short of memory is no reason
// to hold it (the wait is for the first reviewer a round has to launch).
func TestAReReviewOfRunningReviewersDoesNotWaitForRoom(t *testing.T) {
	dir := t.TempDir()
	mem := writeProc(t, dir, "meminfo", meminfoText(12000, 0, 16000, 0))
	w := startReviews(t, func(c *daemon.AgentConfig) { c.MeminfoPath = mem })
	w.pr.push(t, "round 1")
	r1 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "claude,agy"})
	if len(r1.Agents) != 2 {
		t.Fatalf("round 1 started %d reviewers: %v", len(r1.Agents), r1.Notes)
	}
	for _, a := range r1.Agents {
		w.quiet(t, a.Id)
		w.say(t, a.Id, v1.MessageType_MESSAGE_TYPE_DONE, a.Kind+": fine")
	}
	waitFor(t, "round 1 finished", func() bool { return len(w.answers(t)) == 1 })
	writeProc(t, dir, "meminfo", meminfoText(100, 0, 16000, 0)) // no memory left: nothing new may start
	w.set(t, "review.wait_room", 60)
	w.pr.push(t, "round 2")
	began := time.Now()
	r2 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "auto"})
	if waited := time.Since(began); waited > 20*time.Second || len(r2.Agents) != 2 {
		t.Errorf("the re-review of running reviewers took %s and started %d (notes %v)", waited, len(r2.Agents), r2.Notes)
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

// A running reviewer the round meant to ask again but couldn't (its reserve refused it) is not part of the
// round: it counts against `caps.agents` again, so the next slot's spawn can't take the host past the cap.
func TestAReviewerThatCouldNotBeAskedAgainLeavesTheRoundsCapPlace(t *testing.T) {
	w := startReviews(t, func(c *daemon.AgentConfig) { c.Usage = &daemon.UsageOps{DisableClaudeNetwork: true} })
	ctx := context.Background()
	w.pr.push(t, "round 1")
	r1 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "claude"})
	w.quiet(t, r1.Agents[0].Id)
	w.say(t, r1.Agents[0].Id, v1.MessageType_MESSAGE_TYPE_DONE, "fine")
	waitFor(t, "round 1 finished", func() bool { return len(w.answers(t)) == 1 })

	reset := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	w.typeInto(w.spawnClaude(t, w.run, "usage", v1.AgentRole_AGENT_ROLE_WORKER), `/ratelimits {"five_hour":{"used_percentage":95,"resets_at":"`+reset+`"}}`+"\r")
	waitFor(t, "the reserve to fill", func() bool {
		return w.d.ReserveGuard(ctx, w.d.Store().Read(ctx), w.run.Id, "claude", "sonnet", "high", "test") != nil
	})
	w.set(t, "caps.agents", 3) // the root, the running reviewer and the worker fill it

	w.pr.push(t, "round 2")
	// claude's reviewer can't be asked (the reserve is full); agy's slot needs a place the host doesn't have.
	_, err := w.asRoot.Ask(ctx, connect.NewRequest(&v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "claude,agy"}))
	if err == nil {
		t.Fatal("the round started an agent past caps.agents 3")
	}
	if n := reviewers(t, w); n != 1 {
		t.Errorf("%d reviewers after the refused round, want the running one only", n)
	}
}
