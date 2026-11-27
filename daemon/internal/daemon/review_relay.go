package daemon

// A relay for a risky PR (ORCHESTRATE.md, "Reviewing a PR"): reviewers go one at a time, each on code the earlier
// one's findings already fixed. One command, `mesh ask --review <PR> --relay`, run again after the author has
// pushed each step's fixes, starts the next step; the daemon keeps the state (which kind reviewed, at which
// head, how many steps) so the root passes no --since and no note, and a step is no round of the PR's
// (`roundsOf` leaves it out).
//
//	step 1: A, a CLI that isn't the author's, reviews the whole PR.
//	step 2: B, the other family (the author's own CLI, fresh, with no other installed), reviews the whole PR at
//	        the new head, told which commits fixed an earlier review.
//	step 3: A again, the same agent, on the fix commits only.

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// relaySteps is at most this many.
const relaySteps = 3

// relayJob is what a relay step tells reviewRoundOf: its number, the first step's ask, the commit to review from
// ("" for the whole PR) and the earlier ask whose reviewer is resumed.
type relayJob struct {
	step  int
	of    string
	since string
	reuse *v1.Ask
}

// relayStepsOf are a relay's asks on a PR in this run, oldest first.
func (d *Daemon) relayStepsOf(r interface {
	PRAsks(string) ([]*v1.Ask, error)
}, runID, pr string) ([]*v1.Ask, error) {
	all, err := r.PRAsks(pr)
	if err != nil {
		return nil, err
	}
	var out []*v1.Ask
	for _, a := range all {
		if a.RelayStep > 0 && a.RunId == runID {
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.AsTime().Before(out[j].CreatedAt.AsTime()) })
	return out, nil
}

// openRelayStep refuses while a step of the relay is still collecting its answer.
func openRelayStep(steps []*v1.Ask) error {
	for _, a := range steps {
		if a.Status == v1.AskStatus_ASK_STATUS_OPEN {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
				"relay step %d of this PR is still collecting its answer (%s): its message comes when the reviewer has answered", a.RelayStep, a.Id))
		}
	}
	return nil
}

// relayOpenStep reads the PR's relay steps and refuses while one is open.
func (d *Daemon) relayOpenStep(r store.Reader, runID, pr string) error {
	steps, err := d.relayStepsOf(r, runID, pr)
	if err != nil {
		return err
	}
	return openRelayStep(steps)
}

// answeringKind is the kind of the first member of an ask that answered.
func answeringKind(a *v1.Ask) string {
	for _, m := range a.Members {
		if m.Status == v1.AskMemberStatus_ASK_MEMBER_STATUS_ANSWERED {
			return m.Kind
		}
	}
	return ""
}

// relayStep runs the next step of a PR's relay.
func (d *Daemon) relayStep(ctx context.Context, actor *v1.Actor, m *v1.AskRequest, body string) (*connect.Response[v1.AskResponse], error) {
	if m.Reviewers != "" || m.Kind != "" || m.Model != "" || m.Since != "" || len(m.Mcp) > 0 {
		return nil, invalid("a relay picks its reviewer, its model and its --since itself: give the repo, the PR (--review) and optionally --size, --tier and --body")
	}
	cm := proto.Clone(m).(*v1.AskRequest)
	if cm.Size == "" {
		cm.Size = "large"
	}
	if _, ok := sizes[cm.Size]; !ok {
		return nil, invalid("size %q: trivial, small or large", cm.Size)
	}
	if cm.Tier == "" {
		cm.Tier = "strong"
	}
	top, err := d.repoTop(ctx, m.Repo)
	if err != nil {
		return nil, err
	}
	pr, err := d.resolvePR(ctx, top, m.Review)
	if err != nil {
		return nil, err
	}
	r := d.store.Read(ctx)
	steps, err := d.relayStepsOf(r, actor.RunId, pr.URL)
	if err != nil {
		return nil, err
	}
	if err := openRelayStep(steps); err != nil {
		return nil, err
	}
	var done []*v1.Ask // the steps that counted: finished with an answer
	for _, a := range steps {
		if answered(a) > 0 {
			done = append(done, a)
		}
	}
	n := len(done)
	if n >= relaySteps {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"the relay is complete (%d steps): check the author's fix commits yourself (`git diff %s..<new head>`); no further review unless the user asks",
			relaySteps, short(done[n-1].Head)))
	}
	if n > 0 && done[n-1].Head == pr.Head {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
			"no commits since relay step %d reviewed %s: the author pushes the fixes for its findings first (when nothing is worth fixing, the relay is over)",
			n, short(pr.Head)))
	}
	job := &relayJob{step: n + 1}
	if n > 0 {
		job.of = done[0].Id
	}
	author := d.reviewAuthorKind(ctx, r, actor, top, pr.HeadRef)
	all, _, err := d.pickReviewers(r, "all", cm.Size, cm.Tier, author, nil, top)
	if err != nil {
		return nil, err
	}
	var kind string
	note := ""
	switch job.step {
	case 1:
		kind = firstOther(all, author)
	case 2:
		first := answeringKind(done[0])
		switch others := otherThan(all, first); {
		case len(others) > 0:
			kind = firstOther(others, author)
		case author != "":
			kind = author // no other family installed: the author's own CLI, in a fresh session
		default:
			kind = first
		}
		note = fmt.Sprintf("commits %s..%s fixed an earlier review; don't re-report what they fixed unless the fix is wrong", short(done[0].Head), short(pr.Head))
	default:
		kind = answeringKind(done[0])
		if !slices.Contains(all, kind) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
				"step 3 asks %s again, and it is not installed and free now (`mesh models`): wait for it, or review the fix commits yourself (`git diff %s..%s`)",
				kind, short(done[0].Head), short(pr.Head)))
		}
		job.reuse, job.since = done[0], done[0].Head
		note = fmt.Sprintf("commits %s..%s fixed an earlier review; don't re-report what they fixed unless the fix is wrong", short(done[0].Head), short(pr.Head))
	}
	if kind == "" {
		return nil, invalid("no CLI to review with: none is installed and free (`mesh models`)")
	}
	cm.Reviewers = kind
	if note != "" {
		if body != "" {
			body = note + "\n\n" + body
		} else {
			body = note
		}
	}
	cm.Body = body
	return d.reviewRoundOf(ctx, actor, cm, body, job)
}

// firstOther is the first kind of kinds that isn't `not`, else the first (an empty `not` takes the first).
func firstOther(kinds []string, not string) string {
	for _, k := range kinds {
		if k != not {
			return k
		}
	}
	if len(kinds) > 0 {
		return kinds[0]
	}
	return ""
}

func otherThan(kinds []string, not string) []string {
	var out []string
	for _, k := range kinds {
		if k != not {
			out = append(out, k)
		}
	}
	return out
}

// relayHint is the line under a relay step's result: what the root does next.
func relayHint(a *v1.Ask, finished bool) string {
	next := fmt.Sprintf("`mesh ask --repo %s --review %d --relay`", a.Repo, a.PrNumber)
	switch {
	case finished && answered(a) == 0:
		return fmt.Sprintf("(relay step %d didn't count: no reviewer answered; run %s again)", a.RelayStep, next)
	case a.RelayStep >= relaySteps:
		return fmt.Sprintf("(relay step %d of %d, its last: check the author's fix commits yourself, `git diff %s..<new head>`; no further review unless the user asks)",
			a.RelayStep, relaySteps, short(a.Head))
	}
	return fmt.Sprintf("(relay step %d of %d: the author fixes what you agree with and pushes; then run %s for step %d. Nothing worth fixing: the relay is over)",
		a.RelayStep, relaySteps, next, a.RelayStep+1)
}
