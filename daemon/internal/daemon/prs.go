package daemon

// Pull requests per agent (bin/mesh's pr_states, report_prs, waiting_rows and work_evidence; design §13). A change
// worker owns the PRs of its branch (`gh pr list --head`) and the ones its `done` names. Their last state is kept in
// SQLite (`agent_prs`), read again from GitHub at most every prTTL (prRetryTTL after a failed read) and only when a
// reader asks (`mesh prs`, the brief, an alert's evidence): there is no timer. A change of state or checks is an
// event of the run's log, never a message.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	prJSON      = "number,url,state,isDraft,headRefOid,statusCheckRollup"
	prListLimit = "20"
)

var prURLRE = regexp.MustCompile(`https://github\.com/[\w.-]+/[\w.-]+/pull/\d+`)

// prTracker remembers, per agent, when its PRs were last read and whether that worked.
type prTracker struct {
	mu    sync.Mutex
	tried map[string]prTry
}

type prTry struct {
	at time.Time
	ok bool
}

func (t *prTracker) due(agentID string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, seen := t.tried[agentID]
	if !seen {
		return true
	}
	ttl := prTTL
	if !p.ok {
		ttl = prRetryTTL
	}
	return now.Sub(p.at) >= ttl
}

func (t *prTracker) failed(agentID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, seen := t.tried[agentID]
	return seen && !p.ok
}

func (t *prTracker) note(agentID string, now time.Time, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tried == nil {
		t.tried = map[string]prTry{}
	}
	t.tried[agentID] = prTry{now, ok}
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
	Rollup     []map[string]any `json:"statusCheckRollup"`
}

// checksWord folds `gh`'s statusCheckRollup to one word: failing, pending or pass ("" with no checks). A check run
// has a status and a conclusion; a commit status only a state.
func checksWord(rollup []map[string]any) string {
	failed := map[string]bool{"FAILURE": true, "ERROR": true, "TIMED_OUT": true, "CANCELLED": true, "ACTION_REQUIRED": true, "STARTUP_FAILURE": true}
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
		case failed[outcome]:
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
		Checks: checksWord(g.Rollup), Source: source, CheckedAt: timestamppb.New(at)}
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

// branchPRs reads the PRs from an agent's branch: all states, those whose head is on the branch.
func (d *Daemon) branchPRs(ctx context.Context, a *v1.Agent) ([]ghPRState, error) {
	remotes := githubRepos(ctx, a.Repo)
	if len(remotes) == 0 {
		return nil, nil // not a GitHub repo: no PRs to find
	}
	remote := prRemote(ctx, a.Repo, a.Branch, remotes)
	args := []string{"pr", "list", "--repo", remotes[remote], "--head", a.Branch, "--state", "all", "--json", prJSON, "--limit", prListLimit}
	out, err := d.runGH(ctx, ghDir(a), args...)
	if err != nil {
		return nil, err
	}
	var prs []ghPRState
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, err
	}
	dir := ghDir(a)
	var own []ghPRState
	for _, p := range prs {
		if prOnBranch(ctx, dir, p.HeadRefOid, a.Branch, a.BaseCommit) {
			own = append(own, p)
		}
	}
	return own, nil
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

// reportPRs are the PRs of an agent's newest `done` that names any: a fix round's report may not repeat the link.
func reportPRs(msgs []*v1.Message, a *v1.Agent) []string {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
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

// refreshPRs reads an agent's PRs from GitHub when they are due (or `force`) and records what changed. It
// never fails the caller: a read that fails keeps the last state, marks it stale and is tried again after prRetryTTL.
func (d *Daemon) refreshPRs(ctx context.Context, a *v1.Agent, force bool) {
	if !ownsPRs(a) {
		return
	}
	now := d.nowTime()
	if !force && !d.prs.due(a.Id, now) {
		return
	}
	// A claim first, so two readers at once make one round of calls (the tracker is the throttle).
	d.prs.note(a.Id, now, true)
	r := d.store.Read(ctx)
	msgs, err := r.RunMessages(a.RunId, "", -1)
	if err != nil {
		return
	}
	var found []*v1.PullRequest
	seen := map[string]bool{}
	var ferr error
	branch, err := d.branchPRs(ctx, a)
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
		p, err := d.viewPR(ctx, a, u)
		if err != nil {
			ferr = err
			continue
		}
		seen[prKey(p.URL)] = true
		found = append(found, p.record(a.Id, "report", now))
	}
	d.prs.note(a.Id, now, ferr == nil)
	if ctx.Err() != nil {
		return
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
			if o != nil && o.State == p.State && o.Checks == p.Checks && o.Draft == p.Draft && !o.Stale {
				// Nothing changed: the row is rewritten only to move its read time on.
				if err := tx.PutAgentPR(p); err != nil {
					return err
				}
				continue
			}
			if err := tx.PutAgentPR(p); err != nil {
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
		// Rows the read didn't return keep their last state; after a failed read they are marked stale.
		for _, o := range prev {
			if ferr != nil && !o.Stale {
				o.Stale = true
				if err := tx.PutAgentPR(o); err != nil {
					return err
				}
			}
		}
		if ferr != nil { // a link in a report whose read failed: shown as unknown until it can be read
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
	if err != nil && ctx.Err() == nil {
		d.log.Warn("pull requests", "agent", a.Name, "err", err)
	}
}

// refreshAll refreshes every change worker's PRs, prFetchers at a time.
func (d *Daemon) refreshAll(ctx context.Context, agents []*v1.Agent, force bool) {
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
			d.refreshPRs(ctx, a, force)
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
func evidenceText(ev *v1.WorkEvidence, prs []*v1.PullRequest) string {
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
		var open []string
		for _, p := range prs {
			if p.State == "open" {
				open = append(open, "PR #"+strconv.Itoa(int(p.Number))+" open")
			}
		}
		if len(open) > 0 {
			parts = append(parts, strings.Join(open, ", "))
		} else if len(prs) == 0 {
			parts = append(parts, "no PR")
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
	d.refreshPRs(ctx, a, false)
	prs, _ := d.store.Read(ctx).AgentPRs(a.Id)
	if t := evidenceText(workEvidence(ctx, a), prs); t != "" {
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
// reviewers still on each, and the work evidence of a worker whose newest report is a done with no PR.
func (d *Daemon) pullRequests(ctx context.Context, r store.Reader, runID string, agents []*v1.Agent, force bool) ([]*v1.AgentPullRequests, error) {
	d.refreshAll(ctx, agents, force)
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
		row := &v1.AgentPullRequests{AgentId: a.Id, AgentName: a.Name, PullRequests: prs, ReadFailed: d.prs.failed(a.Id)}
		seen := map[string]bool{}
		for _, p := range prs {
			for _, name := range reviewing[prKey(p.Url)] {
				if name != a.Name && !seen[name] {
					seen[name] = true
					row.Reviewing = append(row.Reviewing, name)
				}
			}
		}
		if rep, err := lastReport(r, a); err != nil {
			return nil, err
		} else if rep != nil && rep.Type == v1.MessageType_MESSAGE_TYPE_DONE || len(prs) == 0 {
			row.Evidence = workEvidence(ctx, a)
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
		one, err := d.FindAgent(r, actor, ref)
		if err != nil {
			return nil, err
		}
		agents = []*v1.Agent{one}
	}
	leadFirst(agents)
	rows, err := d.pullRequests(ctx, r, runID, agents, req.Msg.Refresh)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.ListPullRequestsResponse{Agents: rows}), nil
}

// briefPRs reads the PRs of the change workers the brief lists (the first briefRows), by agent id.
func (d *Daemon) briefPRs(ctx context.Context, r store.Reader, runID string, agents []*v1.Agent) map[string]*v1.AgentPullRequests {
	var listed []*v1.Agent
	for _, a := range agents {
		if a.Role != v1.AgentRole_AGENT_ROLE_ROOT && len(listed) < briefRows {
			listed = append(listed, a)
		}
	}
	rows, err := d.pullRequests(ctx, r, runID, listed, false)
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
		if p.Checks != "" {
			t += ", checks " + p.Checks
		}
		if p.Stale {
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
