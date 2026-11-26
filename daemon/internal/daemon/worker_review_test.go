package daemon_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// authorWorld is a review world with a worker whose branch is the PR's head: the PR is the worker's own.
type authorWorld struct {
	*reviewWorld
	worker   *v1.Agent
	asWorker agentmeshv1connect.MeshServiceClient
}

func startAuthor(t *testing.T, reviews string) *authorWorld {
	t.Helper()
	w := startReviews(t)
	worker := w.spawnUnder(t, w.asRoot, "fix", &v1.SpawnAgentRequest{Repo: w.pr.repo, Reviews: reviews})
	w.pr.headRef = worker.Branch
	return &authorWorld{reviewWorld: w, worker: worker, asWorker: w.as(w.token(t, worker.Id))}
}

func (a *authorWorld) ask(t *testing.T, req *v1.AskRequest) (*v1.AskResponse, error) {
	t.Helper()
	res, err := a.asWorker.Ask(context.Background(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func (a *authorWorld) verdict(t *testing.T, c agentmeshv1connect.MeshServiceClient, reviewer string, agree, disagree int32) error {
	t.Helper()
	_, err := c.RecordVerdict(context.Background(), connect.NewRequest(&v1.RecordVerdictRequest{Meta: meta(""), Pr: "7", Reviewer: reviewer,
		Agree: agree, Disagree: disagree}))
	return err
}

// doneTo is the newest `done` the lead was sent by the worker.
func (a *authorWorld) doneBody(t *testing.T) string {
	t.Helper()
	msgs, err := a.d.Store().Read(context.Background()).Inbox(a.lead.Id, true)
	if err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, m := range msgs {
		if m.Type == v1.MessageType_MESSAGE_TYPE_DONE && m.From.GetId() == a.worker.Id {
			body = m.Body
		}
	}
	return body
}

func TestAWorkerAsksItsOwnPRsRoundFromItsWorktreeWithTheSizesTier(t *testing.T) {
	a := startAuthor(t, "")
	a.pr.push(t, "the feature")
	a.set(t, "review.reviewers.small", []any{[]any{"claude"}})
	// `--repo .` in its worktree stands for its repo; no tier means the size's (`review.tier.small`).
	res, err := a.ask(t, &v1.AskRequest{Repo: a.worker.Worktree, Review: "7", Size: "small", Reviewers: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Agents) != 1 || res.Ask.Tier != "mid" || res.Ask.Repo != a.pr.repo || res.Ask.CallerId != a.worker.Id {
		t.Fatalf("round %v", res)
	}
	rev := res.Agents[0]
	if rev.ParentId != a.worker.Id || rev.Name != filepath.Base(a.pr.repo)+"-review-7-claude" || rev.Role != v1.AgentRole_AGENT_ROLE_REVIEWER {
		t.Errorf("reviewer %v", rev)
	}
	for _, want := range []string{"judge each finding", "mesh verdict --pr " + a.pr.url(), "its last", "report `done`"} {
		if !strings.Contains(res.Hint, want) {
			t.Errorf("a worker's hint lacks %q: %s", want, res.Hint)
		}
	}
}

func TestAWorkerReviewsOnlyItsOwnPRAndNotWhereItsParentRunsTheRounds(t *testing.T) {
	a := startAuthor(t, "")
	a.pr.headRef = "somebody-elses"
	a.pr.push(t, "the feature")
	if _, err := a.ask(t, &v1.AskRequest{Review: "7", Size: "small", Reviewers: "auto"}); code(err) != connect.CodePermissionDenied ||
		!strings.Contains(err.Error(), "only its own PR") {
		t.Errorf("another PR: %v", err)
	}
	if _, err := a.ask(t, &v1.AskRequest{Review: "7", Size: "small", Kind: "claude"}); code(err) != connect.CodePermissionDenied {
		t.Errorf("another PR, one reviewer: %v", err)
	}
	a.pr.headRef = a.worker.Branch
	a.pr.push(t, "again")
	// The root's own round on it is not limited.
	if res := a.reviewWorld.ask(t, &v1.AskRequest{Repo: a.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "claude"}); len(res.Agents) != 1 {
		t.Errorf("the root's round %v", res)
	}
	// `review.by root` (the host default) or `--reviews root` (this worker): the parent runs them.
	b := startAuthor(t, "root")
	b.pr.push(t, "the feature")
	_, err := b.ask(t, &v1.AskRequest{Review: "7", Size: "small", Reviewers: "auto"})
	if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "your parent runs the reviews of your PR") {
		t.Errorf("--reviews root: %v", err)
	}
	if got := b.agent(b.worker.Id).ReviewBy; got != "root" {
		t.Errorf("review_by %q", got)
	}
	c := startAuthor(t, "")
	c.pr.push(t, "the feature")
	c.set(t, "review.by", "root")
	if _, err := c.ask(t, &v1.AskRequest{Review: "7", Size: "small", Reviewers: "auto"}); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("review.by root: %v", err)
	}
	// And the worker can pick for itself against the host's default.
	d := startAuthor(t, "worker")
	d.set(t, "review.by", "root")
	d.pr.push(t, "the feature")
	if _, err := d.ask(t, &v1.AskRequest{Review: "7", Size: "small", Reviewers: "claude"}); err != nil {
		t.Errorf("--reviews worker under review.by root: %v", err)
	}
	// A value that is neither.
	if _, err := c.asRoot.SpawnAgent(context.Background(), connect.NewRequest(&v1.SpawnAgentRequest{Repo: c.pr.repo, Slug: "bad", Tier: "mid", Reviews: "both"})); code(err) != connect.CodeInvalidArgument {
		t.Errorf("reviews both: %v", err)
	}
}

func TestAnApprovalCanMoveAWorkersReviewsToTheRoot(t *testing.T) {
	w := startReviews(t)
	worker := w.spawnUnder(t, w.asRoot, "risky", &v1.SpawnAgentRequest{Repo: w.pr.repo, Plan: true, ExecTier: "mid"})
	w.say(t, worker.Id, v1.MessageType_MESSAGE_TYPE_PLAN, "the plan")
	if _, err := w.asRoot.ApproveAgent(context.Background(), connect.NewRequest(&v1.ApproveAgentRequest{Meta: meta(""), Agent: worker.Name, Tier: "mid", Reviews: "root"})); err != nil {
		t.Fatal(err)
	}
	if got := w.agent(worker.Id).ReviewBy; got != "root" {
		t.Errorf("review_by after approve %q", got)
	}
	if _, err := w.asRoot.ApproveAgent(context.Background(), connect.NewRequest(&v1.ApproveAgentRequest{Meta: meta(""), Agent: worker.Name, Reviews: "both"})); code(err) != connect.CodeInvalidArgument {
		t.Errorf("approve reviews both: %v", err)
	}
}

func TestAWorkerIsHeldToTheRoundCeilingAndCannotAskAReviewerAgainByName(t *testing.T) {
	a := startAuthor(t, "")
	a.pr.push(t, "the feature")
	a.set(t, "review.reviewers.small", []any{[]any{"claude"}})
	res, err := a.ask(t, &v1.AskRequest{Review: "7", Size: "small", Reviewers: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	a.say(t, res.Agents[0].Id, v1.MessageType_MESSAGE_TYPE_DONE, "one finding")
	waitFor(t, "the round to finish", func() bool {
		k, _ := a.d.Store().Read(context.Background()).GetAsk(res.Ask.Id)
		return k.Status == v1.AskStatus_ASK_STATUS_FINISHED
	})
	a.pr.push(t, "the fix")
	_, err = a.ask(t, &v1.AskRequest{Review: "7", Size: "small", Reviewers: "auto"})
	if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "1 of at most 1 review rounds") {
		t.Errorf("past the ceiling: %v", err)
	}
	if _, err := a.ask(t, &v1.AskRequest{Review: "7", Size: "small", Kind: "agy"}); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("one reviewer past the ceiling: %v", err)
	}
	if _, err := a.ask(t, &v1.AskRequest{Agent: res.Agents[0].Name, Review: "7", Size: "small"}); code(err) != connect.CodeInvalidArgument {
		t.Errorf("asking a reviewer again by name with --review: %v", err)
	}
	// A larger ceiling lets the re-review through: it reads only the fix.
	a.set(t, "review.rounds.small", 2)
	again, err := a.ask(t, &v1.AskRequest{Review: "7", Size: "small", Reviewers: "auto"})
	if err != nil || again.Ask.Round != 2 || again.Ask.Since == "" {
		t.Fatalf("re-review %v, %v", again, err)
	}
}

func TestAWorkersDoneCarriesItsRoundsAndVerdictsAndTheRootsVerdictStands(t *testing.T) {
	a := startAuthor(t, "")
	a.pr.push(t, "the feature")
	a.set(t, "review.reviewers.small", []any{[]any{"claude"}})
	// A done naming the PR before any round: the footer says no round was asked.
	a.say(t, a.worker.Id, v1.MessageType_MESSAGE_TYPE_DONE, "PR "+a.pr.url()+" opened")
	if got := a.doneBody(t); !strings.Contains(got, "[mesh reviews] PR #7: "+a.worker.Name+" asked no review round") {
		t.Errorf("done without a round: %q", got)
	}
	// A done with no PR link gets no footer.
	a.say(t, a.worker.Id, v1.MessageType_MESSAGE_TYPE_DONE, "nothing to show")
	if got := a.doneBody(t); strings.Contains(got, "[mesh reviews]") {
		t.Errorf("a footer on a done with no PR: %q", got)
	}

	res, err := a.ask(t, &v1.AskRequest{Review: "7", Size: "small", Reviewers: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	reviewer := res.Agents[0]
	a.say(t, reviewer.Id, v1.MessageType_MESSAGE_TYPE_DONE, "a.go:3 blocking")
	waitFor(t, "the round to finish", func() bool {
		k, _ := a.d.Store().Read(context.Background()).GetAsk(res.Ask.Id)
		return k.Status == v1.AskStatus_ASK_STATUS_FINISHED
	})
	// The round's answers went to the worker, not the root.
	if len(a.answers(t)) != 0 {
		t.Errorf("the root was told of the worker's round")
	}
	a.say(t, a.worker.Id, v1.MessageType_MESSAGE_TYPE_DONE, "PR "+a.pr.url()+" fixed")
	got := a.doneBody(t)
	for _, want := range []string{"[mesh reviews] PR #7", "round 1 (small, mid)", reviewer.Name + " answered", "answers " + res.Ask.AnswersPath, reviewer.Name + " no verdict recorded"} {
		if !strings.Contains(got, want) {
			t.Errorf("footer lacks %q: %s", want, got)
		}
	}

	// The worker judges its own round; the root's verdict replaces it, and then stands.
	if err := a.verdict(t, a.asWorker, reviewer.Name, 2, 1); err != nil {
		t.Fatal(err)
	}
	a.say(t, a.worker.Id, v1.MessageType_MESSAGE_TYPE_DONE, "PR "+a.pr.url()+" settled")
	if got := a.doneBody(t); !strings.Contains(got, reviewer.Name+" 2/0/1/0 by author") {
		t.Errorf("footer after the author's verdict: %s", got)
	}
	if err := a.verdict(t, a.asRoot, reviewer.Name, 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := a.verdict(t, a.asWorker, reviewer.Name, 3, 0); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("the author over the root's verdict: %v", err)
	}
	k, _ := a.d.Store().Read(context.Background()).GetAsk(res.Ask.Id)
	if len(k.Verdicts) != 1 || k.Verdicts[0].Judge != "root" || k.Verdicts[0].Agree != 1 {
		t.Errorf("verdicts %v", k.Verdicts)
	}
	// A worker judges only the rounds it asked: a root's round is not its to judge.
	other := a.reviewWorld.ask(t, &v1.AskRequest{Repo: a.pr.repo, Review: "7", Size: "large", Tier: "mid", Reviewers: "agy"})
	a.say(t, other.Agents[0].Id, v1.MessageType_MESSAGE_TYPE_DONE, "x")
	if err := a.verdict(t, a.asWorker, other.Agents[0].Name, 1, 0); code(err) != connect.CodeNotFound {
		t.Errorf("a worker's verdict on the root's round: %v", err)
	}
	// By the cost report, how many reviews carry an author's verdict (none now: the root's stands).
	cost, err := a.asRoot.GetCost(context.Background(), connect.NewRequest(&v1.GetCostRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range cost.Msg.Verdicts {
		if v.ByAuthor != 0 {
			t.Errorf("by_author %d with the root's verdict standing: %v", v.ByAuthor, v)
		}
	}
}

func TestAWorkerWaitingOnItsOwnRoundIsNotNudgedUntilItEnds(t *testing.T) {
	a := newAcct(t)
	a.setting("accountability.nudge_after", 1.0)
	a.state(v1.AgentState_AGENT_STATE_IDLE, time.Now())
	a.taskRead()
	put := func(status v1.AskStatus) {
		t.Helper()
		err := a.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
			return tx.PutAsk(&v1.Ask{Id: "k1", RunId: a.run.Id, CallerId: "a-w1", CallerName: "repo-one", Kind: v1.AskKind_ASK_KIND_REVIEW,
				Status: status, Collected: true, CreatedAt: timestamppb.Now(), Deadline: timestamppb.New(time.Now().Add(time.Hour))})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	put(v1.AskStatus_ASK_STATUS_OPEN)
	time.Sleep(1200 * time.Millisecond)
	a.pass()
	if n := a.mailOf("a-w1", "[mesh nudge]"); n != 0 {
		t.Fatalf("nudged while its round is open: %d", n)
	}
	put(v1.AskStatus_ASK_STATUS_FINISHED)
	a.state(v1.AgentState_AGENT_STATE_IDLE, time.Now().Add(-2*time.Second))
	a.pass()
	if n := a.mailOf("a-w1", "[mesh nudge]"); n != 1 {
		t.Fatalf("nudges after the round ended: %d", n)
	}
}
