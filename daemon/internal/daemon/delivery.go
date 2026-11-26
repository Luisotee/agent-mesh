package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// ── doorbells ──
//
// A message is stored first; the doorbell only tells the agent to read it (design §3). Delivery is a guarantee
// the daemon keeps by checking, not by typing once: a doorbell is "delivered" only when the agent is known to
// have taken it, and until then it is retried, never typed into a human's draft, and never typed at a dialog.
//
//   - The lane: a Claude agent that takes cross-session messages gets the doorbell written into its inbox socket
//     (adapter DeliverNative); anything else, or a socket whose frame wasn't taken, gets it typed.
//   - Confirmation: every doorbell carries a `(ref <token>)` and is confirmed by the agent's next sign of life
//     (a turn, any activity after the ring), by the agent reading the messages, or by its ref showing above the
//     input box. Not confirmed within `delivery.confirm_after`, it is rung again (up to `delivery.ring_tries`),
//     then given up with one DoorbellFailed event until the agent's state next changes.
//   - The draft: a typed doorbell first reads the input box (adapter ReadInputBox). Typed text that isn't one of
//     our doorbells holds it; anything unclear types, as bin/mesh does, but a retry types only into a box it can
//     read as empty, so a second Enter never lands on a dialog.

// ring is a doorbell in flight for one agent. Its fields are read and written only under the agent's lock.
type ring struct {
	terminalID string
	token      string
	at         time.Time // taken before the text was typed or the frame written: activity after it is the doorbell's
	msgIDs     []string  // the messages it announced
	lane       string    // "socket" or "pty"
	attempts   int       // doorbells typed or written, and tries that found the box unreadable
	typed      bool      // a doorbell has been typed into the terminal by this ring (a retry is stricter)
	first      bool      // it carried the protocol pointer

	failed         bool      // given up: nothing more is typed until the agent's activity moves
	failedActivity time.Time // its last activity when it was given up
}

// held is a doorbell kept back by a human's draft.
type held struct {
	since   time.Time
	alerted bool
}

// settled is what checking a ring found.
type settled int

const (
	stillWaiting settled = iota // not confirmed, not yet due: nothing to do now
	ringEnded                   // the doorbell got through (its messages are marked delivered), or was given up on: ring afresh for what is left
	retryDue                    // not confirmed in time: ring again from the same ring
)

// deliverSoon rings an agent's doorbell for its undelivered messages, once it is at its prompt (atPrompt). A run
// already queued for the agent covers a trigger that arrives meanwhile: a host that is slow to answer can't pile
// up a goroutine per tick.
func (d *Daemon) deliverSoon(agentID string) {
	if d.rt.term == nil {
		return
	}
	d.rt.mu.Lock()
	if d.rt.deliverQueued[agentID] {
		d.rt.mu.Unlock()
		return
	}
	d.rt.deliverQueued[agentID] = true
	d.rt.mu.Unlock()
	go func() {
		d.rt.mu.Lock()
		delete(d.rt.deliverQueued, agentID)
		d.rt.mu.Unlock()
		if err := d.deliver(context.Background(), agentID); err != nil {
			d.log.Warn("doorbell", "agent", agentID, "err", err)
		}
	}()
}

// agentLock is held by whatever types into an agent's terminal or replaces it: a doorbell, a reopen, a stop.
func (d *Daemon) agentLock(agentID string) *sync.Mutex {
	d.rt.mu.Lock()
	defer d.rt.mu.Unlock()
	mu := d.rt.delivering[agentID]
	if mu == nil {
		mu = &sync.Mutex{}
		d.rt.delivering[agentID] = mu
	}
	return mu
}

func (d *Daemon) worktreeLock(worktree string) *sync.Mutex {
	d.rt.mu.Lock()
	defer d.rt.mu.Unlock()
	if d.rt.worktrees == nil {
		d.rt.worktrees = map[string]*sync.Mutex{}
	}
	mu := d.rt.worktrees[worktree]
	if mu == nil {
		mu = &sync.Mutex{}
		d.rt.worktrees[worktree] = mu
	}
	return mu
}

// hasRing reports whether an agent has a doorbell in flight: a hook that reports its activity then checks it
// at once, instead of at the next tick.
func (d *Daemon) hasRing(agentID string) bool {
	d.rt.mu.Lock()
	defer d.rt.mu.Unlock()
	return d.rt.rings[agentID] != nil
}

func (d *Daemon) ringOf(agentID string) *ring {
	d.rt.mu.Lock()
	defer d.rt.mu.Unlock()
	return d.rt.rings[agentID]
}

func (d *Daemon) setRing(agentID string, rg *ring) {
	d.rt.mu.Lock()
	defer d.rt.mu.Unlock()
	if rg == nil {
		delete(d.rt.rings, agentID)
		return
	}
	d.rt.rings[agentID] = rg
}

// deliver brings an agent up to date: a pending reopen first (the messages it was asked with are for the new
// process), then the doorbell in flight (confirmed, due again, or still waiting), else a doorbell for its
// undelivered messages once it is at its prompt (atPrompt).
func (d *Daemon) deliver(ctx context.Context, agentID string) error {
	mu := d.agentLock(agentID)
	mu.Lock()
	defer mu.Unlock()

	r := d.store.Read(ctx)
	a, err := r.GetAgent(agentID)
	if err != nil {
		return err
	}
	if a.PendingReopen != nil {
		return d.reopen(ctx, a)
	}
	rg := d.ringOf(agentID)
	if rg != nil && rg.terminalID != a.TerminalId {
		d.setRing(agentID, nil) // a ring of a process that is gone: its doorbell was typed to nobody
		rg = nil
	}
	if a.TerminalId == "" || d.rt.term == nil {
		return nil
	}
	msgs, err := r.Undelivered(agentID)
	if err != nil {
		return err
	}
	var reading *adapterv1.ReadInputBoxResponse
	if rg != nil {
		var outcome settled
		outcome, msgs, reading, err = d.settle(ctx, a, rg, msgs)
		if err != nil {
			return err
		}
		switch outcome {
		case stillWaiting:
			return nil
		case ringEnded:
			rg = nil
		}
	}
	if len(msgs) == 0 {
		d.setRing(agentID, nil)
		d.clearHold(agentID)
		return nil
	}
	if !takesDoorbell(a.State) {
		return nil
	}
	return d.ringAgent(ctx, a, msgs, rg, reading)
}

// settle checks the doorbell in flight against what the agent did since. It returns the messages still to
// announce (the ring's own that were confirmed are marked delivered and left out), and for a retry the screen it
// read, so the retry doesn't read it twice.
func (d *Daemon) settle(ctx context.Context, a *v1.Agent, rg *ring, msgs []*v1.Message) (settled, []*v1.Message, *adapterv1.ReadInputBoxResponse, error) {
	announced := map[string]bool{}
	for _, id := range rg.msgIDs {
		announced[id] = true
	}
	unread := map[string]*v1.Message{}
	var rest, mine []*v1.Message
	for _, m := range msgs {
		unread[m.Id] = m
		if announced[m.Id] {
			mine = append(mine, m)
		} else {
			rest = append(rest, m)
		}
	}
	// A given-up doorbell waits for a sign the agent's state moved, or for a message it hasn't been rung about.
	if rg.failed {
		if len(rest) == 0 && sameInstant(a.LastActivity, rg.failedActivity) {
			return stillWaiting, msgs, nil, nil
		}
		d.setRing(a.Id, nil)
		return ringEnded, msgs, nil, nil
	}
	// The agent read what it was rung about, before or without any other sign.
	for id := range announced {
		if unread[id] == nil {
			d.endRing(a.Id, rg)
			return ringEnded, rest, nil, nil
		}
	}
	// Activity after the ring, never one from before it: a late event of the previous turn can't confirm it.
	if a.LastActivity != nil && a.LastActivity.AsTime().After(rg.at) {
		via := "turn"
		if rg.lane == "socket" {
			via = "socket"
		}
		return ringEnded, rest, nil, d.confirm(ctx, a, rg, mine, via)
	}
	confirmAfter, err := d.secondsSetting(ctx, "delivery.confirm_after")
	if err != nil {
		return stillWaiting, msgs, nil, err
	}
	if d.now().Sub(rg.at) < confirmAfter {
		return stillWaiting, msgs, nil, nil
	}
	if rg.lane == "socket" {
		// A session that took the frame is busy with it, and a turn that calls no tool fires no hook until it
		// ends: while the terminal is drawing, wait longer before taking the frame for lost (typing a second
		// doorbell into a busy agent would cost it a turn on an empty inbox).
		if d.now().Sub(rg.at) < 3*confirmAfter && d.drawingSince(ctx, rg.terminalID, rg.at) {
			return stillWaiting, msgs, nil, nil
		}
		// The frame was written and nothing came of it: a policy the daemon can't read holds it, or the session
		// never took it. Type it from now on, for this terminal.
		d.rt.mu.Lock()
		d.rt.nativeOff[rg.terminalID] = true
		d.rt.mu.Unlock()
		d.log.Warn("doorbell written to the inbox socket and not taken; typing doorbells to this terminal from now on", "agent", a.Name)
		rg.lane, rg.typed = "pty", false
		return retryDue, msgs, nil, nil
	}
	reading := d.readInputBox(ctx, a, rg.token)
	if reading != nil && reading.TokenAbove > 0 {
		return ringEnded, rest, nil, d.confirm(ctx, a, rg, mine, "screen")
	}
	maxTries, err := d.intSetting(ctx, "delivery.ring_tries")
	if err != nil {
		return stillWaiting, msgs, nil, err
	}
	if rg.attempts >= maxTries {
		rg.failed, rg.failedActivity = true, instant(a.LastActivity)
		return stillWaiting, msgs, nil, d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
			_, err := tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_DoorbellFailed{
				DoorbellFailed: &v1.DoorbellFailed{MessageIds: rg.msgIDs, Tries: int32(rg.attempts),
					Reason: "the agent never took the doorbell"}}})
			return err
		})
	}
	return retryDue, msgs, reading, nil
}

func instant(t interface{ AsTime() time.Time }) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.AsTime()
}

func sameInstant(t interface{ AsTime() time.Time }, at time.Time) bool {
	return instant(t).Equal(at)
}

// confirm marks the ring's messages delivered, once the doorbell is known to have got through, and ends the ring.
func (d *Daemon) confirm(ctx context.Context, a *v1.Agent, rg *ring, msgs []*v1.Message, via string) error {
	d.endRing(a.Id, rg)
	return d.markDelivered(ctx, msgs, via, rg.attempts)
}

// endRing ends a ring the agent took: the protocol it named is now known to that process.
func (d *Daemon) endRing(agentID string, rg *ring) {
	d.setRing(agentID, nil)
	d.clearHold(agentID)
	if rg.first {
		d.rt.mu.Lock()
		d.rt.introduced[rg.terminalID] = true
		d.rt.mu.Unlock()
	}
}

func (d *Daemon) markDelivered(ctx context.Context, msgs []*v1.Message, via string, tries int) error {
	if len(msgs) == 0 {
		return nil
	}
	return d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		for _, m := range msgs {
			// Only the delivery time is written, on the row as it is now: the agent may have read the message
			// since the snapshot above, and a stale copy would make it unread again.
			if err := tx.MarkDelivered(m.Id); err != nil {
				return err
			}
			if _, err := tx.AppendEvent(&v1.Event{RunId: m.RunId, AgentId: m.ToId, Payload: &v1.Event_MessageDelivered{
				MessageDelivered: &v1.MessageDelivered{MessageId: m.Id, Via: via, Tries: int32(tries)}}}); err != nil {
				return err
			}
		}
		return nil
	})
}

// ringAgent rings for msgs: into the agent's inbox socket where it takes one, else typed into its terminal,
// unless a user is typing or a draft is in its box. prev is the ring a retry continues, nil for a first try.
func (d *Daemon) ringAgent(ctx context.Context, a *v1.Agent, msgs []*v1.Message, prev *ring, reading *adapterv1.ReadInputBoxResponse) error {
	rg := &ring{terminalID: a.TerminalId, token: ringToken(), lane: "pty"}
	if prev != nil {
		rg.attempts, rg.typed = prev.attempts, prev.typed
	}
	for _, m := range msgs {
		rg.msgIDs = append(rg.msgIDs, m.Id)
	}
	d.rt.mu.Lock()
	rg.first = !d.rt.introduced[a.TerminalId]
	d.rt.mu.Unlock()
	line := Doorbell(msgs, rg.token)
	if rg.first {
		line += d.protocolPointer(a)
	}

	// A doorbell nothing has typed yet goes into the inbox socket where the agent takes one: it never touches the
	// input box, so a user typing or a draft don't hold it.
	if !rg.typed {
		at := d.now()
		if ok, reason := d.deliverNative(ctx, a, line); ok {
			rg.at, rg.lane = at, "socket"
			rg.attempts++
			d.setRing(a.Id, rg)
			d.clearHold(a.Id)
			return nil
		} else if reason != "" {
			d.log.Debug("doorbell not written to the inbox socket", "agent", a.Name, "reason", reason)
		}
	}

	// Never type into what a user is typing: while a client that can type has typed lately, the doorbell waits
	// (retryDoorbells rings it once they pause).
	if typing, err := d.userTyping(ctx, a.TerminalId); err != nil || typing {
		return err
	}
	if reading == nil {
		reading = d.readInputBox(ctx, a, rg.token)
	}
	text := strings.TrimSpace(reading.GetText())
	rg.at = d.now() // before anything is typed: activity after it is this doorbell's
	switch {
	case reading.GetBox() == adapterv1.InputBox_INPUT_BOX_TYPED && !strings.HasPrefix(text, "📬 [mesh]"):
		// A human's draft: positively seen, so the doorbell waits for it to go.
		d.noteHold(ctx, a, msgs, text)
		return nil
	case reading.GetBox() == adapterv1.InputBox_INPUT_BOX_TYPED:
		// Our own doorbell, typed and not submitted: Enter submits it, typing another after it would fuse two.
		d.clearHold(a.Id)
		if err := d.pressEnter(ctx, a.TerminalId); err != nil {
			return err
		}
		if prev != nil {
			rg.token = prev.token // the text in the box is the earlier try's
		}
	case reading.GetBox() != adapterv1.InputBox_INPUT_BOX_EMPTY && rg.typed:
		// A retry into a screen that shows no readable box (a dialog, an alternate screen): count the try and
		// wait, so Enter never lands on a dialog.
		prev.attempts++
		prev.at = d.now()
		return nil
	default:
		d.clearHold(a.Id)
		if err := d.typeLine(ctx, a.TerminalId, line); err != nil {
			return err
		}
	}
	rg.attempts++
	rg.typed = true
	d.setRing(a.Id, rg)
	return nil
}

// ringToken is the fresh `ref` of one doorbell: only that token counts on the screen, so an earlier doorbell of
// the same sender can't confirm a new one still sitting in the input box.
func ringToken() string {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%06x", time.Now().UnixNano()&0xffffff)
	}
	return hex.EncodeToString(b[:])
}

// noteHold records a doorbell kept back by a draft, and says so once it has waited `delivery.draft_hold_alert`.
func (d *Daemon) noteHold(ctx context.Context, a *v1.Agent, msgs []*v1.Message, draft string) {
	d.rt.mu.Lock()
	h := d.rt.held[a.Id]
	if h == nil {
		h = &held{since: d.now()}
		d.rt.held[a.Id] = h
	}
	alert := !h.alerted
	since := h.since
	d.rt.mu.Unlock()
	if !alert {
		return
	}
	after, err := d.secondsSetting(ctx, "delivery.draft_hold_alert")
	if err != nil || after <= 0 || d.now().Sub(since) < after {
		return
	}
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.Id
	}
	if len(draft) > 80 {
		draft = draft[:80]
	}
	if err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		_, err := tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_DoorbellHeld{
			DoorbellHeld: &v1.DoorbellHeld{MessageIds: ids, Reason: "a draft in the input box", Draft: draft}}})
		return err
	}); err != nil {
		d.log.Warn("doorbell held event", "agent", a.Name, "err", err)
		return
	}
	d.rt.mu.Lock()
	h.alerted = true
	d.rt.mu.Unlock()
}

func (d *Daemon) clearHold(agentID string) {
	d.rt.mu.Lock()
	delete(d.rt.held, agentID)
	d.rt.mu.Unlock()
}

// readInputBox reads what an agent's input box holds. nil when the screen or the adapter can't say: the callers
// then treat the box as unclear.
func (d *Daemon) readInputBox(ctx context.Context, a *v1.Agent, token string) *adapterv1.ReadInputBoxResponse {
	ad, err := d.rt.adapters.Get(a.Kind)
	if err != nil {
		return nil
	}
	raw, err := d.readScreen(ctx, a.TerminalId)
	if err != nil || len(raw) == 0 {
		return nil
	}
	res, err := ad.ReadInputBox(ctx, connect.NewRequest(&adapterv1.ReadInputBoxRequest{Screen: raw, Token: token}))
	if err != nil {
		return nil
	}
	return res.Msg
}

// deliverNative writes the doorbell into the agent's inbox, for an adapter that has one, where `delivery.socket`
// is on and an earlier frame to this terminal wasn't taken. false is not an error: the doorbell is typed.
func (d *Daemon) deliverNative(ctx context.Context, a *v1.Agent, text string) (bool, string) {
	if !d.rt.adapters.HasCapability(a.Kind, claude.CapInboxSocket) {
		return false, ""
	}
	d.rt.mu.Lock()
	off := d.rt.nativeOff[a.TerminalId]
	d.rt.mu.Unlock()
	if off {
		return false, ""
	}
	on, err := d.Setting(d.store.Read(ctx), "delivery.socket")
	if err != nil || on != true {
		return false, ""
	}
	term, err := d.terminalInfo(ctx, a.TerminalId)
	if err != nil || term == nil || term.Exited || term.Pid <= 0 {
		return false, "terminal not running"
	}
	ad, err := d.rt.adapters.Get(a.Kind)
	if err != nil {
		return false, ""
	}
	res, err := ad.DeliverNative(ctx, connect.NewRequest(&adapterv1.DeliverNativeRequest{AgentId: a.Id, Pid: term.Pid,
		PidStartTicks: term.PidStartTicks, Cwd: a.Worktree, Env: d.agentEnv(a, ""), Text: text}))
	if err != nil {
		return false, err.Error()
	}
	return res.Msg.Delivered, res.Msg.Reason
}

func (d *Daemon) secondsSetting(ctx context.Context, key string) (time.Duration, error) {
	v, err := d.Setting(d.store.Read(ctx), key)
	if err != nil {
		return 0, err
	}
	return time.Duration(v.(float64) * float64(time.Second)), nil
}

func (d *Daemon) intSetting(ctx context.Context, key string) (int, error) {
	v, err := d.Setting(d.store.Read(ctx), key)
	if err != nil {
		return 0, err
	}
	return int(v.(float64)), nil
}

// protocolPointer is what the first doorbell of a process adds: where the protocol is, as an absolute path (the
// agent's CLI doesn't expand variables in what is typed to it). A root's names its card, which names the rest. Nothing
// when the daemon has no share folder.
func (d *Daemon) protocolPointer(a *v1.Agent) string {
	share := d.rt.cfg.Share
	if !filepath.IsAbs(share) {
		return ""
	}
	protocol := filepath.Join(share, "protocol")
	if a.Role == v1.AgentRole_AGENT_ROLE_ROOT {
		return " (first one here: read " + filepath.Join(protocol, "ROOT_CARD.md") + " and follow it)"
	}
	return " (first one here: read " + filepath.Join(protocol, "PROTOCOL.md") + " once, and follow it)"
}

// terminalInfo is the terminal host's record of a terminal, nil when it has none.
func (d *Daemon) terminalInfo(ctx context.Context, terminalID string) (*termv1.Terminal, error) {
	res, err := d.rt.term.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
	if err != nil {
		return nil, err
	}
	for _, t := range res.Msg.Terminals {
		if t.Id == terminalID {
			return t, nil
		}
	}
	return nil, nil
}

// drawingSince reports whether a terminal drew something after `since` and again within the last two seconds: a
// session working on a prompt (a spinner) rather than one sitting idle.
func (d *Daemon) drawingSince(ctx context.Context, terminalID string, since time.Time) bool {
	t, err := d.terminalInfo(ctx, terminalID)
	if err != nil || t == nil || t.LastOutput == nil {
		return false
	}
	last := t.LastOutput.AsTime()
	return last.After(since) && d.now().Sub(last) < 2*time.Second
}

// userTyping reports whether a client that can type is attached to a terminal and typed within TypingQuiet.
func (d *Daemon) userTyping(ctx context.Context, terminalID string) (bool, error) {
	t, err := d.terminalInfo(ctx, terminalID)
	if err != nil || t == nil {
		return false, err
	}
	return t.InputClients > 0 && t.LastClientInput != nil && d.now().Sub(t.LastClientInput.AsTime()) < d.rt.cfg.TypingQuiet, nil
}

// retryDoorbells rings again for messages whose doorbell couldn't ring (the agent was busy, a user was
// typing, a trigger was missed), checks the doorbells in flight, and tries the reopens still pending.
func (d *Daemon) retryDoorbells(ctx context.Context) {
	if d.rt.term == nil {
		return
	}
	ids, err := d.store.Read(ctx).AgentsWithUndelivered()
	if err != nil {
		return
	}
	pending, err := d.store.Read(ctx).AgentsWithPendingReopen()
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, a := range pending {
		seen[a.Id] = true
		d.deliverSoon(a.Id)
	}
	for _, id := range ids {
		if !seen[id] {
			d.deliverSoon(id)
		}
	}
}

// typeLine types a line into a terminal, then Enter after a pause, so the TUI doesn't read it as a paste.
func (d *Daemon) typeLine(ctx context.Context, terminalID, line string) error {
	if _, err := d.rt.term.API.WriteTerminal(ctx, connect.NewRequest(&termv1.WriteTerminalRequest{Id: terminalID,
		Data: []byte(line)})); err != nil {
		return err
	}
	time.Sleep(d.rt.cfg.DeliveryDelay)
	// The text is in the input box. If Enter did not get through (the host was unreachable or didn't answer in
	// time: the write didn't happen), try again, and when it still can't be sent take the text back out (Ctrl-U)
	// so the retry doesn't type the line a second time. Any other error leaves it unknown whether the Enter went
	// in: stop, and send nothing more.
	err := d.pressEnter(ctx, terminalID)
	if err == nil {
		return nil
	}
	if !writeDidNotHappen(err) {
		return err
	}
	d.rt.term.API.WriteTerminal(context.WithoutCancel(ctx), connect.NewRequest(&termv1.WriteTerminalRequest{Id: terminalID,
		Data: []byte("\x15")}))
	return err
}

// pressEnter sends Enter, trying again while the host didn't get it.
func (d *Daemon) pressEnter(ctx context.Context, terminalID string) error {
	var err error
	for try := 0; try < 3; try++ {
		if _, err = d.rt.term.API.WriteTerminal(ctx, connect.NewRequest(&termv1.WriteTerminalRequest{Id: terminalID,
			Data: []byte("\r")})); err == nil {
			return nil
		}
		if !writeDidNotHappen(err) {
			return err
		}
		time.Sleep(d.rt.cfg.DeliveryDelay)
	}
	return err
}

// writeDidNotHappen reports whether a failed terminal write certainly did not reach the terminal.
func writeDidNotHappen(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded, connect.CodeCanceled:
		return true
	}
	return false
}

// Doorbell is the one line typed into an agent's terminal for its new messages; the messages themselves are
// read with `mesh inbox`. Its ref token goes early, after the sender, where a CLI that shortens a long queued
// prompt keeps it.
func Doorbell(msgs []*v1.Message, token string) string {
	kind := func(m *v1.Message) string {
		return strings.ToLower(strings.TrimPrefix(m.Type.String(), "MESSAGE_TYPE_")) + " from " + m.From.GetName()
	}
	if len(msgs) == 1 {
		return "📬 [mesh] " + kind(msgs[0]) + " (ref " + token + "): run `mesh inbox`"
	}
	parts := make([]string, len(msgs))
	for i, m := range msgs {
		parts[i] = kind(m)
	}
	return fmt.Sprintf("📬 [mesh] %d messages (ref %s: %s): run `mesh inbox`", len(msgs), token, strings.Join(parts, ", "))
}
