package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// A worker runs and settles its own PR's review round (docs/apps.md: a worker may start read-only agents). The
// round itself is the root's machinery unchanged (ask.go, collect.go); this file holds what is a worker's alone:
// who runs a PR's rounds (`review.by`, `mesh spawn --reviews`), the refusals that keep a worker to its own PR and
// to the round ceiling, its verdicts, and the footer that shows its parent what the rounds found.

const (
	reviewByWorker = "worker"
	reviewByRoot   = "root"
	// reviewByRelay is a risky PR's: the worker runs its reviews itself, as a relay (`mesh ask --relay`), and no other round.
	reviewByRelay = "relay"

	judgeAuthor = "author" // a verdict the PR's author (the worker that asked the round) gave
	judgeRoot   = "root"
	judgeUser   = "user" // a device's
)

// checkReviewBy validates a `reviews` request field.
func checkReviewBy(s string) error {
	if s == "" || s == reviewByWorker || s == reviewByRoot || s == reviewByRelay {
		return nil
	}
	return invalid("reviews %q: worker (it runs its PR's review rounds itself), relay (it runs them as a relay, for a risky PR) or root (its parent does)", s)
}

// reviewByOf is who runs a worker's PR's rounds: the worker's own choice, else the `review.by` setting.
func (d *Daemon) reviewByOf(r store.Reader, a *v1.Agent) string {
	if a.ReviewBy != "" {
		return a.ReviewBy
	}
	if s, err := d.settingString(r, "review.by"); err == nil && s != "" {
		return s
	}
	return reviewByWorker
}

// selfWorker is the calling agent when it is a worker, else nil.
func (d *Daemon) selfWorker(r store.Reader, actor *v1.Actor) *v1.Agent {
	if actor.Class != v1.ActorClass_ACTOR_CLASS_WORKER {
		return nil
	}
	a, err := r.GetAgent(actor.Id)
	if err != nil || a.Role != v1.AgentRole_AGENT_ROLE_WORKER {
		return nil
	}
	return a
}

// reviewTop is the repo a review is of: a worker naming its own worktree, or nothing, means the
// repo that worktree was made from, so the reviewers are named after it and their worktrees come from it.
func (d *Daemon) reviewTop(ctx context.Context, actor *v1.Actor, repo string) (string, error) {
	self := d.selfWorker(d.store.Read(ctx), actor)
	if strings.TrimSpace(repo) == "" {
		if self != nil && self.Repo != "" {
			return self.Repo, nil
		}
		return "", invalid("name the repo (--repo) the pull request is in")
	}
	top, err := d.repoTop(ctx, repo)
	if err != nil {
		return "", err
	}
	if self != nil && self.Repo != "" && self.Worktree != "" && sameDir(top, self.Worktree) {
		return self.Repo, nil
	}
	return top, nil
}

// workerRepoGuard refuses a worker a review in a repo that is not its own, before anything is fetched from it:
// slugs are unique per repo, so two repos' workers can share a branch name, and the branch check alone would let
// one review the other's PR. top is the repo the review is of. Anyone else passes.
func (d *Daemon) workerRepoGuard(ctx context.Context, r store.Reader, actor *v1.Actor, top, review string) error {
	self := d.selfWorker(r, actor)
	if self == nil {
		return nil
	}
	if self.Repo != "" && !sameDir(top, self.Repo) {
		return denied("a worker reviews only its own PR, in its own repo (%s); %s is another: the root reviews the rest", self.Repo, top)
	}
	// A PR given by its URL names its repo, which `gh` reads whatever folder it runs in: it must be one of the
	// repo's remotes (a fork's origin, with the PR on its upstream).
	if want := githubRepoOf(review); want != "" {
		out, err := git(ctx, top, "config", "--get-regexp", `^remote\..*\.url$`)
		if err != nil {
			return nil
		}
		var have []string
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Fields(line); len(f) == 2 {
				if g := githubRepoOf(f[1]); g != "" {
					have = append(have, g)
				}
			}
		}
		if len(have) > 0 && !slices.Contains(have, want) {
			return denied("a worker reviews only its own PR, in its own repo (%s); %s is of %s: the root reviews the rest", strings.Join(have, ", "), review, want)
		}
	}
	return nil
}

var githubRepoRE = regexp.MustCompile(`(?i)github\.com[/:]([^/\s]+)/([^/\s]+?)(?:\.git)?(?:/|$|\?|#)`)

// githubRepoOf is the lowercase owner/repo of a GitHub URL or remote (https, ssh or scp form), else "".
func githubRepoOf(u string) string {
	m := githubRepoRE.FindStringSubmatch(u)
	if m == nil {
		return ""
	}
	return strings.ToLower(m[1] + "/" + m[2])
}

// workerGuard refuses what a worker may not do with a review of this PR: one that is not its own (its head is not
// the worker's branch, nor the branch checked out in its own worktree: a worker may open its PR from another
// branch than the one it was registered on), one whose rounds its parent runs (`review.by`, `--reviews root`), and,
// for a worker on `--reviews relay` (a risky PR), any ask but a relay step. Anyone else passes.
func (d *Daemon) workerGuard(ctx context.Context, r store.Reader, actor *v1.Actor, pr *prInfo, relay bool) error {
	self := d.selfWorker(r, actor)
	if self == nil {
		return nil
	}
	if d.reviewByOf(r, self) == reviewByRoot {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"your parent runs the reviews of your PR (review.by or --reviews root): "+
				"don't ask a round; report `done` and it starts them"))
	}
	if d.reviewByOf(r, self) == reviewByRelay && !relay {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"your PR's reviews are a relay (it is risky): no round and no single reviewer, only `mesh ask --repo <repo> --review <PR> --relay` (one step, then fix, then the next)"))
	}
	if pr.HeadRef == "" || (pr.HeadRef != self.Branch && !d.movedBranchIsItsOwn(ctx, r, self, pr)) {
		return denied("a worker reviews only its own PR (head branch %s); PR #%d is of %q: the root reviews the rest", self.Branch, pr.Number, pr.HeadRef)
	}
	return nil
}

// movedBranchIsItsOwn reports whether the PR is the worker's own though its head is not the branch it was
// registered on: the head is the branch checked out in the worker's own worktree, with commits of its own past its
// base, or the next branch of its own series (`mesh/<run>/<slug>` and `mesh/<run>/<slug>-N`, as a worker that
// stacks PRs names them: registered on `-2`, it opens `-3`); and nothing says it is anyone else's: it is not the
// repo's default branch, no other agent of the repo is registered on it or has it checked out, and no other worker
// owns or asked a round on the PR. `git switch` onto another worker's branch (or `main`) does not make that PR the
// worker's own.
func (d *Daemon) movedBranchIsItsOwn(ctx context.Context, r store.Reader, self *v1.Agent, pr *prInfo) bool {
	head := pr.HeadRef
	if head == "" || head == "main" || head == "master" || head == strings.TrimPrefix(defaultBaseRO(ctx, self.Repo), "origin/") {
		return false
	}
	if !(worktreeBranch(ctx, self) == head && hasBranchWork(ctx, self)) && !inSeriesOf(self.Branch, head) {
		return false
	}
	agents, err := r.ListAgents("")
	if err != nil {
		return false
	}
	for _, o := range agents {
		if o.Id == self.Id || o.Repo == "" || !sameDir(o.Repo, self.Repo) {
			continue
		}
		if o.Branch == head || worktreeBranch(ctx, o) == head {
			return false
		}
		if o.Role != v1.AgentRole_AGENT_ROLE_WORKER {
			continue
		}
		if prs, err := r.AgentPRs(o.Id); err != nil {
			return false
		} else {
			for _, p := range prs {
				if prKey(p.Url) == prKey(pr.URL) {
					return false
				}
			}
		}
	}
	asks, err := r.PRAsks(pr.URL)
	if err != nil {
		return false
	}
	for _, a := range asks {
		if a.CallerId == self.Id {
			continue
		}
		if c, err := r.GetAgent(a.CallerId); err == nil && c.Role == v1.AgentRole_AGENT_ROLE_WORKER {
			return false // another worker asked a round on it: it is theirs
		}
	}
	return true
}

// seriesRE is a branch name `mesh/<run>/<slug>` whose slug may end in `-N`.
var seriesRE = regexp.MustCompile(`^(mesh/[^/]+/.+?)(?:-[0-9]+)?$`)

// inSeriesOf reports whether head is registered's own series: the same `mesh/<run>/` and the same slug, with or
// without a `-N` suffix on either (`ui-overhaul`, `ui-overhaul-2`, `ui-overhaul-3`).
func inSeriesOf(registered, head string) bool {
	a, b := seriesRE.FindStringSubmatch(registered), seriesRE.FindStringSubmatch(head)
	return a != nil && b != nil && a[1] == b[1]
}

// worktreeBranch is the branch checked out in a worker's own worktree (read-only), or "" when it has none, the
// worktree is gone or HEAD is detached: then only the registered branch counts.
func worktreeBranch(ctx context.Context, a *v1.Agent) string {
	if a.Worktree == "" {
		return ""
	}
	b, err := gitRO(ctx, a.Worktree, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return b
}

// workerRoundsUsed is how many reviews of the PR count against a worker's ceiling: the answered rounds (any
// caller's, any run: rounds are the PR's) and the single reviewers the worker asked, which have no round to
// count. The ask named except is left out (the one being described).
func workerRoundsUsed(r store.Reader, callerID, pr, except string) (int, error) {
	all, err := r.PRAsks(pr)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range all {
		if a.Kind != v1.AskKind_ASK_KIND_REVIEW || a.Id == except {
			continue
		}
		if a.Collected && a.Status == v1.AskStatus_ASK_STATUS_FINISHED && answered(a) > 0 || !a.Collected && a.CallerId == callerID {
			n++
		}
	}
	return n, nil
}

// openRound is a collected review round of the PR still waiting for its reviewers, other than the ask `except`
// (this request's own, when a retry finds the round it left half started).
func openRound(r store.Reader, pr, except string) (*v1.Ask, error) {
	all, err := r.PRAsks(pr)
	if err != nil {
		return nil, err
	}
	for _, a := range all {
		if a.Kind == v1.AskKind_ASK_KIND_REVIEW && a.Collected && a.Status == v1.AskStatus_ASK_STATUS_OPEN && a.Id != except {
			return a, nil
		}
	}
	return nil, nil
}

// workerCeiling refuses a worker's round past `review.rounds.<size>`: the daemon's cap, not the author's; the
// root may still run one. A round is also refused while another of the PR is open, or two asked together would
// both pass the count of the finished ones.
func (d *Daemon) workerCeiling(r store.Reader, actor *v1.Actor, meta *v1.RequestMeta, pr *prInfo, size string) error {
	if d.selfWorker(r, actor) == nil {
		return nil
	}
	except := ""
	if meta.GetMutationId() != "" {
		except = askIDFor(actor, meta)
	}
	if open, err := openRound(r, pr.URL, except); err != nil {
		return err
	} else if open != nil {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"a review round of PR #%d is still open (ask %s by %s, until %s at the latest): wait for its answers",
			pr.Number, open.Id, open.CallerName, open.Deadline.AsTime().UTC().Format("15:04:05Z")))
	}
	max, err := d.settingInt(r, "review.rounds."+sizeKey(size))
	if err != nil {
		return err
	}
	used, err := workerRoundsUsed(r, actor.Id, pr.URL, "")
	if err != nil {
		return err
	}
	if used >= max {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"PR #%d has had %d of at most %d review rounds for a %s PR (`review.rounds.%s`): fix what you agree with from them and "+
				"report `done`; your parent decides on more", pr.Number, used, max, size, sizeKey(size)))
	}
	return nil
}

// reviewTier is the tier a review without one runs its reviewers on: `review.tier.<size>`.
func (d *Daemon) reviewTier(r store.Reader, size string) (string, error) {
	return d.settingString(r, "review.tier."+sizeKey(size))
}

// callerIsWorker tells whether a round was asked by a worker (the PR's author), whose hint reads differently.
func callerIsWorker(r store.Reader, a *v1.Ask) bool {
	c, err := r.GetAgent(a.CallerId)
	return err == nil && c.Role == v1.AgentRole_AGENT_ROLE_WORKER
}

// judgeOf is the label of who gave a verdict.
func judgeOf(actor *v1.Actor) string {
	switch actor.Class {
	case v1.ActorClass_ACTOR_CLASS_WORKER:
		return judgeAuthor
	case v1.ActorClass_ACTOR_CLASS_ROOT:
		return judgeRoot
	}
	return judgeUser
}

// openOwnAsk tells whether the agent has an ask it waits on: a collected one (a round, or the peers) still waiting
// for its members, or a single reviewer it asked that has not reported yet. It waits on purpose, so its parent is
// not owed a report yet (accountability).
func (d *Daemon) openOwnAsk(r store.Reader, a *v1.Agent, now time.Time) bool {
	asks, err := r.OpenAsks()
	if err != nil {
		return false
	}
	for _, k := range asks {
		if k.Collected && k.CallerId == a.Id && (k.Deadline == nil || now.Before(k.Deadline.AsTime())) {
			return true
		}
	}
	all, err := r.RunAsks(a.RunId)
	if err != nil {
		return false
	}
	// A single reviewer is waited on for as long as a round's members are (`ask.timeout`): one that hung without
	// reporting must not keep its asker from being nudged for good.
	limit := time.Duration(0)
	if v, err := d.Setting(r, "ask.timeout"); err == nil {
		limit = time.Duration(v.(float64) * float64(time.Second))
	}
	for _, k := range all {
		if k.Collected || k.CallerId != a.Id || k.Kind != v1.AskKind_ASK_KIND_REVIEW || !now.Before(k.CreatedAt.AsTime().Add(limit)) {
			continue
		}
		for _, m := range k.Members {
			rev, err := r.GetAgent(m.AgentId)
			if err != nil || stopped(rev) {
				continue
			}
			if rep, err := lastReport(r, rev); err == nil && rep == nil {
				return true
			}
		}
	}
	return false
}

// hasBranchWork tells whether the worker's branch has commits past where it started. It runs git, so it is asked
// before a write, not in one; where it can't tell (no worktree) the answer is yes.
func hasBranchWork(ctx context.Context, a *v1.Agent) bool {
	if a.Worktree == "" || a.BaseCommit == "" {
		return true
	}
	n, err := git(ctx, a.Worktree, "rev-list", "--count", a.BaseCommit+"..HEAD")
	return err != nil || n != "0"
}

// reviewFooter is what a worker's `done` to its parent carries about its PRs' reviews: one outcome line per PR, from
// the store alone. SETTLED, or NEEDS YOU with what is open (the parent then reads only what the line names), never the
// findings and never the declines. An unknown is not a pass: a PR with no round, a round still collecting, a
// reviewer that did not answer, an answer with no verdict, a blocking finding the author declined, a relay not done,
// failing or pending checks, or a PR the worker opened and asked nothing on, all make it NEEDS YOU. A worker whose
// parent runs its reviews gets none.
func (d *Daemon) reviewFooter(r store.Reader, self *v1.Agent, body string, facts gitFacts) string {
	outs := d.reviewOutcomes(r, self, body, facts)
	if len(outs) == 0 {
		return ""
	}
	var lines []string
	for _, o := range outs {
		lines = append(lines, o.line)
	}
	return "\n\n" + strings.Join(lines, "\n")
}

// gitFacts is what a `done`'s outcome lines need that only git or GitHub can say, read before the store write (a
// write runs no git): whether the worker's branch has commits, and by PR key how many commits the PR's head has past
// the head its newest review round covered (-1: the head differs and the count can't be told).
type gitFacts struct {
	hasWork bool
	after   map[string]int
}

// commitsAfterReview reads, for each PR of the worker's that it asked a round on, the commits the PR's head has past
// the head that round reviewed. The head is the one GitHub says now (the recorded row's is the last read's: a fix pushed
// just before the `done` is what this is for), the row's when GitHub can't be read.
func (d *Daemon) commitsAfterReview(ctx context.Context, self *v1.Agent) map[string]int {
	r := d.store.Read(ctx)
	asks, err := r.RunAsks(self.RunId)
	if err != nil {
		return nil
	}
	reviewed := map[string]*v1.Ask{} // the worker's newest round on each PR
	for _, a := range asks {
		if a.Kind != v1.AskKind_ASK_KIND_REVIEW || a.CallerId != self.Id || a.Pr == "" || a.Head == "" {
			continue
		}
		if k := prKey(a.Pr); reviewed[k] == nil || a.CreatedAt.AsTime().After(reviewed[k].CreatedAt.AsTime()) {
			reviewed[k] = a
		}
	}
	if len(reviewed) == 0 {
		return nil
	}
	recorded := map[string]*v1.PullRequest{}
	if prs, err := r.AgentPRs(self.Id); err == nil {
		for _, p := range prs {
			recorded[prKey(p.Url)] = p
		}
	}
	gctx, cancel := context.WithTimeout(ctx, briefGitHubWait)
	defer cancel()
	out := map[string]int{}
	for k, a := range reviewed {
		p := recorded[k] // none yet for a PR no reader has refreshed
		if p != nil && p.State != "open" {
			continue
		}
		head := ""
		if p != nil {
			head = p.Head
		}
		if v, err := d.viewPR(gctx, self, a.Pr); err == nil && v.HeadRefOid != "" {
			head = v.HeadRefOid
		}
		if head == "" || head == a.Head {
			continue
		}
		n := -1
		if c, err := git(ctx, ghDir(self), "rev-list", "--count", a.Head+".."+head); err == nil {
			if v, err := strconv.Atoi(c); err == nil && v > 0 {
				n = v
			}
		}
		out[k] = n
	}
	return out
}

// outcome is one PR's review outcome: its line, and its state (settled, running: only a round still collecting,
// or needs you).
type outcome struct{ line, state string }

// reviewOutcomes is the outcome of each PR a worker's `done` (body) concerns, in order; none for a worker whose
// parent runs its reviews.
func (d *Daemon) reviewOutcomes(r store.Reader, self *v1.Agent, body string, facts gitFacts) []outcome {
	hasWork := facts.hasWork
	if self.Role != v1.AgentRole_AGENT_ROLE_WORKER || d.reviewByOf(r, self) == reviewByRoot {
		return nil
	}
	var urls []string
	seen := map[string]bool{}
	for _, u := range prURLRE.FindAllString(body, -1) {
		if k := prKey(u); !seen[k] {
			seen[k] = true
			urls = append(urls, u)
		}
	}
	asks, err := r.RunAsks(self.RunId)
	if err != nil {
		return nil
	}
	byPR := map[string][]*v1.Ask{} // the review asks of the worker, by PR
	var prOrder []string
	prURL := map[string]string{}
	for _, a := range asks {
		if a.Kind != v1.AskKind_ASK_KIND_REVIEW || a.CallerId != self.Id || a.Pr == "" {
			continue
		}
		k := prKey(a.Pr)
		if byPR[k] == nil {
			prOrder = append(prOrder, k)
			prURL[k] = a.Pr
		}
		byPR[k] = append(byPR[k], a)
	}
	sort.Strings(prOrder)
	recorded := map[string]*v1.PullRequest{}
	if prs, err := r.AgentPRs(self.Id); err == nil {
		for _, p := range prs {
			recorded[prKey(p.Url)] = p
		}
	}
	// The PRs the done names that are the worker's own. A recorded PR whose head branch GitHub gave is decided by it
	// (the worker's branch or one of its series; a PR the worker asked a round on is listed below whatever its head); one the branch lookup found is the worker's; a link a
	// `done` once named, recorded with no head branch, proves nothing; and when none is its own and the branch has
	// commits (a new PR, a reused worker's older ones), the first link not already known to be another branch's.
	own := map[string]bool{}
	decided := map[string]bool{}
	for _, u := range urls {
		k := prKey(u)
		p := recorded[k]
		switch {
		case p == nil:
		case p.HeadRef != "":
			decided[k] = true
			own[k] = p.HeadRef == self.Branch || inSeriesOf(self.Branch, p.HeadRef)
		case p.Source == "branch":
			own[k] = true // found by `gh pr list --head <its branch>`
		}
	}
	// hasWork: the worker's branch has commits. A worker with none (research, a question) that names a PR names
	// somebody else's, and "asked no review round" would be wrong of it.
	if !slices.ContainsFunc(urls, func(u string) bool { return own[prKey(u)] }) && hasWork {
		for _, u := range urls {
			if !decided[prKey(u)] {
				own[prKey(u)] = true
				break
			}
		}
	}
	var outs []outcome
	done := map[string]bool{}
	line := func(k, u string) {
		if done[k] {
			return
		}
		done[k] = true
		l, st := d.reviewOutcome(r, self, prNumberOf(u), u, byPR[k], recorded[k], facts.after[k])
		outs = append(outs, outcome{l, st})
	}
	for _, u := range urls {
		if own[prKey(u)] {
			line(prKey(u), u)
		}
	}
	for _, k := range prOrder { // a PR it asked rounds on that its done doesn't name
		if p := recorded[k]; p != nil && (p.State == "merged" || p.State == "closed") && !seen[k] {
			continue // over and done: its checks are no news
		}
		line(k, prURL[k])
	}
	// A PR of its own the worker opened that is still open with no round of its own: not named in the done, but not
	// settled either.
	var open []string
	for k := range recorded {
		open = append(open, k)
	}
	sort.Strings(open)
	for _, k := range open {
		p := recorded[k]
		if p.State == "open" && len(byPR[k]) == 0 && (p.Source == "branch" || p.HeadRef == self.Branch || p.HeadRef != "" && inSeriesOf(self.Branch, p.HeadRef)) {
			line(k, p.Url)
		}
	}
	return outs
}

// reviewWord is the one word the brief shows of a finished worker's PR reviews: "needs you" when any PR's outcome is
// NEEDS YOU for a reason of its own, "running" when only a round is still collecting, else "settled"; "" for a worker
// with no PR or round to speak of. The caller asks only after the worker's `done`: before it, an open PR with no
// round yet is no news.
func (d *Daemon) reviewWord(r store.Reader, a *v1.Agent) string {
	outs := d.reviewOutcomes(r, a, "", gitFacts{})
	if len(outs) == 0 {
		return ""
	}
	word := "settled"
	for _, o := range outs {
		switch {
		case o.state == "needs you":
			return "needs you"
		case o.state == "running":
			word = "running"
		}
	}
	return word
}

// reviewOutcome is the line for one PR of a worker: SETTLED with what the reviews came to, or NEEDS YOU with what is
// open. rounds are the asks the worker made of it (none: the rounds others asked are named), p its recorded row.
func (d *Daemon) reviewOutcome(r store.Reader, self *v1.Agent, number, url string, rounds []*v1.Ask, p *v1.PullRequest, after int) (string, string) {
	head := "[mesh reviews] PR #" + number + ": "
	checks := "checks none"
	if p != nil && p.Checks != "" {
		checks = "checks " + p.Checks
	}
	if len(rounds) == 0 {
		others, err := prAsksByKey(r, url)
		var by []string
		if err == nil {
			for _, a := range others {
				if a.Kind == v1.AskKind_ASK_KIND_REVIEW && !slices.Contains(by, a.CallerName) {
					by = append(by, a.CallerName)
				}
			}
		}
		if len(by) == 0 {
			return head + "NEEDS YOU: " + self.Name + " asked no review round: ask it why, or start one yourself.", "needs you"
		}
		return head + "NEEDS YOU: " + self.Name + " asked no review round itself; rounds of it were asked by " + strings.Join(by, ", ") + " (`mesh asks`).", "needs you"
	}
	sort.SliceStable(rounds, func(i, j int) bool { return rounds[i].CreatedAt.AsTime().Before(rounds[j].CreatedAt.AsTime()) })
	last := rounds[len(rounds)-1]
	var why []string
	collecting := false
	var findings, agreed, other, disagreed, declinedBlocking int32
	steps := 0
	for _, a := range rounds {
		if a.Status == v1.AskStatus_ASK_STATUS_OPEN && !collecting {
			why = append(why, "a round is still collecting its answers")
			collecting = true
		}
		// A step that nobody answered is run again under the same number: only answered steps count.
		if a.RelayStep > 0 && answered(a) > 0 && int(a.RelayStep) > steps {
			steps = int(a.RelayStep)
		}
		for _, v := range a.Verdicts {
			findings += v.Agree + v.Other + v.Disagree
			agreed += v.Agree
			other += v.Other
			disagreed += v.Disagree
			// The author's own count stands until the root judges the reviewer's findings (its verdict replaces it).
			if v.Judge == judgeAuthor && v.DeclinedBlocking > 0 {
				declinedBlocking += v.DeclinedBlocking
				why = append(why, fmt.Sprintf("%s: %d blocking finding(s) declined by the author", v.Reviewer, v.DeclinedBlocking))
			}
		}
	}
	verdictOf := func(a *v1.Ask, name string) *v1.Verdict {
		for _, v := range a.Verdicts {
			if v.Reviewer == name {
				return v
			}
		}
		return nil
	}
	asked := 0
	var kinds []string
	for _, m := range last.Members {
		if m.Status == v1.AskMemberStatus_ASK_MEMBER_STATUS_NOT_ASKED {
			continue
		}
		asked++
		kinds = append(kinds, m.Kind)
		v := verdictOf(last, m.Name)
		switch {
		case last.Status == v1.AskStatus_ASK_STATUS_OPEN: // still collecting: said above, once
		case last.Collected && m.Status != v1.AskMemberStatus_ASK_MEMBER_STATUS_ANSWERED:
			why = append(why, m.Name+" did not answer"+bracket(m.Line))
		case v == nil: // answered (or asked alone, its done went to the asker) and not judged
			why = append(why, m.Name+" answered and has no verdict recorded")
		}
	}
	if asked == 0 {
		why = append(why, "no reviewer could be asked")
	}
	if d.reviewByOf(r, self) == reviewByRelay || steps > 0 {
		switch {
		case steps == 0:
			why = append(why, "its relay has not started")
		case steps < 3 && relayHasFindings(last):
			why = append(why, fmt.Sprintf("its relay is unfinished (step %d of 3, with findings to fix)", steps))
		}
	}
	if p != nil && (p.Checks == "failing" || p.Checks == "pending") {
		why = append(why, "checks "+p.Checks)
	}
	if p != nil && (p.State == "unknown" || p.Stale) {
		why = append(why, "GitHub's state of the PR was not read (checks unknown)")
	}
	if len(why) > 0 {
		out := head + "NEEDS YOU: " + strings.Join(why, "; ") + "."
		if last.AnswersPath != "" {
			out += " Answers: " + filepath.Clean(last.AnswersPath)
		}
		if collecting && len(why) == 1 {
			return out, "running"
		}
		return out, "needs you"
	}
	what := fmt.Sprintf("%d round(s)", len(rounds))
	if steps > 0 {
		what = fmt.Sprintf("relay, %d step(s)", steps)
	}
	late := ""
	switch {
	case after > 0:
		late = fmt.Sprintf("; %d commit(s) after the last reviewed head", after)
	case after < 0:
		late = "; the PR's head is not the last reviewed head (commits after it, count unknown)"
	}
	return head + fmt.Sprintf("SETTLED: %s (%s); %d finding(s): %d agreed, %d other, %d disagreed (%d blocking declined); %s%s",
		what, strings.Join(kinds, ", "), findings, agreed, other, disagreed, declinedBlocking, checks, late), "settled"
}

// relayHasFindings tells whether the relay's latest step left findings the author used (agreed with, or found
// otherwise useful: a step whose verdicts take nothing leaves nothing to fix: "stop at any step that leaves
// nothing worth fixing").
func relayHasFindings(last *v1.Ask) bool {
	for _, v := range last.Verdicts {
		if v.Agree+v.Other > 0 {
			return true
		}
	}
	return false
}

// bracket is " (s)" for a note, "" for none.
func bracket(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}

func prNumberOf(url string) string {
	return url[strings.LastIndex(url, "/")+1:]
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
