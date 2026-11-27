package daemon

// PR feedback to the worker that owns the PR. A review, a comment or a failing CI on a worker's open PR is told to
// that worker (an `info` from the daemon), never relayed by the root: the root's context stays out of the loop, and
// the worker, which knows the change, fixes it (PROTOCOL.md rule 8: findings are claims). A pass every
// `prs.watch_every` looks at the open PRs of the change workers of the open runs (one `gh pr view` and one inline
// comments read per open PR; GitHub is read for no one else) and is told of what is newer than the PR's cursor
// (PullRequest.feedback_cursor). The first look at a PR only sets the cursor, so a PR with old comments tells
// nothing. `prs.feedback` says what is done with it: off, notify (the worker; a retired worker's parent, one
// line, no text) or resume (a retired worker is reopened with the text as a direct task, at most
// feedbackResumeMax times per PR, past the usual resume guards; a refusal tells its parent as notify does).
//
// All agents share the user's GitHub account, so an agent's own comment looks like a person's. A comment from a
// bot, or one that carries an agent's attribution line, is skipped, as are the daemon's own: that, the cursor and
// the resume cap stop a worker answering its own comment in a loop.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

const (
	feedbackResumeMax = 3    // resumes of a retired worker per PR
	feedbackItemMax   = 10   // items told at once
	feedbackTextMax   = 600  // characters of one item's text
	feedbackBodyMax   = 4000 // bytes of the item lines
	prWatchCheckEvery = 5 * time.Second
)

// feedbackItem is one review, comment or failing check told to a worker.
type feedbackItem struct {
	at     time.Time
	author string
	where  string // "path:line" of an inline comment, "review (changes requested)", "" for a comment
	text   string
}

// ghFeedback is what `gh pr view --json headRefOid,statusCheckRollup,reviews,comments` says.
type ghFeedback struct {
	HeadRefOid string           `json:"headRefOid"`
	Rollup     []map[string]any `json:"statusCheckRollup"`
	Reviews    []struct {
		Author      struct{ Login string } `json:"author"`
		Assoc       string                 `json:"authorAssociation"`
		Body        string                 `json:"body"`
		State       string                 `json:"state"`
		SubmittedAt time.Time              `json:"submittedAt"`
	} `json:"reviews"`
	Comments []struct {
		Author    struct{ Login string } `json:"author"`
		Assoc     string                 `json:"authorAssociation"`
		Body      string                 `json:"body"`
		CreatedAt time.Time              `json:"createdAt"`
	} `json:"comments"`
}

// ghInline is one inline comment of `gh api repos/<o>/<r>/pulls/<n>/comments`.
type ghInline struct {
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	Assoc        string    `json:"author_association"`
	Body         string    `json:"body"`
	Path         string    `json:"path"`
	Line         int       `json:"line"`
	OriginalLine int       `json:"original_line"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

var prRepoRE = regexp.MustCompile(`^https://github\.com/([\w.-]+/[\w.-]+)/pull/(\d+)`)

// agentAuthored is a comment an agent (or a bot) wrote: not feedback for a worker. An agent's attribution is the
// last line of what it wrote, so a person quoting one mid-comment is still heard.
func agentAuthored(login, body string) bool {
	body = strings.TrimSpace(body)
	last := body[strings.LastIndex(body, "\n")+1:]
	return strings.HasSuffix(login, "[bot]") || strings.Contains(last, "Generated with [Claude Code]") ||
		strings.HasPrefix(strings.TrimSpace(last), "Co-Authored-By:") || strings.HasPrefix(body, "[mesh")
}

// trusted is who may give a worker work through a PR: the repo's owner, its members and collaborators. Anyone
// else (a drive-by commenter on a public repo, a first-time contributor) is skipped, and so is a comment whose
// association GitHub did not say: what a worker is told is acted on, and on `resume` unattended.
func trusted(assoc string) bool {
	switch strings.ToUpper(assoc) {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return true
	}
	return false
}

// clip cuts text to n characters, on a rune.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// feedbackSince is what the PR's reviews and comments hold that is newer than the cursor and is not an agent's: the
// items in time order, and the newest time seen of any (the cursor's next place, skipped ones included).
func feedbackSince(v ghFeedback, inline []ghInline, cursor time.Time) (items []feedbackItem, newest time.Time) {
	newest = cursor
	add := func(at time.Time, login, assoc, where, body string) {
		if at.IsZero() || !at.After(cursor) {
			return
		}
		if at.After(newest) {
			newest = at
		}
		if strings.TrimSpace(body) == "" || !trusted(assoc) || agentAuthored(login, body) {
			return
		}
		items = append(items, feedbackItem{at: at, author: login, where: where, text: body})
	}
	for _, rv := range v.Reviews {
		if strings.EqualFold(rv.State, "PENDING") {
			continue
		}
		where := "review"
		if s := strings.ToLower(strings.ReplaceAll(rv.State, "_", " ")); s != "" && s != "commented" {
			where += " (" + s + ")"
		}
		add(rv.SubmittedAt, rv.Author.Login, rv.Assoc, where, rv.Body)
	}
	for _, c := range v.Comments {
		add(c.CreatedAt, c.Author.Login, c.Assoc, "", c.Body)
	}
	for _, c := range inline {
		line := c.Line
		if line == 0 {
			line = c.OriginalLine
		}
		where := c.Path
		if line > 0 {
			where = fmt.Sprintf("%s:%d", c.Path, line)
		}
		at := c.CreatedAt // a draft's comment is created when drafted: its submit moves the update time
		if c.UpdatedAt.After(at) {
			at = c.UpdatedAt
		}
		add(at, c.User.Login, c.Assoc, where, c.Body)
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].at.Before(items[j].at) })
	return items, newest
}

// failingChecks names the failed checks of a rollup, with their conclusions and links.
func failingChecks(rollup []map[string]any) []string {
	str := func(c map[string]any, k string) string { s, _ := c[k].(string); return s }
	var out []string
	for _, c := range rollup {
		var name, outcome, link string
		if _, run := c["conclusion"]; run || c["status"] != nil {
			if !strings.EqualFold(str(c, "status"), "COMPLETED") {
				continue
			}
			name, outcome, link = str(c, "name"), str(c, "conclusion"), str(c, "detailsUrl")
		} else {
			name, outcome, link = str(c, "context"), str(c, "state"), str(c, "targetUrl")
		}
		if !failedOutcomes[strings.ToUpper(outcome)] {
			continue
		}
		t := name + " " + strings.ToLower(outcome)
		if link != "" {
			t += " " + link
		}
		out = append(out, t)
	}
	return out
}

// feedbackText is what a worker is told: each item, the failing checks, and what to do with them.
func feedbackText(p *v1.PullRequest, items []feedbackItem, checks []string, head string) (string, int) {
	included := len(items)
	var b strings.Builder
	fmt.Fprintf(&b, "[mesh pr-feedback #%d] %s has", p.Number, p.Url)
	var parts []string
	if len(items) > 0 {
		parts = append(parts, fmt.Sprintf("%d new review comment(s)", len(items)))
	}
	if len(checks) > 0 {
		parts = append(parts, "failing checks")
		if len(head) >= 7 {
			parts[len(parts)-1] += " on " + head[:7]
		}
	}
	b.WriteString(" " + strings.Join(parts, " and ") + ".\n")
	used := 0
	for i, it := range items {
		where := it.author
		if it.where != "" {
			where += ", " + it.where
		}
		line := "- " + where + ": " + strings.ReplaceAll(clip(it.text, feedbackTextMax), "\n", "\n  ") + "\n"
		if i >= feedbackItemMax || used+len(line) > feedbackBodyMax {
			fmt.Fprintf(&b, "- … %d more follow at the next look (or read the PR).\n", len(items)-i)
			included = i
			break
		}
		used += len(line)
		b.WriteString(line)
	}
	for _, c := range checks {
		b.WriteString("- CI: " + c + "\n")
	}
	b.WriteString("Treat each as a claim (PROTOCOL.md rule 8): check it against the code, fix what holds, push, " +
		"and say in your next report what you fixed and what you declined and why.")
	return b.String(), included
}

// prWatchEvery is the seconds between passes (`prs.watch_every`; 0 is off).
func (d *Daemon) prWatchEvery(ctx context.Context) time.Duration {
	v, err := d.Setting(d.store.Read(ctx), "prs.watch_every")
	f, _ := v.(float64)
	if err != nil || f <= 0 {
		return 0
	}
	return time.Duration(f * float64(time.Second))
}

// prFeedbackPass looks at the open PRs of the change workers of the open runs and tells their workers what is new.
func (d *Daemon) prFeedbackPass(ctx context.Context) {
	r := d.store.Read(ctx)
	mode, _ := d.settingString(r, "prs.feedback")
	if mode == "off" {
		return
	}
	runs, err := r.ListRuns(false)
	if err != nil || len(runs) == 0 {
		return
	}
	for _, run := range runs {
		agents, err := r.ListAgents(run.Id)
		if err != nil {
			continue
		}
		for _, a := range agents {
			if ctx.Err() != nil {
				return
			}
			if ownsPRs(a) {
				d.watchAgentPRs(ctx, a, mode)
			}
		}
	}
}

// watchAgentPRs refreshes an agent's PRs when one is open (or when it reported a done and none is known yet: its PR
// is not found until something reads), then looks at each open one.
func (d *Daemon) watchAgentPRs(ctx context.Context, a *v1.Agent, mode string) {
	r := d.store.Read(ctx)
	rows, err := r.AgentPRs(a.Id)
	if err != nil {
		return
	}
	open := false
	for _, p := range rows {
		open = open || p.State == "open"
	}
	if !open && len(rows) == 0 {
		if rep, err := lastReport(r, a); err != nil || rep == nil || rep.Type != v1.MessageType_MESSAGE_TYPE_DONE {
			return
		}
	} else if !open {
		return
	}
	d.refreshPRs(ctx, ctx, a, false)
	rows, err = d.store.Read(ctx).AgentPRs(a.Id)
	if err != nil {
		return
	}
	for _, p := range rows {
		if p.State == "open" && prRepoRE.MatchString(p.Url) {
			d.watchPR(ctx, a, p, mode)
		}
	}
}

// watchPR reads one open PR and tells what is new on it.
func (d *Daemon) watchPR(ctx context.Context, a *v1.Agent, p *v1.PullRequest, mode string) {
	m := prRepoRE.FindStringSubmatch(p.Url)
	out, err := d.runGH(ctx, ghDir(a), "pr", "view", p.Url, "--json", "headRefOid,statusCheckRollup,reviews,comments")
	if err != nil {
		return // tried again at the next pass; the cursor stays
	}
	var v ghFeedback
	if json.Unmarshal(out, &v) != nil {
		return
	}
	now := d.nowTime()
	failing := checksWord(v.Rollup) == "failing"
	if p.FeedbackCursor == nil { // first look: what is there already is not news
		d.saveCursor(ctx, a, p, now, ternary(failing, v.HeadRefOid, ""), 0, nil, "")
		return
	}
	out, err = d.runGH(ctx, ghDir(a), "api", "repos/"+m[1]+"/pulls/"+m[2]+"/comments?per_page=100&sort=created&direction=desc")
	if err != nil {
		return
	}
	var inline []ghInline
	if json.Unmarshal(out, &inline) != nil {
		return
	}
	cursor := p.FeedbackCursor.AsTime()
	items, newest := feedbackSince(v, inline, cursor)
	var checks []string
	if failing && v.HeadRefOid != "" && v.HeadRefOid != p.FailingHead {
		checks = failingChecks(v.Rollup)
	}
	failedHead := p.FailingHead
	if !failing {
		failedHead = "" // a later failure of the same commit (a rerun) is news again
	} else if checks != nil {
		failedHead = v.HeadRefOid
	}
	if len(items) == 0 && len(checks) == 0 {
		if !newest.Equal(cursor) || failedHead != p.FailingHead {
			d.saveCursor(ctx, a, p, newest, failedHead, 0, nil, "")
		}
		return
	}
	body, included := feedbackText(p, items, checks, v.HeadRefOid)
	if included < len(items) { // the rest is told at the next look: the cursor stops at the last one told
		newest = items[included-1].at
	}
	d.tellFeedback(ctx, a, p, mode, body, included+len(checks), newest, failedHead)
}

func ternary(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

// saveCursor moves a PR's cursor (and the head whose failure was told, and its resume count by `resumed`) and, in
// the same write, tells the one who is told: a message to the worker, or a line to its parent.
func (d *Daemon) saveCursor(ctx context.Context, a *v1.Agent, p *v1.PullRequest, at time.Time, failingHead string, resumed int32,
	toWorker *string, toParent string) {
	var ring []string
	err := d.store.Write(ctx, meshActor, "", func(tx *store.Tx) error {
		cur, err := tx.AgentPRs(a.Id)
		if err != nil {
			return err
		}
		var row *v1.PullRequest
		for _, c := range cur {
			if prKey(c.Url) == prKey(p.Url) {
				row = c
			}
		}
		if row == nil {
			return nil // its row went (a branch rewritten): nothing to move
		}
		row.FeedbackCursor, row.FailingHead = timestamppb.New(at), failingHead
		row.FeedbackResumes += resumed
		if err := tx.PutAgentPR(row); err != nil {
			return err
		}
		if toWorker != nil {
			if live, err := tx.GetAgent(a.Id); err == nil && !stopped(live) {
				if _, err := tellTx(tx, live, *toWorker); err != nil {
					return err
				}
				ring = append(ring, live.Id)
				toParent = "" // it was told itself
			}
		}
		if toParent != "" { // a worker retired since it was looked at is told through its parent
			if live, err := tx.GetAgent(a.Id); err == nil {
				if m, err := d.noteParentTx(tx, live, "pr-feedback", toParent); err != nil {
					return err
				} else if m != nil {
					ring = append(ring, m.ToId)
				}
			}
		}
		return nil
	})
	if err != nil {
		if ctx.Err() == nil {
			d.log.Warn("pr feedback", "agent", a.Name, "err", err)
		}
		return
	}
	for _, id := range ring {
		d.deliverSoon(id)
	}
}

// tellFeedback delivers what is new: to a live worker as an info (it rings when the worker is idle); for a retired
// one by `prs.feedback`: its parent is told in one line (notify), or the worker is reopened with the text (resume).
func (d *Daemon) tellFeedback(ctx context.Context, a *v1.Agent, p *v1.PullRequest, mode, body string, n int, newest time.Time, failedHead string) {
	live, err := d.getAgent(d.store.Read(ctx), a.Id)
	if err != nil {
		return
	}
	line := fmt.Sprintf("PR #%d has %d new review item(s) or failing checks; %s is retired: `mesh resume %s` to have it act on them.",
		p.Number, n, a.Name, a.Name)
	if !stopped(live) {
		d.saveCursor(ctx, a, p, newest, failedHead, 0, &body, line) // the line only if it retires before the write
		return
	}
	if mode == "resume" {
		if p.FeedbackResumes >= feedbackResumeMax {
			line = fmt.Sprintf("PR #%d has new feedback; %s was resumed for this PR %d times already: `mesh resume %s` yourself.",
				p.Number, a.Name, feedbackResumeMax, a.Name)
		} else {
			mid := fmt.Sprintf("prfb:%s:%s:%s:%s", a.Id, prKey(p.Url), newest.UTC().Format(time.RFC3339Nano), failedHead)
			res, err := d.SendMessage(auth.WithActor(ctx, meshActor), connect.NewRequest(&v1.SendMessageRequest{
				Meta: &v1.RequestMeta{MutationId: mid}, To: a.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK,
				Body: "Direct task (no plan)\n\n" + body}))
			switch {
			case err == nil && res.Msg.Resumed != nil:
				d.saveCursor(ctx, a, p, newest, failedHead, 1, nil, "")
				return
			case err == nil:
				line += " (It could not be resumed here; the feedback waits in its inbox.)"
			default:
				line += " (Resuming it was refused: " + clip(err.Error(), 200) + ")"
			}
		}
	}
	d.saveCursor(ctx, a, p, newest, failedHead, 0, nil, line)
}
