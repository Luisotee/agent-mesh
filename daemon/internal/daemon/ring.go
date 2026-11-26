package daemon

import (
	"context"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// RingAgent re-rings an agent that has unread mail now, through the same delivery path, per-agent lock,
// native inbox socket, and human-draft guard that automatic doorbell delivery uses.
func (d *Daemon) RingAgent(ctx context.Context, req *connect.Request[v1.RingAgentRequest]) (*connect.Response[v1.RingAgentResponse], error) {
	actor := auth.ActorFrom(ctx)
	r := d.store.Read(ctx)
	found, err := d.FindAgent(r, actor, req.Msg.Agent)
	if err != nil {
		return nil, err
	}
	if err := mayMove(actor, found, "ring"); err != nil {
		return nil, err
	}
	if mid := req.Msg.Meta.GetMutationId(); mid != "" {
		if _, resp, err := r.Mutation(actor.Id, mid); err == nil {
			out := &v1.RingAgentResponse{}
			if err := proto.Unmarshal(resp, out); err != nil {
				return nil, err
			}
			return connect.NewResponse(out), nil
		}
	}

	mu := d.agentLock(found.Id)
	mu.Lock()
	defer mu.Unlock()

	r = d.store.Read(ctx)
	a, err := r.GetAgent(found.Id)
	if err != nil {
		return nil, err
	}
	msgs, err := r.Inbox(a.Id, false)
	if err != nil {
		return nil, err
	}

	out := &v1.RingAgentResponse{
		Agent:  d.view(a),
		Unread: int32(len(msgs)),
	}
	switch {
	case len(msgs) == 0:
	case a.PendingReopen != nil:
		out.Error = "reopen pending"
	case stopped(a):
		out.Error = "agent is " + enumShort(a.State.String(), "AGENT_STATE_")
	case a.TerminalId == "" || d.rt.term == nil:
		out.Error = "no terminal"
	case !takesDoorbell(a.State):
		out.Error = "agent is " + enumShort(a.State.String(), "AGENT_STATE_")
	default:
		var prev *ring
		if rg := d.ringOf(a.Id); rg != nil && rg.terminalID == a.TerminalId {
			cp := *rg
			cp.attempts = 0
			cp.failed = false
			prev = &cp
		} else if rg != nil {
			d.setRing(a.Id, nil)
		}
		outcome, err := d.ringAgent(ctx, a, msgs, prev, nil)
		if err != nil {
			return nil, err
		}
		out.Rung = outcome.rung
		out.Via = outcome.via
		out.Error = outcome.error
	}

	if _, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceRingAgentProcedure, out, func(tx *store.Tx) error {
		return nil
	}); err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}
