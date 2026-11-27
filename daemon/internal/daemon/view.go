package daemon

import (
	"context"
	"strings"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// OpenView asks the user's app to show a view. The request goes to the apps connected now and is never stored: an
// app acts on it only when its window is focused or the user turned `app.open_views` on, so an agent can point the
// user at something but never takes their screen over. A root may name only agents and the run of its own run.
func (d *Daemon) OpenView(ctx context.Context, req *connect.Request[v1.OpenViewRequest]) (*connect.Response[v1.OpenViewResponse], error) {
	actor := auth.ActorFrom(ctx)
	m := req.Msg
	view := strings.ToLower(strings.TrimSpace(m.View))
	target := strings.TrimSpace(m.Target)
	if len(target) > 200 {
		return nil, invalid("the target is too long")
	}
	runID, err := scopedRun(actor, m.RunId)
	if err != nil {
		return nil, err
	}
	frame := &v1.ViewRequest{View: view, Target: target, From: actor.Name}
	switch view {
	case "settings":
	case "agent", "terminal":
		if target == "" {
			return nil, invalid("name the agent to show (--target)")
		}
		r := d.store.Read(ctx)
		var a *v1.Agent
		if IsAgent(actor.Class) {
			a, err = d.FindAgent(r, actor, target)
		} else {
			a, err = d.findAgentCrossRun(r, actor, target)
		}
		if err != nil {
			return nil, err
		}
		frame.Target, frame.RunId = a.Id, a.RunId
	case "run":
		if target == "" {
			target = runID
		}
		if target == "" {
			return nil, invalid("name the run to show (--target)")
		}
		if runID != "" && target != runID {
			return nil, denied("an agent may show only its own run")
		}
		if _, err := d.store.Read(ctx).GetRun(target); err != nil {
			return nil, notFound("no run %q", target)
		}
		frame.Target, frame.RunId = target, target
	default:
		return nil, invalid("view must be agent, terminal, settings or run")
	}
	out := &v1.OpenViewResponse{}
	_, err = d.write(ctx, m.Meta, agentmeshv1connect.MeshServiceOpenViewProcedure, out, func(*store.Tx) error {
		out.Delivered = int32(d.bus.PublishView(frame))
		return nil
	})
	return connect.NewResponse(out), err
}
