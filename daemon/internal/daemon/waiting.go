package daemon

import (
	"context"
	"sort"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
)

// before is the store's order of messages: time to the nanosecond, then id as bytes.
func before(a, b *v1.Message) bool {
	ta, tb := a.CreatedAt.AsTime(), b.CreatedAt.AsTime()
	if !ta.Equal(tb) {
		return ta.Before(tb)
	}
	return a.Id < b.Id
}

// leadFirst orders a run's agents the way the apps do: the lead (a root) first, then by start, then by name.
func leadFirst(agents []*v1.Agent) {
	sort.SliceStable(agents, func(i, j int) bool {
		a, b := agents[i], agents[j]
		if ar, br := a.Role == v1.AgentRole_AGENT_ROLE_ROOT, b.Role == v1.AgentRole_AGENT_ROLE_ROOT; ar != br {
			return ar
		}
		if !a.CreatedAt.AsTime().Equal(b.CreatedAt.AsTime()) {
			return a.CreatedAt.AsTime().Before(b.CreatedAt.AsTime())
		}
		return a.Name < b.Name
	})
}

func waitingKind(m *v1.Message) v1.WaitingKind {
	if m.Type == v1.MessageType_MESSAGE_TYPE_PLAN {
		return v1.WaitingKind_WAITING_KIND_PLAN
	}
	return v1.WaitingKind_WAITING_KIND_QUESTION
}

// ListWaiting lists what waits on the user, each marked `for_user` when the user is the one to act (the rest is what
// the run's lead handles: a worker's plan or question to a lead the user lets decide). In an open run: a plan or a question sent to the run's lead that no
// answer to its author followed, unless the author has exited (nobody is left to hear the answer; a retired one
// is woken by the answer, so it still counts); and every agent stopped at a prompt, by the state it shows (a
// prompt left unanswered past the unknown threshold shows as unknown, and drops out, as it does in a listing).
// The user may answer in the lead's place, and the lead answering first closes the item.
func (d *Daemon) ListWaiting(ctx context.Context, req *connect.Request[v1.ListWaitingRequest]) (*connect.Response[v1.ListWaitingResponse], error) {
	runID, err := scopedRun(auth.ActorFrom(ctx), req.Msg.RunId)
	if err != nil {
		return nil, err
	}
	r := d.store.Read(ctx)
	runs, err := r.ListRuns(false)
	if err != nil {
		return nil, err
	}
	type item struct {
		w *v1.Waiting
		m *v1.Message
	}
	var items []item
	var prompts []*v1.Waiting
	for _, run := range runs {
		if runID != "" && run.Id != runID {
			continue
		}
		agents, err := r.ListAgents(run.Id)
		if err != nil {
			return nil, err
		}
		byID := map[string]*v1.Agent{}
		for i, a := range agents {
			agents[i] = d.agentView(a)
			byID[a.Id] = agents[i]
		}
		leadFirst(agents)
		if len(agents) == 0 {
			continue
		}
		if lead := agents[0]; lead.Role == v1.AgentRole_AGENT_ROLE_ROOT {
			// The store keeps what waits (plans and questions no answer has followed): a read of the open items,
			// not of the lead's and each author's whole inbox.
			open, err := r.Waiting(lead.Id)
			if err != nil {
				return nil, err
			}
			// What the lead put to the user (`mesh send user`) waits on the user alone.
			asked, err := r.Waiting(userRecipient)
			if err != nil {
				return nil, err
			}
			for _, m := range asked {
				if m.RunId == run.Id && byID[m.From.GetId()] != nil {
					items = append(items, item{&v1.Waiting{Kind: waitingKind(m), RunId: run.Id, AgentId: m.From.GetId(), Message: m, ForUser: true}, m})
				}
			}
			for _, m := range open {
				author := byID[m.From.GetId()]
				if author == nil || author.State == v1.AgentState_AGENT_STATE_EXITED {
					continue
				}
				// A plan is the user's where the user approves plans; a worker's question to its lead is the lead's.
				forUser := m.Type == v1.MessageType_MESSAGE_TYPE_PLAN && d.userApprovesPlans(r, run, author.Repo)
				items = append(items, item{&v1.Waiting{Kind: waitingKind(m), RunId: run.Id, AgentId: author.Id, Message: m, ForUser: forUser}, m})
			}
		}
		for _, a := range agents {
			if a.State == v1.AgentState_AGENT_STATE_NEEDS_INPUT {
				prompts = append(prompts, &v1.Waiting{Kind: v1.WaitingKind_WAITING_KIND_INPUT, RunId: run.Id, AgentId: a.Id, Detail: a.StateDetail, ForUser: true})
			}
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return before(items[i].m, items[j].m) })
	out := &v1.ListWaitingResponse{}
	for _, it := range items {
		out.Waiting = append(out.Waiting, it.w)
	}
	out.Waiting = append(out.Waiting, prompts...)
	return connect.NewResponse(out), nil
}
