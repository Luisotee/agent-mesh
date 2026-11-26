package daemon

import (
	"context"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/copilot"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// Copilot fires no hooks, so the daemon reads its turns from the session's events file and its limits from the
// events (a 402 quota, a 403 policy refusal) and the screen (a rate limit), as `bin/mesh`'s CopilotCli did
// (design §22). It also answers the folder-trust dialog, which neither `--yolo` nor `--add-dir` skips.

// Provider credentials passed to Copilot agents only: its state folder and the tokens it reads.
var copilotInheritedEnv = []string{"COPILOT_HOME", "COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"}

// copilotTrustTries bounds the Enter presses a terminal gets for the trust dialog: one answers it, the rest
// cover a keypress lost while the TUI was still drawing.
const copilotTrustTries = 5

// copilotTrustUnanswered is the detail of a Copilot agent whose trust dialog the Enter presses did not answer.
const copilotTrustUnanswered = "folder trust dialog not answered"

type copilotPoll struct {
	terminalID string
	key        string // the newest event the agent's state was driven by
	state      string // "working" or "idle" as last applied
	limitKey   string // the limit event or screen line already reported
}

type copilotRuntime struct {
	mu     sync.Mutex
	polled map[string]*copilotPoll // agent id → what the last poll applied
	trust  map[string]int          // terminal id → Enter presses given to its trust dialog
}

func (d *Daemon) clearCopilotTerminal(terminalID string) {
	if d == nil || d.rt == nil || terminalID == "" {
		return
	}
	d.rt.copilot.mu.Lock()
	for id, ps := range d.rt.copilot.polled {
		if ps.terminalID == terminalID {
			delete(d.rt.copilot.polled, id)
		}
	}
	delete(d.rt.copilot.trust, terminalID)
	d.rt.copilot.mu.Unlock()
}

// copilotTrustGate answers a folder-trust dialog on a starting Copilot's screen (Enter takes "1. Yes": this
// session only, nothing written to Copilot's config) and reports whether the dialog is up, so the agent isn't
// marked idle (and a doorbell typed into the dialog) until it is gone.
func (d *Daemon) copilotTrustGate(ctx context.Context, a *v1.Agent) bool {
	raw, err := d.readScreen(ctx, a.TerminalId)
	if err != nil || !copilot.TrustDialog(copilot.ScreenLines(raw)) {
		return false
	}
	d.rt.copilot.mu.Lock()
	if d.rt.copilot.trust == nil {
		d.rt.copilot.trust = map[string]int{}
	}
	tries := d.rt.copilot.trust[a.TerminalId]
	d.rt.copilot.trust[a.TerminalId]++
	d.rt.copilot.mu.Unlock()
	if tries == copilotTrustTries {
		// The agent is not stuck silently: it needs its human (or its parent's), and says what for. Once the
		// dialog is gone the readiness pass takes it on to idle (settleStartingAgents).
		d.log.Warn("copilot trust dialog is still up after the Enter presses", "agent", a.Name)
		_ = d.applyCopilotEvents(ctx, a.Id, a.TerminalId, []*adapterv1.AgentEvent{{Time: timestamppb.New(d.now()),
			Kind: &adapterv1.AgentEvent_NeedsInput{NeedsInput: &adapterv1.NeedsInput{Reason: copilotTrustUnanswered}}}})
	}
	if tries < copilotTrustTries {
		if _, err := d.rt.term.API.WriteTerminal(ctx, connect.NewRequest(&termv1.WriteTerminalRequest{Id: a.TerminalId,
			Data: []byte("\r")})); err != nil {
			d.log.Warn("copilot trust dialog", "agent", a.Name, "err", err)
		}
	}
	return true
}

// pollCopilot reads each live Copilot agent's events file and screen: turn starts and ends, and the limits it ran
// into. A 402 (quota) or a 403 (policy) holds every Copilot model for a day or an hour; a newer answer lifts a hold
// from before it.
func (d *Daemon) pollCopilot(ctx context.Context) {
	if d.rt == nil || d.rt.term == nil {
		return
	}
	agents, err := d.store.Read(ctx).ListAgents("")
	if err != nil {
		return
	}
	var live []*v1.Agent
	for _, a := range agents {
		if a.Kind == copilot.Kind && !stopped(a) && a.TerminalId != "" && a.SessionId != "" &&
			a.State != v1.AgentState_AGENT_STATE_STARTING {
			live = append(live, a)
		}
	}
	if len(live) == 0 {
		return
	}
	res, err := d.rt.term.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
	if err != nil {
		return
	}
	started := map[string]time.Time{}
	for _, tm := range res.Msg.Terminals {
		if tm.StartedAt != nil && !tm.Exited {
			started[tm.Id] = tm.StartedAt.AsTime()
		}
	}
	for _, a := range live {
		if ctx.Err() != nil {
			return
		}
		if at, ok := started[a.TerminalId]; ok {
			d.pollCopilotAgent(ctx, a, at)
		}
	}
}

func (d *Daemon) pollCopilotAgent(ctx context.Context, a *v1.Agent, started time.Time) {
	events, err := copilot.ReadEvents(copilot.EventsPath(d.agentEnv(a, ""), a.SessionId))
	if err != nil {
		return
	}
	st := copilot.Inspect(events)
	now := d.now()

	d.rt.copilot.mu.Lock()
	if d.rt.copilot.polled == nil {
		d.rt.copilot.polled = map[string]*copilotPoll{}
	}
	ps := d.rt.copilot.polled[a.Id]
	if ps == nil || ps.terminalID != a.TerminalId {
		// A fresh process (or a daemon restart): its events file holds what an earlier process did, so only what
		// follows the process's start is new (a prompt typed before the first poll is not). The baseline is the
		// events up to that start.
		var older []copilot.Event
		for _, e := range events {
			if t := e.Time(); t.IsZero() || !t.After(started) {
				older = append(older, e)
			}
		}
		old := copilot.Inspect(older)
		ps = &copilotPoll{terminalID: a.TerminalId, key: old.Key}
		if a.State == v1.AgentState_AGENT_STATE_WORKING {
			ps.state = "working"
		} else {
			ps.state = "idle"
		}
		if old.Limit != nil {
			ps.limitKey = old.Key
		}
		d.rt.copilot.polled[a.Id] = ps
	}
	prevKey, prevState, prevLimit := ps.key, ps.state, ps.limitKey
	d.rt.copilot.mu.Unlock()

	var evs []*adapterv1.AgentEvent
	nextKey, nextState, nextLimit := prevKey, prevState, prevLimit
	ev := func(e *adapterv1.AgentEvent) { evs = append(evs, e) }

	limited := st.Limit
	var screenLimit *copilot.Limit
	screenGone := false
	if limited == nil && st.State != "idle" && a.State != v1.AgentState_AGENT_STATE_LIMITED {
		// A rate limit shows on the screen only. A line already reported (a `continue` resumed the agent and the
		// line is still among the last ones, however many polls its turn takes) is old: it must not limit the
		// turns that follow. It counts as new again once it has left the screen.
		if raw, serr := d.readScreen(ctx, a.TerminalId); serr == nil {
			l, ok := copilot.ScreenLimit(copilot.ScreenLines(raw))
			switch {
			case !ok:
				screenGone = true
			case "screen:"+l.Key() != prevLimit:
				screenLimit = &l
				limited = &l
			}
		}
	}
	switch {
	case limited != nil:
		key := st.Key
		if screenLimit != nil {
			key = "screen:" + screenLimit.Key()
		}
		if a.State != v1.AgentState_AGENT_STATE_LIMITED && key != prevLimit {
			at := limited.At
			if at.IsZero() {
				at = now
			}
			ev(&adapterv1.AgentEvent{Time: timestamppb.New(now), Kind: &adapterv1.AgentEvent_RateLimited{
				RateLimited: &adapterv1.RateLimited{Message: limited.Message, ResetAt: timestamppb.New(at.Add(limited.Hold)),
					AllModels: !limited.PerModel}}})
		}
		nextLimit = key
	case st.State != "" && (st.Key != prevKey || (st.State == "working" && a.State != v1.AgentState_AGENT_STATE_WORKING)):
		if st.State == "working" {
			if a.State != v1.AgentState_AGENT_STATE_WORKING {
				ev(&adapterv1.AgentEvent{Time: timestamppb.New(now), Kind: &adapterv1.AgentEvent_TurnStarted{TurnStarted: &adapterv1.TurnStarted{}}})
			} else {
				ev(&adapterv1.AgentEvent{Time: timestamppb.New(now), Kind: &adapterv1.AgentEvent_Activity{Activity: &adapterv1.Activity{Detail: "copilot"}}})
			}
		} else if st.State == "idle" {
			if a.State != v1.AgentState_AGENT_STATE_WORKING {
				// An answered prompt the daemon never saw start (an agent at a limit that answered a `continue`).
				ev(&adapterv1.AgentEvent{Time: timestamppb.New(now), Kind: &adapterv1.AgentEvent_TurnStarted{TurnStarted: &adapterv1.TurnStarted{}}})
			}
			ev(&adapterv1.AgentEvent{Time: timestamppb.New(now), Kind: &adapterv1.AgentEvent_TurnEnded{TurnEnded: &adapterv1.TurnEnded{}}})
		}
		nextKey, nextState = st.Key, st.State
	}

	if screenGone && strings.HasPrefix(nextLimit, "screen:") {
		nextLimit = ""
	}
	if len(evs) > 0 {
		if err := d.applyCopilotEvents(ctx, a.Id, a.TerminalId, evs); err != nil {
			return
		}
	}
	// It answered a prompt typed after a hold was set: the quota is back (topped up, or the policy changed), so
	// lift the hold. An answer to an earlier prompt (a turn that was running when the quota ran out) says nothing.
	if st.Limit == nil && !st.PromptAt.IsZero() {
		d.liftCopilotHolds(ctx, st.PromptAt, a.Model)
	}
	d.rt.copilot.mu.Lock()
	if cur := d.rt.copilot.polled[a.Id]; cur != nil && cur.terminalID == a.TerminalId {
		cur.key, cur.state, cur.limitKey = nextKey, nextState, nextLimit
	}
	d.rt.copilot.mu.Unlock()
}

// liftCopilotHolds deletes the Copilot holds an answer disproves: those recorded before promptAt, the time of the
// prompt that was answered, on the model that answered (a per-model hold) or on every model (`holdAll`: a quota or
// policy hold is the account's, so an answer on any model disproves it). Another model's hold stays: this answer
// says nothing of it.
func (d *Daemon) liftCopilotHolds(ctx context.Context, promptAt time.Time, model string) {
	holds, err := d.store.Read(ctx).ModelLimits(copilot.Kind)
	if err != nil {
		return
	}
	var stale []store.ModelLimit
	for _, h := range holds {
		if (h.Model == holdAll || h.Model == model) && h.UpdatedAt.Before(promptAt) && h.Until.After(d.now()) {
			stale = append(stale, h)
		}
	}
	if len(stale) == 0 {
		return
	}
	if err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		for _, h := range stale {
			if err := tx.DeleteModelLimit(h.Kind, h.Model); err != nil {
				return err
			}
		}
		return nil
	}); err != nil && ctx.Err() == nil {
		d.log.Warn("copilot hold lift", "err", err)
	}
}

func (d *Daemon) applyCopilotEvents(ctx context.Context, agentID, terminalID string, evs []*adapterv1.AgentEvent) error {
	var ready bool
	err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		cur, err := tx.GetAgent(agentID)
		if err != nil || stopped(cur) || cur.TerminalId != terminalID {
			return err
		}
		before := cur.State
		tx.Actor = &v1.Actor{Class: ClassOf(cur.Role), Id: cur.Id, Name: cur.Name, RunId: cur.RunId}
		if err := d.applyEvents(tx, cur, evs); err != nil {
			return err
		}
		ready = takesDoorbell(cur.State) && cur.State != before
		return nil
	})
	if err != nil {
		if ctx.Err() == nil {
			d.log.Warn("copilot poll", "agent", agentID, "err", err)
		}
		return err
	}
	if ready {
		d.deliverSoon(agentID)
	}
	return nil
}
