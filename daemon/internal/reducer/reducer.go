// Package reducer derives an agent's state from the neutral events its adapter produces. It is pure: the same
// events always give the same state, whatever CLI they came from, live or replayed.
package reducer

import (
	"time"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// State is what the reducer keeps per agent.
type State struct {
	State        v1.AgentState
	NeedsInput   int32
	Detail       string
	SessionID    string
	LastActivity time.Time
	ExitCode     int32
	// BeforeCompact is the state a compaction interrupted, restored when the compacted session starts.
	BeforeCompact v1.AgentState
}

// Start is a freshly launched agent's state.
func Start(now time.Time) State {
	return State{State: v1.AgentState_AGENT_STATE_STARTING, LastActivity: now}
}

// Of reads the reducer's state out of an agent record.
func Of(a *v1.Agent) State {
	s := State{State: a.State, NeedsInput: a.NeedsInput, Detail: a.StateDetail, SessionID: a.SessionId,
		ExitCode: a.ExitCode, BeforeCompact: a.StateBeforeCompact}
	if a.LastActivity != nil {
		s.LastActivity = a.LastActivity.AsTime()
	}
	return s
}

// Reduce applies one event. Every state has an event that leaves it, except EXITED, which is final. A decode
// error changes nothing: it is never read as a turn's end.
func Reduce(s State, ev *adapterv1.AgentEvent) State {
	if s.State == v1.AgentState_AGENT_STATE_EXITED {
		return s
	}
	if _, bad := ev.Kind.(*adapterv1.AgentEvent_DecodeError); bad || ev.Kind == nil {
		return s
	}
	if t := ev.Time; t != nil && t.AsTime().After(s.LastActivity) {
		s.LastActivity = t.AsTime()
	}
	switch k := ev.Kind.(type) {
	case *adapterv1.AgentEvent_SessionStarted:
		// A fresh session (a start or a /clear) waits for a prompt. A compaction or a resume says nothing of the
		// turn: it may come mid-turn, and the agent is not idle then. "turn" means the CLI only reports SessionStart
		// on the first prompt (agy): record the id, and move only STARTING -> IDLE.
		switch src := k.SessionStarted.Source; {
		case src == "compact" && s.BeforeCompact != v1.AgentState_AGENT_STATE_UNSPECIFIED:
			// The compaction ended: back to what it interrupted (idle between turns, working mid-turn).
			s.State, s.Detail = s.BeforeCompact, ""
			if s.State != v1.AgentState_AGENT_STATE_NEEDS_INPUT {
				// A count of prompts left from before the compaction would show a working or idle agent as needing input.
				s.NeedsInput = 0
			}
		case src != "compact" && src != "resume" && src != "turn", s.State == v1.AgentState_AGENT_STATE_STARTING:
			s.State, s.NeedsInput, s.Detail = v1.AgentState_AGENT_STATE_IDLE, 0, ""
		}
		if src := k.SessionStarted.Source; src != "resume" {
			s.BeforeCompact = v1.AgentState_AGENT_STATE_UNSPECIFIED
		}
		if k.SessionStarted.SessionId != "" {
			s.SessionID = k.SessionStarted.SessionId
		}
	case *adapterv1.AgentEvent_TurnStarted:
		s.State, s.NeedsInput, s.Detail, s.BeforeCompact = v1.AgentState_AGENT_STATE_WORKING, 0, "", v1.AgentState_AGENT_STATE_UNSPECIFIED
	case *adapterv1.AgentEvent_Activity:
		// A tool ran: the turn goes on, and a prompt it waited on was answered.
		s.State, s.NeedsInput, s.Detail, s.BeforeCompact = v1.AgentState_AGENT_STATE_WORKING, 0, k.Activity.Detail, v1.AgentState_AGENT_STATE_UNSPECIFIED
	case *adapterv1.AgentEvent_NeedsInput:
		s.State, s.Detail = v1.AgentState_AGENT_STATE_NEEDS_INPUT, k.NeedsInput.Reason
		s.NeedsInput++
	case *adapterv1.AgentEvent_TurnEnded:
		s.State, s.NeedsInput, s.Detail, s.BeforeCompact = v1.AgentState_AGENT_STATE_IDLE, 0, "", v1.AgentState_AGENT_STATE_UNSPECIFIED
	case *adapterv1.AgentEvent_RateLimited:
		msg := k.RateLimited.Message
		if msg == "" {
			msg = "rate limited"
		}
		s.State, s.NeedsInput, s.Detail, s.BeforeCompact = v1.AgentState_AGENT_STATE_LIMITED, 0, msg, v1.AgentState_AGENT_STATE_UNSPECIFIED
	case *adapterv1.AgentEvent_TurnFailed:
		msg := k.TurnFailed.Error
		if msg == "" {
			msg = "error"
		}
		s.State, s.NeedsInput, s.Detail, s.BeforeCompact = v1.AgentState_AGENT_STATE_ERROR, 0, msg, v1.AgentState_AGENT_STATE_UNSPECIFIED
	case *adapterv1.AgentEvent_Compacting:
		if s.BeforeCompact == v1.AgentState_AGENT_STATE_UNSPECIFIED {
			s.BeforeCompact = s.State
		}
		s.State, s.Detail = v1.AgentState_AGENT_STATE_WORKING, "compacting"
	case *adapterv1.AgentEvent_SessionEnded:
		// The process may go on (a /clear starts the next session); only its exit ends the agent.
		s.NeedsInput, s.Detail = 0, "session ended: "+k.SessionEnded.Reason
	case *adapterv1.AgentEvent_ProcessExited:
		s.State, s.NeedsInput, s.ExitCode = v1.AgentState_AGENT_STATE_EXITED, 0, k.ProcessExited.ExitCode
		s.Detail = ""
		if k.ProcessExited.Signal != 0 {
			s.Detail = "killed by signal"
		}
	}
	return s
}

// Display is the state to show at now: a working agent, or one waiting on a prompt, that nothing was heard from
// for unknownAfter reads as UNKNOWN. lastOutput is the terminal's newest output, which counts as a sign of life.
// A prompt is aged out too because Claude may fire no hook when one is dismissed (Esc): the agent would show
// NEEDS_INPUT for good. The cost: a prompt a person leaves unanswered past unknownAfter shows UNKNOWN, still a
// state that needs a look.
func Display(s State, now, lastOutput time.Time, unknownAfter time.Duration) v1.AgentState {
	if (s.State != v1.AgentState_AGENT_STATE_WORKING && s.State != v1.AgentState_AGENT_STATE_NEEDS_INPUT) || unknownAfter <= 0 {
		return s.State
	}
	last := s.LastActivity
	if lastOutput.After(last) {
		last = lastOutput
	}
	if now.Sub(last) > unknownAfter {
		return v1.AgentState_AGENT_STATE_UNKNOWN
	}
	return s.State
}
