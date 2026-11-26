package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"connectrpc.com/connect"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/agy"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/settings"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// OpenAgent prepares opening an agent for the user: attaching to a running agent's terminal in mesh-term, or
// reopening a stopped agent's conversation directly in the caller's terminal outside any run.
func (d *Daemon) OpenAgent(ctx context.Context, req *connect.Request[v1.OpenAgentRequest]) (*connect.Response[v1.OpenAgentResponse], error) {
	actor := auth.ActorFrom(ctx)
	m := req.Msg
	if strings.TrimSpace(m.Agent) == "" {
		return nil, invalid("no agent given")
	}
	if m.Effort != "" && !validEffort(m.Effort) {
		return nil, invalid("effort %q: one of %s", m.Effort, strings.Join(settings.Efforts, ", "))
	}

	r := d.store.Read(ctx)
	a, err := d.findAgentForOpen(r, m.Agent, m.RunId)
	if err != nil {
		return nil, err
	}

	if !stopped(a) {
		if m.Model != "" || m.Effort != "" || len(m.ExtraArgs) > 0 {
			return nil, invalid("%s is still running: omit --model, --effort and extra arguments to attach", a.Name)
		}
		if a.PendingReopen != nil && a.TerminalId == "" {
			return nil, failedPrecondition("%s is reopening; try again in a moment", a.Name)
		}
		if a.TerminalId == "" || d.rt.term == nil {
			return nil, failedPrecondition("%s has no terminal", a.Name)
		}
		path, _, _, _ := d.issueTerminalTicket(actor, a, true)
		return connect.NewResponse(&v1.OpenAgentResponse{
			Agent:        d.view(a),
			Mode:         v1.OpenMode_OPEN_MODE_ATTACH,
			TerminalPath: path,
		}), nil
	}

	if a.PendingReopen != nil {
		return nil, failedPrecondition("%s is reopening; try again in a moment", a.Name)
	}
	if a.SessionId == "" {
		d.learnSession(ctx, actor, a.Id, func(*v1.Agent) bool { return true })
		a, err = d.store.Read(ctx).GetAgent(a.Id)
		if err != nil {
			return nil, err
		}
		if a.SessionId == "" {
			return nil, failedPrecondition("%s never reported a conversation id, so there is nothing to open", a.Name)
		}
	}

	r = d.store.Read(ctx)
	if err := d.hasProcessOnSessionOrWorktree(r, a); err != nil {
		return nil, err
	}

	model, effort := a.Model, a.Effort
	if m.Model != "" {
		model = m.Model
		if m.Effort == "" {
			if me := d.rt.adapters.ModelEffort(ctx, a.Kind, m.Model); me != "" {
				effort = me
			} else if effort == "" {
				effort = namedEffort
			}
		}
	}
	if m.Effort != "" {
		effort = m.Effort
	}

	var notes []string
	recreated := false
	branch := a.Branch
	if a.Worktree != "" {
		wmu := d.worktreeLock(a.Worktree)
		wmu.Lock()
		var peer *v1.Peer
		if a.Peer {
			if p, perr := r.GetPeer(a.Name); perr == nil {
				peer = p
			}
		}
		if peer != nil {
			branch = ""
			_, statErr := os.Stat(peer.Worktree)
			wasMissing := os.IsNotExist(statErr)
			if err := d.ensurePeerWorktree(ctx, peer); err != nil {
				wmu.Unlock()
				return nil, failedPrecondition("%v", err)
			}
			recreated = wasMissing
		} else {
			if a.Peer {
				branch = ""
			}
			var cerr error
			recreated, _, cerr = ensureWorktree(ctx, a.Repo, a.Worktree, branch, a.BaseCommit)
			if cerr != nil {
				wmu.Unlock()
				return nil, failedPrecondition("%v", cerr)
			}
			if recreated && branch != "" {
				notes = append(notes, fmt.Sprintf("the worktree was recreated; remove it with `git worktree remove %s` when done", a.Worktree))
			}
		}
		if a.Kind == agy.Kind {
			if cerr := clearAgyMeshHooks(ctx, a.Worktree); cerr != nil {
				d.log.Warn("clearing agy hooks before open", "agent", a.Name, "err", cerr)
			}
		}
		wmu.Unlock()
	}

	ad, err := d.rt.adapters.Get(a.Kind)
	if err != nil {
		return nil, err
	}
	baseEnv := standaloneBaseEnv(a)
	plan, err := ad.PlanLaunch(ctx, connect.NewRequest(&adapterv1.PlanLaunchRequest{
		AgentId:         a.Id,
		AgentName:       a.Name,
		RunId:           a.RunId,
		Role:            a.Role,
		Cwd:             a.Worktree,
		Repo:            a.Repo,
		Model:           model,
		Effort:          effort,
		Binary:          d.rt.cfg.Binaries[a.Kind],
		Env:             baseEnv,
		ResumeSessionId: a.SessionId,
		Standalone:      true,
	}))
	if err != nil {
		return nil, failedPrecondition("planning open for %s: %v", a.Name, err)
	}
	argv := append([]string{}, plan.Msg.Argv...)
	argv = append(argv, m.ExtraArgs...)

	envDelta := map[string]string{}
	for k, v := range plan.Msg.Env {
		if base, had := baseEnv[k]; !had || base != v {
			envDelta[k] = v
		}
	}

	return connect.NewResponse(&v1.OpenAgentResponse{
		Agent:             d.view(a),
		Mode:              v1.OpenMode_OPEN_MODE_EXEC,
		Argv:              argv,
		Env:               envDelta,
		Cwd:               a.Worktree,
		Branch:            branch,
		WorktreeRecreated: recreated,
		Notes:             notes,
	}), nil
}

// standaloneBaseEnv is the non-mesh environment passed to PlanLaunch when planning a standalone open (e.g.
// so agy/opencode model lookups find HOME/PATH without leaking AGENT_MESH_* into the returned env).
func standaloneBaseEnv(a *v1.Agent) map[string]string {
	env := map[string]string{}
	for _, k := range inheritedEnv {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	if a.Kind == agy.Kind {
		for _, k := range agy.SessionVars {
			delete(env, k)
		}
	}
	return env
}

// findAgentForOpen resolves ref by id, name (scoped to runID when set; else open runs then closed runs),
// or branch (newest match).
func (d *Daemon) findAgentForOpen(r store.Reader, ref, runID string) (*v1.Agent, error) {
	if a, err := r.GetAgent(ref); err == nil {
		if runID != "" && a.RunId != runID {
			return nil, notFound("no agent %q in run %s", ref, runID)
		}
		return a, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	var found []*v1.Agent
	var err error
	if runID != "" {
		found, err = r.AgentsNamed(runID, ref)
	} else {
		found, err = r.AgentsNamed("", ref)
		if err == nil && len(found) == 0 {
			found, err = r.AgentsNamedClosed(ref)
		}
	}
	if err != nil {
		return nil, err
	}
	if len(found) == 1 {
		return found[0], nil
	}
	if len(found) > 1 {
		names := make([]string, len(found))
		for i, x := range found {
			names[i] = x.Id + " (run " + x.RunId + ")"
		}
		return nil, failedPrecondition("%q names several agents: %s; give an id or --run-id", ref, strings.Join(names, ", "))
	}

	all, err := r.AllAgents()
	if err != nil {
		return nil, err
	}
	for i := len(all) - 1; i >= 0; i-- {
		a := all[i]
		if runID != "" && a.RunId != runID {
			continue
		}
		if a.Branch != "" && a.Branch == ref {
			return a, nil
		}
	}
	if runID != "" {
		return nil, notFound("no agent %q in run %s", ref, runID)
	}
	return nil, notFound("no agent %q", ref)
}
