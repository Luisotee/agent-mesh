package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
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

	judgeAuthor = "author" // a verdict the PR's author (the worker that asked the round) gave
	judgeRoot   = "root"
	judgeUser   = "user" // a device's
)

// checkReviewBy validates a `reviews` request field.
func checkReviewBy(s string) error {
	if s == "" || s == reviewByWorker || s == reviewByRoot {
		return nil
	}
	return invalid("reviews %q: worker (it runs its PR's review rounds itself) or root (its parent does)", s)
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
// the worker's branch), and one whose rounds its parent runs (`review.by`, `--reviews root`: a relay for a risky
// PR). Anyone else passes.
func (d *Daemon) workerGuard(r store.Reader, actor *v1.Actor, pr *prInfo) error {
	self := d.selfWorker(r, actor)
	if self == nil {
		return nil
	}
	if d.reviewByOf(r, self) == reviewByRoot {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"your parent runs the reviews of your PR (review.by or --reviews root: a relay, or a PR it reads itself): "+
				"don't ask a round; report `done` and it starts them"))
	}
	if pr.HeadRef == "" || pr.HeadRef != self.Branch {
		return denied("a worker reviews only its own PR (head branch %s); PR #%d is of %q: the root reviews the rest", self.Branch, pr.Number, pr.HeadRef)
	}
	return nil
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

// reviewFooter is what a worker's `done` to its parent carries about its PR's reviews, from the store alone: each
// round it asked (who answered, the answers file, the verdicts and who gave them), and for a PR the `done` names
// that it asked no round on, a line saying so. The parent then needs no relay to find the findings, and a
// skipped round shows instead of passing silently. A worker whose parent runs its reviews gets none.
func (d *Daemon) reviewFooter(r store.Reader, self *v1.Agent, body string, hasWork bool) string {
	if self.Role != v1.AgentRole_AGENT_ROLE_WORKER || d.reviewByOf(r, self) == reviewByRoot {
		return ""
	}
	var urls []string
	seen := map[string]bool{}
	for _, u := range prURLRE.FindAllString(body, -1) {
		if k := prKey(u); !seen[k] {
			seen[k] = true
			urls = append(urls, u)
		}
	}
	// The PRs the done names that are the worker's own. A recorded PR whose head branch GitHub gave is decided by
	// it (the worker's own is the one on its branch); one the branch lookup found is the worker's; a link a `done`
	// once named, recorded with no head branch, proves nothing; and when none is its own and the branch has
	// commits (a new PR, a reused worker's older ones), the first link not already known to be another branch's.
	own := map[string]bool{}
	decided := map[string]bool{}
	if prs, err := r.AgentPRs(self.Id); err == nil {
		recorded := map[string]*v1.PullRequest{}
		for _, p := range prs {
			recorded[prKey(p.Url)] = p
		}
		for _, u := range urls {
			p := recorded[prKey(u)]
			switch {
			case p == nil:
			case p.HeadRef != "":
				decided[prKey(u)] = true
				own[prKey(u)] = p.HeadRef == self.Branch
			case p.Source == "branch":
				own[prKey(u)] = true // found by `gh pr list --head <its branch>`
			}
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
	asks, err := r.RunAsks(self.RunId)
	if err != nil {
		return ""
	}
	byPR := map[string][]*v1.Ask{}
	var prOrder []string
	for _, a := range asks {
		if a.Kind != v1.AskKind_ASK_KIND_REVIEW || a.CallerId != self.Id || a.Pr == "" {
			continue
		}
		k := prKey(a.Pr)
		if byPR[k] == nil {
			prOrder = append(prOrder, k)
		}
		byPR[k] = append(byPR[k], a)
	}
	sort.Strings(prOrder)
	var lines []string
	covered := map[string]bool{}
	for _, u := range urls {
		k := prKey(u)
		if len(byPR[k]) > 0 || !own[k] {
			continue
		}
		// Rounds of the PR that others asked (the root, an earlier run) count against the ceiling too.
		others, err := prAsksByKey(r, u)
		if err != nil {
			continue
		}
		var by []string
		for _, a := range others {
			if a.Kind == v1.AskKind_ASK_KIND_REVIEW && !slices.Contains(by, a.CallerName) {
				by = append(by, a.CallerName)
			}
		}
		if len(by) == 0 {
			lines = append(lines, fmt.Sprintf("[mesh reviews] PR #%s: %s asked no review round: ask it why, or start one yourself.", prNumberOf(u), self.Name))
		} else {
			lines = append(lines, fmt.Sprintf("[mesh reviews] PR #%s: %s asked no review round itself; rounds of it were asked by %s (`mesh asks`).",
				prNumberOf(u), self.Name, strings.Join(by, ", ")))
		}
		covered[k] = true
	}
	for _, k := range prOrder {
		if !covered[k] {
			lines = append(lines, roundsFooter(byPR[k])...)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(lines, "\n")
}

func prNumberOf(url string) string {
	return url[strings.LastIndex(url, "/")+1:]
}

// roundsFooter describes one PR's asks by one worker, oldest first.
func roundsFooter(asks []*v1.Ask) []string {
	sort.Slice(asks, func(i, j int) bool { return asks[i].CreatedAt.AsTime().Before(asks[j].CreatedAt.AsTime()) })
	out := []string{fmt.Sprintf("[mesh reviews] PR #%d: %d review ask(s) by its author:", asks[0].PrNumber, len(asks))}
	for _, a := range asks {
		var who []string
		for _, m := range a.Members {
			switch {
			case !a.Collected: // its `done` went to the asking agent: the ask never learns whether it answered
				who = append(who, m.Name+" asked alone (its done went to "+a.CallerName+")")
			case m.Status == v1.AskMemberStatus_ASK_MEMBER_STATUS_ANSWERED:
				who = append(who, m.Name+" answered")
			case m.Line != "":
				who = append(who, m.Name+" no answer ("+m.Line+")")
			default:
				who = append(who, m.Name+" pending")
			}
		}
		line := fmt.Sprintf("- round %d (%s, %s) at %s: %s", a.Round, a.Size, orDash(a.Tier), short(a.Head), strings.Join(who, ", "))
		if !a.Collected {
			line = fmt.Sprintf("- one reviewer (%s, %s) at %s: %s", a.Size, orDash(a.Tier), short(a.Head), strings.Join(who, ", "))
		}
		if a.AnswersPath != "" {
			line += "; answers " + filepath.Clean(a.AnswersPath)
		}
		out = append(out, line)
		var judged []string
		have := map[string]bool{}
		for _, v := range a.Verdicts {
			have[v.Reviewer] = true
			judged = append(judged, fmt.Sprintf("%s %d/%d/%d/%d by %s", v.Reviewer, v.Agree, v.Other, v.Disagree, v.Blocking, orDefault(v.Judge, judgeRoot)))
		}
		for _, m := range a.Members {
			if (m.Status == v1.AskMemberStatus_ASK_MEMBER_STATUS_ANSWERED || !a.Collected) && !have[m.Name] {
				judged = append(judged, m.Name+" no verdict recorded")
			}
		}
		if len(judged) > 0 {
			out = append(out, "  verdicts (agree/other/disagree/blocking): "+strings.Join(judged, "; "))
		}
	}
	return out
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
