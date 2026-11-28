package daemon

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// What an agent is for (`Agent.about`): one short line shown beside its name, so the user who comes back to a run can
// tell who does what. The parent says it (`--about` on a spawn, a task or a resume, or `mesh about`); where nobody did,
// the daemon derives one, and keeps where it came from (`about_source`) so a derived line never replaces a set one.
const (
	maxAbout       = 80  // characters of an about line
	maxHeadline    = 400 // characters of a run's headline
	maxDecision    = 400 // characters of a decision
	aboutSet       = "set"
	aboutPlan      = "plan"
	aboutTask      = "task"
	aboutPeer      = "peer"
	aboutRun       = "run"
	overviewDecide = 20 // decisions in an overview
)

// squeeze is a text on one line, its white space collapsed.
func squeeze(s string) string { return strings.Join(strings.Fields(s), " ") }

// capLine is a text on one line cut to limit characters (the last one an ellipsis when it was cut).
func capLine(s string, limit int) string {
	s = squeeze(s)
	if r := []rune(s); len(r) > limit {
		return strings.TrimSpace(string(r[:limit-1])) + "…"
	}
	return s
}

// capAbout is an about line: on one line, at most maxAbout characters.
func capAbout(s string) string { return capLine(s, maxAbout) }

// markdownNoise is what a heading or a first line carries that a one-line description does not: emphasis marks, a
// list bullet or a `Task:` label.
var markdownNoise = regexp.MustCompile("^(?:[#>]+\\s*)+|^[-*+]\\s+|\\*\\*|`")

// directMarker is the protocol's marker a task may start with.
var directMarker = regexp.MustCompile(`(?i)^(?:(?:direct task(?: \(no plan\)|\b)|question \(read-only\)|review \(read-only\))\s*[:.-]?\s*|task\s*[:.-]\s*)`)

// headingOf is the text of a Markdown heading line, "" when the line is none.
func headingOf(line string) string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "#") {
		return ""
	}
	return strings.TrimSpace(strings.TrimLeft(line, "# "))
}

// cleanLine is a line without its Markdown noise: heading marks, a bullet, emphasis and code marks.
func cleanLine(ln string) string {
	return squeeze(markdownNoise.ReplaceAllString(strings.TrimSpace(ln), ""))
}

// taskAbout is a task's own description of itself: its first line that says something, with the protocol's
// `Direct task` marker and Markdown noise taken off (a marker alone on a heading line leaves the next line).
func taskAbout(body string) string {
	for _, ln := range strings.Split(body, "\n") {
		if t := squeeze(directMarker.ReplaceAllString(cleanLine(ln), "")); strings.ContainsFunc(t, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
			return capAbout(t)
		}
	}
	return ""
}

// planPrefix is the template's `Plan:` label.
var planPrefix = regexp.MustCompile(`(?i)^plan\s*[:.-]\s*`)

// planAbout is a plan's title: its first heading without the template's `Plan:` prefix.
func planAbout(body string) string {
	for i, ln := range strings.Split(body, "\n") {
		if i >= 10 {
			break
		}
		if h := headingOf(ln); h != "" {
			return capAbout(planPrefix.ReplaceAllString(cleanLine(h), ""))
		}
	}
	return ""
}

// setAbout puts an about line on an agent in a transaction (a no-op when it already reads so), with the event.
func setAbout(t *store.Tx, a *v1.Agent, about, source string) error {
	about = capAbout(about)
	if a.About == about && a.AboutSource == source {
		return nil
	}
	prev := a.About
	a.About, a.AboutSource = about, source
	if err := t.PutAgent(a); err != nil {
		return err
	}
	_, err := t.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_AgentAboutChanged{
		AgentAboutChanged: &v1.AgentAboutChanged{About: about, Source: source, Previous: prev}}})
	return err
}

// deriveAbout is what an agent's about line becomes where nobody set one: a worker's plan title is better than its
// task's first line, so a plan replaces a task-derived line but never a set one.
func deriveAbout(t *store.Tx, a *v1.Agent, about, source string) error {
	if about == "" || a.AboutSource == aboutSet {
		return nil
	}
	if a.About != "" && source == aboutTask {
		return nil // the first task describes the job: a follow-up's first line (`fix the review findings`) does not
	}
	return setAbout(t, a, about, source)
}

// taskAboutOn is a task's effect on the about line of the agent it is for: an explicit `about` sets it; a task without
// one keeps what the agent has (and says which: the returned note) or, for an agent with none, derives one.
func taskAboutOn(t *store.Tx, a *v1.Agent, explicit, body string) (string, error) {
	switch explicit = capAbout(explicit); {
	case explicit != "":
		return "", setAbout(t, a, explicit, aboutSet)
	case a.About == "":
		return "", deriveAbout(t, a, taskAbout(body), aboutTask)
	case a.Role == v1.AgentRole_AGENT_ROLE_WORKER || a.Role == v1.AgentRole_AGENT_ROLE_ROOT:
		return fmt.Sprintf("about kept: %q (change it with --about on a task, or `mesh about`)", a.About), nil
	}
	return "", nil
}

// noteAbout is the about line's part of storing a message: a task goes through taskAboutOn, and a plan sent to a
// parent makes its title the author's line.
func (d *Daemon) noteAbout(t *store.Tx, toID string, msg *v1.Message, explicit string) (string, error) {
	switch msg.Type {
	case v1.MessageType_MESSAGE_TYPE_TASK:
		a, err := t.GetAgent(toID)
		if err != nil {
			return "", err
		}
		return taskAboutOn(t, a, explicit, msg.Body)
	case v1.MessageType_MESSAGE_TYPE_PLAN:
		if id := msg.From.GetId(); id != "" {
			if a, err := t.GetAgent(id); err == nil {
				return "", deriveAbout(t, a, planAbout(msg.Body), aboutPlan)
			}
		}
	}
	return "", nil
}

// SetAgentAbout says what an agent is for. A root may set only an agent of its own run, a device any.
func (d *Daemon) SetAgentAbout(ctx context.Context, req *connect.Request[v1.SetAgentAboutRequest]) (*connect.Response[v1.SetAgentAboutResponse], error) {
	actor := auth.ActorFrom(ctx)
	a, err := d.FindAgent(d.store.Read(ctx), actor, req.Msg.Agent)
	if err != nil {
		return nil, err
	}
	out := &v1.SetAgentAboutResponse{}
	_, err = d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceSetAgentAboutProcedure, out, func(t *store.Tx) error {
		cur, err := t.GetAgent(a.Id)
		if errors.Is(err, store.ErrNotFound) {
			return notFound("no agent %q", req.Msg.Agent)
		}
		if err != nil {
			return err
		}
		if about := capAbout(req.Msg.Text); about != "" {
			err = setAbout(t, cur, about, aboutSet)
		} else {
			// Cleared: the derived line comes back from the agent's plan or first task.
			cur.AboutSource = ""
			err = setAbout(t, cur, d.derivedAbout(t.Reader, cur), d.derivedSource(t.Reader, cur))
		}
		out.Agent = cur
		return err
	})
	return connect.NewResponse(out), err
}

// derivedAbout and derivedSource are the line an agent has with none set: its plan's title, else its first task's
// heading or line, else the run's title for a lead.
func (d *Daemon) derivedAbout(r store.Reader, a *v1.Agent) string {
	if a.Role == v1.AgentRole_AGENT_ROLE_ROOT {
		if run, err := r.GetRun(a.RunId); err == nil {
			return capAbout(run.Title)
		}
		return ""
	}
	if a.Peer { // a peer's line is the one its own record carries
		if p, err := r.GetPeer(a.Name); err == nil {
			return capAbout(p.About)
		}
		return ""
	}
	msgs, err := r.RunMessages(a.RunId, "", -1)
	if err != nil {
		return ""
	}
	var plan, task string
	for _, m := range msgs {
		switch {
		case m.From.GetId() == a.Id && m.Type == v1.MessageType_MESSAGE_TYPE_PLAN:
			plan = planAbout(m.Body)
		case m.ToId == a.Id && m.Type == v1.MessageType_MESSAGE_TYPE_TASK && task == "":
			task = taskAbout(m.Body)
		}
	}
	if plan != "" {
		return plan
	}
	return task
}

func (d *Daemon) derivedSource(r store.Reader, a *v1.Agent) string {
	switch {
	case a.Role == v1.AgentRole_AGENT_ROLE_ROOT:
		return aboutRun
	case a.Peer:
		return aboutPeer
	}
	msgs, err := r.RunMessages(a.RunId, "", -1)
	if err != nil {
		return aboutTask
	}
	for _, m := range msgs {
		if m.From.GetId() == a.Id && m.Type == v1.MessageType_MESSAGE_TYPE_PLAN && planAbout(m.Body) != "" {
			return aboutPlan
		}
	}
	return aboutTask
}

// SetRunHeadline writes the run's headline: where it is and what comes next, for the user who comes back to it.
func (d *Daemon) SetRunHeadline(ctx context.Context, req *connect.Request[v1.SetRunHeadlineRequest]) (*connect.Response[v1.SetRunHeadlineResponse], error) {
	runID, err := scopedRun(auth.ActorFrom(ctx), req.Msg.RunId)
	if err != nil {
		return nil, err
	}
	if runID == "" {
		return nil, invalid("name the run (run_id)")
	}
	text := strings.TrimSpace(req.Msg.Text)
	if text == "" {
		return nil, invalid("write the headline (one to three lines: where the run is, what comes next)")
	}
	text = cutLines(text, 3, maxHeadline)
	out := &v1.SetRunHeadlineResponse{}
	_, err = d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceSetRunHeadlineProcedure, out, func(t *store.Tx) error {
		run, err := t.GetRun(runID)
		if errors.Is(err, store.ErrNotFound) {
			return notFound("no run %q", runID)
		}
		if err != nil {
			return err
		}
		out.Run = run
		prev := run.Headline
		run.Headline, run.HeadlineAt = text, tsOf(t.Now())
		// The event is the run's newest when it is appended: events after it are what came since.
		ev, err := t.AppendEvent(&v1.Event{RunId: runID, Payload: &v1.Event_RunHeadlineChanged{
			RunHeadlineChanged: &v1.RunHeadlineChanged{Headline: text, Previous: prev}}})
		if err != nil {
			return err
		}
		run.HeadlineSeq = ev.Seq
		return t.UpdateRun(run)
	})
	return connect.NewResponse(out), err
}

// cutLines keeps a text's first n lines and cuts it to limit characters.
func cutLines(s string, n, limit int) string {
	var lines []string
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			lines = append(lines, ln)
		}
		if len(lines) == n {
			break
		}
	}
	s = strings.Join(lines, "\n")
	if r := []rune(s); len(r) > limit {
		return strings.TrimSpace(string(r[:limit-1])) + "…"
	}
	return s
}

// RecordDecision keeps a decision the user may want to revisit.
func (d *Daemon) RecordDecision(ctx context.Context, req *connect.Request[v1.RecordDecisionRequest]) (*connect.Response[v1.RecordDecisionResponse], error) {
	runID, err := scopedRun(auth.ActorFrom(ctx), req.Msg.RunId)
	if err != nil {
		return nil, err
	}
	if runID == "" {
		return nil, invalid("name the run (run_id)")
	}
	text := cutLines(req.Msg.Text, 3, maxDecision)
	if text == "" {
		return nil, invalid("write the decision: what was decided, and why")
	}
	out := &v1.RecordDecisionResponse{}
	_, err = d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceRecordDecisionProcedure, out, func(t *store.Tx) error {
		if _, err := t.GetRun(runID); errors.Is(err, store.ErrNotFound) {
			return notFound("no run %q", runID)
		} else if err != nil {
			return err
		}
		dec, err := t.PutDecision(runID, &v1.RunDecision{By: t.Actor, Text: text})
		if err != nil {
			return err
		}
		out.Decision = dec
		_, err = t.AppendEvent(&v1.Event{RunId: runID, Payload: &v1.Event_DecisionRecorded{
			DecisionRecorded: &v1.DecisionRecorded{Decision: dec}}})
		return err
	})
	return connect.NewResponse(out), err
}

// spawnAbout is the about line a new agent starts with: the one it was given, else a lead's run title, else the
// task's own description.
func spawnAbout(m *v1.SpawnAgentRequest, role v1.AgentRole, run *v1.Run) (string, string) {
	if about := capAbout(m.About); about != "" {
		return about, aboutSet
	}
	if role == v1.AgentRole_AGENT_ROLE_ROOT {
		if run != nil && run.Title != "" {
			return capAbout(run.Title), aboutRun
		}
		return "", ""
	}
	if about := taskAbout(m.Task); about != "" {
		return about, aboutTask
	}
	return "", ""
}

// askAbout is what a question asker is for: the question's first line.
func askAbout(body string) string {
	if t := taskAbout(body); t != "" {
		return capAbout("Question: " + t)
	}
	return ""
}

// reviewAbout is what a reviewer is for: the PR it reviews, and that it is a later round when it narrows to what
// changed since one.
func reviewAbout(pr *prInfo, since string) string {
	if since != "" {
		return fmt.Sprintf("Review of #%d, a later round", pr.Number)
	}
	return fmt.Sprintf("Review of #%d", pr.Number)
}
