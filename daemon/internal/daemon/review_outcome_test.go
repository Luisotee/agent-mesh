package daemon_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// verdictFull records a verdict with every count.
func (a *authorWorld) verdictFull(t *testing.T, c agentmeshv1connect.MeshServiceClient, reviewer string, agree, other, disagree, blocking, declinedBlocking int32) error {
	t.Helper()
	_, err := c.RecordVerdict(context.Background(), connect.NewRequest(&v1.RecordVerdictRequest{Meta: meta(""), Pr: "7", Reviewer: reviewer,
		Agree: agree, Other: other, Disagree: disagree, Blocking: blocking, DeclinedBlocking: declinedBlocking}))
	return err
}

// a round of one reviewer that answered, as the worker's own PR #7 has after its first round.
func (a *authorWorld) answeredRound(t *testing.T) *v1.AskResponse {
	t.Helper()
	a.pr.push(t, "the feature")
	a.commit(t)
	a.set(t, "review.reviewers.small", []any{[]any{"claude"}})
	res, err := a.ask(t, &v1.AskRequest{Review: "7", Size: "small", Reviewers: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	a.say(t, res.Agents[0].Id, v1.MessageType_MESSAGE_TYPE_DONE, "findings")
	waitFor(t, "the round to finish", func() bool {
		k, _ := a.d.Store().Read(context.Background()).GetAsk(res.Ask.Id)
		return k.Status == v1.AskStatus_ASK_STATUS_FINISHED
	})
	return res
}

func (a *authorWorld) doneNaming7(t *testing.T) string {
	t.Helper()
	a.say(t, a.worker.Id, v1.MessageType_MESSAGE_TYPE_DONE, "PR "+a.pr.url()+" done")
	return a.doneBody(t)
}

func (a *authorWorld) recordPR(t *testing.T, num int32, url, state, checks, head string) {
	t.Helper()
	err := a.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		return tx.PutAgentPR(&v1.PullRequest{AgentId: a.worker.Id, Url: url, Number: num, State: state, Checks: checks, Source: "report", HeadRef: head})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The lead reads one line per PR: SETTLED, or NEEDS YOU with what is open, never the findings.
func TestAWorkersDoneCarriesOneOutcomeLinePerPR(t *testing.T) {
	t.Run("settled", func(t *testing.T) {
		a := startAuthor(t, "")
		a.answeredRound(t)
		reviewer := filepath.Base(a.pr.repo) + "-review-7-claude"
		if err := a.verdictFull(t, a.asWorker, reviewer, 3, 1, 1, 2, 0); err != nil {
			t.Fatal(err)
		}
		got := a.doneNaming7(t)
		if !strings.Contains(got, "PR #7: SETTLED: 1 round(s) (claude); 5 finding(s): 3 agreed, 2 declined (0 blocking); checks none") {
			t.Errorf("%s", got)
		}
		if strings.Contains(got, "NEEDS YOU") || strings.Contains(got, "Answers:") {
			t.Errorf("a settled PR points at its findings: %s", got)
		}
	})
	t.Run("a blocking finding the author declined needs the lead until the root judges it", func(t *testing.T) {
		a := startAuthor(t, "")
		res := a.answeredRound(t)
		reviewer := filepath.Base(a.pr.repo) + "-review-7-claude"
		if err := a.verdictFull(t, a.asWorker, reviewer, 1, 1, 0, 1, 1); err != nil {
			t.Fatal(err)
		}
		got := a.doneNaming7(t)
		for _, want := range []string{"PR #7: NEEDS YOU", reviewer + ": 1 blocking finding(s) declined by the author", "Answers: " + res.Ask.AnswersPath} {
			if !strings.Contains(got, want) {
				t.Errorf("lacks %q: %s", want, got)
			}
		}
		if strings.Contains(got, "SETTLED") {
			t.Errorf("settled over a declined blocking finding: %s", got)
		}
		// The root judged it: its verdict replaces the author's, and settles.
		if err := a.verdictFull(t, a.asRoot, reviewer, 1, 1, 0, 1, 0); err != nil {
			t.Fatal(err)
		}
		if got := a.doneNaming7(t); !strings.Contains(got, "PR #7: SETTLED") {
			t.Errorf("after the root's verdict: %s", got)
		}
	})
	t.Run("declined blocking is at most disagree and other", func(t *testing.T) {
		a := startAuthor(t, "")
		a.answeredRound(t)
		reviewer := filepath.Base(a.pr.repo) + "-review-7-claude"
		if err := a.verdictFull(t, a.asWorker, reviewer, 2, 0, 0, 1, 1); code(err) != connect.CodeInvalidArgument {
			t.Errorf("a declined blocking finding with nothing declined: %v", err)
		}
	})
	t.Run("an answer nobody judged, a failing check and a pending one", func(t *testing.T) {
		a := startAuthor(t, "")
		a.answeredRound(t)
		reviewer := filepath.Base(a.pr.repo) + "-review-7-claude"
		if got := a.doneNaming7(t); !strings.Contains(got, "NEEDS YOU: "+reviewer+" answered and has no verdict recorded") {
			t.Errorf("no verdict: %s", got)
		}
		if err := a.verdictFull(t, a.asWorker, reviewer, 1, 0, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
		for _, checks := range []string{"failing", "pending"} {
			a.recordPR(t, 7, a.pr.url(), "open", checks, a.worker.Branch)
			if got := a.doneNaming7(t); !strings.Contains(got, "PR #7: NEEDS YOU: checks "+checks) {
				t.Errorf("checks %s: %s", checks, got)
			}
		}
		a.recordPR(t, 7, a.pr.url(), "open", "pass", a.worker.Branch)
		if got := a.doneNaming7(t); !strings.Contains(got, "SETTLED") || !strings.Contains(got, "checks pass") {
			t.Errorf("checks pass: %s", got)
		}
	})
	t.Run("a reviewer that did not answer", func(t *testing.T) {
		a := startAuthor(t, "")
		a.pr.push(t, "the feature")
		a.commit(t)
		a.set(t, "review.reviewers.small", []any{[]any{"claude"}})
		res, err := a.ask(t, &v1.AskRequest{Review: "7", Size: "small", Reviewers: "auto"})
		if err != nil {
			t.Fatal(err)
		}
		a.say(t, res.Agents[0].Id, v1.MessageType_MESSAGE_TYPE_FAILED, "no")
		waitFor(t, "the round to finish", func() bool {
			k, _ := a.d.Store().Read(context.Background()).GetAsk(res.Ask.Id)
			return k.Status == v1.AskStatus_ASK_STATUS_FINISHED
		})
		if got := a.doneNaming7(t); !strings.Contains(got, "NEEDS YOU: ") || !strings.Contains(got, " did not answer") {
			t.Errorf("an unanswered round is a pass: %s", got)
		}
	})
	t.Run("a round still collecting", func(t *testing.T) {
		a := startAuthor(t, "")
		a.pr.push(t, "the feature")
		a.commit(t)
		a.set(t, "review.reviewers.small", []any{[]any{"claude"}})
		if _, err := a.ask(t, &v1.AskRequest{Review: "7", Size: "small", Reviewers: "auto"}); err != nil {
			t.Fatal(err)
		}
		if got := a.doneNaming7(t); !strings.Contains(got, "NEEDS YOU: a round is still collecting its answers") {
			t.Errorf("%s", got)
		}
	})
	t.Run("an open PR of its own with no round is not settled, named or not", func(t *testing.T) {
		a := startAuthor(t, "")
		a.answeredRound(t)
		reviewer := filepath.Base(a.pr.repo) + "-review-7-claude"
		if err := a.verdictFull(t, a.asWorker, reviewer, 1, 0, 0, 0, 0); err != nil {
			t.Fatal(err)
		}
		a.recordPR(t, 8, strings.TrimSuffix(a.pr.url(), "7")+"8", "open", "", a.worker.Branch)
		got := a.doneNaming7(t)
		if !strings.Contains(got, "PR #7: SETTLED") || !strings.Contains(got, "PR #8: NEEDS YOU: "+a.worker.Name+" asked no review round") {
			t.Errorf("a second open PR with no round: %s", got)
		}
		// A done that names none of its PRs does not read as settled either.
		a.say(t, a.worker.Id, v1.MessageType_MESSAGE_TYPE_DONE, "finished")
		if got := a.doneBody(t); !strings.Contains(got, "PR #8: NEEDS YOU") {
			t.Errorf("a done with no PR link: %s", got)
		}
	})
}

// A PR on a branch of the worker's series (`<slug>-3`) is its own: the footer knows it (TODO: a worker's PR on a
// branch outside its registered one).
func TestTheFooterKnowsAWorkersPROnABranchOfItsSeries(t *testing.T) {
	a := startAuthor(t, "")
	a.commit(t)
	series := a.worker.Branch + "-3"
	a.recordPR(t, 7, a.pr.url(), "merged", "", series) // not open: only the done's own-PR test can name it
	got := a.doneNaming7(t)
	if !strings.Contains(got, "PR #7: NEEDS YOU: "+a.worker.Name+" asked no review round") {
		t.Errorf("its stacked PR is not its own: %s", got)
	}
	// Another branch's is not.
	a.recordPR(t, 7, a.pr.url(), "merged", "", a.worker.Branch+"x-3")
	if got := a.doneNaming7(t); strings.Contains(got, "PR #7") {
		t.Errorf("a foreign branch taken for its series: %s", got)
	}
}

// A PR the worker asked a round on is its own whatever its recorded head says (the guard let the round through).
func TestTheFooterKnowsAPRTheWorkerAskedARoundOn(t *testing.T) {
	a := startAuthor(t, "")
	a.answeredRound(t)
	a.recordPR(t, 7, a.pr.url(), "open", "", "some-other-branch")
	if got := a.doneNaming7(t); !strings.Contains(got, "PR #7: NEEDS YOU: "+filepath.Base(a.pr.repo)+"-review-7-claude answered and has no verdict") {
		t.Errorf("%s", got)
	}
}

// A worker whose PR is risky (`--reviews relay`) runs a relay and no plain round; its outcome is settled only once
// the relay has run.
func TestAWorkerOnARelayRunsItAndIsSettledOnlyWhenItHas(t *testing.T) {
	a := startAuthor(t, "relay")
	a.pr.push(t, "the feature")
	a.commit(t)
	for _, req := range []*v1.AskRequest{
		{Review: "7", Size: "small", Reviewers: "auto"},
		{Review: "7", Size: "small", Kind: "claude"},
	} {
		if _, err := a.ask(t, req); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "relay") {
			t.Errorf("%v on a relay worker: %v", req, err)
		}
	}
	if got := a.doneNaming7(t); !strings.Contains(got, "PR #7: NEEDS YOU: "+a.worker.Name+" asked no review round") {
		t.Errorf("before the relay: %s", got)
	}
	res, err := a.ask(t, &v1.AskRequest{Repo: a.pr.repo, Review: "7", Relay: true})
	if err != nil {
		t.Fatalf("a relay step by its worker: %v", err)
	}
	a.say(t, res.Agents[0].Id, v1.MessageType_MESSAGE_TYPE_DONE, "findings")
	waitFor(t, "the step to finish", func() bool {
		k, _ := a.d.Store().Read(context.Background()).GetAsk(res.Ask.Id)
		return k.Status == v1.AskStatus_ASK_STATUS_FINISHED
	})
	reviewer := res.Agents[0].Name
	if err := a.verdictFull(t, a.asWorker, reviewer, 2, 0, 0, 1, 0); err != nil {
		t.Fatal(err)
	}
	if got := a.doneNaming7(t); !strings.Contains(got, "its relay is unfinished (step 1 of 3, with findings to fix)") {
		t.Errorf("a relay with findings left: %s", got)
	}
	// A step with nothing to fix ends it.
	if err := a.verdictFull(t, a.asWorker, reviewer, 0, 1, 1, 0, 0); err != nil {
		t.Fatal(err)
	}
	if got := a.doneNaming7(t); !strings.Contains(got, "PR #7: SETTLED: relay, 1 step(s)") {
		t.Errorf("a relay with nothing left: %s", got)
	}
}

// The root's brief shows one word of a worker's reviews, so it can run the board without opening a done.
func TestTheBriefShowsAWorkersReviewOutcomeInOneWord(t *testing.T) {
	a := startAuthor(t, "")
	brief := func() string {
		t.Helper()
		res, err := a.asRoot.GetBrief(context.Background(), connect.NewRequest(&v1.GetBriefRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.Text
	}
	if strings.Contains(brief(), "reviews:") {
		t.Errorf("a review word for a worker with no PR: %s", brief())
	}
	a.pr.push(t, "the feature")
	a.commit(t)
	a.set(t, "review.reviewers.small", []any{[]any{"claude"}})
	res, err := a.ask(t, &v1.AskRequest{Review: "7", Size: "small", Reviewers: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if b := brief(); !strings.Contains(b, "reviews: running") {
		t.Errorf("a round collecting: %s", b)
	}
	a.say(t, res.Agents[0].Id, v1.MessageType_MESSAGE_TYPE_DONE, "findings")
	waitFor(t, "the round to finish", func() bool {
		k, _ := a.d.Store().Read(context.Background()).GetAsk(res.Ask.Id)
		return k.Status == v1.AskStatus_ASK_STATUS_FINISHED
	})
	if b := brief(); !strings.Contains(b, "reviews: needs you") {
		t.Errorf("an answer nobody judged: %s", b)
	}
	if err := a.verdictFull(t, a.asWorker, filepath.Base(a.pr.repo)+"-review-7-claude", 1, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if b := brief(); !strings.Contains(b, "reviews: settled") {
		t.Errorf("a judged round: %s", b)
	}
}
