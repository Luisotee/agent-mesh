package daemon_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

func reviewers(t *testing.T, w *reviewWorld) int {
	t.Helper()
	agents, err := w.asRoot.ListAgents(context.Background(), connect.NewRequest(&v1.ListAgentsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range agents.Msg.Agents {
		if a.Role == v1.AgentRole_AGENT_ROLE_REVIEWER {
			n++
		}
	}
	return n
}

// A round interrupted after some of its reviewers started leaves its ask open and starting; the same request
// retried finds that ask (its id comes from the mutation id), keeps the reviewers started and opens no second one.
func TestARetriedRoundFindsTheAskItLeftAndStartsNobodyTwice(t *testing.T) {
	w := startReviews(t)
	ctx := context.Background()
	w.pr.push(t, "the feature")
	req := func() *v1.AskRequest {
		return &v1.AskRequest{Meta: meta("ask-int"), Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "claude"}
	}
	first := w.ask(t, req())
	// Put the state back as an interruption would have left it: open and starting, and no memory of the request.
	err := w.d.Store().Write(ctx, &v1.Actor{Id: w.lead.Id, Class: v1.ActorClass_ACTOR_CLASS_ROOT, RunId: w.run.Id}, "", func(tx *store.Tx) error {
		ask, err := tx.GetAsk(first.Ask.Id)
		if err != nil {
			return err
		}
		ask.Status, ask.Starting, ask.FinishedAt = v1.AskStatus_ASK_STATUS_OPEN, true, nil
		if err := tx.PutAsk(ask); err != nil {
			return err
		}
		_, err = tx.PruneMutations(time.Now().Add(time.Hour), time.Now().Add(time.Hour))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	second := w.ask(t, req())
	if second.Ask.Id != first.Ask.Id || len(second.Agents) != 1 || second.Agents[0].Id != first.Agents[0].Id {
		t.Fatalf("first %v, second %v %v", first.Ask, second.Ask, second.Agents)
	}
	if n := reviewers(t, w); n != 1 {
		t.Errorf("%d reviewers after the retried round", n)
	}
	asks, _ := w.asRoot.ListAsks(ctx, connect.NewRequest(&v1.ListAsksRequest{Pr: "7"}))
	if len(asks.Msg.Asks) != 1 || asks.Msg.Asks[0].Starting {
		t.Errorf("asks %v", asks.Msg.Asks)
	}
}

// A reviewer that asked a question instead of answering round 1 never saw the earlier commits: the new head's
// round starts a fresh one of its kind on the whole PR, and asks again only the one that answered.
func TestAReReviewStartsFreshAReviewerThatNeverAnsweredAndAsksTheOneThatDidAgain(t *testing.T) {
	w := startReviews(t)
	w.pr.push(t, "the feature")
	round1 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "large", Tier: "mid", Reviewers: "claude,agy"})
	var claude, agy *v1.Agent
	for _, a := range round1.Agents {
		if a.Kind == "claude" {
			claude = a
		} else {
			agy = a
		}
	}
	w.quiet(t, claude.Id)
	w.quiet(t, agy.Id)
	w.say(t, claude.Id, v1.MessageType_MESSAGE_TYPE_QUESTION, "Is the base main?")
	w.say(t, agy.Id, v1.MessageType_MESSAGE_TYPE_DONE, "a finding")
	waitFor(t, "round 1's message", func() bool { return len(w.answers(t)) == 1 })

	head2 := w.pr.push(t, "the fix")
	round2 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "large", Tier: "mid", Reviewers: "auto"})
	got := map[string]*v1.AskMember{}
	for _, m := range round2.Ask.Members {
		got[m.Kind] = m
	}
	if got["agy"] == nil || got["agy"].AgentId != agy.Id || got["agy"].Tier != "mid" {
		t.Errorf("agy: %v", got["agy"])
	}
	fresh := got["claude"]
	if fresh == nil || fresh.AgentId == claude.Id || fresh.How != "fresh" {
		t.Fatalf("claude: %v", fresh)
	}
	task := w.task(t, fresh.AgentId)
	if strings.Contains(task, "You last reviewed") || !strings.Contains(task, "at "+head2[:9]) || !strings.Contains(task, "/code-review high "+w.pr.url()) {
		t.Errorf("the fresh claude's task reads as a re-review: %s", task)
	}
}

// Every kind of the last round skipped in a re-review: the slots fill again without them, as bin/mesh does.
func TestAReReviewWhoseEarlierKindsAreAllSkippedFillsTheSlotsAgain(t *testing.T) {
	w := startReviews(t)
	w.set(t, "review.re_review.skip", []any{"claude"})
	w.pr.push(t, "the feature")
	round1 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "large", Tier: "mid", Reviewers: "claude"})
	w.quiet(t, round1.Agents[0].Id)
	w.say(t, round1.Agents[0].Id, v1.MessageType_MESSAGE_TYPE_DONE, "a finding")
	waitFor(t, "round 1's message", func() bool { return len(w.answers(t)) == 1 })
	w.pr.push(t, "the fix")
	round2 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "large", Tier: "mid", Reviewers: "auto"})
	if len(round2.Ask.Members) == 0 {
		t.Fatalf("round 2 %v", round2)
	}
	for _, m := range round2.Ask.Members {
		if m.Kind == "claude" {
			t.Errorf("claude was asked again: %v", m)
		}
	}
}

// A PR given as a URL is found whatever its case, trailing slash or fragment, as bin/mesh's pr_key does.
func TestAVerdictFindsAPRGivenAsAnyFormOfItsURL(t *testing.T) {
	w := startReviews(t)
	w.pr.push(t, "the feature")
	round := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "claude,agy"})
	typed := strings.ToUpper(w.pr.url()) + "/#discussion"
	if _, err := w.asRoot.RecordVerdict(context.Background(), connect.NewRequest(&v1.RecordVerdictRequest{Meta: meta(""), Pr: typed,
		Reviewer: round.Agents[0].Name, Agree: 1})); err != nil {
		t.Fatalf("%s: %v", typed, err)
	}
}

// An ask of a fresh asker retried with the same mutation id replays its spawn: one asker, the same one.
func TestAFreshQuestionRetriedWithTheSameMutationIdStartsOneAsker(t *testing.T) {
	w := startReviews(t)
	req := func() *v1.AskRequest {
		return &v1.AskRequest{Meta: meta("ask-q"), Repo: w.pr.repo, Tier: "mid", Body: "Where is the retry logic?"}
	}
	a, b := w.ask(t, req()), w.ask(t, req())
	if a.Agents[0].Id != b.Agents[0].Id {
		t.Errorf("two askers: %s and %s", a.Agents[0].Name, b.Agents[0].Name)
	}
	// An existing agent is asked by its name and a body only: the fields of a fresh one are refused.
	if _, err := w.asRoot.Ask(context.Background(), connect.NewRequest(&v1.AskRequest{Agent: a.Agents[0].Name, Tier: "mid", Body: "x"})); code(err) != connect.CodeInvalidArgument {
		t.Errorf("an ask of an agent with a tier: %v", err)
	}
}
