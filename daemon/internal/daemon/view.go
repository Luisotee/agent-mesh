package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// viewEvery is how often one agent may ask the user's app to show something: a view is a request for the user's
// attention, and an agent that asks more often than this is noise or an attack.
const viewEvery = 10 * time.Second

// viewLimiter remembers when each agent last asked (agents only: a device is the user).
type viewLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func (l *viewLimiter) allow(actorID string, now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if at, ok := l.last[actorID]; ok && now.Sub(at) < viewEvery {
		return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("an agent may ask the app to show something once in %d seconds: wait %d s", int(viewEvery.Seconds()), int((viewEvery-now.Sub(at))/time.Second)+1))
	}
	if l.last == nil {
		l.last = map[string]time.Time{}
	}
	for id, at := range l.last { // forget the ones that are over
		if now.Sub(at) >= viewEvery {
			delete(l.last, id)
		}
	}
	l.last[actorID] = now
	return nil
}

// agentInRun finds an agent of one run, by id or by name.
func agentInRun(r store.Reader, runID, ref string) (*v1.Agent, error) {
	if a, err := r.GetAgent(ref); err == nil {
		if a.RunId != runID {
			return nil, notFound("no agent %q in run %s", ref, runID)
		}
		return a, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	found, err := r.AgentsNamed(runID, ref)
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, notFound("no agent %q in run %s", ref, runID)
	}
	return found[0], nil
}

// OpenView asks the user's app to show a view. The request goes to the apps connected now and is never stored: an
// app shows it as a toast the user opens or dismisses, and switches by itself only when the user turned `app.open_views` on (and
// then never while they type), so an agent can point the
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
		if runID != "" {
			// An agent's own run, or the run a device named: the target is looked for in that run only.
			a, err = agentInRun(r, runID, target)
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
			if IsAgent(actor.Class) {
				return nil, denied("an agent may show only its own run")
			}
			return nil, invalid("the run to show (--target %s) is not the run named (--run-id %s)", target, runID)
		}
		if _, err := d.store.Read(ctx).GetRun(target); errors.Is(err, store.ErrNotFound) {
			return nil, notFound("no run %q", target)
		} else if err != nil {
			return nil, err
		}
		frame.Target, frame.RunId = target, target
	default:
		return nil, invalid("view must be agent, terminal, settings or run")
	}
	if IsAgent(actor.Class) {
		if err := d.views.allow(actor.Id, d.now()); err != nil {
			return nil, err
		}
	}
	out := &v1.OpenViewResponse{}
	_, err = d.write(ctx, m.Meta, agentmeshv1connect.MeshServiceOpenViewProcedure, out, func(*store.Tx) error {
		out.Delivered = int32(d.bus.PublishView(frame))
		return nil
	})
	return connect.NewResponse(out), err
}
