package daemon_test

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// A relay is one command run after each push: A reviews the whole PR, B (the other family) the whole PR at the new
// head, then A's own agent the fix commits only; the daemon fills in the note and --since, counts at most three
// steps, refuses a step with nothing new to read, and keeps the steps out of the PR's rounds.
func TestARelayRunsABAOnTheFixRangeAndCountsThreeSteps(t *testing.T) {
	w := startReviews(t)
	ctx := context.Background()
	relay := func() (*v1.AskResponse, error) {
		res, err := w.asRoot.Ask(ctx, connect.NewRequest(&v1.AskRequest{Repo: w.pr.repo, Review: "7", Relay: true}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	answer := func(res *v1.AskResponse, n int) {
		t.Helper()
		a := res.Agents[0]
		w.quiet(t, a.Id)
		w.say(t, a.Id, v1.MessageType_MESSAGE_TYPE_DONE, "finding "+a.Kind)
		waitFor(t, "the step's message", func() bool { return len(w.answers(t)) == n })
	}

	head1 := w.pr.push(t, "the feature")
	s1, err := relay()
	if err != nil {
		t.Fatal(err)
	}
	if len(s1.Agents) != 1 || s1.Ask.RelayStep != 1 || s1.Ask.RelayOf != s1.Ask.Id || s1.Ask.Size != "large" || s1.Ask.Tier != "strong" ||
		s1.Ask.Since != "" || !s1.Ask.Collected || !strings.Contains(s1.Hint, "relay step 1 of 3") {
		t.Fatalf("step 1: %v (hint %q)", s1.Ask, s1.Hint)
	}
	kindA := s1.Agents[0].Kind
	// Not again while it collects.
	if _, err := relay(); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "still collecting") {
		t.Errorf("a step while one collects: %v", err)
	}
	answer(s1, 1)
	// Nothing was pushed since step 1 read it: nothing for a second reviewer.
	if _, err := relay(); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "no commits since relay step 1") {
		t.Errorf("a step on the same head: %v", err)
	}

	head2 := w.pr.push(t, "the fix")
	s2, err := relay()
	if err != nil {
		t.Fatal(err)
	}
	kindB := s2.Agents[0].Kind
	note := "commits " + head1[:9] + ".." + head2[:9] + " fixed an earlier review; don't re-report what they fixed unless the fix is wrong"
	if s2.Ask.RelayStep != 2 || s2.Ask.RelayOf != s1.Ask.Id || s2.Ask.Since != "" || kindB == kindA || s2.Agents[0].Id == s1.Agents[0].Id ||
		!strings.Contains(w.task(t, s2.Agents[0].Id), note) {
		t.Fatalf("step 2: %v, A %s B %s, task %q", s2.Ask, kindA, kindB, w.task(t, s2.Agents[0].Id))
	}
	if strings.Contains(w.task(t, s2.Agents[0].Id), "finding "+kindA) {
		t.Errorf("step 2 was given step 1's findings")
	}
	answer(s2, 2)

	head3 := w.pr.push(t, "the second fix")
	s3, err := relay()
	if err != nil {
		t.Fatal(err)
	}
	task3 := w.task(t, s3.Agents[0].Id)
	if s3.Ask.RelayStep != 3 || s3.Agents[0].Id != s1.Agents[0].Id || s3.Agents[0].Kind != kindA || s3.Ask.Head != head3 ||
		!strings.Contains(task3, "You last reviewed it at "+head1[:9]) || !strings.Contains(task3, "git diff "+head1[:9]+" "+head3[:9]) ||
		!strings.Contains(task3, "commits "+head1[:9]+".."+head3[:9]+" fixed an earlier review") ||
		!strings.Contains(s3.Hint, "its last") {
		t.Fatalf("step 3: %v (hint %q)\n%s", s3.Ask, s3.Hint, task3)
	}
	answer(s3, 3)
	w.pr.push(t, "a third fix")
	if _, err := relay(); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "the relay is complete") {
		t.Errorf("a fourth step: %v", err)
	}
	// The steps are no rounds of the PR: an ordinary round is its first, with no relay in its hint.
	round := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "large", Tier: "mid", Reviewers: "claude"})
	if round.Ask.Round != 1 || round.Ask.RelayStep != 0 || strings.Contains(round.Hint, "relay") {
		t.Errorf("a round after a relay: round %d, hint %q", round.Ask.Round, round.Hint)
	}
	// A relay picks its own reviewers.
	for name, req := range map[string]*v1.AskRequest{
		"reviewers": {Repo: w.pr.repo, Review: "7", Relay: true, Reviewers: "all"},
		"since":     {Repo: w.pr.repo, Review: "7", Relay: true, Since: head1[:9]},
		"kind":      {Repo: w.pr.repo, Review: "7", Relay: true, Kind: "claude"},
		"no review": {Repo: w.pr.repo, Relay: true, Body: "a question"},
	} {
		if _, err := w.asRoot.Ask(ctx, connect.NewRequest(req)); code(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}
