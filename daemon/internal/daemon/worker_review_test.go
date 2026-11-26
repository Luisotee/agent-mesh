package daemon_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

// commit makes a commit on the worker's branch, as its work.
func (a *authorWorld) commit(t *testing.T) {
	t.Helper()
	cmd := exec.Command("git", "-C", a.worker.Worktree, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "work")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v %s", err, out)
	}
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
	a.commit(t)
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
	byAuthor := func() (n int64) {
		cost, err := a.asRoot.GetCost(context.Background(), connect.NewRequest(&v1.GetCostRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range cost.Msg.Verdicts {
			n += v.ByAuthor
		}
		return n
	}
	if n := byAuthor(); n != 1 {
		t.Errorf("by_author %d while the author's verdict stands, want 1", n)
	}
	if err := a.verdict(t, a.asRoot, reviewer.Name, 1, 2); err != nil {
		t.Fatal(err)
	}
	if n := byAuthor(); n != 0 {
		t.Errorf("by_author %d with the root's verdict standing, want 0", n)
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

// A round that is still open counts against the ceiling: two asked together must not both pass.
func TestAWorkerCannotAskAnotherRoundWhileOneIsOpen(t *testing.T) {
	a := startAuthor(t, "")
	a.pr.push(t, "the feature")
	a.set(t, "review.reviewers.small", []any{[]any{"claude"}})
	res, err := a.ask(t, &v1.AskRequest{Review: "7", Size: "small", Reviewers: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []*v1.AskRequest{
		{Review: "7", Size: "small", Reviewers: "auto"},
		{Review: "7", Size: "small", Reviewers: "claude"},
		{Review: "7", Size: "small", Kind: "claude"},
	} {
		if _, err := a.ask(t, req); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "still open") {
			t.Errorf("%v while a round is open: %v", req, err)
		}
	}
	// A round that ended with no answer does not count: the worker may run it again.
	a.say(t, res.Agents[0].Id, v1.MessageType_MESSAGE_TYPE_FAILED, "no")
	waitFor(t, "the round to finish", func() bool {
		k, _ := a.d.Store().Read(context.Background()).GetAsk(res.Ask.Id)
		return k.Status == v1.AskStatus_ASK_STATUS_FINISHED
	})
	if _, err := a.ask(t, &v1.AskRequest{Review: "7", Size: "small", Reviewers: "auto"}); err != nil {
		t.Errorf("a round after one nobody answered: %v", err)
	}
}

// A single reviewer's `done` goes to the worker, so the ask can't know it answered: the footer says how it was
// asked, and still flags the missing verdict.
func TestTheFooterNamesASingleReviewerAskedAloneAndItsMissingVerdict(t *testing.T) {
	a := startAuthor(t, "")
	a.pr.push(t, "the feature")
	res, err := a.ask(t, &v1.AskRequest{Review: "7", Size: "small", Kind: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	reviewer := res.Agents[0]
	a.say(t, reviewer.Id, v1.MessageType_MESSAGE_TYPE_DONE, "a.go:3")
	a.say(t, a.worker.Id, v1.MessageType_MESSAGE_TYPE_DONE, "PR "+a.pr.url()+" fixed")
	got := a.doneBody(t)
	for _, want := range []string{"one reviewer (small, mid)", reviewer.Name + " asked alone (its done went to " + a.worker.Name + ")", reviewer.Name + " no verdict recorded"} {
		if !strings.Contains(got, want) {
			t.Errorf("footer lacks %q: %s", want, got)
		}
	}
	if strings.Contains(got, "no answer") {
		t.Errorf("a single reviewer reported unanswered: %s", got)
	}
	if err := a.verdict(t, a.asWorker, reviewer.Name, 1, 0); err != nil {
		t.Fatal(err)
	}
	a.say(t, a.worker.Id, v1.MessageType_MESSAGE_TYPE_DONE, "PR "+a.pr.url()+" settled")
	if got := a.doneBody(t); strings.Contains(got, "no verdict recorded") || !strings.Contains(got, reviewer.Name+" 1/0/0/0 by author") {
		t.Errorf("footer after the verdict: %s", got)
	}
}

// The round's own `[mesh answers]` is unread mail from the daemon: it is the worker's next work, so the tick that
// settles the round (an overdue one here) must not nudge it for a report it has had no time to write.
func TestAWorkerIsNotNudgedInTheTickItsRoundSettlesIn(t *testing.T) {
	a := newAcct(t)
	a.setting("accountability.nudge_after", 1.0)
	a.state(v1.AgentState_AGENT_STATE_IDLE, time.Now())
	a.taskRead()
	err := a.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		return tx.PutAsk(&v1.Ask{Id: "k1", RunId: a.run.Id, CallerId: "a-w1", CallerName: "repo-one", Kind: v1.AskKind_ASK_KIND_REVIEW,
			Status: v1.AskStatus_ASK_STATUS_OPEN, Collected: true, CreatedAt: timestamppb.New(time.Now().Add(-time.Hour)),
			Deadline: timestamppb.New(time.Now().Add(-time.Minute)), AnswersPath: filepath.Join(t.TempDir(), "a.md"),
			Members: []*v1.AskMember{{AgentId: "a-gone", Name: "repo-review-7-claude", Status: v1.AskMemberStatus_ASK_MEMBER_STATUS_PENDING}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	a.pass()
	if n := a.mailOf("a-w1", "[mesh answers"); n != 1 {
		t.Fatalf("answers to the worker %d, want 1", n)
	}
	if n := a.mailOf("a-w1", "[mesh nudge]"); n != 0 {
		t.Errorf("nudged with its round's answers unread: %d", n)
	}
}

// A worker that asked one reviewer and ended its turn waits on it: no nudge until that reviewer reports.
func TestAWorkerWaitingOnItsSingleReviewerIsNotNudged(t *testing.T) {
	a := newAcct(t)
	a.setting("accountability.nudge_after", 1.0)
	a.state(v1.AgentState_AGENT_STATE_IDLE, time.Now())
	a.taskRead()
	revTok := a.seedAgent(a.run.Id, "a-r1", "repo-review-7-claude", "a-w1", v1.AgentRole_AGENT_ROLE_REVIEWER)
	err := a.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		return tx.PutAsk(&v1.Ask{Id: "k1", RunId: a.run.Id, CallerId: "a-w1", CallerName: "repo-one", Kind: v1.AskKind_ASK_KIND_REVIEW,
			Status: v1.AskStatus_ASK_STATUS_FINISHED, CreatedAt: timestamppb.Now(),
			Members: []*v1.AskMember{{AgentId: "a-r1", Name: "repo-review-7-claude", Status: v1.AskMemberStatus_ASK_MEMBER_STATUS_PENDING}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	a.pass()
	if n := a.mailOf("a-w1", "[mesh nudge]"); n != 0 {
		t.Fatalf("nudged while its single reviewer runs: %d", n)
	}
	if err := a.send(revTok, "repo-one", v1.MessageType_MESSAGE_TYPE_DONE, "a.go:3"); err != nil {
		t.Fatal(err)
	}
	readInbox(t, a.as(a.workerTok))
	a.state(v1.AgentState_AGENT_STATE_IDLE, time.Now().Add(-2*time.Second))
	a.pass()
	if n := a.mailOf("a-w1", "[mesh nudge]"); n != 1 {
		t.Errorf("nudges once the reviewer has reported: %d", n)
	}
}

// Two repos' workers can share a branch name: the PR must be of the worker's own repo too.
func TestAWorkerReviewsNoPROfAnotherRepoEvenOnItsBranchName(t *testing.T) {
	a := startAuthor(t, "")
	a.pr.push(t, "the feature") // the head is the worker's branch name, as another repo's worker of the same slug would have
	other := gitRepo(t)
	for _, req := range []*v1.AskRequest{
		{Repo: other, Review: "7", Size: "small", Reviewers: "auto"},
		{Repo: other, Review: "7", Size: "small", Kind: "claude"},
	} {
		if _, err := a.ask(t, req); code(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "own repo") {
			t.Errorf("%v: %v", req, err)
		}
	}
}

// The repo default (its own) is a worker's: a root that leaves out --repo is told to name it.
func TestARootReviewWithoutARepoIsRefused(t *testing.T) {
	a := startAuthor(t, "")
	a.pr.push(t, "the feature")
	_, err := a.asRoot.Ask(context.Background(), connect.NewRequest(&v1.AskRequest{Review: "7", Size: "small", Reviewers: "claude", Tier: "mid"}))
	if code(err) != connect.CodeInvalidArgument {
		t.Errorf("a root's review with no repo: %v", err)
	}
}

// A worker's hint says "last" when the ceiling would refuse another round, single reviewers it asked included.
func TestAWorkersHintCountsItsSingleReviewersTowardTheLastRound(t *testing.T) {
	a := startAuthor(t, "")
	a.pr.push(t, "the feature")
	a.set(t, "review.reviewers.small", []any{[]any{"claude"}})
	a.set(t, "review.rounds.small", 2)
	res, err := a.ask(t, &v1.AskRequest{Review: "7", Size: "small", Reviewers: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Hint, "its last") || !strings.Contains(res.Hint, "run this round again") {
		t.Errorf("round 1 of 2: %s", res.Hint)
	}
	a.say(t, res.Agents[0].Id, v1.MessageType_MESSAGE_TYPE_FAILED, "no")
	waitFor(t, "the round to finish", func() bool {
		k, _ := a.d.Store().Read(context.Background()).GetAsk(res.Ask.Id)
		return k.Status == v1.AskStatus_ASK_STATUS_FINISHED
	})
	// One single reviewer used up one of the two: the next round is the last, though it is round 1.
	if _, err := a.ask(t, &v1.AskRequest{Review: "7", Size: "small", Kind: "claude"}); err != nil {
		t.Fatal(err)
	}
	again, err := a.ask(t, &v1.AskRequest{Review: "7", Size: "small", Reviewers: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(again.Hint, "round 2 of at most 2") || !strings.Contains(again.Hint, "its last") || strings.Contains(again.Hint, "run this round again") {
		t.Errorf("a round the ceiling leaves no other after: %s", again.Hint)
	}
}

// The "asked no round" line reads every ask of the PR, and a reused worker's older recorded PRs don't hide it.
func TestTheFooterNamesWhoAskedTheRoundsOfAPRTheWorkerDidNot(t *testing.T) {
	a := startAuthor(t, "")
	a.pr.push(t, "the feature")
	a.commit(t)
	// The root ran a round on the worker's PR.
	a.reviewWorld.ask(t, &v1.AskRequest{Repo: a.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "claude"})
	a.say(t, a.worker.Id, v1.MessageType_MESSAGE_TYPE_DONE, "PR "+a.pr.url()+" opened")
	got := a.doneBody(t)
	if !strings.Contains(got, a.worker.Name+" asked no review round itself; rounds of it were asked by "+a.lead.Name) {
		t.Errorf("rounds the root asked: %s", got)
	}
	// An older PR of a reused worker is recorded; the done names a new one with no round on it at all.
	err := a.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		return tx.PutAgentPR(&v1.PullRequest{AgentId: a.worker.Id, Url: strings.TrimSuffix(a.pr.url(), "7") + "6", Number: 6})
	})
	if err != nil {
		t.Fatal(err)
	}
	a.say(t, a.worker.Id, v1.MessageType_MESSAGE_TYPE_DONE, "PR "+strings.TrimSuffix(a.pr.url(), "7")+"8 opened")
	if got := a.doneBody(t); !strings.Contains(got, "PR #8: "+a.worker.Name+" asked no review round: ask it why") {
		t.Errorf("a new PR beside a stale recorded one: %s", got)
	}
}

func TestAVerdictsNotFoundErrorSaysByYouOnlyToAWorker(t *testing.T) {
	a := startAuthor(t, "")
	a.pr.push(t, "the feature")
	if err := a.verdict(t, a.asRoot, "nobody", 1, 0); code(err) != connect.CodeNotFound || strings.Contains(err.Error(), "by you") {
		t.Errorf("root: %v", err)
	}
	if err := a.verdict(t, a.asWorker, "nobody", 1, 0); code(err) != connect.CodeNotFound || !strings.Contains(err.Error(), "by you") {
		t.Errorf("worker: %v", err)
	}
}

// A reviewer that hung without reporting does not hold its asker's nudge past `ask.timeout`.
func TestAWorkerIsNudgedOnceItsSingleReviewerHasHadAskTimeoutToAnswer(t *testing.T) {
	a := newAcct(t)
	a.setting("accountability.nudge_after", 1.0)
	a.setting("ask.timeout", 60.0)
	a.state(v1.AgentState_AGENT_STATE_IDLE, time.Now())
	a.taskRead()
	a.seedAgent(a.run.Id, "a-r1", "repo-review-7-claude", "a-w1", v1.AgentRole_AGENT_ROLE_REVIEWER)
	err := a.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		return tx.PutAsk(&v1.Ask{Id: "k1", RunId: a.run.Id, CallerId: "a-w1", CallerName: "repo-one", Kind: v1.AskKind_ASK_KIND_REVIEW,
			Status: v1.AskStatus_ASK_STATUS_FINISHED, CreatedAt: timestamppb.New(time.Now().Add(-2 * time.Minute)),
			Members: []*v1.AskMember{{AgentId: "a-r1", Name: "repo-review-7-claude", Status: v1.AskMemberStatus_ASK_MEMBER_STATUS_PENDING}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	a.pass()
	if n := a.mailOf("a-w1", "[mesh nudge]"); n != 1 {
		t.Errorf("nudges with its reviewer long overdue: %d", n)
	}
}

// Only an answers info of the daemon (newer than the task) is the agent's next work; its other infos hold nothing.
func TestOnlyARoundsAnswersHoldAWorkersNudge(t *testing.T) {
	a := newAcct(t)
	a.setting("accountability.nudge_after", 1.0)
	a.state(v1.AgentState_AGENT_STATE_IDLE, time.Now())
	tell := func(body string, at time.Time) {
		t.Helper()
		err := a.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
			return tx.PutMessage(&v1.Message{Id: "m-" + body[:12], RunId: a.run.Id, From: &v1.Actor{Id: "meshd", Name: "mesh"}, ToId: "a-w1", ToName: "repo-one",
				Type: v1.MessageType_MESSAGE_TYPE_INFO, Body: body, CreatedAt: timestamppb.New(at)})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	tell("[mesh answers k0] an answer from before the task", time.Now().Add(-time.Hour))
	a.taskRead()
	// The task's reading marks the first one read too: write the unread ones after it.
	tell("[mesh alert exited x] a child ended", time.Now())
	time.Sleep(1200 * time.Millisecond)
	a.pass()
	if n := a.mailOf("a-w1", "[mesh nudge]"); n != 1 {
		t.Fatalf("nudges with only an alert unread: %d", n)
	}
}

// The round ceiling is checked where the ask is stored: two rounds asked at once can't both pass.
func TestTwoRoundsAskedAtOnceDoNotBothPassTheCeiling(t *testing.T) {
	a := startAuthor(t, "")
	a.pr.push(t, "the feature")
	a.set(t, "review.reviewers.small", []any{[]any{"claude"}})
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = a.ask(t, &v1.AskRequest{Review: "7", Size: "small", Reviewers: "auto"})
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		}
	}
	if ok != 1 {
		t.Errorf("%d of 3 simultaneous rounds started, want 1: %v", ok, errs)
	}
}

// A done of a worker with no commits that names a PR names somebody else's: no "asked no round" line.
func TestTheFooterSaysNothingOfAPRAWorkerWithNoCommitsNamed(t *testing.T) {
	a := startAuthor(t, "")
	a.say(t, a.worker.Id, v1.MessageType_MESSAGE_TYPE_DONE, "I read PR "+a.pr.url()+" and found nothing")
	if got := a.doneBody(t); strings.Contains(got, "[mesh reviews]") {
		t.Errorf("a footer on a worker with no work: %s", got)
	}
	a.commit(t)
	a.say(t, a.worker.Id, v1.MessageType_MESSAGE_TYPE_DONE, "PR "+a.pr.url()+" opened")
	if got := a.doneBody(t); !strings.Contains(got, "asked no review round") {
		t.Errorf("no line once it has commits: %s", got)
	}
}

// The footer finds a PR's rounds however the done spells its URL.
func TestTheFooterFindsRoundsByTheNormalizedPRURL(t *testing.T) {
	a := startAuthor(t, "")
	a.pr.push(t, "the feature")
	a.commit(t)
	a.reviewWorld.ask(t, &v1.AskRequest{Repo: a.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "claude"})
	typed := "https://github.com/O/Shop-API/pull/7"
	a.say(t, a.worker.Id, v1.MessageType_MESSAGE_TYPE_DONE, "PR "+typed+" opened")
	if got := a.doneBody(t); !strings.Contains(got, "asked no review round itself; rounds of it were asked by "+a.lead.Name) {
		t.Errorf("a differently typed URL: %s", got)
	}
}

func TestAReviewTierCannotBeTop(t *testing.T) {
	w := startReviews(t)
	for _, k := range []string{"review.tier.small", "review.tier.large"} {
		_, err := w.device.Mesh.SetSetting(context.Background(), connect.NewRequest(&v1.SetSettingRequest{Meta: meta(""), Key: k, Value: val(t, "top")}))
		if code(err) != connect.CodeInvalidArgument {
			t.Errorf("%s = top: %v", k, err)
		}
	}
}

// A round with a model names a one-reviewer ask's option: the error says so, not that the tier is missing.
func TestARoundWithAModelIsToldThatModelIsForOneReviewer(t *testing.T) {
	a := startAuthor(t, "")
	a.pr.push(t, "the feature")
	_, err := a.ask(t, &v1.AskRequest{Review: "7", Size: "small", Reviewers: "auto", Model: "opus"})
	if code(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "model and kind are for one reviewer") {
		t.Errorf("round with a model: %v", err)
	}
}

// A PR given by URL names its repo: it must be the worker's own, though no --repo says otherwise.
func TestAWorkerReviewsNoPRByURLOfAnotherGitHubRepo(t *testing.T) {
	a := startAuthor(t, "")
	a.pr.push(t, "the feature")
	a.set(t, "review.reviewers.small", []any{[]any{"claude"}})
	// The origin is a GitHub URL that git maps back to the local repo, so fetches still work.
	for _, args := range [][]string{
		{"config", "url." + a.pr.repo + ".insteadOf", "https://github.com/o/shop-api.git"},
		{"remote", "set-url", "origin", "https://github.com/o/shop-api.git"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", a.pr.repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	_, err := a.ask(t, &v1.AskRequest{Review: "https://github.com/o/other/pull/7", Size: "small", Reviewers: "auto"})
	if code(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "own repo") {
		t.Errorf("another repo's PR URL: %v", err)
	}
	if _, err := a.ask(t, &v1.AskRequest{Review: "https://github.com/O/Shop-API/pull/7", Size: "small", Reviewers: "auto"}); err != nil {
		t.Errorf("its own repo's PR URL: %v", err)
	}
	// A fork's origin, the PR on its upstream remote: the PR is still its own.
	if out, err := exec.Command("git", "-C", a.pr.repo, "remote", "add", "upstream", "https://github.com/org/shop-api.git").CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v %s", err, out)
	}
	if _, err := a.ask(t, &v1.AskRequest{Review: "https://github.com/org/shop-api/pull/7", Size: "small", Reviewers: "auto"}); code(err) == connect.CodePermissionDenied {
		t.Errorf("a PR on its upstream remote: %v", err)
	}
}
