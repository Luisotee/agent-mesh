package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/copilot"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/opencode"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// The task texts of a question and a review, ported from bin/mesh (ASK_PREFIX, REVIEW_PREFIX and the REVIEW_*
// and CODE_REVIEW* texts). A task that starts with either prefix is read-only (isReadOnlyTask).
const askPrefix = "Question (read-only): answer it from the code. Change no files, make no commits, push nothing and send no plan; " +
	"reply with one `done` that holds the answer, or `failed`."

const reviewPrefix = "Review (read-only) of %s%s: review this pull request. Change no files, make no commits, push nothing, send no plan " +
	"and post nothing on GitHub. %s Reply with one `done` that lists your findings, most severe first, each with file:line, the concrete " +
	"failure (input or state, and what goes wrong), why it matters and a suggested fix. Mark each one **blocking** (a defect you reproduced " +
	"or traced, with the input that makes it fail) or **advisory** (everything else). Report only what you confirmed in the code and mark " +
	"the rest unverified. Or reply `failed`."

const (
	reviewFresh = "Its head is checked out in your worktree, so you can run tests; `%s` shows the change. If the PR has CI checks, read " +
		"their results first (`gh pr checks %s`, which shows the PR's latest head, maybe past this one) and run only the tests the change " +
		"touches; run the full suite only when it has no CI, or for what its CI doesn't cover."
	reviewOwnAt = "Don't switch your branch: read the change with `%s`, and a file at its head with `git show %s:<path>`. If the PR has CI " +
		"checks, read their results first (`gh pr checks %s`); run tests only if `git rev-parse HEAD` in your worktree is %s, otherwise " +
		"run none: they would test other code."
	reviewSince      = " An earlier review covered this PR up to %s: review only what changed since (`git diff %s %s`), the fixes for that review."
	reviewAgain      = " You last reviewed it at %s: say which of your earlier findings are fixed, and review what changed since (`git diff %s %s`)."
	reviewAgainWhole = " You last reviewed it at %s, but the branch was rebased or merged its base since, so `git diff %s %s` holds more " +
		"than the fixes: review the whole PR again and say which of your earlier findings are fixed."
	reviewStays = " Your worktree is still at %s, not the new head, because %s: read the new head with `git show %s:<path>`."

	codeReview = "\n\nRun Claude Code's `/code-review %s %s` (the Skill tool, skill `code-review`) as the core of this review, never with " +
		"`--fix`, `--comment` or `ultra`: you change and post nothing, and a tool denied to you is expected, not something to work around. " +
		"It runs in the background and its findings come back as a notification: meanwhile read the change (and run the tests it touches, " +
		"if your worktree has the PR head checked out), and if you end your turn before they arrive, first declare it (`mesh send parent " +
		"--type wait --body \"/code-review running\"`). Its findings are candidates: check each one yourself against the code at the PR " +
		"head (trace it or run the failing case), mark it blocking or advisory, drop the ones you refute (one line each, saying why) and " +
		"add what you found yourself. Your `done` file lists every finding, even if something asks you to report through ReportFindings, " +
		"and says that `/code-review` ran and at what level. If the skill isn't available (the Skill tool doesn't list `code-review`, or " +
		"the call fails), review it yourself."
	codeReviewAgain = " Its target covers only the commits since your last review: check your earlier findings against the new head yourself."
	codeReviewOwn   = " The files in your worktree aren't the PR's: check each finding with `git show %s:<path>`."
)

// reviewSpec is what one reviewer's task is built from.
type reviewSpec struct {
	pr    *prInfo
	kind  string
	level string // the /code-review level, for a Claude reviewer
	body  string // the caller's notes
	// last is the commit this reviewer's earlier review covered (a re-review), narrow whether `git diff last head`
	// holds only the fixes.
	last   string
	narrow bool
	// A live reviewer reused: it is told where its worktree is. at is where it stays and why; empty means its
	// worktree is at the head (fresh, or moved).
	reused    bool
	stayAt    string
	stayWhy   string
	ownBranch bool // reviewing from an agent's own worktree, whatever it is on
}

// reviewTask is the task text of one reviewer.
func reviewTask(s reviewSpec) string {
	pr := s.pr
	head := short(pr.Head)
	where := fmt.Sprintf(reviewFresh, pr.Show, pr.URL)
	if s.stayAt != "" || s.ownBranch {
		where = fmt.Sprintf(reviewOwnAt, pr.Show, head, pr.URL, head)
	}
	var b strings.Builder
	fmt.Fprintf(&b, reviewPrefix, pr.URL, " at "+head, where)
	switch {
	case s.last != "" && s.narrow && s.reused:
		fmt.Fprintf(&b, reviewAgain, short(s.last), short(s.last), head)
	case s.last != "" && s.narrow:
		fmt.Fprintf(&b, reviewSince, short(s.last), short(s.last), head)
	case s.last != "" && s.reused:
		fmt.Fprintf(&b, reviewAgainWhole, short(s.last), short(s.last), head)
	}
	if s.stayAt != "" {
		fmt.Fprintf(&b, reviewStays, short(s.stayAt), s.stayWhy, head)
	}
	if n := strings.TrimSpace(s.body); n != "" {
		b.WriteString("\n\nNotes from your parent:\n" + n)
	}
	if s.kind == "claude" {
		target := pr.URL
		if s.last != "" && s.narrow {
			target = short(s.last) + ".." + head
		}
		fmt.Fprintf(&b, codeReview, s.level, target)
		if s.last != "" && s.narrow {
			b.WriteString(codeReviewAgain)
		}
		if s.stayAt != "" || s.ownBranch {
			fmt.Fprintf(&b, codeReviewOwn, head)
		}
	}
	return withOpenCodeBodyNote(s.kind, b.String())
}

var nonWord = regexp.MustCompile(`[^a-z0-9]+`)

// askSlug names a fresh asker: "ask" and the words of the body, cut to 20 characters (bin/mesh's ask_slug).
func askSlug(body string) string {
	w := strings.Trim(nonWord.ReplaceAllString(strings.ToLower(body), "-"), "-")
	if w == "" {
		return "ask"
	}
	if s := "ask-" + w; len(s) > 20 {
		return strings.Trim(s[:20], "-")
	} else {
		return s
	}
}

// freeSlug is slug, or slug-2, slug-3 … when an agent of that name is already in the run.
func (d *Daemon) freeSlug(r store.Reader, runID, repoTop, slug string) (string, error) {
	base := filepath.Base(repoTop)
	for n := 1; n < 100; n++ {
		s := slug
		if n > 1 {
			s = fmt.Sprintf("%s-%d", slug, n)
		}
		same, err := r.AgentsNamed(runID, base+"-"+s)
		if err != nil {
			return "", err
		}
		if len(same) == 0 {
			return s, nil
		}
	}
	return "", fmt.Errorf("no free name for %s-%s", base, slug)
}

var sizes = map[string]string{"trivial": "small", "small": "small", "large": "large"}

// sizeKey is the settings key a size reads its slots and rounds from: a trivial PR has a small one's.
func sizeKey(size string) string { return sizes[size] }

func (d *Daemon) settingStrings(r store.Reader, key string) ([]string, error) {
	v, err := d.Setting(r, key)
	if err != nil {
		return nil, err
	}
	var out []string
	if l, ok := v.([]any); ok {
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out, nil
}

func (d *Daemon) settingString(r store.Reader, key string) (string, error) {
	v, err := d.Setting(r, key)
	s, _ := v.(string)
	return s, err
}

func (d *Daemon) settingInt(r store.Reader, key string) (int, error) {
	v, err := d.Setting(r, key)
	f, _ := v.(float64)
	return int(f), err
}

// installed reports whether a kind has an adapter here (and for OpenCode, a runnable binary).
func (d *Daemon) installed(kind string) bool {
	if _, err := d.rt.adapters.Get(kind); err != nil {
		return false
	}
	if kind == opencode.Kind || kind == copilot.Kind {
		_, ok := d.cliBinary(kind)
		return ok
	}
	return true
}

// kindAvailable is whether a kind with limits of its own can take a review slot now: OpenCode by its tier's models,
// Copilot by the kind-wide hold a 402 or a policy refusal leaves. Other kinds are always available.
func (d *Daemon) kindAvailable(r store.Reader, kind, tier, top, freeOff string) (bool, string) {
	switch kind {
	case opencode.Kind:
		return d.openCodeAvailable(r, tier, top, freeOff)
	case copilot.Kind:
		return d.copilotAvailable(r, tier)
	}
	return true, ""
}

// copilotAvailable is whether Copilot is installed, has the asked tier, and is not out of quota (or refused by
// policy): its holds are kind-wide (`*`), set by a 402 for a day or a 403 for an hour, until it next answers
// (copilot.go).
func (d *Daemon) copilotAvailable(r store.Reader, tier string) (bool, string) {
	if !d.installed(copilot.Kind) {
		return false, ""
	}
	if tier != "" {
		if _, ok := d.schema.Lookup("tiers." + copilot.Kind + "." + tier + ".model"); !ok {
			return false, fmt.Sprintf("copilot has no %s tier", tier)
		}
	}
	if held, _, err := r.ModelHold(copilot.Kind, holdAll, d.now()); err == nil && !held.IsZero() {
		return false, fmt.Sprintf("copilot is at its limit until %s", held.UTC().Format(time.RFC3339))
	}
	return true, ""
}

func (d *Daemon) openCodeAvailable(r store.Reader, tier, top, freeOff string) (bool, string) {
	if !d.installed(opencode.Kind) {
		return false, ""
	}
	now := d.now()
	if allHeld, _, err := r.ModelHold(opencode.Kind, holdAll, now); err == nil && !allHeld.IsZero() {
		return false, fmt.Sprintf("opencode is at its limit until %s", allHeld.UTC().Format(time.RFC3339))
	}
	if tier == "" {
		return true, ""
	}
	chain, _, err := d.tierChain(r, opencode.Kind, tier)
	if err != nil {
		return true, ""
	}
	chain = d.expandChain(opencode.Kind, chain)
	if len(chain) == 0 {
		return false, d.openCodeNoFreeError()
	}
	if freeOff != "" {
		kept, err := d.dropFree(top, freeOff, chain)
		if err != nil {
			return false, freeModelsNote(top)
		}
		chain = kept
	}
	var soonest time.Time
	for _, m := range chain {
		if m == opencode.FreeFamily {
			continue
		}
		if held, _, err := r.ModelHold(opencode.Kind, m, now); err == nil && !held.IsZero() {
			if soonest.IsZero() || held.Before(soonest) {
				soonest = held
			}
			continue
		}
		return true, ""
	}
	if !soonest.IsZero() {
		return false, fmt.Sprintf("all opencode %s models are held until %s", tier, soonest.UTC().Format(time.RFC3339))
	}
	return true, ""
}

// prAuthorKind is the CLI kind of the agent that wrote the PR: the newest agent on the host whose branch is
// the PR's head ref in this repo. A reviewer of another CLI is worth more, so it goes last in a slot's choices.
func prAuthorKind(r store.Reader, top, headRef string) string {
	if headRef == "" {
		return ""
	}
	agents, err := r.ListAgents("")
	if err != nil {
		return ""
	}
	for i := len(agents) - 1; i >= 0; i-- {
		if agents[i].Repo == top && agents[i].Branch == headRef && agents[i].Role == v1.AgentRole_AGENT_ROLE_WORKER {
			return agents[i].Kind
		}
	}
	return ""
}

// authorLast moves the PR author's kind later in a slot's chain: the author is removed and reinserted right
// after the slot's last non-OpenCode kind, so the order is the other CLIs, then the author's CLI in a fresh
// session, with OpenCode wherever the slot put it relative to them (first for a small PR, last for a large one).
func authorLast(chain []string, author string) []string {
	if author == "" {
		return chain
	}
	found := false
	out := make([]string, 0, len(chain))
	for _, k := range chain {
		if k == author {
			found = true
			continue
		}
		out = append(out, k)
	}
	if !found {
		return chain
	}
	at := len(out)
	last := -1
	for i, k := range out {
		if k != opencode.Kind {
			last = i
		}
	}
	if last >= 0 {
		at = last + 1
	}
	out = append(out, "")
	copy(out[at+1:], out[at:])
	out[at] = author
	return out
}

// pickReviewers chooses the kinds of a round (bin/mesh's pick_reviewers): "auto" fills the size's slots, each
// with its first installed kind not picked yet and the author's kind moved last; "all" every installed kind;
// otherwise a comma list. A re-review asks the kinds of the last round again, except those in
// `review.re_review.skip` (a size's `keep` list brings one back). It also returns what was left out, one
// line each.
func (d *Daemon) pickReviewers(r store.Reader, mode, size, tier, author string, earlier []string, top string) (kinds, notes []string, err error) {
	key := sizeKey(size)
	// A repo set against free models (freemodels.go) gets OpenCode only through a paid model of its tier.
	off := d.freeModelsOff(r, top)
	has := func(l []string, k string) bool {
		for _, x := range l {
			if x == k {
				return true
			}
		}
		return false
	}
	add := func(k string) {
		if !has(kinds, k) {
			kinds = append(kinds, k)
		}
	}
	fill := func(left []string) error {
		v, err := d.Setting(r, "review.reviewers."+key)
		if err != nil {
			return err
		}
		slots, _ := v.([]any)
		for i, sl := range slots {
			var chain []string
			for _, x := range sl.([]any) {
				chain = append(chain, x.(string))
			}
			chain = authorLast(chain, author)
			picked := false
			var missing, held []string
			for _, k := range chain {
				if !d.installed(k) {
					missing = append(missing, k)
					continue
				}
				if k == opencode.Kind || k == copilot.Kind {
					if ok, why := d.kindAvailable(r, k, tier, top, off); !ok {
						if why != "" {
							held = append(held, why)
						}
						continue
					}
				}
				if !has(kinds, k) && !has(left, k) {
					kinds, picked = append(kinds, k), true
					if len(missing) > 0 {
						notes = append(notes, fmt.Sprintf("%s is not installed here: slot %d takes %s", strings.Join(missing, ", "), i+1, k))
					}
					for _, h := range held {
						notes = append(notes, fmt.Sprintf("%s: slot %d takes %s", h, i+1, k))
					}
					break
				}
			}
			if !picked {
				notes = append(notes, held...)
				notes = append(notes, fmt.Sprintf("slot %d (%s): no kind of it is installed or free: no reviewer for it", i+1, strings.Join(chain, ", ")))
			}
		}
		return nil
	}
	switch {
	case mode == "all":
		for _, k := range d.rt.adapters.Kinds() {
			if !d.installed(k) {
				continue
			}
			if k == opencode.Kind || k == copilot.Kind {
				if ok, why := d.kindAvailable(r, k, tier, top, off); !ok {
					if why != "" {
						notes = append(notes, why+": not asked")
					}
					continue
				}
			}
			add(k)
		}
	case mode != "auto" && mode != "":
		for _, k := range strings.Split(mode, ",") {
			k = strings.TrimSpace(k)
			switch {
			case k == "":
			case !d.installed(k):
				notes = append(notes, fmt.Sprintf("%s is not installed here: not asked", k))
			case k == opencode.Kind || k == copilot.Kind:
				if ok, why := d.kindAvailable(r, k, tier, top, off); !ok {
					if why != "" {
						notes = append(notes, why+": not asked")
					}
					continue
				}
				add(k)
			default:
				add(k)
			}
		}
	case len(earlier) > 0:
		skip, err := d.settingStrings(r, "review.re_review.skip")
		if err != nil {
			return nil, nil, err
		}
		keep, err := d.settingStrings(r, "review.re_review.keep."+key)
		if err != nil {
			return nil, nil, err
		}
		for _, k := range earlier {
			switch {
			case has(skip, k) && !has(keep, k):
				notes = append(notes, fmt.Sprintf("%s reviewed it once and is not asked again (review.re_review.skip)", k))
			case !d.installed(k):
				notes = append(notes, fmt.Sprintf("%s is not installed here: not asked", k))
			case k == opencode.Kind || k == copilot.Kind:
				if ok, why := d.kindAvailable(r, k, tier, top, off); !ok {
					if why != "" {
						notes = append(notes, why+": not asked")
					}
					continue
				}
				add(k)
			default:
				add(k)
			}
		}
		// Nobody left of the earlier round (all skipped or gone): the slots fill again without the skipped kinds.
		if len(kinds) == 0 {
			var left []string
			for _, k := range skip {
				if !has(keep, k) {
					left = append(left, k)
				}
			}
			if err := fill(left); err != nil {
				return nil, nil, err
			}
		}
	default:
		if err := fill(nil); err != nil {
			return nil, nil, err
		}
	}
	if len(kinds) == 0 {
		return nil, notes, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("no reviewer to ask (%s)", strings.Join(notes, "; ")))
	}
	return kinds, notes, nil
}

// roundsOf is a PR's earlier rounds: the ones that finished with at least one review answered, oldest first.
func roundsOf(r store.Reader, pr string) ([]*v1.Ask, error) {
	all, err := r.PRAsks(pr)
	if err != nil {
		return nil, err
	}
	var out []*v1.Ask
	for _, a := range all {
		if a.Kind == v1.AskKind_ASK_KIND_REVIEW && a.RelayStep == 0 && a.Collected && a.Status == v1.AskStatus_ASK_STATUS_FINISHED && answered(a) > 0 {
			out = append(out, a)
		}
	}
	return out, nil
}

func answered(a *v1.Ask) int {
	n := 0
	for _, m := range a.Members {
		if m.Status == v1.AskMemberStatus_ASK_MEMBER_STATUS_ANSWERED {
			n++
		}
	}
	return n
}

// roundsHint is the line under a round's result: where it stands against `review.rounds.<size>`, a ceiling and
// not a quota.
func (d *Daemon) roundsHint(r store.Reader, a *v1.Ask, finished bool) string {
	if a.RelayStep > 0 {
		return relayHint(a, finished)
	}
	key := sizeKey(a.Size)
	if key == "" {
		return ""
	}
	max, err := d.settingInt(r, "review.rounds."+key)
	if err != nil {
		return ""
	}
	counted := a.Round
	limit := fmt.Sprintf("%d of at most %d for a %s PR (`review.rounds.%s`)", a.Round, max, a.Size, key)
	if callerIsWorker(r, a) {
		return d.workerHint(r, a, finished, max)
	}
	switch {
	case finished && answered(a) == 0:
		return fmt.Sprintf("(this round didn't count: no reviewer answered; run it again. Rounds so far: %d of at most %d for a %s PR "+
			"(`review.rounds.%s`))", counted-1, max, a.Size, key)
	case int(a.Round) > max:
		return fmt.Sprintf("(round %s, past the ceiling: the rounds of a %s PR are used up. Check any fix yourself, `git diff %s..<new head>`; "+
			"no re-review unless the user asks)", limit, a.Size, short(a.Head))
	case int(a.Round) == max:
		return fmt.Sprintf("(round %s, its last: check any fix yourself, `git diff %s..<new head>`; no re-review: findings that need no fix, "+
			"none you agree with or only advisory ones you decline end its reviews)", limit, short(a.Head))
	}
	return fmt.Sprintf("(round %s: findings that need no fix, none you agree with or only advisory ones you decline, end its reviews. "+
		"Only a fix you ask for gets a re-review: run this round again once it's pushed. Ask one reviewer again: `mesh ask <name> --body …`)", limit)
}

// workerHint is the line under a round the PR's author asked: it judges the findings itself ("PROTOCOL.md" rule 8),
// records its verdicts, fixes what it agrees with, and reports once; the root checks what it declined.
func (d *Daemon) workerHint(r store.Reader, a *v1.Ask, finished bool, max int) string {
	// What counts against the ceiling: the answered rounds (this one too, once it answered) and the single
	// reviewers the worker asked, which a round's own number does not show.
	used, err := workerRoundsUsed(r, a.CallerId, a.Pr, a.Id)
	if err != nil {
		used = int(a.Round) - 1
	}
	if !finished || answered(a) > 0 {
		used++
	}
	limit := fmt.Sprintf("%d of at most %d for a %s PR (`review.rounds.%s`)", used, max, a.Size, sizeKey(a.Size))
	verdict := "record `mesh verdict --pr " + a.Pr + " --reviewer <name> --agree <n> --other <n> --disagree <n> --blocking <n>` for each reviewer"
	switch {
	case finished && answered(a) == 0:
		return fmt.Sprintf("(this round didn't count: no reviewer answered; run it again. Rounds so far: %s)", limit)
	case used >= max:
		return fmt.Sprintf("(round %s, its last: judge each finding, fix what you agree with, push, %s, and report `done` with each finding fixed or declined with your reason "+
			"(declined ones first); no re-review: name the fix commits `git diff %s..<new head>` as not re-reviewed)", limit, verdict, short(a.Head))
	}
	return fmt.Sprintf("(round %s: judge each finding, fix what you agree with and push; %s. Findings that need no fix, or only advisory ones you decline, end the reviews: "+
		"report `done` with each declined one and your reason. After a fix, run this round again: its reviewers read only the fix commits)", limit, verdict)
}

// stamp is a file name's time: UTC, with microseconds, as bin/mesh's `%Y%m%dT%H%M%S%fZ`.
func stamp(t time.Time) string {
	t = t.UTC()
	return t.Format("20060102T150405") + fmt.Sprintf("%06d", t.Nanosecond()/1000) + "Z"
}

// answersPath is where a review's answers go, fixed when it starts.
func (d *Daemon) answersPath(runID string, prNumber int32, t time.Time) string {
	return filepath.Join(d.paths.RunDir(runID), "reports", fmt.Sprintf("review-%d-%s.md", prNumber, stamp(t)))
}

// writeAnswers writes a finished ask's answers into its file, in bin/mesh's format.
func writeAnswers(a *v1.Ask) error {
	var names []string
	for _, m := range a.Members {
		names = append(names, m.Name)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Review of %s at %s\n<!-- mesh: %d reviews -->\n\n", a.Pr, short(a.Head), answered(a))
	fmt.Fprintf(&b, "Asked %s in run %s, size %s, of %d reviewer(s): %s.\n", a.CreatedAt.AsTime().UTC().Format(time.RFC3339), a.RunId,
		a.Size, len(a.Members), strings.Join(names, ", "))
	if n := strings.TrimSpace(a.Body); n != "" {
		b.WriteString("\n## Notes to the reviewers\n\n" + n + "\n")
	}
	for _, m := range a.Members {
		fmt.Fprintf(&b, "\n## %s (%s)\n\n", m.Name, m.Kind)
		switch {
		case m.Status == v1.AskMemberStatus_ASK_MEMBER_STATUS_ANSWERED:
			b.WriteString(strings.TrimSpace(m.Text) + "\n")
		case strings.TrimSpace(m.Text) != "":
			fmt.Fprintf(&b, "**%s:** %s\n", m.Type, strings.TrimSpace(m.Text))
		default:
			fmt.Fprintf(&b, "No answer: %s\n", m.Line)
		}
	}
	if err := os.MkdirAll(filepath.Dir(a.AnswersPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(a.AnswersPath, []byte(b.String()), 0o600)
}

// summary is the one message the caller gets when a collected ask finishes.
func (d *Daemon) summary(r store.Reader, a *v1.Ask) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[mesh answers %s] %d of %d reviewers answered: %s at %s\n", a.Id, answered(a), len(a.Members), a.Pr, short(a.Head))
	for _, m := range a.Members {
		if m.Status == v1.AskMemberStatus_ASK_MEMBER_STATUS_ANSWERED {
			fmt.Fprintf(&b, "- %s (%s): answered\n", m.Name, m.Kind)
		} else {
			fmt.Fprintf(&b, "- %s (%s): no answer: %s\n", m.Name, m.Kind, m.Line)
		}
	}
	if a.AnswersPath != "" {
		fmt.Fprintf(&b, "answers: %s", a.AnswersPath)
	}
	if h := d.roundsHint(r, a, true); h != "" {
		b.WriteString("\n" + h)
	}
	return b.String()
}

// moveWorktree moves a reused reviewer's worktree to the new head when that is safe: its branch is clean, at
// the head it reviewed, and it is not working. Otherwise it stays, and why is returned.
func moveWorktree(ctx context.Context, a *v1.Agent, from, head string) (why string) {
	if a.State == v1.AgentState_AGENT_STATE_WORKING {
		return "it is still working"
	}
	cur, err := git(ctx, a.Worktree, "rev-parse", "HEAD")
	if err != nil {
		return "its worktree could not be read"
	}
	if from != "" && cur != from {
		return "its worktree is not at the commit it reviewed"
	}
	if st, err := git(ctx, a.Worktree, "status", "--porcelain"); err != nil || st != "" {
		return "its worktree has changes"
	}
	if _, err := git(ctx, a.Worktree, "checkout", "--quiet", "-B", a.Branch, head); err != nil {
		return "its worktree could not be moved"
	}
	return ""
}
