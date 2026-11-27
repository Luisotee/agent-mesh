package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
	"github.com/Luisotee/agent-mesh/daemon/internal/testutil/shortdir"
)

// prWorld is a seeded run (a root and a worker with no processes) on a repo with a GitHub remote, and a fake `gh`
// whose answers and call count a test sets.
type prWorld struct {
	*harness
	run              *v1.Run
	rootTok, workTok string
	repo, branchTip  string
	base             string

	mu      sync.Mutex
	list    []map[string]any            // `gh pr list --head`
	byHead  map[string][]map[string]any // the answer for a `--head <branch>` when the test names that branch
	heads   []string                    // the branches `gh pr list` was asked about, in order
	raw     []byte                      // when set, the answer to any list call (gh pr list --state open)
	views   map[string]map[string]any
	inline  []map[string]any // `gh api repos/…/pulls/N/comments`
	fail    error
	gate    chan struct{} // while set, a gh call waits for it to close
	calls   atomic.Int32
	clockMu sync.Mutex
	skew    time.Duration
}

func startPR(t *testing.T) *prWorld {
	t.Helper()
	w := &prWorld{views: map[string]map[string]any{}}
	state := shortdir.State(t)
	d, err := daemon.New(daemon.Config{StateDir: state, TCP: "127.0.0.1:0", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { w.clockMu.Lock(); defer w.clockMu.Unlock(); return time.Now().Add(w.skew) },
		Agents: daemon.AgentConfig{Usage: &daemon.UsageOps{DisableClaudeNetwork: true,
			RunGH: func(ctx context.Context, _ string, args ...string) ([]byte, error) {
				w.calls.Add(1)
				w.mu.Lock()
				gate := w.gate
				w.mu.Unlock()
				if gate != nil { // a slow GitHub: held until the gate opens or the call is cancelled
					select {
					case <-gate:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				w.mu.Lock()
				defer w.mu.Unlock()
				if w.fail != nil {
					return nil, w.fail
				}
				if len(args) > 1 && args[0] == "api" {
					return json.Marshal(w.inline)
				}
				if len(args) > 1 && args[1] == "view" {
					v, ok := w.views[args[2]]
					if !ok {
						return nil, errors.New("not found")
					}
					return json.Marshal(v)
				}
				if w.raw != nil {
					return w.raw, nil
				}
				for i, a := range args {
					if a == "--head" && i+1 < len(args) {
						w.heads = append(w.heads, args[i+1])
						if l, ok := w.byHead[args[i+1]]; ok {
							return json.Marshal(l)
						}
						if w.byHead != nil {
							return json.Marshal([]any{})
						}
					}
				}
				return json.Marshal(w.list)
			}}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, d: d, state: state, cancel: cancel, done: make(chan error, 1)}
	go func() { h.done <- d.Serve(ctx) }()
	waitFor(t, "meshd.json", func() bool { _, err := daemon.ReadInfo(state); return err == nil })
	tok, err := client.Token(state)
	if err != nil {
		t.Fatal(err)
	}
	h.device, _ = client.Dial(state, tok, client.Options{})
	t.Cleanup(h.stop)
	w.harness = h
	w.run = startRun(t, h.device.Mesh, "")
	w.rootTok = h.seedAgent(w.run.Id, "a-root", "repo-lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
	w.workTok = h.seedAgent(w.run.Id, "a-w1", "demo-fix", "a-root", v1.AgentRole_AGENT_ROLE_WORKER)
	repo, _ := githubBackedRepo(t)
	w.repo = repo
	w.base = runGit(t, repo, "rev-parse", "HEAD")
	runGit(t, repo, "checkout", "-b", "mesh/"+w.run.Id+"/fix")
	runGit(t, repo, "commit", "--allow-empty", "-m", "work")
	w.branchTip = runGit(t, repo, "rev-parse", "HEAD")
	w.edit("a-w1", func(a *v1.Agent) {
		a.Repo, a.Worktree, a.Branch, a.BaseCommit = repo, repo, "mesh/"+w.run.Id+"/fix", w.base
	})
	return w
}

func (w *prWorld) edit(id string, f func(*v1.Agent)) {
	w.t.Helper()
	err := w.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		a, err := tx.GetAgent(id)
		if err != nil {
			return err
		}
		f(a)
		return tx.PutAgent(a)
	})
	if err != nil {
		w.t.Fatal(err)
	}
}

func (w *prWorld) setRaw(b []byte) { w.mu.Lock(); w.raw = b; w.mu.Unlock() }

func (w *prWorld) setList(prs ...map[string]any) { w.mu.Lock(); w.list = prs; w.mu.Unlock() }

func (w *prWorld) prs(agent string, refresh bool) []*v1.AgentPullRequests {
	w.t.Helper()
	res, err := w.device.Mesh.ListPullRequests(context.Background(), connect.NewRequest(&v1.ListPullRequestsRequest{
		RunId: w.run.Id, Agent: agent, Refresh: refresh}))
	if err != nil {
		w.t.Fatal(err)
	}
	return res.Msg.Agents
}

func (w *prWorld) advance(d time.Duration) { w.clockMu.Lock(); w.skew += d; w.clockMu.Unlock() }

func (w *prWorld) changes() []*v1.PullRequestChanged {
	w.t.Helper()
	evs, err := w.d.Store().Read(context.Background()).ListEvents(w.run.Id)
	if err != nil {
		w.t.Fatal(err)
	}
	var out []*v1.PullRequestChanged
	for _, e := range evs {
		if c := e.GetPullRequestChanged(); c != nil {
			out = append(out, c)
		}
	}
	return out
}

func ghPR(number int, state, oid string, draft bool, rollup ...map[string]any) map[string]any {
	return map[string]any{"number": number, "url": "https://github.com/acme/demo/pull/" + itoa(number), "state": state,
		"isDraft": draft, "headRefOid": oid, "headRefName": "feature-" + itoa(number), "statusCheckRollup": rollup}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func run(status, conclusion string) map[string]any {
	return map[string]any{"__typename": "CheckRun", "status": status, "conclusion": conclusion}
}

func TestAWorkersBranchAndReportedPRsAreListedWithStateAndChecks(t *testing.T) {
	w := startPR(t)
	// PR 12 is the branch's (failing check, draft); PR 3 was opened from the same branch name before the base
	// moved, so its head is on the base: not this worker's.
	w.setList(
		ghPR(12, "OPEN", w.branchTip, true, run("COMPLETED", "SUCCESS"), run("COMPLETED", "FAILURE")),
		ghPR(3, "MERGED", w.base, false))
	w.views["https://github.com/other/lib/pull/9"] = map[string]any{"number": 9, "url": "https://github.com/other/lib/pull/9",
		"state": "MERGED", "isDraft": false, "headRefOid": "deadbeef", "headRefName": "their-branch", "statusCheckRollup": []any{}}
	// Its done names a PR in another repo; a later one that names none doesn't forget it.
	asWorker := w.as(w.workTok)
	if _, err := w.as(w.rootTok).SendMessage(context.Background(), connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""),
		To: "demo-fix", Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "go"})); err != nil {
		t.Fatal(err)
	}
	readInbox(t, asWorker)
	for _, body := range []string{"done: https://github.com/other/lib/pull/9 and https://github.com/acme/demo/pull/12", "fixed the review findings"} {
		if _, err := asWorker.SendMessage(context.Background(), connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""),
			To: "parent", Type: v1.MessageType_MESSAGE_TYPE_DONE, Body: body})); err != nil {
			t.Fatal(err)
		}
	}
	rows := w.prs("", false)
	if len(rows) != 1 || rows[0].AgentName != "demo-fix" {
		t.Fatalf("rows %v (a root has none)", rows)
	}
	got := map[int32]*v1.PullRequest{}
	for _, p := range rows[0].PullRequests {
		got[p.Number] = p
	}
	if len(got) != 2 || got[3] != nil {
		t.Fatalf("PRs %v, want 12 (branch) and 9 (report) only", rows[0].PullRequests)
	}
	if p := got[12]; p.State != "open" || !p.Draft || p.Checks != "failing" || p.Source != "branch" || p.Stale {
		t.Errorf("PR 12: %v", p)
	}
	if p := got[9]; p.State != "merged" || p.Source != "report" || p.Checks != "" {
		t.Errorf("PR 9: %v", p)
	}
	// The head branch GitHub gave is kept, for a branch lookup and for a report's link.
	if got[12].HeadRef != "feature-12" || got[9].HeadRef != "their-branch" {
		t.Errorf("head refs: %q, %q", got[12].HeadRef, got[9].HeadRef)
	}
	if ch := w.changes(); len(ch) != 2 || ch[0].FromState != "" {
		t.Errorf("events %v, want one first-seen per PR", ch)
	}
}

func TestReadsAreCachedRefreshedOnRequestAndAFailedReadKeepsTheLastState(t *testing.T) {
	w := startPR(t)
	w.setList(ghPR(12, "OPEN", w.branchTip, false, run("IN_PROGRESS", "")))
	if rows := w.prs("", false); rows[0].PullRequests[0].Checks != "pending" {
		t.Fatalf("%v", rows)
	}
	n := w.calls.Load()
	w.setList(ghPR(12, "OPEN", w.branchTip, false, run("COMPLETED", "SUCCESS")))
	if rows := w.prs("", false); rows[0].PullRequests[0].Checks != "pending" || w.calls.Load() != n {
		t.Fatalf("a second read inside the TTL asked GitHub again (%d calls) or changed: %v", w.calls.Load()-n, rows)
	}
	w.advance(3 * time.Minute)
	rows := w.prs("", false)
	if p := rows[0].PullRequests[0]; p.Checks != "pass" || p.Stale {
		t.Fatalf("after the TTL: %v", p)
	}
	w.setList(ghPR(12, "MERGED", w.branchTip, false, run("COMPLETED", "SUCCESS")))
	rows = w.prs("", true) // refresh: ignores the TTL
	if p := rows[0].PullRequests[0]; p.State != "merged" {
		t.Fatalf("refresh: %v", p)
	}
	ch := w.changes()
	if len(ch) != 3 || ch[1].FromChecks != "pending" || ch[1].PullRequest.Checks != "pass" || ch[2].FromState != "open" || ch[2].PullRequest.State != "merged" {
		t.Errorf("events %v: want first seen, checks pending→pass, open→merged", ch)
	}
	// GitHub unreachable: the last state is kept, marked stale, and the failed read is retried after a minute.
	w.mu.Lock()
	w.fail = errors.New("no network")
	w.mu.Unlock()
	rows = w.prs("", true)
	if p := rows[0].PullRequests[0]; p.State != "merged" || !p.Stale || !rows[0].ReadFailed {
		t.Errorf("failed read: %v failed=%v", p, rows[0].ReadFailed)
	}
	n = w.calls.Load()
	w.prs("", false)
	if w.calls.Load() != n {
		t.Error("a failed read was retried at once")
	}
	w.advance(90 * time.Second)
	w.mu.Lock()
	w.fail = nil
	w.mu.Unlock()
	if rows = w.prs("", false); rows[0].PullRequests[0].Stale || rows[0].ReadFailed {
		t.Errorf("not recovered after the retry window: %v", rows[0])
	}
	if len(w.changes()) != 3 {
		t.Errorf("a stale mark or a recovery is no change: %d events", len(w.changes()))
	}
}

func TestOnlyChangeWorkersOwnPRs(t *testing.T) {
	w := startPR(t)
	asker := w.seedAgent(w.run.Id, "a-ask", "demo-ask", "a-root", v1.AgentRole_AGENT_ROLE_ASKER)
	_ = asker
	w.edit("a-ask", func(a *v1.Agent) { a.Repo, a.Branch = w.repo, "mesh/"+w.run.Id+"/fix" })
	w.setList(ghPR(12, "OPEN", w.branchTip, false))
	rows := w.prs("", false)
	if len(rows) != 1 || rows[0].AgentName != "demo-fix" {
		t.Errorf("rows %v: only the worker owns PRs", rows)
	}
}

func TestAWorkerWithNoPRShowsItsWorkEvidence(t *testing.T) {
	w := startPR(t)
	w.setList() // no PR
	rows := w.prs("", false)
	ev := rows[0].Evidence
	if len(rows[0].PullRequests) != 0 || ev == nil || !ev.HasCommits || ev.Commits != 1 || !ev.HasPushed || ev.Pushed || !ev.HasDirty || ev.Dirty {
		t.Errorf("evidence %v (one commit, not pushed, clean)", ev)
	}
}

func TestChecksFoldToOneWord(t *testing.T) {
	timed := func(conclusion, start, end string) map[string]any {
		return map[string]any{"__typename": "CheckRun", "status": "COMPLETED", "conclusion": conclusion,
			"startedAt": "2026-11-27T" + start, "completedAt": "2026-11-27T" + end}
	}
	status := func(state string) map[string]any {
		return map[string]any{"__typename": "StatusContext", "state": state}
	}
	for _, c := range []struct {
		name   string
		rollup []map[string]any
		want   string
	}{
		{"none", nil, ""},
		{"all pass", []map[string]any{run("COMPLETED", "SUCCESS"), run("COMPLETED", "SKIPPED"), status("SUCCESS")}, "pass"},
		{"one pending", []map[string]any{run("COMPLETED", "SUCCESS"), run("IN_PROGRESS", "")}, "pending"},
		{"a pending status", []map[string]any{status("PENDING")}, "pending"},
		{"failing beats pending", []map[string]any{run("QUEUED", ""), run("COMPLETED", "TIMED_OUT")}, "failing"},
		{"a failed status", []map[string]any{status("ERROR")}, "failing"},
		{"cancelled fails", []map[string]any{run("COMPLETED", "CANCELLED")}, "failing"},
		// GitHub refusing to start Actions (billing) fails a job in 2 s with nothing run: no result, not a failing PR.
		{"a failure that ran nothing", []map[string]any{timed("FAILURE", "09:17:19Z", "09:17:21Z")}, ""},
		{"a failure that ran nothing beside a pass", []map[string]any{timed("FAILURE", "09:17:19Z", "09:17:21Z"), run("COMPLETED", "SUCCESS")}, "pass"},
		{"a failure that ran nothing beside a real one", []map[string]any{timed("FAILURE", "09:17:19Z", "09:17:21Z"), timed("FAILURE", "09:17:19Z", "09:19:21Z")}, "failing"},
		{"a failure after minutes", []map[string]any{timed("FAILURE", "09:17:19Z", "09:19:21Z")}, "failing"},
		{"a startup failure is news however fast", []map[string]any{timed("STARTUP_FAILURE", "09:17:19Z", "09:17:21Z")}, "failing"},
		{"a failure with no times", []map[string]any{run("COMPLETED", "FAILURE")}, "failing"},
	} {
		if got := daemon.ChecksWord(c.rollup); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestTheBriefListsEachWorkersPRWithItsStateAndChecks(t *testing.T) {
	w := startPR(t)
	asWorker, asRoot := w.as(w.workTok), w.as(w.rootTok)
	send := func(c interface {
		SendMessage(context.Context, *connect.Request[v1.SendMessageRequest]) (*connect.Response[v1.SendMessageResponse], error)
	}, to string, typ v1.MessageType, body string) {
		t.Helper()
		if _, err := c.SendMessage(context.Background(), connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: to, Type: typ, Body: body})); err != nil {
			t.Fatal(err)
		}
	}
	send(asRoot, "demo-fix", v1.MessageType_MESSAGE_TYPE_TASK, "go")
	readInbox(t, asWorker)
	brief := func() string {
		res, err := asRoot.GetBrief(context.Background(), connect.NewRequest(&v1.GetBriefRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.Text
	}
	// Reported done, no PR: its commits and that they are not pushed.
	w.setList()
	send(asWorker, "parent", v1.MessageType_MESSAGE_TYPE_DONE, "done, see the branch")
	if text := brief(); !strings.Contains(text, "demo-fix") || !strings.Contains(text, "1 commit(s), not pushed, no PR") {
		t.Errorf("a done with no PR:\n%s", text)
	}
	// With a PR: its state and checks; a pending reviewer round on it shows.
	w.setList(ghPR(12, "MERGED", w.branchTip, false, run("COMPLETED", "FAILURE")))
	w.advance(3 * time.Minute)
	if text := brief(); !strings.Contains(text, "PR #12 merged, checks failing") || strings.Contains(text, "no PR") {
		t.Errorf("a done with a PR:\n%s", text)
	}
	// GitHub down: the last state, said to be old; with no state ever, `unknown` and never "no PR".
	w.mu.Lock()
	w.fail = errors.New("offline")
	w.mu.Unlock()
	w.advance(3 * time.Minute)
	if text := brief(); !strings.Contains(text, "PR #12 merged, checks failing (GitHub not read: last state seen)") {
		t.Errorf("GitHub down:\n%s", text)
	}
}

func TestAPRStillInReviewNamesItsPendingReviewers(t *testing.T) {
	w := startPR(t)
	w.setList(ghPR(12, "OPEN", w.branchTip, false))
	err := w.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		return tx.PutAsk(&v1.Ask{Id: "k-1", RunId: w.run.Id, Kind: v1.AskKind_ASK_KIND_REVIEW, Status: v1.AskStatus_ASK_STATUS_OPEN,
			Pr: "https://github.com/acme/demo/pull/12", Members: []*v1.AskMember{
				{AgentId: "r1", Name: "demo-rev-a", Status: v1.AskMemberStatus_ASK_MEMBER_STATUS_PENDING},
				{AgentId: "r2", Name: "demo-rev-b", Status: v1.AskMemberStatus_ASK_MEMBER_STATUS_ANSWERED}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := w.prs("", false)
	if got := strings.Join(rows[0].Reviewing, ","); got != "demo-rev-a" {
		t.Errorf("reviewing %q, want only the reviewer that still owes its answer", got)
	}
}

// A clean read is the whole truth about an agent's PRs: one that is no longer returned is forgotten, while a
// failed read forgets nothing.
func TestAPRTheBranchNoLongerHasIsForgottenAfterACleanRead(t *testing.T) {
	w := startPR(t)
	w.setList(ghPR(12, "OPEN", w.branchTip, false), ghPR(13, "OPEN", w.branchTip, false))
	if rows := w.prs("", false); len(rows[0].PullRequests) != 2 {
		t.Fatalf("%v", rows)
	}
	w.setList(ghPR(13, "OPEN", w.branchTip, false))
	rows := w.prs("", true)
	if len(rows[0].PullRequests) != 1 || rows[0].PullRequests[0].Number != 13 {
		t.Errorf("after a clean read: %v, want only #13", rows[0].PullRequests)
	}
}

func TestEvidenceNeverCallsAnUnreadPRNoPR(t *testing.T) {
	ev := &v1.WorkEvidence{Commits: 3, HasCommits: true, Pushed: true, HasPushed: true}
	if got := daemon.EvidenceText(ev, nil, false); !strings.Contains(got, "PRs not read") || strings.Contains(got, "no PR") {
		t.Errorf("PRs never read: %q", got)
	}
	if got := daemon.EvidenceText(ev, nil, true); !strings.Contains(got, "no PR") {
		t.Errorf("PRs read, none: %q", got)
	}
	prs := []*v1.PullRequest{{Number: 10, State: "open"}, {Number: 9, State: "open", Stale: true}}
	if got := daemon.EvidenceText(ev, prs, true); !strings.Contains(got, "PR #9 open (as last read), PR #10 open") {
		t.Errorf("order or stale mark: %q", got)
	}
}

// The agent of `mesh prs --run-id R --agent X` is one of run R's: a name that two open runs share is not
// ambiguous, and an id of another run is not found.
func TestListPullRequestsFindsTheAgentInTheNamedRunOnly(t *testing.T) {
	w := startPR(t)
	other := startRun(t, w.device.Mesh, "")
	w.seedAgent(other.Id, "b-root", "repo-lead", "", v1.AgentRole_AGENT_ROLE_ROOT)
	w.seedAgent(other.Id, "b-w1", "demo-fix", "b-root", v1.AgentRole_AGENT_ROLE_WORKER)
	w.setList(ghPR(12, "OPEN", w.branchTip, false))
	rows := w.prs("demo-fix", false)
	if len(rows) != 1 || rows[0].AgentId != "a-w1" {
		t.Errorf("by a name both runs have: %v", rows)
	}
	if rows := w.prs("a-w1", false); len(rows) != 1 || rows[0].AgentId != "a-w1" {
		t.Errorf("by id: %v", rows)
	}
	_, err := w.device.Mesh.ListPullRequests(context.Background(), connect.NewRequest(&v1.ListPullRequestsRequest{
		RunId: w.run.Id, Agent: "b-w1"}))
	if code(err) != connect.CodeNotFound {
		t.Errorf("an agent of another run: %v", err)
	}
}

// GitHub slower than the brief waits: the brief still shows the worker's commits from local git, and a cached
// PR says it is old.
func TestASlowGitHubKeepsTheBriefsLocalEvidenceAndMarksCachedPRsOld(t *testing.T) {
	defer daemon.SetBriefGitHubWait(300 * time.Millisecond)()
	w := startPR(t)
	asWorker, asRoot := w.as(w.workTok), w.as(w.rootTok)
	for _, m := range []struct {
		c interface {
			SendMessage(context.Context, *connect.Request[v1.SendMessageRequest]) (*connect.Response[v1.SendMessageResponse], error)
		}
		to   string
		typ  v1.MessageType
		body string
	}{{asRoot, "demo-fix", v1.MessageType_MESSAGE_TYPE_TASK, "go"}, {asWorker, "parent", v1.MessageType_MESSAGE_TYPE_DONE, "done"}} {
		if _, err := m.c.SendMessage(context.Background(), connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: m.to, Type: m.typ, Body: m.body})); err != nil {
			t.Fatal(err)
		}
		if m.to == "demo-fix" {
			readInbox(t, asWorker)
		}
	}
	brief := func() string {
		res, err := asRoot.GetBrief(context.Background(), connect.NewRequest(&v1.GetBriefRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.Text
	}
	gate := make(chan struct{}) // never opened
	w.mu.Lock()
	w.gate = gate
	w.mu.Unlock()
	if text := brief(); !strings.Contains(text, "1 commit(s), not pushed") || !strings.Contains(text, "PR unknown (GitHub not read)") {
		t.Errorf("no PR ever read, GitHub too slow:\n%s", text)
	}
	w.mu.Lock()
	w.gate = nil
	w.mu.Unlock()
	w.setList(ghPR(12, "OPEN", w.branchTip, false))
	w.advance(3 * time.Minute)
	if text := brief(); !strings.Contains(text, "PR #12 open") || strings.Contains(text, "not read") {
		t.Fatalf("a read that works:\n%s", text)
	}
	w.mu.Lock()
	w.gate = gate
	w.mu.Unlock()
	w.advance(3 * time.Minute)
	if text := brief(); !strings.Contains(text, "PR #12 open (GitHub not read: last state seen)") {
		t.Errorf("GitHub too slow, a PR cached:\n%s", text)
	}
	// The row itself says it is old (what alerts and apps read), not only the brief's text.
	if rows := w.prs("", false); len(rows[0].PullRequests) != 1 || !rows[0].PullRequests[0].Stale || !rows[0].ReadFailed {
		t.Errorf("after the timeout: %v, want the PR marked stale", rows)
	}
	if got := daemon.EvidenceText(&v1.WorkEvidence{Commits: 1, HasCommits: true}, []*v1.PullRequest{{Number: 12, State: "open"}}, false); !strings.Contains(got, "PR #12 open (as last read)") {
		t.Errorf("evidence with PRs unread: %q", got)
	}
}

// A PR whose head is on the checkout's HEAD but not on the worker's branch is not the worker's once its worktree
// is gone and gh runs in the main checkout.
func TestAPRIsNotMatchedToAWorkerByTheMainCheckoutsHead(t *testing.T) {
	w := startPR(t)
	runGit(t, w.repo, "checkout", "--detach", w.base)
	runGit(t, w.repo, "commit", "--allow-empty", "-m", "someone else's work")
	elsewhere := runGit(t, w.repo, "rev-parse", "HEAD")
	w.edit("a-w1", func(a *v1.Agent) { a.Worktree = filepath.Join(w.repo, "gone") })
	w.setList(ghPR(12, "OPEN", w.branchTip, false), ghPR(8, "MERGED", elsewhere, false))
	rows := w.prs("", false)
	if len(rows) != 1 || len(rows[0].PullRequests) != 1 || rows[0].PullRequests[0].Number != 12 {
		t.Errorf("PRs %v, want only #12 (the branch's)", rows)
	}
}

// While the first read of GitHub is under way, a second reader hears "not read", never "no PR".
func TestAReadUnderWayIsNotNoPR(t *testing.T) {
	w := startPR(t)
	gate := make(chan struct{})
	w.mu.Lock()
	w.gate = gate
	w.mu.Unlock()
	first := make(chan []*v1.AgentPullRequests, 1)
	go func() { first <- w.prs("", false) }()
	waitFor(t, "the first gh call", func() bool { return w.calls.Load() > 0 })
	w.setList(ghPR(12, "OPEN", w.branchTip, false))
	rows := w.prs("", false)
	if len(rows) != 1 || len(rows[0].PullRequests) != 0 || !rows[0].ReadFailed {
		t.Errorf("while the read is under way: %v, want no PRs and read_failed (not read)", rows)
	}
	if calls := w.calls.Load(); calls > 2 { // the branch call, and at most one more per claim
		t.Errorf("%d gh calls: a second reader started its own round", calls)
	}
	close(gate)
	<-first
	if rows := w.prs("", false); len(rows[0].PullRequests) != 1 || rows[0].ReadFailed {
		t.Errorf("after it finished: %v", rows)
	}
}

// A newer task to the worker ends the reach of its earlier report: the PR it named then is not this task's.
func TestAReportBeforeANewTaskNamesNoPRsOfThisOne(t *testing.T) {
	w := startPR(t)
	asWorker, asRoot := w.as(w.workTok), w.as(w.rootTok)
	w.views["https://github.com/other/lib/pull/9"] = map[string]any{"number": 9, "url": "https://github.com/other/lib/pull/9",
		"state": "MERGED", "isDraft": false, "headRefOid": "deadbeef", "statusCheckRollup": []any{}}
	send := func(c interface {
		SendMessage(context.Context, *connect.Request[v1.SendMessageRequest]) (*connect.Response[v1.SendMessageResponse], error)
	}, to string, typ v1.MessageType, body string) {
		t.Helper()
		if _, err := c.SendMessage(context.Background(), connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: to, Type: typ, Body: body})); err != nil {
			t.Fatal(err)
		}
	}
	send(asRoot, "demo-fix", v1.MessageType_MESSAGE_TYPE_TASK, "one")
	readInbox(t, asWorker)
	send(asWorker, "parent", v1.MessageType_MESSAGE_TYPE_DONE, "done: https://github.com/other/lib/pull/9")
	if rows := w.prs("", true); len(rows[0].PullRequests) != 1 {
		t.Fatalf("the report's PR: %v", rows)
	}
	send(asRoot, "demo-fix", v1.MessageType_MESSAGE_TYPE_TASK, "two")
	if rows := w.prs("", true); len(rows[0].PullRequests) != 0 {
		t.Errorf("after a new task: %v, want none", rows[0].PullRequests)
	}
}

func TestAnUnknownPRIsNotAStateAndEvidenceCallsItUnread(t *testing.T) {
	ev := &v1.WorkEvidence{Commits: 2, HasCommits: true, Pushed: true, HasPushed: true}
	prs := []*v1.PullRequest{{Number: 9, State: "unknown", Stale: true}}
	if got := daemon.EvidenceText(ev, prs, false); !strings.Contains(got, "PRs not read") {
		t.Errorf("only an unknown row: %q", got)
	}
	if got := daemon.PRText(&v1.AgentPullRequests{PullRequests: prs}, true); got != "PR #9 unknown (GitHub not read)" {
		t.Errorf("unknown row: %q", got)
	}
}

func TestMeshLogShowsAPRChange(t *testing.T) {
	w := startPR(t)
	w.setList(ghPR(12, "OPEN", w.branchTip, false, run("IN_PROGRESS", "")))
	w.prs("", false)
	w.setList(ghPR(12, "MERGED", w.branchTip, false, run("COMPLETED", "FAILURE")))
	w.prs("", true)
	w.setList(ghPR(12, "MERGED", w.branchTip, false))
	w.prs("", true)
	stream, err := w.device.Mesh.StreamLog(context.Background(), connect.NewRequest(&v1.StreamLogRequest{RunId: w.run.Id}))
	if err != nil {
		t.Fatal(err)
	}
	var details []string
	for stream.Receive() {
		if e := stream.Msg().Entry; e.Kind == "pr" {
			details = append(details, e.Who+" "+e.Detail)
		}
	}
	if len(details) != 3 || !strings.Contains(details[0], "#12 open, checks pending") ||
		!strings.Contains(details[1], "#12 open → merged, checks pending → failing") ||
		!strings.Contains(details[2], "#12 merged, checks failing → none") {
		t.Errorf("log entries: %q", details)
	}
}

func TestAWorkersStackOfBranchesHasItsPRsFoundNotOnlyTheRecordedBranchs(t *testing.T) {
	w := startPR(t)
	stem := "mesh/" + w.run.Id + "/fix"
	// The worker moved on to -2 and -3 itself; the daemon recorded the first. Another agent of the run is `fix-4`: a
	// branch that is not the worker's, though its name reads like the next one.
	tips := map[string]string{stem: w.branchTip}
	for _, b := range []string{stem + "-2", stem + "-3"} {
		runGit(t, w.repo, "checkout", "-b", b)
		runGit(t, w.repo, "commit", "--allow-empty", "-m", b)
		tips[b] = runGit(t, w.repo, "rev-parse", "HEAD")
	}
	runGit(t, w.repo, "branch", stem+"-4", w.base)
	w.harness.seedAgent(w.run.Id, "a-w2", "demo-fix-4", "a-root", v1.AgentRole_AGENT_ROLE_WORKER)
	w.edit("a-w2", func(a *v1.Agent) { a.Branch = stem + "-4" })
	w.mu.Lock()
	w.byHead = map[string][]map[string]any{
		stem:        {ghPR(1, "MERGED", tips[stem], false)},
		stem + "-2": {ghPR(2, "MERGED", tips[stem+"-2"], false)},
		stem + "-3": {ghPR(3, "OPEN", tips[stem+"-3"], false)},
		stem + "-4": {ghPR(4, "OPEN", w.base, false)},
	}
	w.mu.Unlock()
	rows := w.prs("demo-fix", true)
	if len(rows) != 1 {
		t.Fatalf("rows %v", rows)
	}
	var numbers []int32
	for _, p := range rows[0].PullRequests {
		numbers = append(numbers, p.Number)
	}
	if len(numbers) != 3 {
		t.Errorf("PRs %v, want 1, 2 and 3 (the whole stack)", numbers)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, h := range w.heads {
		if h == stem+"-4" {
			t.Errorf("asked GitHub about %s, another agent's branch: %v", h, w.heads)
		}
	}
}

func TestAStackOfBranchesAsksAboutTheNewestEightAtMost(t *testing.T) {
	w := startPR(t)
	stem := "mesh/" + w.run.Id + "/fix"
	for i := 2; i <= 11; i++ {
		runGit(t, w.repo, "checkout", "-b", stem+"-"+itoa(i))
		runGit(t, w.repo, "commit", "--allow-empty", "-m", itoa(i))
	}
	w.mu.Lock()
	w.byHead = map[string][]map[string]any{}
	w.mu.Unlock()
	w.prs("demo-fix", true)
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.heads) != 8 {
		t.Fatalf("asked about %d branches %v, want the newest 8", len(w.heads), w.heads)
	}
	for _, h := range w.heads {
		if h == stem || h == stem+"-2" {
			t.Errorf("an old branch %s was asked about: %v", h, w.heads)
		}
	}
}

// A failed check run that ran no job is not named among the failing checks a worker is told (as checksWord).
func TestFailingChecksLeaveOutARunThatRanNoJob(t *testing.T) {
	timed := func(name, start, end string) map[string]any {
		return map[string]any{"__typename": "CheckRun", "name": name, "status": "COMPLETED", "conclusion": "FAILURE",
			"startedAt": "2026-11-27T" + start, "completedAt": "2026-11-27T" + end}
	}
	got := daemon.FailingChecks([]map[string]any{timed("billing", "09:17:19Z", "09:17:21Z"), timed("tests", "09:17:19Z", "09:19:21Z")})
	if len(got) != 1 || !strings.HasPrefix(got[0], "tests failure") {
		t.Errorf("%v", got)
	}
}
