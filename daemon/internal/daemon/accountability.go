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

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
	"github.com/Luisotee/agent-mesh/daemon/internal/transcript"
)

// meshActor is who the messages the daemon writes are from. It has no class: it is no one an action can be
// called as, only a name in the inbox and the event log.
var meshActor = &v1.Actor{Id: "meshd", Name: "mesh"}

// The first words of the daemon's messages, which the pass finds its own by (a nudge, and one alert per kind).
const (
	nudgeTag   = "[mesh nudge]"
	answersTag = "[mesh answers "
	alertTag   = "[mesh alert "
	alertEnds  = "]"
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
	owesNothing   owed = iota
	owesNudge          // ended a turn with no report: nudge it
	owesWaitNudge      // its declared wait ran out with no deadline of its own and nothing that could wake it: nudge it about that
	owesAlert          // nudged, and ended another turn the same way: tell its parent
)

// reportOwed judges an agent at its prompt: does it owe a report, and has the nudge been given? It reads only
// the store and the clock, so the pass and the write that acts on it agree. It returns the time the agent's
// task or answer came, which dedupes an alert to its parent.
//
// unseen is how long a declared wait with no deadline of its own holds when nothing the daemon can see would wake
// the agent from it (waitHold); it is asked only once the wait is older than that.
func (d *Daemon) reportOwed(r store.Reader, a *v1.Agent, now time.Time, nudgeAfter, waitMax time.Duration, w waitHold) (owed, time.Time, error) {
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
	var wait, waitUntil, nudgedAt time.Time
	for _, m := range msgs {
		switch {
		case m.ToId == a.Id && (m.Type == v1.MessageType_MESSAGE_TYPE_TASK || m.Type == v1.MessageType_MESSAGE_TYPE_ANSWER):
			task, reported, nudged, wait, waitUntil, nudgedAt = m.CreatedAt.AsTime(), false, false, time.Time{}, time.Time{}, time.Time{}
		case m.From.GetId() == a.Id && m.ToId == a.ParentId && isReport(m.Type):
			reported = true
		case m.From.GetId() == a.Id && m.ToId == a.ParentId && m.Type == v1.MessageType_MESSAGE_TYPE_WAIT:
			// A wait answers an earlier nudge: its next silent wake is a first one again, nudged, not alerted (an
			// agent that waits on background jobs ends a turn at each job's notification, each time still waiting).
			wait, nudged, nudgedAt = m.CreatedAt.AsTime(), false, time.Time{}
			waitUntil = time.Time{}
			if m.WaitUntil != nil {
				waitUntil = m.WaitUntil.AsTime()
			}
		case m.ToId == a.Id && m.From.GetId() == meshActor.Id && strings.HasPrefix(m.Body, nudgeTag):
			nudged, nudgedAt = true, m.CreatedAt.AsTime()
		}
	}
	if task.IsZero() || reported {
		return owesNothing, task, nil
	}
	// Unread mail other than the daemon's own: work is coming, or a doorbell was lost. Of the daemon's, only a
	// finished round's `[mesh answers]` counts (it is the agent's next work: it must not be nudged for a report it
	// has not had the time to write); its nudge and other infos do not.
	unread, err := r.Inbox(a.Id, false)
	if err != nil {
		return owesNothing, task, err
	}
	for _, m := range unread {
		if m.From.GetId() != meshActor.Id || strings.HasPrefix(m.Body, answersTag) && m.CreatedAt.AsTime().After(task) {
			return owesNothing, task, nil
		}
	}
	// A round (or a peers ask) the agent started and is waiting on: its members' answers are what it waits for.
	if d.openOwnAsk(r, a, now) {
		return owesNothing, task, nil
	}
	// A declared wait: the agent said it is waiting on purpose, until it is over: its deadline, or the agent woke
	// (was active again after the turn end that declared it) and ended a turn with no report (bin/mesh's wait_over).
	// It holds until its own deadline (`until`) when it gave one, and never past wait_max from its start.
	holdEnd := wait.Add(waitMax)
	if !waitUntil.IsZero() && waitUntil.Before(holdEnd) {
		holdEnd = waitUntil
	}
	// No deadline of its own: the hold is the full wait_max only if something the daemon can see would wake the
	// agent (a background command of a Claude or OpenCode agent: its notification). A job started with `nohup … &` sends none,
	// so a wait on one would sleep for wait_max after the job ended (a root slept 2 h 51 min on its finished gates).
	unseenHold := false
	if !wait.IsZero() && waitUntil.IsZero() && w.after > 0 && w.after < waitMax && !now.Before(wait.Add(w.after)) && now.Before(holdEnd) && (w.visible == nil || !w.visible()) {
		holdEnd, unseenHold = wait.Add(w.after), true
	}
	if !wait.IsZero() && now.Before(holdEnd) && !d.waits.woke(a, wait) {
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
	if unseenHold && !d.waits.woke(a, wait) {
		return owesWaitNudge, task, nil // the hold was cut to wait_unseen: the text says why
	}
	return owesNudge, task, nil
}

// waitHold is what reportOwed asks about a declared wait that has no deadline: how long it holds when nothing
// visible would wake the agent (`accountability.wait_unseen`; 0 is off), and whether something visible would
// (visible reads the agent's terminal, so it is made once per check, before the write that acts on it).
type waitHold struct {
	after   time.Duration
	visible func() bool
}

// waitHoldFor is the question for one agent: a background command is visible only for a Claude or OpenCode agent's terminal.
func (d *Daemon) waitHoldFor(ctx context.Context, a *v1.Agent, after time.Duration) waitHold {
	var once sync.Once
	var seen bool
	return waitHold{after: after, visible: func() bool {
		once.Do(func() { seen = d.backgroundWork(ctx, a) })
		return seen
	}}
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

// peek says whether the agent was seen waking from this wait, without taking a first reading of it (the pass that judges
// the agent does): what an app shows must not move the baseline the nudges are judged from.
func (t *waitTracker) peek(a *v1.Agent, wait time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	w, ok := t.seen[a.Id]
	return ok && w.wait.Equal(wait) && w.woke
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
	// One read of each run's messages for the whole pass: every idle, quiet or limited agent judges from them.
	r := d.store.Read(ctx).WithMessageCache()
	nudgeAfter, err := d.Setting(r, "accountability.nudge_after")
	if err != nil {
		return
	}
	waitMaxV, err := d.Setting(r, "accountability.wait_max")
	if err != nil {
		return
	}
	waitUnseen := time.Duration(0)
	if v, err := d.Setting(r, "accountability.wait_unseen"); err == nil {
		waitUnseen = time.Duration(v.(float64) * float64(time.Second))
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
			err = d.alertOnce(ctx, r, a, "unknown", func(ctx context.Context) string {
				return fmt.Sprintf("%s has gone quiet: nothing has been heard from it for %s (state %s). "+
					"It may be hung or waiting at a prompt; look at its terminal.", a.Name, d.rt.cfg.UnknownAfter.Round(time.Minute),
					strings.ToLower(strings.TrimPrefix(a.State.String(), "AGENT_STATE_"))) + d.afterwards(ctx, a)
			})
		case a.State == v1.AgentState_AGENT_STATE_LIMITED:
			err = d.alertOnce(ctx, r, a, "limited", func(ctx context.Context) string { return d.limitedText(ctx, a) })
		case a.State == v1.AgentState_AGENT_STATE_NEEDS_INPUT:
			if inputAfter > 0 && a.LastActivity != nil && d.now().Sub(a.LastActivity.AsTime()) >= inputAfter {
				err = d.alertOnce(ctx, r, a, "input", func(ctx context.Context) string {
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
					err = d.alertOnce(ctx, r, a, "error", func(ctx context.Context) string { return errorText(a, login) + d.afterwards(ctx, a) })
				}
			}
			if err == nil && !skipNudge && atPrompt(a.State) && nudge > 0 {
				err = d.checkOwed(ctx, r, a, nudge, waitMax, waitUnseen)
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
func (d *Daemon) alertOnce(ctx context.Context, r store.Reader, a *v1.Agent, kind string, text func(context.Context) string) error {
	_, err := d.alertOnceSince(ctx, r, a, kind, time.Time{}, text)
	return err
}

// alertOnceSince is alertOnce where an alert already sent counts as told only when it is newer than `floor`, if
// that is later than the agent's last activity: a stall that ends and starts again in one reply (no event moves the
// agent's activity) is told again. It says whether the alert is in the store afterwards (written now, or found
// already): false when the write's re-check found the agent moved on and left nothing, so a caller that remembers
// "told" must not.
func (d *Daemon) alertOnceSince(ctx context.Context, r store.Reader, a *v1.Agent, kind string, floor time.Time, text func(context.Context) string) (bool, error) {
	if !owesParent(a) {
		return false, nil // no parent to tell: nothing to read, build or write
	}
	since := time.Time{}
	if a.LastActivity != nil {
		since = a.LastActivity.AsTime()
	}
	if floor.After(since) {
		since = floor
	}
	// Told already? Looked at before the text is built: it can read GitHub, git and the screen.
	if told, err := toldParent(r, a, a.ParentId, alertHead(kind, a.Id), since); told || err != nil {
		return told, err
	}
	body := text(ctx)
	var ring *v1.Message
	var inStore bool
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
		case "stalled":
			// Not while the view says UNKNOWN: that hang is told by its own alert, and one hang is one alert.
			if cur.State != v1.AgentState_AGENT_STATE_WORKING || d.view(cur).State == v1.AgentState_AGENT_STATE_UNKNOWN {
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
		if floor.After(since) {
			since = floor
		}
		ring, err = d.alertParentTx(tx, cur, kind, body, since)
		inStore = err == nil // written, or found told inside the write
		return err
	})
	if err != nil {
		return false, err
	}
	if ring != nil && d.notify != nil {
		d.notify(ring.ToId)
	}
	return inStore, nil
}

// checkOwed nudges or reports on an agent at its prompt that owes a report, re-checking inside the write that
// a report or a message didn't arrive since the pass looked.
func (d *Daemon) checkOwed(ctx context.Context, r store.Reader, a *v1.Agent, nudge, waitMax, waitUnseen time.Duration) error {
	hold := d.waitHoldFor(ctx, a, waitUnseen)
	verdict, _, err := d.reportOwed(r, a, d.now(), nudge, waitMax, hold)
	if err != nil || verdict == owesNothing {
		return err
	}
	// A Claude or OpenCode agent whose background command still runs is waiting on it, for a while; after that it is nudged
	// once about the command (background.go).
	// Only for the first nudge: once an agent ignored it, the alert follows as for any silent agent.
	bg := hold.visible() // memoized: the terminal is read here, before the write, once
	if (verdict == owesNudge || verdict == owesWaitNudge) && bg && d.withinBackgroundGrace(a, waitMax) {
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
		v, task, err := d.reportOwed(tx.Reader, cur, tx.Now(), nudge, waitMax, hold)
		if err != nil || v != verdict {
			return err
		}
		switch v {
		case owesWaitNudge:
			if bg {
				ring, err = tellTx(tx, cur, backgroundNudge(cur.Name))
				break
			}
			ring, err = tellTx(tx, cur, waitNudge(cur.Name))
		case owesNudge:
			if bg {
				ring, err = tellTx(tx, cur, backgroundNudge(cur.Name))
				break
			}
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
// lastSaid is the agent's state detail from before the exit event was applied (which overwrites it); words is the
// agent's newest reply read from its transcript before the write (exitWords), which is quoted in preference to it.
func (d *Daemon) alertExitTx(tx *store.Tx, a *v1.Agent, lastSaid, words string, code, signal int32, reason string) (*v1.Message, error) {
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
	if words != "" {
		lastSaid = words
	}
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

// waitNudge is the nudge of an agent whose declared wait (no deadline of its own) ran out with nothing that
// could have woken it: a job it started with `nohup … &` or `setsid` sends no notification when it ends.
func waitNudge(name string) string {
	return nudgeTag + " Your declared wait has run its course with no deadline of its own, and the daemon sees nothing under this terminal that would have woken you " +
		"(a job ending wakes you only when the CLI notifies you of it: Claude Code's Bash `run_in_background` and Monitor do, a job started with `nohup … &` or `setsid` does not, and the other CLIs have no such notification). " +
		"If your job ended, run `mesh inbox`, then report: `mesh send parent --type done --body @$(mesh dir)/reports/" + name + ".md` (or `plan`, `question`, `failed`). " +
		"If it still runs, check its log, then declare a fresh wait with a deadline: `mesh send parent --type wait --until <duration> --body \"<what>\"`."
}

// exitWordsRead and exitWordsBytes bound what exitWords reads: the end of the transcript, for at most this long.
const (
	exitWordsRead  = 2 * time.Second
	exitWordsBytes = 64 << 10
	exitWordsCut   = 300
)

// exitWords is what an agent last said, for the exit alert: its newest reply in its conversation, on one line and cut,
// read through its adapter. It is read before the write that records the exit, which holds the store's one writer, so a
// slow disk costs the exit nothing; "" when the agent owes its parent no alert, its CLI keeps no record we read, or the
// conversation has no reply yet (the alert then quotes the agent's state detail, as it did before).
func (d *Daemon) exitWords(ctx context.Context, a *v1.Agent) string {
	if a == nil || !owesParent(a) || a.SessionId == "" || stopped(a) || !d.rt.adapters.HasCapability(a.Kind, transcript.Cap) {
		return "" // a stopped agent's exit (a retire, a stop) is recorded already and tells nobody
	}
	if report, err := lastReport(d.store.Read(ctx), a); err != nil || report != nil {
		return "" // it reported before it went: nothing is owed (alertExitTx), so nothing is quoted
	}
	ad, err := d.rt.adapters.Get(a.Kind)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, exitWordsRead)
	defer cancel()
	res, err := ad.ReadTranscript(ctx, connect.NewRequest(&adapterv1.ReadTranscriptRequest{
		SessionId: a.SessionId, Cwd: a.Worktree, Env: d.agentEnv(a, ""), Offset: -1, Backward: true, MaxBytes: exitWordsBytes}))
	if err != nil {
		return ""
	}
	es := res.Msg.Entries
	for i := len(es) - 1; i >= 0; i-- {
		if es[i].Kind == v1.TranscriptKind_TRANSCRIPT_KIND_ASSISTANT {
			if t := cutLine(es[i].Text, exitWordsCut); t != "" {
				return t
			}
		}
	}
	return ""
}
