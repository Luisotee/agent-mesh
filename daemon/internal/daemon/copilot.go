package daemon

import (
	"context"
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
		d.log.Warn("copilot trust dialog is still up after the Enter presses; the agent stays starting", "agent", a.Name)
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
	for _, a := range agents {
		if ctx.Err() != nil {
			return
		}
		if a.Kind != copilot.Kind || stopped(a) || a.TerminalId == "" || a.SessionId == "" ||
			a.State == v1.AgentState_AGENT_STATE_STARTING {
			continue
		}
		d.pollCopilotAgent(ctx, a)
	}
}

func (d *Daemon) pollCopilotAgent(ctx context.Context, a *v1.Agent) {
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
		// follows is new. The state the agent has is the baseline.
		ps = &copilotPoll{terminalID: a.TerminalId, key: st.Key}
		if a.State == v1.AgentState_AGENT_STATE_WORKING {
			ps.state = "working"
		} else {
			ps.state = "idle"
		}
		if st.Limit != nil {
			ps.limitKey = st.Key
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
	if limited == nil && st.State != "idle" && a.State != v1.AgentState_AGENT_STATE_LIMITED {
		// A rate limit shows on the screen only; one from before the newest prompt was answered is old.
		if raw, serr := d.readScreen(ctx, a.TerminalId); serr == nil {
			// A limit line already reported for this prompt (a `continue` resumed the agent, the line is still on
			// screen) is old: it must not hold back the turns that follow.
			if l, ok := copilot.ScreenLimit(copilot.ScreenLines(raw)); ok && "screen:"+l.Message+"@"+st.Key != prevLimit {
				screenLimit = &l
				limited = &l
			}
		}
	}
	switch {
	case limited != nil:
		key := st.Key
		if screenLimit != nil {
			key = "screen:" + screenLimit.Message + "@" + st.Key
		}
		if a.State != v1.AgentState_AGENT_STATE_LIMITED && key != prevLimit {
			at := limited.At
			if at.IsZero() {
				at = now
			}
			ev(&adapterv1.AgentEvent{Time: timestamppb.New(now), Kind: &adapterv1.AgentEvent_RateLimited{
				RateLimited: &adapterv1.RateLimited{Message: limited.Message, ResetAt: timestamppb.New(at.Add(limited.Hold)),
					AllModels: true}}})
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

	if len(evs) > 0 {
		if err := d.applyCopilotEvents(ctx, a.Id, a.TerminalId, evs); err != nil {
			return
		}
	}
	// It answered after a hold was set: the quota is back (topped up, or the policy changed), so lift the hold.
	if st.Limit == nil && !st.AnsweredAt.IsZero() {
		d.liftCopilotHolds(ctx, st.AnsweredAt)
	}
	d.rt.copilot.mu.Lock()
	if cur := d.rt.copilot.polled[a.Id]; cur != nil && cur.terminalID == a.TerminalId {
		cur.key, cur.state, cur.limitKey = nextKey, nextState, nextLimit
	}
	d.rt.copilot.mu.Unlock()
}

// liftCopilotHolds deletes the Copilot holds recorded before answeredAt.
func (d *Daemon) liftCopilotHolds(ctx context.Context, answeredAt time.Time) {
	holds, err := d.store.Read(ctx).ModelLimits(copilot.Kind)
	if err != nil {
		return
	}
	var stale []store.ModelLimit
	for _, h := range holds {
		if h.UpdatedAt.Before(answeredAt) && h.Until.After(d.now()) {
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
