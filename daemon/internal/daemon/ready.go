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

// settleCap is how many ReadySettle periods after its first output a terminal that never goes quiet is marked
// idle anyway.
const settleCap = 30

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
		if tm == nil || tm.Exited || tm.Offset == 0 || tm.LastOutput == nil {
			continue
		}
		// Idle once its output has been quiet for settle: a TUI still drawing may drop what is typed into it. The
		// cap, counted from the terminal's first output, is only a backstop for one that never stops redrawing
		// (a clock, a spinner) and would otherwise never get its first doorbell.
		d.rt.mu.Lock()
		first, seen := d.rt.firstOutput[tm.Id]
		if !seen {
			first = tm.LastOutput.AsTime()
			d.rt.firstOutput[tm.Id] = first
		}
		d.rt.mu.Unlock()
		if now.Sub(tm.LastOutput.AsTime()) < settle && now.Sub(first) < settleCap*settle {
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
		if err == nil {
			d.rt.mu.Lock()
			delete(d.rt.firstOutput, tm.Id)
			d.rt.mu.Unlock()
		}
		if err == nil && becameIdle {
			d.deliverSoon(a.Id)
		}
	}
}

// learnSession finds the conversation id of an agent no hook reported one for yet, through its adapter
// (LookupSession reads the CLI's own stores, which may be slow), and records it. It runs before the write
// transaction of the action that needs the id, never inside one: the daemon's write lock is not held while a CLI's
// SQLite db or history is read, and the caller's deadline holds. ref names the agent as the action does ("parent"
// for the actor's parent). It looks only for an agent the actor may act on (an agent: its parent or a child of
// its own) and only when need says the action will use the id (a reopen or a resume): the newest conversation in
// a worktree is the agent's only by assumption, so it is not taken when nothing needs it.
func (d *Daemon) learnSession(ctx context.Context, actor *v1.Actor, ref string, need func(*v1.Agent) bool) {
	if actor == nil {
		return
	}
	r := d.store.Read(ctx)
	var self, a *v1.Agent
	var err error
	if IsAgent(actor.Class) {
		if self, err = d.getAgent(r, actor.Id); err != nil {
			return
		}
	}
	if ref == "parent" {
		if self == nil || self.ParentId == "" {
			return
		}
		a, err = d.getAgent(r, self.ParentId)
	} else {
		a, err = d.findAgentCrossRun(r, actor, ref)
	}
	if err != nil || a.SessionId != "" || !d.rt.adapters.HasCapability(a.Kind, "session-lookup") {
		return
	}
	if self != nil && a.Id != self.ParentId && a.ParentId != self.Id &&
		!(actor.Class == v1.ActorClass_ACTOR_CLASS_ROOT && actor.RunId != "" && a.Role != v1.AgentRole_AGENT_ROLE_ROOT) {
		return
	}
	if !need(a) {
		return
	}
	ad, err := d.rt.adapters.Get(a.Kind)
	if err != nil {
		return
	}
	res, err := ad.LookupSession(ctx, connect.NewRequest(&adapterv1.LookupSessionRequest{
		AgentId:   a.Id,
		Cwd:       a.Worktree,
		Repo:      a.Repo,
		Env:       d.agentEnv(a, ""),
		NotBefore: a.CreatedAt,
	}))
	if err != nil || res.Msg.SessionId == "" {
		return
	}
	if err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		cur, err := tx.GetAgent(a.Id)
		if err != nil || cur.SessionId != "" {
			return err // a hook reported one meanwhile: it wins
		}
		cur.SessionId = res.Msg.SessionId
		return tx.PutAgent(cur)
	}); err != nil {
		d.log.Warn("session lookup", "agent", a.Name, "err", err)
	}
}
