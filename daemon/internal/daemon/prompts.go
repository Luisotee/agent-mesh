package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// The chat's queue: a prompt the user sends while the agent can't take one (it works, starts, waits at a prompt, or
// its box holds a draft) is stored (store/prompts.go) and typed at the agent's next idle by the delivery (deliver),
// before its doorbells. Until the delivery claims it, the user may take it back (WithdrawPrompt, the chat's Up
// arrow); the claim and the withdrawal are each a compare-and-set on the row, so a prompt is typed or withdrawn,
// never both.

// queuePrompt stores a prompt for its agent's next idle. Called under the agent's lock by PromptAgent.
func (d *Daemon) queuePrompt(ctx context.Context, actor *v1.Actor, meta *v1.RequestMeta, a *v1.Agent, text, hold, heldText string) (*connect.Response[v1.PromptAgentResponse], error) {
	p := &v1.QueuedPrompt{Id: ids.New("q", 10), AgentId: a.Id, Text: text, CreatedAt: timestamppb.New(d.now()),
		From: &v1.Actor{Id: actor.Id, Class: actor.Class, Name: actor.Name}}
	// What it waits for is said with it (the stored row doesn't keep it: ListPrompts says it as it is then).
	shown := proto.CloneOf(p)
	shown.Held = d.promptHold(a)
	if shown.Held == "" {
		shown.Held = hold // at its prompt but not typed now: a draft or a dialog, read by PromptAgent; else simply due
		if hold == "draft" {
			shown.HeldText = heldText
		}
	}
	// What the box held is for the answer to this caller only: the event log and the stored answer would keep it (a draft
	// may hold a secret typed in the terminal).
	logged := proto.CloneOf(shown)
	logged.HeldText = ""
	stored := &v1.PromptAgentResponse{Queued: logged}
	if _, err := d.write(ctx, meta, agentmeshv1connect.MeshServicePromptAgentProcedure, stored, func(tx *store.Tx) error {
		if err := tx.QueuePrompt(p); err != nil {
			return err
		}
		_, err := tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_PromptQueued{
			PromptQueued: &v1.PromptQueued{Prompt: logged}}})
		return err
	}); err != nil {
		return nil, err
	}
	out := &v1.PromptAgentResponse{Queued: shown}
	// It may be due already (the agent went idle meanwhile): the delivery takes the lock once it is free.
	d.deliverSoon(a.Id)
	return connect.NewResponse(out), nil
}

// typeQueuedPrompt types an agent's oldest queued prompt, if it has one and its box takes it now. Called by deliver,
// under the agent's lock, with the agent at its prompt and no doorbell in flight. typed reports that one was typed:
// the agent is busy with it, so nothing more is typed this pass.
func (d *Daemon) typeQueuedPrompt(ctx context.Context, a *v1.Agent) (typed bool, err error) {
	queued, err := d.store.Read(ctx).QueuedPrompts(a.Id)
	if err != nil || len(queued) == 0 {
		d.setPromptHeld(a.Id, "")
		return false, err
	}
	hold, err := d.whyNotToType(ctx, a)
	if err != nil {
		return false, err
	}
	if hold.code != "" {
		// A draft, someone typing, a dialog: it waits, and the app says what for. The doorbells may still go (they
		// have their own rules for the same screens).
		d.setPromptHeld(a.Id, hold.code)
		return false, nil
	}
	for _, p := range queued {
		claimed, err := d.claimPrompt(ctx, a, p.Id)
		if err != nil {
			return false, err
		}
		if !claimed {
			continue // withdrawn since it was read: the next one is due instead
		}
		d.setPromptHeld(a.Id, "")
		at := d.now()
		if err := d.typePrompt(ctx, a, p.Text); err != nil {
			if writeDidNotHappen(err) {
				// Nothing reached the terminal: back in its place, typed at a later pass.
				if rerr := d.requeuePrompt(context.WithoutCancel(ctx), a, p); rerr != nil {
					d.log.Warn("putting a prompt back in the queue", "agent", a.Name, "err", rerr)
				}
			}
			return false, fmt.Errorf("typing a queued prompt into %s's terminal: %w", a.Name, err)
		}
		d.notePromptTyped(a.Id, at)
		return true, nil
	}
	return false, nil
}

// claimPrompt takes a queued prompt for typing, with its PromptTyped event. false: it was withdrawn first.
func (d *Daemon) claimPrompt(ctx context.Context, a *v1.Agent, id string) (bool, error) {
	claimed := false
	err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		if _, err := tx.ClaimPrompt(a.Id, id); errors.Is(err, store.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		claimed = true
		_, err := tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_PromptTyped{
			PromptTyped: &v1.PromptTyped{PromptId: id}}})
		return err
	})
	return claimed, err
}

// requeuePrompt puts back a claimed prompt whose typing never reached the terminal, and says so to the apps.
func (d *Daemon) requeuePrompt(ctx context.Context, a *v1.Agent, p *v1.QueuedPrompt) error {
	return d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		if err := tx.RequeuePrompt(p.Id); err != nil {
			return err
		}
		_, err := tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_PromptQueued{
			PromptQueued: &v1.PromptQueued{Prompt: p}}})
		return err
	})
}

// promptSettle is how long after a prompt was typed nothing more is typed into the agent unless its CLI reported
// activity: the box reads empty again at once, before the turn's first hook makes the agent WORKING.
const promptSettle = 20 * time.Second

func (d *Daemon) notePromptTyped(agentID string, at time.Time) {
	d.rt.mu.Lock()
	defer d.rt.mu.Unlock()
	d.rt.promptTyped[agentID] = at
}

// promptInFlight reports whether a prompt typed into the agent may not have started its turn yet: typed less than
// promptSettle ago, with no activity reported since. Then nothing else (a prompt, a doorbell) is typed after it.
func (d *Daemon) promptInFlight(a *v1.Agent) bool {
	d.rt.mu.Lock()
	defer d.rt.mu.Unlock()
	at, ok := d.rt.promptTyped[a.Id]
	if !ok {
		return false
	}
	if (a.LastActivity != nil && a.LastActivity.AsTime().After(at)) || d.now().Sub(at) >= promptSettle {
		delete(d.rt.promptTyped, a.Id)
		return false
	}
	return true
}

func (d *Daemon) setPromptHeld(agentID, code string) {
	d.rt.mu.Lock()
	defer d.rt.mu.Unlock()
	if code == "" {
		delete(d.rt.promptHeld, agentID)
		return
	}
	d.rt.promptHeld[agentID] = code
}

// promptHold is what an agent's queued prompts wait for now: its state first, else what the last try found.
func (d *Daemon) promptHold(a *v1.Agent) string {
	switch a.State {
	case v1.AgentState_AGENT_STATE_WORKING:
		return "working"
	case v1.AgentState_AGENT_STATE_STARTING:
		return "starting"
	case v1.AgentState_AGENT_STATE_NEEDS_INPUT:
		return "at_prompt"
	case v1.AgentState_AGENT_STATE_LIMITED:
		return "limited"
	case v1.AgentState_AGENT_STATE_EXITED, v1.AgentState_AGENT_STATE_RETIRED:
		return "stopped"
	}
	d.rt.mu.Lock()
	defer d.rt.mu.Unlock()
	return d.rt.promptHeld[a.Id]
}

// ListPrompts lists an agent's queued prompts, oldest first, each with what it waits for.
func (d *Daemon) ListPrompts(ctx context.Context, req *connect.Request[v1.ListPromptsRequest]) (*connect.Response[v1.ListPromptsResponse], error) {
	r := d.store.Read(ctx)
	a, err := d.FindAgent(r, auth.ActorFrom(ctx), req.Msg.Agent)
	if err != nil {
		return nil, err
	}
	prompts, err := r.QueuedPrompts(a.Id)
	if err != nil {
		return nil, err
	}
	hold := d.promptHold(a)
	draft := ""
	if hold == "draft" {
		// What it holds now, for the app to name before it offers to send over it. A box that reads empty by now is no draft.
		if box := d.readInputBox(ctx, a, ""); box.GetBox() == adapterv1.InputBox_INPUT_BOX_TYPED {
			draft = shownDraft(box.GetText())
		} else {
			hold = ""
		}
	}
	for _, p := range prompts {
		p.Held = hold
		p.HeldText = draft
	}
	return connect.NewResponse(&v1.ListPromptsResponse{Prompts: prompts}), nil
}

// WithdrawPrompt takes back a queued prompt (the newest when none is named) before it is typed.
func (d *Daemon) WithdrawPrompt(ctx context.Context, req *connect.Request[v1.WithdrawPromptRequest]) (*connect.Response[v1.WithdrawPromptResponse], error) {
	actor := auth.ActorFrom(ctx)
	a, err := d.FindAgent(d.store.Read(ctx), actor, req.Msg.Agent)
	if err != nil {
		return nil, err
	}
	out := &v1.WithdrawPromptResponse{}
	_, err = d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceWithdrawPromptProcedure, out, func(tx *store.Tx) error {
		id := req.Msg.PromptId
		if id == "" {
			queued, err := tx.QueuedPrompts(a.Id)
			if err != nil {
				return err
			}
			if len(queued) == 0 {
				return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s has no prompt waiting to be typed", a.Name))
			}
			id = queued[len(queued)-1].Id
		}
		p, err := tx.WithdrawPrompt(a.Id, id)
		if errors.Is(err, store.ErrNotFound) {
			return connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("that prompt is no longer waiting: it was typed into %s's terminal, or taken back already", a.Name))
		}
		if err != nil {
			return err
		}
		out.Prompt = p
		_, err = tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_PromptWithdrawn{
			PromptWithdrawn: &v1.PromptWithdrawn{PromptId: p.Id}}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

// How InterruptAgent watches for the agent back at its prompt: a box that reads empty on a screen that stopped
// drawing (a turn's spinner redraws all the time).
const (
	interruptWait  = 6 * time.Second
	interruptQuiet = 1500 * time.Millisecond
	interruptPoll  = 150 * time.Millisecond
	// escCooldown is how long after an Esc no second one is sent unless the agent showed activity since.
	escCooldown = 10 * time.Second
	// escGrace is how long after an Esc the interrupted turn's own last hooks may still come in: activity in it doesn't
	// count as a new turn.
	escGrace = 3 * time.Second
)

func (d *Daemon) noteEsc(agentID string) {
	d.rt.mu.Lock()
	defer d.rt.mu.Unlock()
	d.rt.lastEsc[agentID] = d.now()
}

func (d *Daemon) clearEsc(agentID string) {
	d.rt.mu.Lock()
	defer d.rt.mu.Unlock()
	delete(d.rt.lastEsc, agentID)
}

// escSettling reports whether an Esc whose effect wasn't seen yet (a confirmed stop clears it) was sent to the agent
// less than escCooldown ago with no activity reported since (past escGrace: a late hook of the turn it interrupted is
// not a new turn).
func (d *Daemon) escSettling(a *v1.Agent) bool {
	d.rt.mu.Lock()
	defer d.rt.mu.Unlock()
	at, ok := d.rt.lastEsc[a.Id]
	if !ok || d.now().Sub(at) >= escCooldown || (a.LastActivity != nil && a.LastActivity.AsTime().After(at.Add(escGrace))) {
		delete(d.rt.lastEsc, a.Id)
		return false
	}
	return true
}

// InterruptAgent sends an agent's CLI the key that interrupts its turn, as the user would in its terminal, and waits
// a moment for it to be back at its prompt. Claude Code fires no hook when a turn is interrupted, so the daemon
// records the turn's end itself once it has seen the empty box. Only a CLI whose adapter names its interrupt key may
// be interrupted; a second Esc at an empty box opens Claude Code's rewind menu, so the request's mutation id is
// honoured and an agent not at work gets no key at all.
func (d *Daemon) InterruptAgent(ctx context.Context, req *connect.Request[v1.InterruptAgentRequest]) (*connect.Response[v1.InterruptAgentResponse], error) {
	actor := auth.ActorFrom(ctx)
	found, err := d.FindAgent(d.store.Read(ctx), actor, req.Msg.Agent)
	if err != nil {
		return nil, err
	}
	if IsAgent(actor.Class) && found.RunId != actor.RunId {
		return nil, denied("an agent interrupts only agents of its own run")
	}
	if !IsAgent(actor.Class) && !d.reg.Has("terminal.input", actor.Class) {
		return nil, denied("%s may not type into a terminal", actor.Class)
	}
	precondition := func(format string, args ...any) error {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(format, args...))
	}
	if !d.rt.adapters.HasCapability(found.Kind, claude.CapInterruptEsc) {
		return nil, precondition("%s runs %s, whose interrupt key the daemon doesn't know: stop it in its terminal", found.Name, found.Kind)
	}
	mid := req.Msg.Meta.GetMutationId()
	replay := func() (*connect.Response[v1.InterruptAgentResponse], error) {
		if mid == "" {
			return nil, nil
		}
		proc, resp, err := d.store.Read(ctx).Mutation(actor.Id, mid)
		switch {
		case err == nil && proc != agentmeshv1connect.MeshServiceInterruptAgentProcedure:
			return nil, reusedMutation(mid, proc)
		case err == nil:
			out := &v1.InterruptAgentResponse{}
			if err := proto.Unmarshal(resp, out); err != nil {
				return nil, err
			}
			return connect.NewResponse(out), nil
		case !errors.Is(err, store.ErrNotFound):
			return nil, err
		}
		return nil, nil
	}
	if res, err := replay(); res != nil || err != nil {
		return res, err
	}
	if err := expired(req.Msg.Meta, d.now()); err != nil {
		return nil, err
	}
	mu := d.agentLock(found.Id)
	mu.Lock()
	defer mu.Unlock()
	if res, err := replay(); res != nil || err != nil {
		return res, err
	}
	a, err := d.store.Read(ctx).GetAgent(found.Id)
	if err != nil {
		return nil, err
	}
	if stopped(a) || a.TerminalId == "" || d.rt.term == nil {
		return nil, precondition("%s has no running process to interrupt", a.Name)
	}
	out := &v1.InterruptAgentResponse{}
	switch a.State {
	case v1.AgentState_AGENT_STATE_WORKING, v1.AgentState_AGENT_STATE_NEEDS_INPUT:
		if d.escSettling(a) {
			// An Esc went in moments ago and nothing has happened since: it may still be stopping the turn, and a
			// second Esc at the prompt it leaves would open Claude's rewind menu. Look again instead of sending one.
			if out.Stopped = d.atIdleScreen(ctx, a); out.Stopped {
				d.clearEsc(a.Id)
				if err := d.recordInterrupted(ctx, a.Id); err != nil {
					d.log.Warn("recording an interrupted turn", "agent", a.Name, "err", err)
				}
			}
			break
		}
		if _, err := d.rt.term.API.WriteTerminal(ctx, connect.NewRequest(&termv1.WriteTerminalRequest{Id: a.TerminalId,
			Data: []byte("\x1b")})); err != nil {
			if !writeDidNotHappen(err) {
				d.noteEsc(a.Id) // it may have gone in: no second one for a while
			}
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("sending Esc to %s's terminal: %w", a.Name, err))
		}
		d.noteEsc(a.Id)
		out.Stopped = d.waitForPrompt(ctx, a)
		if out.Stopped {
			d.clearEsc(a.Id) // seen stopped: a later Esc is for a new turn
			if err := d.recordInterrupted(ctx, a.Id); err != nil {
				d.log.Warn("recording an interrupted turn", "agent", a.Name, "err", err)
			}
		}
	default:
		// Not at work: nothing to interrupt, and an Esc here could open a menu.
		out.Stopped = true
	}
	meta := proto.CloneOf(req.Msg.Meta)
	if meta != nil {
		meta.ExpiresAt = nil // the key was sent: a retry must find that, however late
	}
	if _, err := d.write(context.WithoutCancel(ctx), meta, agentmeshv1connect.MeshServiceInterruptAgentProcedure, out,
		func(*store.Tx) error { return nil }); err != nil {
		d.log.Warn("recording an interrupt", "agent", a.Name, "err", err)
	}
	// Its queued prompts and doorbells are due now, as Claude Code sends its own queue after an Esc.
	defer d.deliverSoon(a.Id)
	return connect.NewResponse(out), nil
}

// waitForPrompt watches an agent's screen after its interrupt key: true once its box reads empty and the screen has
// been still for a moment.
func (d *Daemon) waitForPrompt(ctx context.Context, a *v1.Agent) bool {
	end := time.After(interruptWait) // the real clock: a test's fixed d.now() must not make this endless
	for {
		select {
		case <-ctx.Done():
			return false
		case <-end:
			return false
		case <-time.After(interruptPoll):
		}
		if d.atIdleScreen(ctx, a) {
			return true
		}
		if cur, err := d.store.Read(ctx).GetAgent(a.Id); err == nil && atPrompt(cur.State) && cur.State != v1.AgentState_AGENT_STATE_NEEDS_INPUT {
			return true // its own hook said so
		}
	}
}

// atIdleScreen reports whether an agent's screen shows it at its prompt: an empty input box on a screen that hasn't
// drawn for interruptQuiet (a working turn's spinner redraws all the time).
func (d *Daemon) atIdleScreen(ctx context.Context, a *v1.Agent) bool {
	if d.readInputBox(ctx, a, "").GetBox() != adapterv1.InputBox_INPUT_BOX_EMPTY {
		return false
	}
	t, err := d.terminalInfo(ctx, a.TerminalId)
	if err != nil || t == nil {
		return false
	}
	last := t.LastOutput
	return last == nil || time.Since(last.AsTime()) >= interruptQuiet
}

// recordInterrupted ends the turn of an agent seen back at its prompt after an interrupt: a TurnEnded through the
// reducer, as the Stop hook the CLI doesn't fire would have.
func (d *Daemon) recordInterrupted(ctx context.Context, agentID string) error {
	err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		a, err := tx.GetAgent(agentID)
		if err != nil {
			return err
		}
		if a.State != v1.AgentState_AGENT_STATE_WORKING && a.State != v1.AgentState_AGENT_STATE_NEEDS_INPUT {
			return nil // a hook got there first
		}
		return d.applyEvents(tx, a, []*adapterv1.AgentEvent{{Time: timestamppb.New(tx.Now()),
			Kind: &adapterv1.AgentEvent_TurnEnded{TurnEnded: &adapterv1.TurnEnded{}}}})
	})
	if err == nil {
		d.perms.endFor(agentID, "", "") // a permission prompt the Esc dismissed is over, as a Stop hook would say
	}
	return err
}

// SendPromptAnyway types a held prompt although the agent's input box holds a draft: the caller was shown the draft and
// confirmed it may go. It clears the box (Ctrl-U, as PromptAgent does to take back a paste that was never sent) and types
// the prompt. Clearing a box a person may be typing in is the one thing here that can lose their words, so it refuses unless
// all of these hold, read under the agent's lock just before it acts: the prompt still waits; the agent is at its prompt with
// nothing in flight (so the draft is not a prompt Claude is about to take); nobody has typed in its terminal lately
// (`TypingQuiet`); and the box holds exactly the text the caller confirmed. A draft that changed since is shown again and
// not discarded. A retry with the same mutation id gets the first answer.
func (d *Daemon) SendPromptAnyway(ctx context.Context, req *connect.Request[v1.SendPromptAnywayRequest]) (*connect.Response[v1.SendPromptAnywayResponse], error) {
	actor := auth.ActorFrom(ctx)
	if !d.reg.Has("terminal.input", actor.Class) {
		return nil, denied("%s may not type into a terminal", actor.Class)
	}
	if req.Msg.PromptId == "" {
		return nil, invalid("name the prompt")
	}
	mid := req.Msg.Meta.GetMutationId()
	replay := func() (*connect.Response[v1.SendPromptAnywayResponse], error) {
		if mid == "" {
			return nil, nil
		}
		proc, resp, err := d.store.Read(ctx).Mutation(actor.Id, mid)
		switch {
		case err == nil && proc != agentmeshv1connect.MeshServiceSendPromptAnywayProcedure:
			return nil, reusedMutation(mid, proc)
		case err == nil:
			out := &v1.SendPromptAnywayResponse{}
			if err := proto.Unmarshal(resp, out); err != nil {
				return nil, err
			}
			return connect.NewResponse(out), nil
		case !errors.Is(err, store.ErrNotFound):
			return nil, err
		}
		return nil, nil
	}
	if res, err := replay(); res != nil || err != nil {
		return res, err
	}
	// After the replay: a retry of a send that was done answers with it, even past its expiry.
	if err := expired(req.Msg.Meta, d.now()); err != nil {
		return nil, err
	}
	found, err := d.FindAgent(d.store.Read(ctx), actor, req.Msg.Agent)
	if err != nil {
		return nil, err
	}
	precondition := func(format string, args ...any) error {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(format, args...))
	}
	mu := d.agentLock(found.Id)
	mu.Lock()
	defer mu.Unlock()
	if res, err := replay(); res != nil || err != nil {
		return res, err
	}
	a, err := d.store.Read(ctx).GetAgent(found.Id)
	if err != nil {
		return nil, err
	}
	if a.TerminalId == "" || d.rt.term == nil {
		return nil, precondition("%s has no terminal", a.Name)
	}
	queued, err := d.store.Read(ctx).QueuedPrompts(a.Id)
	if err != nil {
		return nil, err
	}
	var prompt *v1.QueuedPrompt
	for _, p := range queued {
		if p.Id == req.Msg.PromptId {
			prompt = p
		}
	}
	if prompt == nil {
		return nil, precondition("that prompt is no longer waiting: it was typed into %s's terminal, or taken back", a.Name)
	}
	if queued[0].Id != prompt.Id {
		return nil, precondition("an older prompt waits before this one: send or take back that one first (they are typed in the order they were sent)")
	}
	if !takesDoorbell(a.State) || d.ringOf(a.Id) != nil || d.promptInFlight(a) || a.PendingReopen != nil || a.PendingCompaction != nil || a.PendingReset != nil {
		return nil, precondition("%s is not at its prompt now: the prompt is typed once it is", a.Name)
	}
	typing, err := d.userTyping(ctx, a.TerminalId)
	if err != nil {
		return nil, err
	}
	if typing {
		return nil, precondition("someone is typing in %s's terminal: try again when they stop", a.Name)
	}
	box := d.readInputBox(ctx, a, "")
	now := squeezed(box.GetText()) // all of it: a draft is confirmed only as a whole
	switch box.GetBox() {
	case adapterv1.InputBox_INPUT_BOX_EMPTY:
		now = "" // nothing to clear any more: it is typed as it would have been
	case adapterv1.InputBox_INPUT_BOX_TYPED:
		if now != squeezed(req.Msg.Draft) {
			return nil, precondition("the draft in %s's box changed since you saw it (it holds: %s): look at it and send again", a.Name, shownDraft(now))
		}
	default:
		return nil, precondition("%s's screen shows no input box (a dialog may be open in the terminal)", a.Name)
	}
	claimed, err := d.claimPrompt(ctx, a, prompt.Id)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return nil, precondition("that prompt is no longer waiting: it was typed into %s's terminal, or taken back", a.Name)
	}
	d.setPromptHeld(a.Id, "")
	put := func(cause error) error {
		if rerr := d.requeuePrompt(context.WithoutCancel(ctx), a, prompt); rerr != nil {
			d.log.Warn("putting a prompt back in the queue", "agent", a.Name, "err", rerr)
		}
		if now != "" {
			d.setPromptHeld(a.Id, "draft") // what it waits for stays true until the box is clear
		}
		return cause
	}
	if now != "" && !d.clearInputBox(ctx, a) {
		return nil, put(connect.NewError(connect.CodeUnavailable, fmt.Errorf("could not clear the draft in %s's box (part of it may remain: clear it in the terminal)", a.Name)))
	}
	at := d.now()
	if err := d.typePrompt(ctx, a, prompt.Text); err != nil {
		if writeDidNotHappen(err) {
			return nil, put(connect.NewError(connect.CodeUnavailable, fmt.Errorf("typing into %s's terminal: %w", a.Name, err)))
		}
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("typing into %s's terminal: %w", a.Name, err))
	}
	d.notePromptTyped(a.Id, at)
	meta := proto.CloneOf(req.Msg.Meta)
	if meta != nil {
		meta.ExpiresAt = nil
	}
	// The answer to this caller carries the draft that was cleared; the stored one, which a retry gets, does not keep it (it may
	// hold a secret typed in the terminal).
	if _, err := d.write(context.WithoutCancel(ctx), meta, agentmeshv1connect.MeshServiceSendPromptAnywayProcedure, &v1.SendPromptAnywayResponse{},
		func(*store.Tx) error { return nil }); err != nil {
		d.log.Warn("recording a prompt sent over a draft", "agent", a.Name, "err", err)
	}
	return connect.NewResponse(&v1.SendPromptAnywayResponse{Discarded: shownDraft(now)}), nil
}
