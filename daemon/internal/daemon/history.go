package daemon

import (
	"context"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// modelHistory lists the models an agent ran on, oldest first, from its run's event log: its spawn, then each reopen
// that moved its model or effort (a reopen that changed neither, a resume or a reset, adds no row). An agent with no
// spawn event (adopted) starts at its first reopen.
func modelHistory(ctx context.Context, r store.Reader, a *v1.Agent) ([]*v1.ModelSwap, error) {
	evs, err := r.ListEvents(a.RunId)
	if err != nil {
		return nil, err
	}
	var out []*v1.ModelSwap
	var model, effort string
	// A reopen writes its model and effort to the agent as they are (an empty effort clears it), so they are told as they are.
	add := func(ev *v1.Event, m, e, why string) {
		if len(out) > 0 && m == model && e == effort {
			return
		}
		model, effort = m, e
		out = append(out, &v1.ModelSwap{Time: ev.Time, Model: m, Effort: e, Why: why})
	}
	for _, ev := range evs {
		if ev.AgentId != a.Id && ev.GetAgentSpawned().GetAgent().GetId() != a.Id {
			continue
		}
		switch p := ev.Payload.(type) {
		case *v1.Event_AgentSpawned:
			add(ev, p.AgentSpawned.Agent.Model, p.AgentSpawned.Agent.Effort, "spawned")
		case *v1.Event_AgentReopened:
			if ro := p.AgentReopened.Reopen; ro != nil {
				add(ev, ro.Model, ro.Effort, ro.Why)
			}
		}
	}
	return out, nil
}
