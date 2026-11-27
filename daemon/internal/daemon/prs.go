package daemon

// Pull requests per agent (bin/mesh's pr_states, report_prs, waiting_rows and work_evidence; design §13). A change
// worker owns the PRs of its branch (`gh pr list --head`) and the ones its `done` names. Their last state is kept in
// SQLite (`agent_prs`), read again from GitHub at most every prTTL (prRetryTTL after a failed read) and only when a
// reader asks (`mesh prs`, the brief, an alert's evidence) or the feedback watch looks (prfeedback.go, every
// `prs.watch_every`, only at workers with an open PR). A change of state or checks is an event of the run's log,
// never a message.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Luisotee/agent-mesh/daemon/internal/adapter"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

const (
	prTTL       = 120 * time.Second // how long a read of GitHub is reused
	prRetryTTL  = 60 * time.Second  // before a failed read is tried again
	prFetchers  = 8                 // GitHub reads at once
	prJSON      = "number,url,state,isDraft,headRefOid,headRefName,statusCheckRollup"
	prListLimit = "20"
)

// briefGitHubWait is how long a brief waits for GitHub before it shows the last state (a var: tests shorten it).
var briefGitHubWait = 8 * time.Second

var prURLRE = regexp.MustCompile(`https://github\.com/[\w.-]+/[\w.-]+/pull/\d+`)

// prTracker remembers, per agent, when its PRs were last read, whether that worked and whether a read is under
// way. A read under way changes none of the first two: until one has completed and been recorded, what is told
// is the last finished read, or "not read" for an agent never read.
type prTracker struct {
	mu    sync.Mutex
	tried map[string]prTry
	ghMu  sync.Mutex // serialises ghProblem's check
	ghAt  time.Time  // when the `gh` CLI was last checked (ghProblem), and what it said
	ghWhy string
}

type prTry struct {
	at       time.Time
	ok       bool
	inFlight bool
}

// begin claims a read of the agent's PRs; false when it is not due or another reader has it.
func (t *prTracker) begin(agentID string, now time.Time, force bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tried == nil {
		t.tried = map[string]prTry{}
	}
	p, seen := t.tried[agentID]
	if p.inFlight {
		return false
	}
	if seen && !force {
		ttl := prTTL
		if !p.ok {
			ttl = prRetryTTL
		}
		if now.Sub(p.at) < ttl {
			return false
		}
	}
	p.inFlight = true
	t.tried[agentID] = p
	return true
}

// lastRead is when the agent's PRs were last read from GitHub in full (zero: not since this daemon started, or the
// last read failed, so that it is tried again).
func (t *prTracker) lastRead(agentID string) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if p := t.tried[agentID]; p.ok {
		return p.at
	}
	return time.Time{}
}

// finish ends a read: ok when GitHub answered in full and what it said is recorded.
func (t *prTracker) finish(agentID string, now time.Time, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tried[agentID] = prTry{at: now, ok: ok}
}

// ghProblemTTL is how long a check of the `gh` CLI is kept: it is asked on every list, and `gh auth status` is a process.
const ghProblemTTL = time.Minute

// ghProblemWait is how long the check may take.
const ghProblemWait = 2 * time.Second

// ghProblem says why GitHub can't be read on this host: "no_gh" (the CLI is missing) or "not_logged_in" (`gh auth status`
// fails), and "" when it works or can't be told (a timeout, a test with no gh). The answer is kept for a minute.
func (d *Daemon) ghProblem(ctx context.Context) string {
	// One check at a time: readers that arrive while it runs wait for it and use its answer.
	d.prs.ghMu.Lock()
	defer d.prs.ghMu.Unlock()
	now := d.now()
	d.prs.mu.Lock()
	if !d.prs.ghAt.IsZero() && now.Sub(d.prs.ghAt) < ghProblemTTL {
		p := d.prs.ghWhy
		d.prs.mu.Unlock()
		return p
	}
	d.prs.mu.Unlock()
	why := ""
	// A slow GitHub must not hold a list: `gh auth status` asks the network, so it is cut short and then says nothing.
	cctx, cancel := context.WithTimeout(ctx, ghProblemWait)
	defer cancel()
	if _, err := d.runGH(cctx, "", "auth", "status"); err != nil {
		var exit *exec.ExitError
		switch {
		case cctx.Err() != nil:
			return "" // too slow to tell (a killed process is an exit error too): no claim, and not kept
		case errors.Is(err, exec.ErrNotFound):
			why = "no_gh"
		case errors.As(err, &exit) && notLoggedIn(string(exit.Stderr)):
			why = "not_logged_in"
		}
	}
	d.prs.mu.Lock()
	d.prs.ghAt, d.prs.ghWhy = now, why
	d.prs.mu.Unlock()
	return why
}

// notLoggedIn reads what `gh auth status` said on a failure: only its own "not logged in" words count. A failure for any
// other reason (offline, a DNS error) says nothing, so an offline user is not told to log in.
func notLoggedIn(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "not logged") || strings.Contains(s, "gh auth login")
}

// read reports whether the agent's PRs were read from GitHub and recorded (the last finished read worked).
func (t *prTracker) read(agentID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, seen := t.tried[agentID]
	return seen && p.ok
}

// ownsPRs is a change worker: one that can have a branch of its own to open a PR from.
func ownsPRs(a *v1.Agent) bool {
	return a.Role != v1.AgentRole_AGENT_ROLE_ROOT && !adapter.ReadOnly(a.Role) && !a.Peer && a.Repo != "" && a.Branch != ""
}

// ghPR is what `gh pr list` and `gh pr view` say of a PR.
type ghPRState struct {
	Number     int32            `json:"number"`
	URL        string           `json:"url"`
	State      string           `json:"state"`
	IsDraft    bool             `json:"isDraft"`
	HeadRefOid string           `json:"headRefOid"`
	HeadRef    string           `json:"headRefName"`
	Rollup     []map[string]any `json:"statusCheckRollup"`
}

// failedOutcomes are the conclusions and states of a check that count as failing.
var failedOutcomes = map[string]bool{"FAILURE": true, "ERROR": true, "TIMED_OUT": true, "CANCELLED": true, "ACTION_REQUIRED": true, "STARTUP_FAILURE": true}

// neverRanWithin: a check run that finished this soon after it started ran no job (GitHub refusing to start Actions
// for billing, 2 s from start to finish): a job that ran takes longer than its set-up. Its failure says nothing of the
// PR's code.
const neverRanWithin = 5 * time.Second

// neverRan tells whether a check run is a failure that ran nothing, by its start and finish times.
func neverRan(c map[string]any) bool {
	at := func(k string) (time.Time, bool) {
		s, _ := c[k].(string)
		t, err := time.Parse(time.RFC3339, s)
		return t, err == nil
	}
	if conclusion, _ := c["conclusion"].(string); !strings.EqualFold(conclusion, "FAILURE") {
		return false // a STARTUP_FAILURE (an invalid workflow) or a timeout is news whatever it took
	}
	start, ok1 := at("startedAt")
	end, ok2 := at("completedAt")
	return ok1 && ok2 && !end.Before(start) && end.Sub(start) < neverRanWithin
}

// checksWord folds `gh`'s statusCheckRollup to one word: failing, pending or pass ("" with no checks). A check run
// has a status and a conclusion; a commit status only a state. A failed run that ran no job (neverRan) is no check.
func checksWord(rollup []map[string]any) string {
	str := func(c map[string]any, k string) string { s, _ := c[k].(string); return strings.ToUpper(s) }
	seen := map[string]bool{}
	for _, c := range rollup {
		var outcome string
		var done bool
		if _, run := c["conclusion"]; run || c["status"] != nil {
			done, outcome = str(c, "status") == "COMPLETED", str(c, "conclusion")
		} else {
			outcome = str(c, "state")
			done = outcome != "PENDING" && outcome != "EXPECTED"
		}
		switch {
		case !done:
			seen["pending"] = true
		case failedOutcomes[outcome] && neverRan(c):
			// no job ran: not a result (all of them: no checks)
		case failedOutcomes[outcome]:
			seen["failing"] = true
		default:
			seen["pass"] = true
		}
	}
	for _, w := range []string{"failing", "pending", "pass"} {
		if seen[w] {
			return w
		}
	}
	return ""
}

func (g ghPRState) record(agentID, source string, at time.Time) *v1.PullRequest {
	return &v1.PullRequest{AgentId: agentID, Url: g.URL, Number: g.Number, State: strings.ToLower(g.State), Draft: g.IsDraft,
		Checks: checksWord(g.Rollup), Source: source, CheckedAt: timestamppb.New(at), HeadRef: g.HeadRef, Head: g.HeadRefOid}
}

// ghDir is where gh runs for an agent: its worktree while that exists, else its repo.
func ghDir(a *v1.Agent) string {
	if a.Worktree != "" {
		if st, err := os.Stat(a.Worktree); err == nil && st.IsDir() {
			return a.Worktree
		}
	}
	return a.Repo
}

// branchPRs reads the PRs from an agent's branches: all states, those whose head is on one of its stack's branches
// (`stackBranches`: the recorded one and the `-2`, `-3` … it moved on to). The branches are asked about at once, so one
// slow `gh` does not hold up the rest inside the brief's wait (`briefGitHubWait`); the answers are read in branch order.
func (d *Daemon) branchPRs(ctx context.Context, a *v1.Agent) ([]ghPRState, error) {
	remotes := githubRepos(ctx, a.Repo)
	if len(remotes) == 0 {
		return nil, nil // not a GitHub repo: no PRs to find
	}
	dir := ghDir(a)
	atHead := a.Worktree != "" && dir == a.Worktree // else dir is the main checkout, whose HEAD is not the branch
	branches := stackBranches(ctx, d.store.Read(ctx), a, dir)
	type answer struct {
		prs []ghPRState
		err error
	}
	answers := make([]answer, len(branches))
	var wg sync.WaitGroup
	sem := make(chan struct{}, stackFetchers)
	for i, branch := range branches {
		wg.Add(1)
		sem <- struct{}{}
		go func() { // joined below, within this call
			defer wg.Done()
			defer func() { <-sem }()
			remote := prRemote(ctx, a.Repo, branch, remotes)
			args := []string{"pr", "list", "--repo", remotes[remote], "--head", branch, "--state", "all", "--json", prJSON, "--limit", prListLimit}
			out, err := d.runGH(ctx, dir, args...)
			if err == nil {
				err = json.Unmarshal(out, &answers[i].prs)
			}
			answers[i].err = err
		}()
	}
	wg.Wait()
	var own []ghPRState
	seen := map[string]bool{} // by URL: two repos' PRs may share a number
	for i, branch := range branches {
		if answers[i].err != nil {
			return nil, answers[i].err
		}
		for _, p := range answers[i].prs {
			if key := prSeenKey(p); !seen[key] && prOnBranch(ctx, dir, p.HeadRefOid, branch, a.BaseCommit, atHead) {
				seen[key] = true
				own = append(own, p)
			}
		}
	}
	return own, nil
}

// prSeenKey is what makes two rows one PR: its URL, else its number.
func prSeenKey(p ghPRState) string {
	if p.URL != "" {
		return p.URL
	}
	return strconv.Itoa(int(p.Number))
}

func (d *Daemon) viewPR(ctx context.Context, a *v1.Agent, url string) (ghPRState, error) {
	out, err := d.runGH(ctx, ghDir(a), "pr", "view", url, "--json", prJSON)
	if err != nil {
		return ghPRState{}, err
	}
	var p ghPRState
	if err := json.Unmarshal(out, &p); err != nil || p.Number == 0 {
		return ghPRState{}, fmt.Errorf("gh pr view %s: unexpected answer", url)
	}
	if p.URL == "" {
		p.URL = url
	}
	return p, nil
}

// reportPRs are the PRs of an agent's newest `done` that names any since its last task: a fix round's report may
// not repeat the link, but a new task starts over.
func reportPRs(msgs []*v1.Message, a *v1.Agent) []string {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Type == v1.MessageType_MESSAGE_TYPE_TASK && m.ToId == a.Id {
			return nil // a newer task: a report before it was about the work before
		}
		if m.From.GetId() != a.Id || m.Type != v1.MessageType_MESSAGE_TYPE_DONE {
			continue
		}
		seen := map[string]bool{}
		var urls []string
		for _, u := range prURLRE.FindAllString(m.Body, -1) {
			if k := prKey(u); !seen[k] {
				seen[k] = true
				urls = append(urls, u)
			}
		}
		if len(urls) > 0 {
			return urls
		}
	}
	return nil
}

// refreshPRs reads an agent's PRs from GitHub (calls on ghCtx, which may be cut short; the database on ctx) when they
// are due (or `force`) and records what changed. It never fails the caller: a read that fails keeps the last state, marks it stale and is tried again after prRetryTTL.
func (d *Daemon) refreshPRs(ctx, ghCtx context.Context, a *v1.Agent, force bool) {
	if !ownsPRs(a) {
		return
	}
	now := d.nowTime()
	// A claim first, so two readers at once make one round of calls (the tracker is the throttle). It stays open
	// until what GitHub said is recorded: a reader meanwhile hears the last finished read, never "no PR" for one
	// that has not answered.
	if !d.prs.begin(a.Id, now, force) {
		return
	}
	complete := false
	defer func() { d.prs.finish(a.Id, now, complete) }()
	r := d.store.Read(ctx)
	msgs, err := r.RunMessages(a.RunId, "", -1)
	if err != nil {
		return
	}
	var found []*v1.PullRequest
	seen := map[string]bool{}
	var ferr error
	branch, err := d.branchPRs(ghCtx, a)
	if err != nil {
		ferr = err
	}
	for _, p := range branch {
		seen[prKey(p.URL)] = true
		found = append(found, p.record(a.Id, "branch", now))
	}
	for _, u := range reportPRs(msgs, a) {
		if seen[prKey(u)] {
			continue
		}
		p, err := d.viewPR(ghCtx, a, u)
		if err != nil {
			ferr = err
			continue
		}
		seen[prKey(p.URL)] = true
		found = append(found, p.record(a.Id, "report", now))
	}
	if ctx.Err() != nil {
		return
	}
	// GitHub cut short (the brief's wait) while the caller is alive: the rows already recorded are marked stale,
	// and no row is invented for a link that was not read.
	timedOut := ghCtx.Err() != nil
	if timedOut && ferr == nil {
		ferr = ghCtx.Err()
	}
	err = d.store.Write(ctx, meshActor, "", func(tx *store.Tx) error {
		old, err := tx.AgentPRs(a.Id)
		if err != nil {
			return err
		}
		prev := map[string]*v1.PullRequest{}
		for _, p := range old {
			prev[prKey(p.Url)] = p
		}
		for _, p := range found {
			o := prev[prKey(p.Url)]
			delete(prev, prKey(p.Url))
			if o != nil { // the watch's cursors are not GitHub's to say (prfeedback.go)
				p.FeedbackCursor, p.FailingHead, p.FeedbackResumes = o.FeedbackCursor, o.FailingHead, o.FeedbackResumes
			}
			if err := tx.PutAgentPR(p); err != nil { // also moves its read time on; the user's hold is set on it there
				return err
			}
			if o == nil || o.State != p.State || o.Checks != p.Checks {
				ev := &v1.PullRequestChanged{PullRequest: p}
				if o != nil {
					ev.FromState, ev.FromChecks = o.State, o.Checks
				}
				if _, err := tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id,
					Payload: &v1.Event_PullRequestChanged{PullRequestChanged: ev}}); err != nil {
					return err
				}
			}
		}
		// Rows the read didn't return: after a clean read they are no longer this agent's (a branch rewritten, a
		// report that names others now), so they go; after a failed read they keep their last state, marked stale.
		for _, o := range prev {
			if ferr == nil {
				if err := tx.DeleteAgentPR(a.Id, o.Url); err != nil {
					return err
				}
			} else if !o.Stale {
				o.Stale = true
				if err := tx.PutAgentPR(o); err != nil {
					return err
				}
			}
		}
		if ferr != nil && !timedOut { // a link in a report whose read failed: shown as unknown until it can be read
			for _, u := range reportPRs(msgs, a) {
				if _, known := prev[prKey(u)]; known || seen[prKey(u)] {
					continue
				}
				num, _ := strconv.Atoi(u[strings.LastIndex(u, "/")+1:])
				if err := tx.PutAgentPR(&v1.PullRequest{AgentId: a.Id, Url: u, Number: int32(num), State: "unknown",
					Source: "report", CheckedAt: timestamppb.New(now), Stale: true}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		if ctx.Err() == nil {
			d.log.Warn("pull requests", "agent", a.Name, "err", err)
		}
		return
	}
	complete = ferr == nil
}

// refreshAll refreshes every change worker's PRs, prFetchers at a time.
func (d *Daemon) refreshAll(ctx, ghCtx context.Context, agents []*v1.Agent, force bool) {
	sem := make(chan struct{}, prFetchers)
	var wg sync.WaitGroup
	for _, a := range agents {
		if !ownsPRs(a) {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			d.refreshPRs(ctx, ghCtx, a, force)
		}()
	}
	wg.Wait()
}

// workEvidence is what an agent's branch and worktree show of its work: commits past its base, whether the tip is
// on its upstream, whether the worktree has uncommitted changes. Each part is unset where it can't be told.
func workEvidence(ctx context.Context, a *v1.Agent) *v1.WorkEvidence {
	ev := &v1.WorkEvidence{}
	if a.Repo != "" && a.Branch != "" {
		if tip, err := gitRO(ctx, a.Repo, "rev-parse", "--verify", "refs/heads/"+a.Branch); err == nil {
			if a.BaseCommit != "" {
				if n, err := gitRO(ctx, a.Repo, "rev-list", "--count", a.BaseCommit+".."+tip); err == nil {
					if c, err := strconv.Atoi(n); err == nil {
						ev.Commits, ev.HasCommits = int32(c), true
					}
				}
			}
			up, err := gitRO(ctx, a.Repo, "rev-parse", "--verify", a.Branch+"@{upstream}")
			ev.Pushed, ev.HasPushed = err == nil && up == tip, true // no upstream: not pushed
		}
	}
	if a.Worktree != "" {
		if st, err := os.Stat(a.Worktree); err == nil && st.IsDir() {
			if out, err := gitRO(ctx, a.Worktree, "status", "--porcelain"); err == nil {
				ev.Dirty, ev.HasDirty = out != "", true
			}
		}
	}
	return ev
}

// evidenceText is the evidence as `its work: …` parts: "3 commit(s) past its base", "not pushed", "worktree dirty".
func evidenceText(ev *v1.WorkEvidence, prs []*v1.PullRequest, prsRead bool) string {
	var parts []string
	if ev.HasCommits {
		if ev.Commits == 0 {
			parts = append(parts, "no commits past its base")
		} else {
			parts = append(parts, fmt.Sprintf("%d commit(s) past its base", ev.Commits))
		}
	}
	if ev.HasCommits && ev.Commits > 0 && ev.HasPushed {
		if ev.Pushed {
			parts = append(parts, "pushed")
		} else {
			parts = append(parts, "not pushed")
		}
	}
	if ev.HasCommits && ev.Commits > 0 {
		prs = append([]*v1.PullRequest(nil), prs...)
		sort.SliceStable(prs, func(i, j int) bool { return prs[i].Number < prs[j].Number })
		var open []string
		known := 0 // PRs whose state was ever read: an "unknown" row names a link, no state
		for _, p := range prs {
			if p.State != "unknown" {
				known++
			}
			if p.State == "open" {
				t := "PR #" + strconv.Itoa(int(p.Number)) + " open"
				if p.Stale || !prsRead {
					t += " (as last read)"
				}
				open = append(open, t)
			}
		}
		switch {
		case len(open) > 0:
			parts = append(parts, strings.Join(open, ", "))
		case known == 0 && len(prs) == 0 && prsRead:
			parts = append(parts, "no PR")
		case known == 0:
			parts = append(parts, "PRs not read")
		}
	}
	if ev.HasDirty {
		if ev.Dirty {
			parts = append(parts, "worktree dirty")
		} else {
			parts = append(parts, "worktree clean")
		}
	}
	return strings.Join(parts, ", ")
}

// workText is `its work: …` for an alert about a change worker (a refresh first, so an open PR is told), or "".
func (d *Daemon) workText(ctx context.Context, a *v1.Agent) string {
	if !ownsPRs(a) {
		return ""
	}
	d.refreshPRs(ctx, ctx, a, false)
	prs, _ := d.store.Read(ctx).AgentPRs(a.Id)
	if t := evidenceText(workEvidence(ctx, a), prs, d.prs.read(a.Id)); t != "" {
		return "Its work: " + t + "."
	}
	return ""
}

// reviewingPRs maps a PR's key to the reviewers of this run that still owe their answer on it.
func reviewingPRs(r store.Reader, runID string) map[string][]string {
	out := map[string][]string{}
	asks, err := r.RunAsks(runID)
	if err != nil {
		return out
	}
	for _, ask := range asks {
		if ask.Kind != v1.AskKind_ASK_KIND_REVIEW || ask.Status != v1.AskStatus_ASK_STATUS_OPEN || ask.Pr == "" {
			continue
		}
		for _, m := range ask.Members {
			if m.Status == v1.AskMemberStatus_ASK_MEMBER_STATUS_PENDING {
				k := prKey(ask.Pr)
				out[k] = append(out[k], m.Name)
			}
		}
	}
	return out
}

// pullRequests builds the answer of ListPullRequests for some agents: their recorded PRs (refreshed first), the
// reviewers still on each, and the work evidence (local git, on ctx: ghCtx bounds only the calls to GitHub) of a worker whose newest report is a done with no PR.
func (d *Daemon) pullRequests(ctx, ghCtx context.Context, r store.Reader, runID string, agents []*v1.Agent, force bool) ([]*v1.AgentPullRequests, error) {
	d.refreshAll(ctx, ghCtx, agents, force)
	reviewing := reviewingPRs(r, runID)
	var out []*v1.AgentPullRequests
	for _, a := range agents {
		if !ownsPRs(a) {
			continue
		}
		prs, err := r.AgentPRs(a.Id)
		if err != nil {
			return nil, err
		}
		sort.SliceStable(prs, func(i, j int) bool { return prs[i].Number < prs[j].Number })
		row := &v1.AgentPullRequests{AgentId: a.Id, AgentName: a.Name, PullRequests: prs, ReadFailed: !d.prs.read(a.Id)}
		seen := map[string]bool{}
		for _, p := range prs {
			for _, name := range reviewing[prKey(p.Url)] {
				if name != a.Name && !seen[name] {
					seen[name] = true
					row.Reviewing = append(row.Reviewing, name)
				}
			}
		}
		rep, err := lastReport(r, a)
		if err != nil {
			return nil, err
		}
		done := rep != nil && rep.Type == v1.MessageType_MESSAGE_TYPE_DONE
		if done || len(prs) == 0 {
			row.Evidence = workEvidence(ctx, a)
		}
		if done { // before it, an open PR with no round yet is no news (reviewWord)
			row.Reviews = d.reviewWord(r, a)
		}
		out = append(out, row)
	}
	return out, nil
}

// ListPullRequests lists each change worker's pull requests with their state and checks.
func (d *Daemon) ListPullRequests(ctx context.Context, req *connect.Request[v1.ListPullRequestsRequest]) (*connect.Response[v1.ListPullRequestsResponse], error) {
	actor := auth.ActorFrom(ctx)
	runID, err := scopedRun(actor, req.Msg.RunId)
	if err != nil {
		return nil, err
	}
	if runID == "" {
		return nil, invalid("name the run (run_id)")
	}
	r := d.store.Read(ctx)
	if _, err := r.GetRun(runID); err != nil {
		return nil, notFound("no run %q", runID)
	}
	agents, err := r.ListAgents(runID)
	if err != nil {
		return nil, err
	}
	if ref := req.Msg.Agent; ref != "" {
		// Among this run's own agents: a name may be in several open runs, and an id of another run is not found.
		var one *v1.Agent
		for _, a := range agents {
			if a.Id == ref || a.Name == ref {
				one = a
				break
			}
		}
		if one == nil {
			return nil, notFound("no agent %q in run %s", ref, runID)
		}
		agents = []*v1.Agent{one}
	}
	leadFirst(agents)
	rows, err := d.pullRequests(ctx, ctx, r, runID, agents, req.Msg.Refresh)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.ListPullRequestsResponse{Agents: rows, GithubProblem: d.ghProblem(ctx)}), nil
}

// briefPRs reads the PRs of the change workers the brief lists (the first briefRows), by agent id.
func (d *Daemon) briefPRs(ctx context.Context, r store.Reader, runID string, agents []*v1.Agent) map[string]*v1.AgentPullRequests {
	var listed []*v1.Agent
	for _, a := range agents {
		if a.Role != v1.AgentRole_AGENT_ROLE_ROOT && len(listed) < briefRows {
			listed = append(listed, a)
		}
	}
	// GitHub is asked for at most a few seconds: a slow `gh` shows the last state recorded, not a stalled brief.
	rctx, cancel := context.WithTimeout(ctx, briefGitHubWait)
	defer cancel()
	rows, err := d.pullRequests(ctx, rctx, r, runID, listed, false)
	if err != nil {
		d.log.Warn("brief pull requests", "run", runID, "err", err)
		return nil
	}
	out := map[string]*v1.AgentPullRequests{}
	for _, row := range rows {
		out[row.AgentId] = row
	}
	return out
}

// prText is how a worker's PRs read in the brief: `PR #12 merged, checks failing`, and for a worker that reported
// done with no PR its commits and whether they are pushed, then `no PR`; `worktree dirty` after a done; `in review
// (a, b)` while reviewers still owe their answers.
func prText(row *v1.AgentPullRequests, done bool) string {
	var parts []string
	for _, p := range row.PullRequests {
		t := fmt.Sprintf("PR #%d %s", p.Number, p.State)
		if p.Draft {
			t += ", draft"
		}
		if p.Held {
			t += ", HELD by the user: do not merge"
		}
		if p.Checks != "" {
			t += ", checks " + p.Checks
		}
		switch {
		case p.State == "unknown":
			t += " (GitHub not read)"
		case p.Stale || row.ReadFailed:
			t += " (GitHub not read: last state seen)"
		}
		parts = append(parts, t)
	}
	if done {
		if ev := row.Evidence; len(row.PullRequests) == 0 {
			switch {
			case ev == nil || !ev.HasCommits:
			case ev.Commits == 0:
				parts = append(parts, "no commits")
			default:
				parts = append(parts, fmt.Sprintf("%d commit(s)", ev.Commits))
				if ev.HasPushed && !ev.Pushed {
					parts = append(parts, "not pushed")
				}
			}
			if row.ReadFailed {
				parts = append(parts, "PR unknown (GitHub not read)")
			} else {
				parts = append(parts, "no PR")
			}
		}
		if row.Evidence != nil && row.Evidence.Dirty {
			parts = append(parts, "worktree dirty")
		}
	}
	if len(row.Reviewing) > 0 {
		parts = append(parts, "in review ("+strings.Join(row.Reviewing, ", ")+")")
	}
	return strings.Join(parts, ", ")
}
