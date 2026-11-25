package daemon

import (
	"context"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// settleStartingAgents marks STARTING agents idle once their terminal has drawn initial output and settled,
// for adapters that lack a startup hook (e.g. agy).
func (d *Daemon) settleStartingAgents(ctx context.Context) {
	if d.rt.term == nil {
		return
	}
	agents, err := d.store.Read(ctx).ListAgents("")
	if err != nil {
		return
	}
	var need []*v1.Agent
	for _, a := range agents {
		if a.State == v1.AgentState_AGENT_STATE_STARTING && a.TerminalId != "" &&
			!d.rt.adapters.HasCapability(a.Kind, "startup-hook") {
			need = append(need, a)
		}
	}
	if len(need) == 0 {
		return
	}
	res, err := d.rt.term.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
	if err != nil {
		return
	}
	byID := make(map[string]*termv1.Terminal, len(res.Msg.Terminals))
	for _, tm := range res.Msg.Terminals {
		byID[tm.Id] = tm
	}
	now := d.now()
	settle := d.rt.cfg.ReadySettle
	for _, a := range need {
		tm := byID[a.TerminalId]
		if tm == nil || tm.Exited || tm.Offset == 0 {
			continue
		}
		d.rt.mu.Lock()
		first, seen := d.rt.firstOutput[tm.Id]
		if !seen {
			first = now
			d.rt.firstOutput[tm.Id] = first
		}
		d.rt.mu.Unlock()
		last := first
		if tm.LastOutput != nil {
			last = tm.LastOutput.AsTime()
		}
		if now.Sub(last) < settle && now.Sub(first) < 5*settle {
			continue
		}
		var becameIdle bool
		err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
			cur, err := tx.GetAgent(a.Id)
			if err != nil || cur.State != v1.AgentState_AGENT_STATE_STARTING {
				return err
			}
			tx.Actor = &v1.Actor{Class: ClassOf(cur.Role), Id: cur.Id, Name: cur.Name, RunId: cur.RunId}
			ev := &adapterv1.AgentEvent{
				Time: timestamppb.New(tx.Now()),
				Kind: &adapterv1.AgentEvent_SessionStarted{
					SessionStarted: &adapterv1.SessionStarted{Source: "startup"},
				},
			}
			if err := d.applyEvents(tx, cur, []*adapterv1.AgentEvent{ev}); err != nil {
				return err
			}
			becameIdle = cur.State == v1.AgentState_AGENT_STATE_IDLE
			return nil
		})
		if err == nil && becameIdle {
			d.deliverSoon(a.Id)
		}
	}
}

// lookupSession asks the agent's adapter to find its conversation ID from local CLI stores when no hook has
// reported one yet.
func (d *Daemon) lookupSession(ctx context.Context, a *v1.Agent) string {
	ad, err := d.rt.adapters.Get(a.Kind)
	if err != nil {
		return ""
	}
	res, err := ad.LookupSession(ctx, connect.NewRequest(&adapterv1.LookupSessionRequest{
		AgentId:   a.Id,
		AgentName: a.Name,
		RunId:     a.RunId,
		Cwd:       a.Worktree,
		Repo:      a.Repo,
		Env:       d.agentEnv(a, ""),
	}))
	if err != nil {
		return ""
	}
	return res.Msg.SessionId
}
