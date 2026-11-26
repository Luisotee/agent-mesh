package daemon

// Accountability: what `bin/mesh watch` did with exit codes, done by the daemon (docs/apps.md, "Switchover").
// A parent is told, by an `info` from the daemon, when a worker of its own exits, is lost, goes quiet (unknown)
// or stops at a usage limit. A worker that ends a turn with no report since its task is nudged once; if its next
// turn ends the same way, its parent is told. A declared `wait` pauses that, for at most `accountability.wait_max`.
// A report is refused while a task or answer is unread (SendMessage). Nothing here reopens or switches an agent:
// acting on a limit is the limits pass's job, and this only tells the parent.

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	msgs, err := tx.RunMessages(a.RunId, "", -1)
	if err != nil {
		return nil, err
	}
	for _, m := range msgs {
		if m.ToId == parent.Id && m.From.GetId() == meshActor.Id && strings.HasPrefix(m.Body, head) &&
			!m.CreatedAt.AsTime().Before(since) {
			return nil, nil
		}
	}
	return tellTx(tx, parent, head+" "+text)
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
func reportOwed(r store.Reader, a *v1.Agent, now time.Time, nudgeAfter, waitMax time.Duration) (owed, time.Time, error) {
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
			wait = m.CreatedAt.AsTime()
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
	if !wait.IsZero() && now.Sub(wait) < waitMax {
		return owesNothing, task, nil // a declared wait: the agent said it is waiting on purpose
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
	for _, a := range agents {
		if !owesParent(a) || ctx.Err() != nil {
			continue
		}
		shown := d.view(a)
		var err error
		switch {
		case shown.State == v1.AgentState_AGENT_STATE_UNKNOWN:
			err = d.alertOnce(ctx, a, "unknown", fmt.Sprintf("%s has gone quiet: nothing has been heard from it for %s (state %s). "+
				"It may be hung or waiting at a prompt; look at its terminal.", a.Name, d.rt.cfg.UnknownAfter.Round(time.Minute),
				strings.ToLower(strings.TrimPrefix(a.State.String(), "AGENT_STATE_"))))
		case a.State == v1.AgentState_AGENT_STATE_LIMITED:
			err = d.alertOnce(ctx, a, "limited", fmt.Sprintf("%s stopped at a usage limit and is at its prompt. "+
				"Move it to another model (`mesh switch %s --next`) or wait for the reset.", a.Name, a.Name))
		case atPrompt(a.State) && nudge > 0:
			err = d.checkOwed(ctx, a, nudge, waitMax)
		}
		if err != nil && ctx.Err() == nil {
			d.log.Warn("accountability", "agent", a.Name, "err", err)
		}
	}
}

// alertOnce tells the parent about an agent once per stretch of silence: the same alert is not repeated while
// the agent's last activity is older than it.
func (d *Daemon) alertOnce(ctx context.Context, a *v1.Agent, kind, text string) error {
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
		}
		since := time.Time{}
		if cur.LastActivity != nil {
			since = cur.LastActivity.AsTime()
		}
		ring, err = d.alertParentTx(tx, cur, kind, text, since)
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
	verdict, _, err := reportOwed(d.store.Read(ctx), a, d.now(), nudge, waitMax)
	if err != nil || verdict == owesNothing {
		return err
	}
	var ring *v1.Message
	err = d.store.Write(ctx, meshActor, "", func(tx *store.Tx) error {
		cur, err := tx.GetAgent(a.Id)
		if err != nil || !atPrompt(cur.State) {
			return err
		}
		v, task, err := reportOwed(tx.Reader, cur, tx.Now(), nudge, waitMax)
		if err != nil || v != verdict {
			return err
		}
		switch v {
		case owesNudge:
			ring, err = tellTx(tx, cur, nudgeTag+" Your turn ended without a report to your parent. If the task is finished, "+
				"run `mesh inbox`, then report it now: `mesh send parent --type done --body @$(mesh dir)/reports/"+cur.Name+".md` "+
				"(or `plan`, `question`, `failed`). If you are waiting on something on purpose, declare it first: "+
				"`mesh send parent --type wait --body \"<what>\"`.")
		case owesAlert:
			ring, err = d.alertParentTx(tx, cur, "silent", fmt.Sprintf("%s ended a turn without reporting to you, and again after a nudge. "+
				"It is idle at its prompt with no `done`, `failed`, `plan` or `question` since your last message to it.", cur.Name), task)
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
