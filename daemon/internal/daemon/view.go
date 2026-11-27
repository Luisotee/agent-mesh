package daemon

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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

// agentForView finds the agent a view names: in the run an agent's own (or a device's `--run-id`), else across the runs the caller can see.
func (d *Daemon) agentForView(ctx context.Context, actor *v1.Actor, runID, ref string) (*v1.Agent, error) {
	r := d.store.Read(ctx)
	if runID != "" {
		return agentInRun(r, runID, ref)
	}
	return d.findAgentCrossRun(r, actor, ref)
}

// messagesAgent finds whose Messages tab to show: the agent named, else (a run's id) the run's lead.
func (d *Daemon) messagesAgent(ctx context.Context, actor *v1.Actor, runID, target string) (*v1.Agent, error) {
	if a, err := d.agentForView(ctx, actor, runID, target); err == nil {
		return a, nil
	} else if connect.CodeOf(err) != connect.CodeNotFound {
		return nil, err
	}
	r := d.store.Read(ctx)
	if runID != "" && target != runID {
		return nil, notFound("no agent %q in run %s", target, runID)
	}
	if _, err := r.GetRun(target); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, notFound("no agent or run %q", target)
		}
		return nil, err
	}
	agents, err := r.ListAgents(target)
	if err != nil {
		return nil, err
	}
	for _, a := range agents {
		if a.Role == v1.AgentRole_AGENT_ROLE_ROOT {
			return a, nil
		}
	}
	return nil, notFound("run %s has no lead", target)
}

// newestPlan is the plan an agent sent last, in its run's stored messages.
func newestPlan(r store.Reader, a *v1.Agent) (*v1.Message, error) {
	msgs, err := r.RunMessages(a.RunId, "", -1)
	if err != nil {
		return nil, err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if m := msgs[i]; m.Type == v1.MessageType_MESSAGE_TYPE_PLAN && m.From.GetId() == a.Id {
			return m, nil
		}
	}
	return nil, notFound("%s has sent no plan", a.Name)
}

// pullRequestForView finds the stored pull request a view names (its URL, or `#number` / `number`), among the agents the caller
// may see: an agent's own run, a device's `--run-id` or every run. A number several repos share is refused: name the URL.
func (d *Daemon) pullRequestForView(ctx context.Context, actor *v1.Actor, runID, target string) (*v1.PullRequest, error) {
	r := d.store.Read(ctx)
	agents, err := r.AllAgents()
	if err != nil {
		return nil, err
	}
	var in []*v1.PullRequest
	for _, a := range agents {
		if runID != "" && a.RunId != runID {
			continue
		}
		prs, err := r.AgentPRs(a.Id)
		if err != nil {
			return nil, err
		}
		in = append(in, prs...)
	}
	var found []*v1.PullRequest
	if n, err := strconv.Atoi(strings.TrimPrefix(target, "#")); err == nil {
		for _, p := range in {
			if int(p.Number) == n {
				found = append(found, p)
			}
		}
	} else {
		key := store.PRKey(target)
		for _, p := range in {
			if store.PRKey(p.Url) == key {
				found = append(found, p)
			}
		}
	}
	urls := map[string]bool{}
	for _, p := range found {
		urls[store.PRKey(p.Url)] = true
	}
	switch {
	case len(found) == 0:
		return nil, notFound("no pull request %q among the agents' (it is shown once an agent has it)", target)
	case len(urls) > 1:
		return nil, invalid("%q names pull requests of more than one repo: give the URL", target)
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
		a, err := d.agentForView(ctx, actor, runID, target)
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
	case "messages":
		if target == "" {
			target = runID
		}
		if target == "" {
			return nil, invalid("name the agent (or the run) whose messages to show (--target)")
		}
		a, err := d.messagesAgent(ctx, actor, runID, target)
		if err != nil {
			return nil, err
		}
		frame.Target, frame.RunId = a.Id, a.RunId
	case "plan":
		if target == "" {
			return nil, invalid("name the agent that wrote the plan (--target)")
		}
		a, err := d.agentForView(ctx, actor, runID, target)
		if err != nil {
			return nil, err
		}
		plan, err := newestPlan(d.store.Read(ctx), a)
		if err != nil {
			return nil, err
		}
		frame.Target, frame.RunId, frame.MessageId = a.Id, a.RunId, plan.Id
	case "pr":
		if target == "" {
			return nil, invalid("name the pull request (--target: its URL, or #number)")
		}
		pr, err := d.pullRequestForView(ctx, actor, runID, target)
		if err != nil {
			return nil, err
		}
		owner, err := d.getAgent(d.store.Read(ctx), pr.AgentId)
		if err != nil {
			return nil, err
		}
		frame.Target, frame.RunId, frame.AgentId = pr.Url, owner.RunId, owner.Id
	default:
		return nil, invalid("view must be agent, terminal, messages, plan, pr, settings or run")
	}
	out := &v1.OpenViewResponse{}
	_, err = d.write(ctx, m.Meta, agentmeshv1connect.MeshServiceOpenViewProcedure, out, func(*store.Tx) error {
		// Counted here, not before: a replay of an applied request (the same mutation id) returns its first answer
		// without running this, so a retry is never limited and never uses up the agent's turn.
		if IsAgent(actor.Class) {
			if err := d.views.allow(actor.Id, d.now()); err != nil {
				return err
			}
		}
		out.Delivered = int32(d.bus.PublishView(frame))
		return nil
	})
	return connect.NewResponse(out), err
}
