package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
)

// fakePR is a repo whose `feature` branch is a pull request, and a fake gh that prints it. gh is only ever run
// as `pr view`: the daemon reads PRs, it never posts.
type fakePR struct {
	repo, json string
	number     int
	gh         string
	headRef    string // the PR's head branch as gh says it; "feature" unless a test makes the PR a worker's own
}

func newFakePR(t *testing.T, number int) *fakePR {
	t.Helper()
	repo := gitRepo(t)
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("remote", "add", "origin", repo) // fetches of main and of a PR head go to itself
	dir := t.TempDir()
	p := &fakePR{repo: repo, number: number, json: filepath.Join(dir, "pr.json"), gh: filepath.Join(dir, "gh"), headRef: "feature"}
	script := "#!/bin/sh\n[ \"$1 $2\" = \"pr view\" ] || { echo \"fake gh: only pr view\" >&2; exit 2; }\ncat " + p.json + "\n"
	if err := os.WriteFile(p.gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func (p *fakePR) tweak(c *daemon.AgentConfig) { c.Binaries["gh"] = p.gh }

// push adds a commit to the PR's branch and points the fake gh at it: the PR's new head.
func (p *fakePR) push(t *testing.T, msg string) string {
	t.Helper()
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", p.repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	parent := "main"
	if _, err := exec.Command("git", "-C", p.repo, "rev-parse", "--verify", "-q", "feature").Output(); err == nil {
		parent = "feature"
	}
	sha := run("commit-tree", "main^{tree}", "-p", parent, "-m", msg)
	run("update-ref", "refs/heads/feature", sha)
	b, _ := json.Marshal(map[string]any{"number": p.number, "url": fmt.Sprintf("https://github.com/o/shop-api/pull/%d", p.number),
		"headRefOid": sha, "headRefName": p.headRef, "baseRefName": "main"})
	if err := os.WriteFile(p.json, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return sha
}

func (p *fakePR) url() string { return fmt.Sprintf("https://github.com/o/shop-api/pull/%d", p.number) }

// reviewWorld is a daemon with the fake gh and a root that asks.
type reviewWorld struct {
	*reopenWorld
	pr     *fakePR
	run    *v1.Run
	lead   *v1.Agent
	asRoot agentmeshv1connect.MeshServiceClient
}

func startReviews(t *testing.T, tweaks ...func(*daemon.AgentConfig)) *reviewWorld {
	t.Helper()
	pr := newFakePR(t, 7)
	fast := func(c *daemon.AgentConfig) { c.AccountEvery = 100 * time.Millisecond }
	w := startReopen(t, append([]func(*daemon.AgentConfig){pr.tweak, fast}, tweaks...)...)
	run := startRun(t, w.device.Mesh, "")
	lead, asRoot := w.root(t, run.Id)
	return &reviewWorld{reopenWorld: w, pr: pr, run: run, lead: lead, asRoot: asRoot}
}

func (w *reviewWorld) ask(t *testing.T, req *v1.AskRequest) *v1.AskResponse {
	t.Helper()
	res, err := w.asRoot.Ask(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg
}

// say is a member sending its parent a report, as its own token: after reading its task, as an agent does.
func (w *reviewWorld) say(t *testing.T, agentID string, typ v1.MessageType, body string) *v1.Message {
	t.Helper()
	c := w.as(w.token(t, agentID))
	readInbox(t, c)
	res, err := c.SendMessage(context.Background(), connect.NewRequest(&v1.SendMessageRequest{To: "parent", Type: typ, Body: body}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.Message
}

// quiet waits until an agent has taken its task's doorbell and ended the turn it started (do it before the
// agent reads its inbox: a task read first is never rung).
func (w *reviewWorld) quiet(t *testing.T, id string) {
	t.Helper()
	waitFor(t, "its task delivered", func() bool {
		in, _ := w.d.Store().Read(context.Background()).Inbox(id, true)
		return len(in) > 0 && in[len(in)-1].DeliveredAt != nil
	})
	w.waitState(id, v1.AgentState_AGENT_STATE_IDLE)
}

// roundMessages are the messages the root got that announce a finished ask.
func (w *reviewWorld) answers(t *testing.T) []*v1.Message {
	t.Helper()
	msgs, err := w.d.Store().Read(context.Background()).RunMessages(w.run.Id, "", -1)
	if err != nil {
		t.Fatal(err)
	}
	var out []*v1.Message
	for _, m := range msgs {
		if m.ToId == w.lead.Id && strings.HasPrefix(m.Body, "[mesh answers ") {
			out = append(out, m)
		}
	}
	return out
}

func (w *reviewWorld) task(t *testing.T, agentID string) string {
	t.Helper()
	msgs, err := w.d.Store().Read(context.Background()).RunMessages(w.run.Id, "", -1)
	if err != nil {
		t.Fatal(err)
	}
	var body string
	for _, m := range msgs {
		if m.ToId == agentID && m.Type == v1.MessageType_MESSAGE_TYPE_TASK {
			body = m.Body // the newest
		}
	}
	return body
}

func TestAFreshQuestionStartsAnAskerWithTheReadOnlyPrefix(t *testing.T) {
	w := startReviews(t)
	res := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Tier: "mid", Body: "Where is the retry logic, exactly?"})
	if len(res.Agents) != 1 || res.Ask != nil {
		t.Fatalf("response %v", res)
	}
	a := res.Agents[0]
	if a.Role != v1.AgentRole_AGENT_ROLE_ASKER || a.ParentId != w.lead.Id || a.Name != "shop-api-ask-where-is-the-ret" || !a.DenyEdits {
		t.Fatalf("asker %v", a)
	}
	task := w.task(t, a.Id)
	if !strings.HasPrefix(task, "Question (read-only): answer it from the code.") || !strings.HasSuffix(task, "Where is the retry logic, exactly?") {
		t.Errorf("task %q", task)
	}
	// The same question again: a free name.
	if again := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Tier: "mid", Body: "Where is the retry logic, exactly?"}); again.Agents[0].Name != "shop-api-ask-where-is-the-ret-2" {
		t.Errorf("second asker %s", again.Agents[0].Name)
	}
	// A device has no one to answer to.
	if _, err := w.device.Mesh.Ask(context.Background(), connect.NewRequest(&v1.AskRequest{Repo: w.pr.repo, Tier: "mid", Body: "x"})); code(err) != connect.CodePermissionDenied {
		t.Errorf("a device's ask: %v", err)
	}
	// Needs a body, a repo or an agent, and a tier.
	for _, bad := range []*v1.AskRequest{{Repo: w.pr.repo, Tier: "mid"}, {Body: "x"}, {Repo: w.pr.repo, Body: "x", Reviewers: "all"}} {
		if _, err := w.asRoot.Ask(context.Background(), connect.NewRequest(bad)); code(err) != connect.CodeInvalidArgument {
			t.Errorf("ask %v: %v", bad, err)
		}
	}
}

func TestAskingAnExistingAgentSendsItAReadOnlyTaskAndResumesARetiredOne(t *testing.T) {
	w := startReviews(t)
	ctx := context.Background()
	first := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Tier: "mid", Body: "first", Slug: "q"}).Agents[0]
	w.waitState(first.Id, v1.AgentState_AGENT_STATE_IDLE)
	res := w.ask(t, &v1.AskRequest{Agent: first.Name, Body: "and the second thing?"})
	if res.Message == nil || !strings.HasPrefix(res.Message.Body, "Question (read-only)") || !strings.HasSuffix(res.Message.Body, "and the second thing?") {
		t.Fatalf("message %v", res.Message)
	}
	if got := w.agent(first.Id); got.Role != v1.AgentRole_AGENT_ROLE_ASKER || !got.DenyEdits {
		t.Errorf("the asker after a question %v", got)
	}
	// Retired: resumed, in its own conversation.
	if _, err := w.asRoot.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: first.Id})); err != nil {
		t.Fatal(err)
	}
	w.waitState(first.Id, v1.AgentState_AGENT_STATE_RETIRED)
	res = w.ask(t, &v1.AskRequest{Agent: first.Name, Body: "once more"})
	if len(res.Notes) == 0 || !strings.Contains(res.Notes[0], "resumed") {
		t.Errorf("notes %v", res.Notes)
	}
	// Not another agent's child.
	other := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Tier: "mid", Body: "other", Slug: "o"}).Agents[0]
	asOther := w.as(w.token(t, other.Id))
	if _, err := asOther.Ask(ctx, connect.NewRequest(&v1.AskRequest{Agent: first.Name, Body: "hi"})); code(err) == 0 {
		t.Errorf("an asker asked its sibling")
	}
}

func TestARoundStartsEveryReviewerAtTheHeadAndTheCallerIsRungOnce(t *testing.T) {
	w := startReviews(t)
	ctx := context.Background()
	head := w.pr.push(t, "the feature")
	w.set(t, "review.reviewers.small", []any{[]any{"claude"}, []any{"copilot", "agy"}})
	res := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "auto", Body: "mind the retry"})
	if len(res.Agents) != 2 || res.Ask == nil || !res.Ask.Collected || res.Ask.Round != 1 || res.Ask.Head != head || res.Ask.Pr != w.pr.url() {
		t.Fatalf("response %v", res)
	}
	byKind := map[string]*v1.Agent{}
	for _, a := range res.Agents {
		byKind[a.Kind] = a
		if a.Role != v1.AgentRole_AGENT_ROLE_REVIEWER || !a.DenyEdits || a.Name != fmt.Sprintf("shop-api-review-7-%s", a.Kind) {
			t.Errorf("reviewer %v", a)
		}
	}
	if byKind["claude"] == nil || byKind["agy"] == nil {
		t.Fatalf("kinds %v (copilot is not installed: its slot takes agy)", byKind)
	}
	// Each starts at the PR's head, with the review text; Claude's also runs /code-review at the size's level.
	for kind, a := range byKind {
		wt, _ := exec.Command("git", "-C", a.Worktree, "rev-parse", "HEAD").Output()
		if strings.TrimSpace(string(wt)) != head {
			t.Errorf("%s's worktree is at %s, not %s", kind, wt, head)
		}
		task := w.task(t, a.Id)
		for _, want := range []string{"Review (read-only) of " + w.pr.url() + " at " + head[:9], "Its head is checked out in your worktree", "mind the retry"} {
			if !strings.Contains(task, want) {
				t.Errorf("%s's task lacks %q: %s", kind, want, task)
			}
		}
		if got := strings.Contains(task, "/code-review medium "); got != (kind == "claude") {
			t.Errorf("%s's task /code-review: %v", kind, got)
		}
	}
	if !strings.Contains(res.Hint, "round 1 of at most 1 for a small PR") || !strings.Contains(res.Hint, "its last") {
		t.Errorf("hint %q", res.Hint)
	}
	if len(res.Notes) == 0 {
		t.Errorf("no note about the skipped copilot")
	}

	// The first answer is held: no message to the root, and it is marked delivered so it never rings.
	m1 := w.say(t, byKind["claude"].Id, v1.MessageType_MESSAGE_TYPE_DONE, "claude: one blocking finding, a.go:3")
	if m1.DeliveredAt == nil || m1.ReadAt == nil || len(w.answers(t)) != 0 {
		t.Fatalf("held answer %v, answers %d", m1, len(w.answers(t)))
	}
	list, err := w.asRoot.ListAsks(ctx, connect.NewRequest(&v1.ListAsksRequest{OpenOnly: true}))
	if err != nil || len(list.Msg.Asks) != 1 {
		t.Fatalf("open asks %v, %v", list, err)
	}
	// The second completes the round: one message, the file, the ask finished.
	w.say(t, byKind["agy"].Id, v1.MessageType_MESSAGE_TYPE_DONE, "agy: nothing found")
	waitFor(t, "the round's message", func() bool { return len(w.answers(t)) == 1 })
	info := w.answers(t)[0]
	for _, want := range []string{"2 of 2 reviewers answered", head[:9], "answers: " + res.Ask.AnswersPath, "round 1 of at most 1"} {
		if !strings.Contains(info.Body, want) {
			t.Errorf("message lacks %q: %s", want, info.Body)
		}
	}
	file, err := os.ReadFile(res.Ask.AnswersPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Review of " + w.pr.url() + " at " + head[:9], "<!-- mesh: 2 reviews -->", "in run " + w.run.Id + ", size small, of 2 reviewer(s)",
		"## Notes to the reviewers\n\nmind the retry", "## shop-api-review-7-claude (claude)\n\nclaude: one blocking finding, a.go:3", "## shop-api-review-7-agy (agy)\n\nagy: nothing found"} {
		if !strings.Contains(string(file), want) {
			t.Errorf("file lacks %q:\n%s", want, file)
		}
	}
	if !strings.Contains(filepath.Base(res.Ask.AnswersPath), "review-7-") || !strings.Contains(res.Ask.AnswersPath, "/reports/") {
		t.Errorf("path %s", res.Ask.AnswersPath)
	}
	done, _ := w.asRoot.ListAsks(ctx, connect.NewRequest(&v1.ListAsksRequest{Pr: "7"}))
	if len(done.Msg.Asks) != 1 || done.Msg.Asks[0].Status != v1.AskStatus_ASK_STATUS_FINISHED || answeredOf(done.Msg.Asks[0]) != 2 {
		t.Errorf("finished ask %v", done.Msg.Asks)
	}
	// Nothing was left in the root's inbox but the one message: the held answers never ring or show.
	unread, _ := w.d.Store().Read(ctx).Inbox(w.lead.Id, false)
	for _, m := range unread {
		if m.Type == v1.MessageType_MESSAGE_TYPE_DONE {
			t.Errorf("a held answer is unread: %v", m)
		}
	}
}

func answeredOf(a *v1.Ask) int {
	n := 0
	for _, m := range a.Members {
		if m.Status == v1.AskMemberStatus_ASK_MEMBER_STATUS_ANSWERED {
			n++
		}
	}
	return n
}

func TestAMembersFailureQuestionExitOrSilenceSettlesItWithoutAnAnswer(t *testing.T) {
	w := startReviews(t, func(c *daemon.AgentConfig) { c.AccountEvery = 100 * time.Millisecond })
	ctx := context.Background()
	w.set(t, "ask.timeout", 2)
	w.pr.push(t, "the feature")
	res := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: w.pr.url(), Size: "large", Tier: "mid", Reviewers: "claude,agy"})
	var claude, agy *v1.Agent
	for _, a := range res.Agents {
		if a.Kind == "claude" {
			claude = a
		} else {
			agy = a
		}
	}
	// A question rings as usual and settles the member; the round is still open for the other.
	q := w.say(t, claude.Id, v1.MessageType_MESSAGE_TYPE_QUESTION, "Is the base main?")
	if q.DeliveredAt != nil {
		t.Errorf("a question was held: %v", q)
	}
	if len(w.answers(t)) != 0 {
		t.Fatalf("finished early")
	}
	// The other says nothing: the deadline settles it, and the root gets what there is.
	waitFor(t, "the deadline's message", func() bool { return len(w.answers(t)) == 1 })
	body := w.answers(t)[0].Body
	for _, want := range []string{"0 of 2 reviewers answered", "no answer: sent a question instead of an answer", "no answer: not answered within 2s (ask.timeout)", "this round didn't count"} {
		if !strings.Contains(body, want) {
			t.Errorf("message lacks %q: %s", want, body)
		}
	}
	file, _ := os.ReadFile(res.Ask.AnswersPath)
	if !strings.Contains(string(file), "No answer: not answered within 2s") || !strings.Contains(string(file), "<!-- mesh: 0 reviews -->") {
		t.Errorf("file %s", file)
	}
	_ = agy
	_ = ctx
}

func TestAMemberStoppedBeforeItAnswersSettlesTheRound(t *testing.T) {
	w := startReviews(t)
	w.pr.push(t, "the feature")
	res := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "claude,agy"})
	var claude, agy *v1.Agent
	for _, a := range res.Agents {
		if a.Kind == "claude" {
			claude = a
		} else {
			agy = a
		}
	}
	w.waitState(claude.Id, v1.AgentState_AGENT_STATE_IDLE)
	// Retired before it reported: the pass settles it.
	if _, err := w.asRoot.RetireAgent(context.Background(), connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: claude.Id})); err != nil {
		t.Fatal(err)
	}
	w.waitState(claude.Id, v1.AgentState_AGENT_STATE_RETIRED)
	if len(w.answers(t)) != 0 {
		t.Fatalf("finished with a member pending")
	}
	w.say(t, agy.Id, v1.MessageType_MESSAGE_TYPE_DONE, "agy: fine")
	waitFor(t, "the round's message", func() bool { return len(w.answers(t)) == 1 })
	body := w.answers(t)[0].Body
	if !strings.Contains(body, "1 of 2 reviewers answered") || !strings.Contains(body, "no answer: was stopped before it answered") {
		t.Errorf("message %s", body)
	}
}

func TestAReReviewInTheSameRunAsksTheSameReviewersAgainOnTheNewHead(t *testing.T) {
	w := startReviews(t)
	ctx := context.Background()
	w.set(t, "review.rounds.large", 3)
	head1 := w.pr.push(t, "the feature")
	round1 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "large", Tier: "mid", Reviewers: "claude,agy,copilot"})
	if len(round1.Agents) != 2 || len(round1.Notes) == 0 {
		t.Fatalf("round 1 %v", round1)
	}
	for _, a := range round1.Agents {
		w.quiet(t, a.Id)
		w.say(t, a.Id, v1.MessageType_MESSAGE_TYPE_DONE, "finding in "+a.Kind)
	}
	waitFor(t, "round 1's message", func() bool { return len(w.answers(t)) == 1 })

	head2 := w.pr.push(t, "the fix")
	// One of the two was retired meanwhile: it is resumed, in its own conversation.
	var retired *v1.Agent
	for _, a := range round1.Agents {
		if a.Kind == "claude" {
			retired = a
		}
	}
	w.waitState(retired.Id, v1.AgentState_AGENT_STATE_IDLE)
	if _, err := w.asRoot.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: retired.Id})); err != nil {
		t.Fatal(err)
	}
	w.waitState(retired.Id, v1.AgentState_AGENT_STATE_RETIRED)
	round2 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "large", Tier: "mid", Reviewers: "auto"})
	if round2.Ask.Round != 2 || round2.Ask.Since != head1 || round2.Ask.Head != head2 || len(round2.Agents) != 2 {
		t.Fatalf("round 2 %v", round2.Ask)
	}
	ids := map[string]string{}
	for _, m := range round2.Ask.Members {
		ids[m.Kind] = m.AgentId
		if m.How != "asked" && m.How != "resumed" {
			t.Errorf("%s was %q, not asked again", m.Name, m.How)
		}
	}
	for _, a := range round1.Agents {
		if ids[a.Kind] != a.Id {
			t.Errorf("%s: a different agent %s took round 2", a.Kind, ids[a.Kind])
		}
		task := w.task(t, a.Id)
		for _, want := range []string{"at " + head2[:9], "You last reviewed it at " + head1[:9], "git diff " + head1[:9] + " " + head2[:9]} {
			if !strings.Contains(task, want) {
				t.Errorf("%s's re-review task lacks %q: %s", a.Kind, want, task)
			}
		}
		if a.Kind == "claude" && !strings.Contains(task, "/code-review high "+head1[:9]+".."+head2[:9]) {
			t.Errorf("claude's /code-review target: %s", task)
		}
		wt, _ := exec.Command("git", "-C", w.agent(a.Id).Worktree, "rev-parse", "HEAD").Output()
		if strings.TrimSpace(string(wt)) != head2 {
			t.Errorf("%s's worktree stayed at %s", a.Kind, wt)
		}
	}
	if !strings.Contains(round2.Hint, "round 2 of at most 3") {
		t.Errorf("hint %q", round2.Hint)
	}
}

func TestAReReviewInAnotherRunStartsFreshReviewersOfTheSameKindsOnTheFixCommits(t *testing.T) {
	w := startReviews(t)
	head1 := w.pr.push(t, "the feature")
	round1 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "large", Tier: "mid", Reviewers: "claude,agy"})
	for _, a := range round1.Agents {
		w.say(t, a.Id, v1.MessageType_MESSAGE_TYPE_DONE, "finding")
	}
	waitFor(t, "round 1's message", func() bool { return len(w.answers(t)) == 1 })
	head2 := w.pr.push(t, "the fix")

	run2 := startRun(t, w.device.Mesh, "")
	_, asRoot2 := w.root(t, run2.Id)
	res, err := asRoot2.Ask(context.Background(), connect.NewRequest(&v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "large", Tier: "mid", Reviewers: "auto"}))
	if err != nil {
		t.Fatal(err)
	}
	ask := res.Msg.Ask
	if ask.Round != 2 || ask.Since != head1 || ask.Head != head2 || len(res.Msg.Agents) != 2 {
		t.Fatalf("round 2 %v", ask)
	}
	for _, m := range ask.Members {
		if m.How != "fresh" || m.AgentId == "" {
			t.Errorf("member %v", m)
		}
		task := ""
		msgs, _ := w.d.Store().Read(context.Background()).RunMessages(run2.Id, "", -1)
		for _, msg := range msgs {
			if msg.ToId == m.AgentId && msg.Type == v1.MessageType_MESSAGE_TYPE_TASK {
				task = msg.Body
			}
		}
		if !strings.Contains(task, "An earlier review covered this PR up to "+head1[:9]+": review only what changed since (`git diff "+head1[:9]+" "+head2[:9]+"`)") {
			t.Errorf("%s's task: %s", m.Name, task)
		}
	}
	// A rebased branch is no fix commit range: no since, the whole PR.
	if out, err := exec.Command("git", "-C", w.pr.repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit-tree", "main^{tree}", "-m", "rebased").Output(); err != nil {
		t.Fatal(err)
	} else {
		sha := strings.TrimSpace(string(out))
		exec.Command("git", "-C", w.pr.repo, "update-ref", "refs/heads/feature", sha).Run()
		b, _ := json.Marshal(map[string]any{"number": 7, "url": w.pr.url(), "headRefOid": sha, "headRefName": "feature", "baseRefName": "main"})
		os.WriteFile(w.pr.json, b, 0o644)
	}
	run3 := startRun(t, w.device.Mesh, "")
	_, asRoot3 := w.root(t, run3.Id)
	res3, err := asRoot3.Ask(context.Background(), connect.NewRequest(&v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "large", Tier: "mid", Reviewers: "claude"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range res3.Msg.Ask.Members {
		msgs, _ := w.d.Store().Read(context.Background()).RunMessages(run3.Id, "", -1)
		for _, msg := range msgs {
			if msg.ToId == m.AgentId && msg.Type == v1.MessageType_MESSAGE_TYPE_TASK && strings.Contains(msg.Body, "An earlier review covered") {
				t.Errorf("a rebased branch's task reads only the fix commits: %s", msg.Body)
			}
		}
	}
}

func TestAReviewNeedsASizeAndAPRGhCanRead(t *testing.T) {
	w := startReviews(t)
	ctx := context.Background()
	w.pr.push(t, "the feature")
	for name, req := range map[string]*v1.AskRequest{
		"no size":           {Repo: w.pr.repo, Review: "7", Tier: "mid", Reviewers: "all"},
		"an option as PR":   {Repo: w.pr.repo, Review: "--web", Size: "small", Tier: "mid", Reviewers: "all"},
		"since not a hash":  {Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "all", Since: "--all"},
		"a model and round": {Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "all", Model: "opus"},
	} {
		if _, err := w.asRoot.Ask(ctx, connect.NewRequest(req)); code(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	// gh can't read it: a clear refusal, nothing started.
	if err := os.WriteFile(w.pr.gh, []byte("#!/bin/sh\necho 'no pull requests found' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := w.asRoot.Ask(ctx, connect.NewRequest(&v1.AskRequest{Repo: w.pr.repo, Review: "9", Size: "small", Tier: "mid", Reviewers: "all"}))
	if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "no pull requests found") {
		t.Errorf("an unreadable PR: %v", err)
	}
	agents, _ := w.asRoot.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{}))
	for _, a := range agents.Msg.Agents {
		if a.Role == v1.AgentRole_AGENT_ROLE_REVIEWER {
			t.Errorf("a reviewer was started: %v", a.Name)
		}
	}
}

func TestAVerdictIsRecordedOnTheReviewersNewestRoundAndChecked(t *testing.T) {
	w := startReviews(t)
	ctx := context.Background()
	w.pr.push(t, "the feature")
	round := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "claude,agy"})
	claude := round.Agents[0]
	for _, bad := range []*v1.RecordVerdictRequest{
		{Pr: "7", Reviewer: claude.Name, Agree: 1, Blocking: 2}, // more blocking than agreed or useful
		{Pr: "7", Reviewer: claude.Name, Agree: -1, Other: 2},   // negative
		{Reviewer: claude.Name, Agree: 1},                       // no PR
	} {
		if _, err := w.asRoot.RecordVerdict(ctx, connect.NewRequest(bad)); code(err) != connect.CodeInvalidArgument {
			t.Errorf("verdict %v: %v", bad, err)
		}
	}
	if _, err := w.asRoot.RecordVerdict(ctx, connect.NewRequest(&v1.RecordVerdictRequest{Meta: meta(""), Pr: "7", Reviewer: "nobody", Agree: 1})); code(err) != connect.CodeNotFound {
		t.Errorf("an unknown reviewer: %v", err)
	}
	res, err := w.asRoot.RecordVerdict(ctx, connect.NewRequest(&v1.RecordVerdictRequest{Meta: meta(""), Pr: w.pr.url(), Reviewer: claude.Name,
		Agree: 2, Other: 1, Disagree: 1, Blocking: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if v := res.Msg.Verdict; v.Kind != claude.Kind || v.Tier != "mid" || v.Model == "" || v.Agree != 2 || v.Blocking != 1 || v.Reviewer != claude.Name {
		t.Errorf("verdict %v", v)
	}
	list, _ := w.asRoot.ListAsks(ctx, connect.NewRequest(&v1.ListAsksRequest{Pr: "7"}))
	if len(list.Msg.Asks) != 1 || len(list.Msg.Asks[0].Verdicts) != 1 {
		t.Errorf("asks %v", list.Msg.Asks)
	}
	// Said again: replaced, not added.
	if _, err := w.asRoot.RecordVerdict(ctx, connect.NewRequest(&v1.RecordVerdictRequest{Meta: meta(""), Pr: "7", Reviewer: claude.Name, Agree: 1})); err != nil {
		t.Fatal(err)
	}
	list, _ = w.asRoot.ListAsks(ctx, connect.NewRequest(&v1.ListAsksRequest{Pr: "7"}))
	if vs := list.Msg.Asks[0].Verdicts; len(vs) != 1 || vs[0].Agree != 1 {
		t.Errorf("verdicts %v", vs)
	}
}

func TestAMemberThatCrashesSettlesWithItsExitAlertInsteadOfRingingTheCaller(t *testing.T) {
	w := startReviews(t)
	w.pr.push(t, "the feature")
	res := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "claude,agy"})
	var claude, agy *v1.Agent
	for _, a := range res.Agents {
		if a.Kind == "claude" {
			claude = a
		} else {
			agy = a
		}
	}
	// Its task's doorbell rang and its turn is over: then it takes typed input.
	w.quiet(t, claude.Id)
	w.typeInto(w.agent(claude.Id), "/crash\r")
	w.waitState(claude.Id, v1.AgentState_AGENT_STATE_EXITED)
	w.say(t, agy.Id, v1.MessageType_MESSAGE_TYPE_DONE, "agy: fine")
	waitFor(t, "the round's message", func() bool { return len(w.answers(t)) == 1 })
	if body := w.answers(t)[0].Body; !strings.Contains(body, "1 of 2 reviewers answered") || !strings.Contains(body, "LOST, its answer is not coming: shop-api-review-7-claude exited with code 7 with no report to you.") {
		t.Errorf("message %s", body)
	}
	// The alert itself was not sent: the round's one message carries it.
	msgs, _ := w.d.Store().Read(context.Background()).RunMessages(w.run.Id, "", -1)
	for _, m := range msgs {
		if m.ToId == w.lead.Id && strings.HasPrefix(m.Body, "[mesh alert exited") {
			t.Errorf("the exit alert rang: %s", m.Body)
		}
	}
}

func TestAReusedReviewersWorktreeThatCantMoveStaysAndSaysSo(t *testing.T) {
	w := startReviews(t)
	head1 := w.pr.push(t, "the feature")
	round1 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "large", Tier: "mid", Reviewers: "claude"})
	rev := round1.Agents[0]
	w.quiet(t, rev.Id)
	w.say(t, rev.Id, v1.MessageType_MESSAGE_TYPE_DONE, "finding")
	waitFor(t, "round 1's message", func() bool { return len(w.answers(t)) == 1 })
	// The reviewer left a file in its worktree: it is not moved.
	if err := os.WriteFile(filepath.Join(rev.Worktree, "scratch.txt"), []byte("notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	head2 := w.pr.push(t, "the fix")
	res := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "large", Tier: "mid", Reviewers: "auto"})
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "stays at "+head1[:9]) || !strings.Contains(res.Notes[0], "has changes") {
		t.Errorf("notes %v", res.Notes)
	}
	task := w.task(t, rev.Id)
	for _, want := range []string{"Don't switch your branch", "Your worktree is still at " + head1[:9] + ", not the new head, because its worktree has changes",
		"git show " + head2[:9] + ":<path>", "check each finding with `git show " + head2[:9]} {
		if !strings.Contains(task, want) {
			t.Errorf("task lacks %q: %s", want, task)
		}
	}
	if wt, _ := exec.Command("git", "-C", rev.Worktree, "rev-parse", "HEAD").Output(); strings.TrimSpace(string(wt)) != head1 {
		t.Errorf("the worktree moved to %s", wt)
	}
}

func TestARoundAskedTwiceWithTheSameMutationIdStartsReviewersOnce(t *testing.T) {
	w := startReviews(t)
	w.pr.push(t, "the feature")
	req := func() *v1.AskRequest {
		return &v1.AskRequest{Meta: meta("ask-1"), Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "claude,agy"}
	}
	first := w.ask(t, req())
	second := w.ask(t, req())
	if first.Ask.Id != second.Ask.Id || len(second.Agents) != 0 && len(second.Agents) != 2 {
		t.Fatalf("first %v, second %v", first.Ask.Id, second.Ask)
	}
	agents, _ := w.asRoot.ListAgents(context.Background(), connect.NewRequest(&v1.ListAgentsRequest{}))
	n := 0
	for _, a := range agents.Msg.Agents {
		if a.Role == v1.AgentRole_AGENT_ROLE_REVIEWER {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d reviewers after a retried ask", n)
	}
}

func TestARoundOnTheSameHeadAgainStartsFreshReviewers(t *testing.T) {
	w := startReviews(t)
	w.pr.push(t, "the feature")
	round1 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "large", Tier: "mid", Reviewers: "claude,agy"})
	for _, a := range round1.Agents {
		w.quiet(t, a.Id)
		w.say(t, a.Id, v1.MessageType_MESSAGE_TYPE_DONE, "finding")
	}
	waitFor(t, "round 1's message", func() bool { return len(w.answers(t)) == 1 })
	round2 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "large", Tier: "mid", Reviewers: "auto"})
	if round2.Ask.Round != 2 || round2.Ask.Since != "" || len(round2.Agents) == 0 {
		t.Fatalf("round 2 %v", round2.Ask)
	}
	for _, m := range round2.Ask.Members {
		// A kind that reviewed round 1 starts again as <name>-2; one that is new to the PR (the default slots add
		// OpenCode) has no earlier name to number past.
		if m.How != "fresh" || ((m.Kind == "claude" || m.Kind == "agy") && !strings.HasSuffix(m.Name, "-2")) {
			t.Errorf("member %v: the same head again is a round of fresh reviewers", m)
		}
	}
}

func TestAnAnswersFileThatCantBeWrittenDoesNotFailTheReviewersReport(t *testing.T) {
	w := startReviews(t)
	w.pr.push(t, "the feature")
	res := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "claude"})
	// reports/ is a file: nothing can be written under it.
	dir := filepath.Dir(res.Ask.AnswersPath)
	os.RemoveAll(dir)
	if err := os.WriteFile(dir, []byte("not a folder"), 0o600); err != nil {
		t.Fatal(err)
	}
	w.say(t, res.Agents[0].Id, v1.MessageType_MESSAGE_TYPE_DONE, "the finding")
	waitFor(t, "the round's message", func() bool { return len(w.answers(t)) == 1 })
	body := w.answers(t)[0].Body
	if !strings.Contains(body, "1 of 1 reviewers answered") || !strings.Contains(body, "the answers file could not be written") || strings.Contains(body, "answers: /") {
		t.Errorf("message %s", body)
	}
	list, _ := w.asRoot.ListAsks(context.Background(), connect.NewRequest(&v1.ListAsksRequest{}))
	if len(list.Msg.Asks) != 1 || list.Msg.Asks[0].Members[0].Text != "the finding" {
		t.Errorf("the answer is not in the ask: %v", list.Msg.Asks)
	}
}
