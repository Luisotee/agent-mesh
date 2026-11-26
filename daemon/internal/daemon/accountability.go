package daemon

// Accountability: what `bin/mesh watch` did with exit codes, done by the daemon (docs/apps.md, "Switchover").
// A parent is told, by an `info` from the daemon, when a worker of its own exits, is lost, goes quiet (unknown)
// or stops at a usage limit. A worker that ends a turn with no report since its task is nudged once; if its next
// turn ends the same way, its parent is told. A declared `wait` pauses that, for at most `accountability.wait_max`,
// or until the agent wakes; a wait declared after a nudge starts the count again.
// A report is refused while a task or answer is unread (SendMessage). Nothing here reopens or switches an agent:
// acting on a limit is the limits pass's job, and this only tells the parent.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// meshActor is who the messages the daemon writes are from. It has no class: it is no one an action can be
// called as, only a name in the inbox and the event log.
var meshActor = &v1.Actor{Id: "meshd", Name: "mesh"}

// The first words of the daemon's messages, which the pass finds its own by (a nudge, and one alert per kind).
const (
	nudgeTag  = "[mesh nudge]"
	alertTag  = "[mesh alert "
	alertEnds = "]"
)

func alertHead(kind, agentID string) string { return alertTag + kind + " " + agentID + alertEnds }

// accountEvery is how often the pass runs by default.
const accountEveryDefault = 5 * time.Second

// owesParent is an agent that owes its parent reports: any agent with a parent (a root has none).
func owesParent(a *v1.Agent) bool {
	return a.ParentId != "" && a.Role != v1.AgentRole_AGENT_ROLE_ROOT
}

// tellTx stores a message from the daemon to an agent, with its event.
func tellTx(tx *store.Tx, to *v1.Agent, body string) (*v1.Message, error) {
	m := &v1.Message{Id: ids.New("m", 10), RunId: to.RunId, From: meshActor, ToId: to.Id, ToName: to.Name,
		Type: v1.MessageType_MESSAGE_TYPE_INFO, Body: body, CreatedAt: tsOf(tx.Now())}
	if err := tx.PutMessage(m); err != nil {
		return nil, err
	}
	_, err := tx.AppendEvent(&v1.Event{RunId: to.RunId, AgentId: to.Id,
		Payload: &v1.Event_MessageSent{MessageSent: &v1.MessageSent{Message: m}}})
	return m, err
}

// alertParentTx tells an agent's parent something about it (an `info`), once per (kind, agent, since): a
// message already sent after `since` says it. It returns the message to ring, or nil.
func (d *Daemon) alertParentTx(tx *store.Tx, a *v1.Agent, kind, text string, since time.Time) (*v1.Message, error) {
	// A member of a round its parent asked for: the alert settles it, and the parent is rung once, at the end.
	if ring, held, err := d.alertAsMember(tx, a, kind, text); held || err != nil {
		return ring, err
	}
	parent, err := tx.GetAgent(a.ParentId)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil // its parent is gone from the store: no one to tell
		}
		return nil, err
	}
	head := alertHead(kind, a.Id)
	if told, err := toldParent(tx.Reader, a, parent.Id, head, since); told || err != nil {
		return nil, err
	}
	return tellTx(tx, parent, head+" "+text)
}

// toldParent reports whether the daemon already sent the parent this alert (same head) at or after `since`.
func toldParent(r store.Reader, a *v1.Agent, parentID, head string, since time.Time) (bool, error) {
	msgs, err := r.RunMessages(a.RunId, "", -1)
	if err != nil {
		return false, err
	}
	for _, m := range msgs {
		if m.ToId == parentID && m.From.GetId() == meshActor.Id && strings.HasPrefix(m.Body, head) &&
			!m.CreatedAt.AsTime().Before(since) {
			return true, nil
		}
	}
	return false, nil
}

// noteParentTx tells an agent's parent what the daemon did about it (an `info`), whatever else was told: unlike an
// alert it settles nothing in a round the agent is a member of.
func (d *Daemon) noteParentTx(tx *store.Tx, a *v1.Agent, kind, text string) (*v1.Message, error) {
	if !owesParent(a) {
		return nil, nil
	}
	parent, err := tx.GetAgent(a.ParentId)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return tellTx(tx, parent, alertHead(kind, a.Id)+" "+text)
}

// owed is what the accountability pass finds about an agent at its prompt.
type owed int

const (
	owesNothing owed = iota
	owesNudge        // ended a turn with no report: nudge it
	owesAlert        // nudged, and ended another turn the same way: tell its parent
)

// reportOwed judges an agent at its prompt: does it owe a report, and has the nudge been given? It reads only
// the store and the clock, so the pass and the write that acts on it agree. It returns the time the agent's
// task or answer came, which dedupes an alert to its parent.
func (d *Daemon) reportOwed(r store.Reader, a *v1.Agent, now time.Time, nudgeAfter, waitMax time.Duration) (owed, time.Time, error) {
	// An agent that asked for its own tools is idle on purpose until the daemon reopens it, for as long as a wait
	// may last; any other pending reopen (a switch, an approval, a limit) is no reason to leave it unwatched.
	if p := a.PendingReopen; p != nil && p.Why == toolsAddedWhy && now.Sub(p.RequestedAt.AsTime()) < waitMax {
		return owesNothing, time.Time{}, nil
	}
	msgs, err := r.RunMessages(a.RunId, "", -1)
	if err != nil {
		return owesNothing, time.Time{}, err
	}
	// Everything since the agent's last task or answer decides: a report, a declared wait, the nudge.
	var task time.Time
	var reported, nudged bool
	var wait, nudgedAt time.Time
	for _, m := range msgs {
		switch {
		case m.ToId == a.Id && (m.Type == v1.MessageType_MESSAGE_TYPE_TASK || m.Type == v1.MessageType_MESSAGE_TYPE_ANSWER):
			task, reported, nudged, wait, nudgedAt = m.CreatedAt.AsTime(), false, false, time.Time{}, time.Time{}
		case m.From.GetId() == a.Id && m.ToId == a.ParentId && isReport(m.Type):
			reported = true
		case m.From.GetId() == a.Id && m.ToId == a.ParentId && m.Type == v1.MessageType_MESSAGE_TYPE_WAIT:
			// A wait answers an earlier nudge: its next silent wake is a first one again, nudged, not alerted (an
			// agent that waits on background jobs ends a turn at each job's notification, each time still waiting).
			wait, nudged, nudgedAt = m.CreatedAt.AsTime(), false, time.Time{}
		case m.ToId == a.Id && m.From.GetId() == meshActor.Id && strings.HasPrefix(m.Body, nudgeTag):
			nudged, nudgedAt = true, m.CreatedAt.AsTime()
		}
	}
	if task.IsZero() || reported {
		return owesNothing, task, nil
	}
	// Unread mail other than the daemon's own nudge: work is coming, or a doorbell was lost.
	unread, err := r.Inbox(a.Id, false)
	if err != nil {
		return owesNothing, task, err
	}
	for _, m := range unread {
		if m.From.GetId() != meshActor.Id {
			return owesNothing, task, nil
		}
	}
	// A declared wait: the agent said it is waiting on purpose, until it is over: its deadline, or the agent woke
	// (was active again after the turn end that declared it) and ended a turn with no report (bin/mesh's wait_over).
	if !wait.IsZero() && now.Sub(wait) < waitMax && !d.waits.woke(a, wait) {
		return owesNothing, task, nil
	}
	idleSince := task
	if a.LastActivity != nil && a.LastActivity.AsTime().After(idleSince) {
		idleSince = a.LastActivity.AsTime()
	}
	if nudged && nudgedAt.After(idleSince) {
		idleSince = nudgedAt
	}
	if now.Sub(idleSince) < nudgeAfter {
		return owesNothing, task, nil
	}
	if nudged {
		// Told only when the nudge had its turn and that turn ended the same way: an agent whose doorbell is held
		// back (a user typing, a state that takes none) has not been given the chance to report yet.
		if a.LastActivity == nil || !a.LastActivity.AsTime().After(nudgedAt) {
			return owesNothing, task, nil
		}
		return owesAlert, task, nil
	}
	return owesNudge, task, nil
}

// waitTracker tells a wait that is over because its agent woke. An agent that declares a wait ends its turn, and
// its turn end is the last activity the pass sees at its prompt; activity after that means something woke it (a
// finished job, a message), and a turn that ends then with no report is judged as any silent stop (bin/mesh's
// wait_over). The first reading of a wait is its baseline, so a wake before the daemon restarted is missed and the
// wait then holds until its deadline.
type waitTracker struct {
	mu   sync.Mutex
	seen map[string]waitSeen
}

type waitSeen struct {
	wait, activity time.Time
	woke           bool
}

func (t *waitTracker) woke(a *v1.Agent, wait time.Time) bool {
	var active time.Time
	if a.LastActivity != nil {
		active = a.LastActivity.AsTime()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seen == nil {
		t.seen = map[string]waitSeen{}
	}
	w, ok := t.seen[a.Id]
	if !ok || !w.wait.Equal(wait) {
		t.seen[a.Id] = waitSeen{wait: wait, activity: active}
		return false
	}
	if !w.woke && active.After(w.activity) {
		w.woke = true
		t.seen[a.Id] = w
	}
	return w.woke
}

// accountability is the periodic pass: for each agent that has a parent, its state says what to tell whom.
func (d *Daemon) accountability(ctx context.Context) {
	d.settleOverdueAsks(ctx)
	r := d.store.Read(ctx)
	nudgeAfter, err := d.Setting(r, "accountability.nudge_after")
	if err != nil {
		return
	}
	waitMaxV, err := d.Setting(r, "accountability.wait_max")
	if err != nil {
		return
	}
	nudge := time.Duration(nudgeAfter.(float64) * float64(time.Second))
	waitMax := time.Duration(waitMaxV.(float64) * float64(time.Second))
	agents, err := r.ListAgents("")
	if err != nil {
		return
	}
	inputAfter := time.Duration(0)
	if v, err := d.Setting(r, "accountability.input_after"); err == nil {
		inputAfter = time.Duration(v.(float64) * float64(time.Second))
	}
	for _, a := range agents {
		if !owesParent(a) || ctx.Err() != nil {
			continue
		}
		shown := d.view(a)
		var err error
		switch {
		case shown.State == v1.AgentState_AGENT_STATE_UNKNOWN:
			err = d.alertOnce(ctx, a, "unknown", func(ctx context.Context) string {
				return fmt.Sprintf("%s has gone quiet: nothing has been heard from it for %s (state %s). "+
					"It may be hung or waiting at a prompt; look at its terminal.", a.Name, d.rt.cfg.UnknownAfter.Round(time.Minute),
					strings.ToLower(strings.TrimPrefix(a.State.String(), "AGENT_STATE_"))) + d.afterwards(ctx, a)
			})
		case a.State == v1.AgentState_AGENT_STATE_LIMITED:
			err = d.alertOnce(ctx, a, "limited", func(ctx context.Context) string { return d.limitedText(ctx, a) })
		case a.State == v1.AgentState_AGENT_STATE_NEEDS_INPUT:
			if inputAfter > 0 && a.LastActivity != nil && d.now().Sub(a.LastActivity.AsTime()) >= inputAfter {
				err = d.alertOnce(ctx, a, "input", func(ctx context.Context) string {
					return fmt.Sprintf("%s is waiting at a prompt for a person (%s). It can't go on until it is answered: "+
						"tell the user, who answers it in its terminal (the app lists it as waiting).", a.Name, orDash(a.StateDetail))
				})
			}
		default:
			skipNudge := false
			if a.State == v1.AgentState_AGENT_STATE_ERROR {
				if rep, rerr := lastReport(r, a); rerr == nil && rep == nil {
					login := authError(a.StateDetail)
					skipNudge = login // a turn that fails on a dead login fails again on a nudge
					err = d.alertOnce(ctx, a, "error", func(ctx context.Context) string { return errorText(a, login) + d.afterwards(ctx, a) })
				}
			}
			if err == nil && !skipNudge && atPrompt(a.State) && nudge > 0 {
				err = d.checkOwed(ctx, a, nudge, waitMax)
			}
		}
		if err != nil && ctx.Err() == nil {
			d.log.Warn("accountability", "agent", a.Name, "err", err)
		}
	}
}

// authError reads a failed turn's error as an expired login.
func authError(detail string) bool {
	l := strings.ToLower(detail)
	for _, w := range []string{"authentication", "login", "log in", "401", "oauth", "invalid api key", "unauthorized"} {
		if strings.Contains(l, w) {
			return true
		}
	}
	return false
}

// errorText is the alert for an agent whose last turn failed and that owes its parent a report.
func errorText(a *v1.Agent, login bool) string {
	if login {
		return fmt.Sprintf("%s's login expired (%s): it can't go on until the user logs in. Ask the user to run `/login` in its "+
			"terminal (the app's terminal view), then send it a task: `mesh send %s --type task --body \"You're logged in again: "+
			"continue where you stopped.\"`.", a.Name, orDash(a.StateDetail), a.Name)
	}
	return fmt.Sprintf("%s's last turn failed (%s) and it is at its prompt with no report to you. Look at its terminal; once it is "+
		"fixed, `mesh send %s --type task --body \"Continue where you stopped.\"`, or resume or reassign the work.",
		a.Name, orDash(a.StateDetail), a.Name)
}

// limitedText is the alert for an agent at a usage limit: what meshd does next, by `limits.auto`, and when it resets.
func (d *Daemon) limitedText(ctx context.Context, a *v1.Agent) string {
	r := d.store.Read(ctx)
	now := d.now()
	until, _, _ := r.ModelHold(a.Kind, a.Model, now)
	if all, _, _ := r.ModelHold(a.Kind, holdAll, now); all.After(until) {
		until = all
	}
	reset := ""
	if until.After(now) {
		reset = " (resets in " + ago(until.Sub(now)) + ")"
	}
	head := fmt.Sprintf("%s stopped at a usage limit%s and is at its prompt.", a.Name, reset)
	switch d.limitsAuto(r) {
	case "switch":
		return head + " meshd moves it to the next model of its chain with quota left, else continues it on the same model " +
			"after the reset, in the same conversation: tell the user, and send it nothing."
	case "continue":
		return head + fmt.Sprintf(" meshd continues it on the same model after the reset, in the same conversation: tell the user, "+
			"and send it nothing. `mesh switch %s --next` moves it now.", a.Name)
	}
	return head + fmt.Sprintf(" Move it to another model (`mesh switch %s --next`) or wait for the reset (`limits.auto` is off: "+
		"meshd does nothing).", a.Name)
}

// alertOnce tells the parent about an agent once per stretch of silence: the same alert is not repeated while
// the agent's last activity is older than it.
func (d *Daemon) alertOnce(ctx context.Context, a *v1.Agent, kind string, text func(context.Context) string) error {
	since := time.Time{}
	if a.LastActivity != nil {
		since = a.LastActivity.AsTime()
	}
	// Told already? Looked at before the text is built: it can read GitHub, git and the screen.
	if told, err := toldParent(d.store.Read(ctx), a, a.ParentId, alertHead(kind, a.Id), since); told || err != nil {
		return err
	}
	body := text(ctx)
	var ring *v1.Message
	err := d.store.Write(ctx, meshActor, "", func(tx *store.Tx) error {
		cur, err := tx.GetAgent(a.Id)
		if err != nil {
			return err
		}
		if !owesParent(cur) {
			return nil
		}
		// Re-checked in the write: the agent may have spoken, been reopened or moved on since the pass looked.
		switch kind {
		case "unknown":
			if d.view(cur).State != v1.AgentState_AGENT_STATE_UNKNOWN {
				return nil
			}
		case "limited":
			if cur.State != v1.AgentState_AGENT_STATE_LIMITED || cur.PendingReopen != nil {
				return nil // a reopen pending is already moving it
			}
		case "input":
			if cur.State != v1.AgentState_AGENT_STATE_NEEDS_INPUT {
				return nil
			}
		case "error":
			if cur.State != v1.AgentState_AGENT_STATE_ERROR {
				return nil
			}
			if rep, err := lastReport(tx.Reader, cur); err != nil || rep != nil {
				return err
			}
		}
		since := time.Time{}
		if cur.LastActivity != nil {
			since = cur.LastActivity.AsTime()
		}
		ring, err = d.alertParentTx(tx, cur, kind, body, since)
		return err
	})
	if err == nil && ring != nil && d.notify != nil {
		d.notify(ring.ToId)
	}
	return err
}

// checkOwed nudges or reports on an agent at its prompt that owes a report, re-checking inside the write that
// a report or a message didn't arrive since the pass looked.
func (d *Daemon) checkOwed(ctx context.Context, a *v1.Agent, nudge, waitMax time.Duration) error {
	verdict, _, err := d.reportOwed(d.store.Read(ctx), a, d.now(), nudge, waitMax)
	if err != nil || verdict == owesNothing {
		return err
	}
	// A Claude agent whose background command still runs is waiting on it (background.go).
	if d.waitsOnBackground(ctx, a, waitMax) {
		return nil
	}
	extras := ""
	if verdict == owesAlert { // read before the write: it runs git and gh and reads the screen
		extras = d.silentExtras(ctx, a)
	}
	var ring *v1.Message
	err = d.store.Write(ctx, meshActor, "", func(tx *store.Tx) error {
		cur, err := tx.GetAgent(a.Id)
		if err != nil || !atPrompt(cur.State) {
			return err
		}
		v, task, err := d.reportOwed(tx.Reader, cur, tx.Now(), nudge, waitMax)
		if err != nil || v != verdict {
			return err
		}
		switch v {
		case owesNudge:
			ring, err = tellTx(tx, cur, nudgeTag+" Your turn ended without a report to your parent. If the task is finished, "+
				"run `mesh inbox`, then report it now: `mesh send parent --type done --body @$(mesh dir)/reports/"+cur.Name+".md` "+
				"(or `plan`, `question`, `failed`). If you are waiting on something on purpose, declare it first: "+
				"`mesh send parent --type wait --body \"<what>\"`. A wait ends when something wakes you (a background job's notification, "+
				"a monitor event): if you end that turn still waiting, declare a fresh wait first.")
		case owesAlert:
			ring, err = d.alertParentTx(tx, cur, "silent", fmt.Sprintf("%s ended a turn without reporting to you, and again after a nudge. "+
				"It is idle at its prompt with no `done`, `failed`, `plan` or `question` since your last message to it.", cur.Name)+extras, task)
		}
		return err
	})
	if err == nil && ring != nil && d.notify != nil {
		d.notify(ring.ToId)
	}
	return err
}

// alertExitTx tells an agent's parent that its process ended, when the agent still owed it a report. It runs in
// the transaction that records the exit, so the alert and the exit are one write; the caller rings the message.
// lastSaid is the agent's state detail from before the exit event was applied (which overwrites it).
func (d *Daemon) alertExitTx(tx *store.Tx, a *v1.Agent, lastSaid string, code, signal int32, reason string) (*v1.Message, error) {
	if !owesParent(a) {
		return nil, nil
	}
	if report, err := lastReport(tx.Reader, a); err != nil || report != nil {
		return nil, err // it reported before it went: nothing is owed
	}
	how := fmt.Sprintf("exited with code %d", code)
	switch {
	case strings.HasPrefix(reason, "reopen failed: "):
		how = "could not be reopened (" + strings.TrimPrefix(reason, "reopen failed: ") + ")"
	case reason == "stopped":
		how = "was stopped"
	case reason == "lost":
		how = "lost its terminal (the process is gone and its exit was not seen)"
	case signal != 0:
		how = fmt.Sprintf("was killed by signal %d", signal)
	}
	text := fmt.Sprintf("%s %s with no report to you.", a.Name, how)
	if lastSaid != "" {
		text += " It last said: " + lastSaid
	}
	if work := d.workTextCached(tx.Reader, a); work != "" {
		text += " " + work
	}
	text += " Resume it (`mesh resume " + a.Name + "`) or reassign the work."
	since := time.Time{}
	if a.LastActivity != nil {
		since = a.LastActivity.AsTime()
	}
	return d.alertParentTx(tx, a, "exited", text, since)
}

// unreadInstructions refuses a report while a task or answer sits unread in the agent's inbox: the report would
// answer stale instructions (the agent runs `mesh inbox`, acts on what it finds, and reports once for all).
func unreadInstructions(r store.Reader, a *v1.Agent) error {
	unread, err := r.Inbox(a.Id, false)
	if err != nil {
		return err
	}
	var waiting []string
	for _, m := range unread {
		if m.Type == v1.MessageType_MESSAGE_TYPE_TASK || m.Type == v1.MessageType_MESSAGE_TYPE_ANSWER {
			waiting = append(waiting, fmt.Sprintf("%s from %s", strings.ToLower(strings.TrimPrefix(m.Type.String(), "MESSAGE_TYPE_")), m.From.GetName()))
		}
	}
	if len(waiting) == 0 {
		return nil
	}
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("you have unread instructions (%s): read them with `mesh inbox`, "+
		"act on them, then send one report that covers them too (nothing was sent)", strings.Join(waiting, ", ")))
}
