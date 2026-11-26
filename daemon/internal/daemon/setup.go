package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// The first-run setup agent: a lead agent (role root) in a run flagged `setup`, started in a scratch repo of the
// host's own and instructed by `protocol/SETUP.md` of the share folder. It is an ordinary agent in every other
// way (chat view, doorbell, resume), so it needs no actor class of its own: the run's flag is what widens its
// rights, for three actions that only the host's own device may otherwise call (requireHostOrSetup).

const (
	setupRunTitle = "Setup"
	setupSlug     = "agent"
	setupTask     = "Start the first-run setup with the user: follow SETUP.md."
	// setupFile is the instructions file, in the share folder's protocol/.
	setupFile = "SETUP.md"
)

// setupKinds is the order StartSetup picks a CLI in when none is named.
var setupKinds = []string{"claude", "agy", "opencode", "copilot"}

// isSetupRun reports whether a run is the setup run.
func (d *Daemon) isSetupRun(r store.Reader, runID string) bool {
	if runID == "" {
		return false
	}
	run, err := r.GetRun(runID)
	return err == nil && run.Setup && run.Status == v1.RunStatus_RUN_STATUS_OPEN
}

// requireHostOrSetup lets the host's own device through, and the root of the setup run: the setup agent pairs
// devices and reads the tailnet for the user, which no other agent may.
func (d *Daemon) requireHostOrSetup(ctx context.Context) error {
	actor := auth.ActorFrom(ctx)
	if actor != nil && actor.Id == localDeviceID {
		return nil
	}
	if actor != nil && actor.Class == v1.ActorClass_ACTOR_CLASS_ROOT && d.isSetupRun(d.store.Read(ctx), actor.RunId) {
		return nil
	}
	return denied("only the host's own device, or the setup agent, may manage paired devices")
}

// StartSetup starts the setup agent, or returns the one that runs (a retired or exited one is resumed). The run, the
// scratch repo and the agent are made once; calling it again, from another app or the CLI, lands on the same agent.
func (d *Daemon) StartSetup(ctx context.Context, req *connect.Request[v1.StartSetupRequest]) (*connect.Response[v1.StartSetupResponse], error) {
	if d.rt.cfg.Share == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("this daemon has no share folder, so the setup agent has no instructions (meshd --share)"))
	}
	if _, err := os.Stat(filepath.Join(d.rt.cfg.Share, "protocol", setupFile)); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("the share folder has no protocol/%s: reinstall the daemon", setupFile))
	}
	kind := req.Msg.Kind
	if kind == "" {
		for _, k := range setupKinds {
			// A binary that runs, not just an adapter: installed() trusts claude and agy without looking.
			if _, err := d.rt.adapters.Get(k); err != nil {
				continue
			}
			if _, ok := d.cliBinary(k); ok {
				kind = k
				break
			}
		}
		if kind == "" {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("no agent CLI is installed (claude, agy, opencode or copilot): install and log in to one, then start the setup again"))
		}
	}
	tier := req.Msg.Tier
	if tier == "" {
		tier = "mid"
	}
	// One at a time, so two apps (or a double click) can't make two setup runs.
	d.setupMu.Lock()
	defer d.setupMu.Unlock()

	run, err := d.setupRun(ctx, req.Msg.Meta)
	if err != nil {
		return nil, err
	}
	r := d.store.Read(ctx)
	agents, err := r.ListAgents(run.Id)
	if err != nil {
		return nil, err
	}
	for _, a := range agents {
		if a.Role != v1.AgentRole_AGENT_ROLE_ROOT {
			continue
		}
		if !stopped(a) {
			return connect.NewResponse(&v1.StartSetupResponse{Agent: a, Run: run, AlreadyRunning: true}), nil
		}
		res, err := d.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Meta: req.Msg.Meta, Agent: a.Id, RunId: run.Id}))
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(&v1.StartSetupResponse{Agent: res.Msg.Agent, Run: run}), nil
	}
	repo, err := d.setupRepo(ctx)
	if err != nil {
		return nil, err
	}
	res, err := d.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Meta: req.Msg.Meta, RunId: run.Id, Repo: repo,
		Slug: setupSlug, Role: v1.AgentRole_AGENT_ROLE_ROOT, Kind: kind, Tier: tier, Task: setupTask}))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.StartSetupResponse{Agent: res.Msg.Agent, Run: run}), nil
}

// setupRun finds the open setup run or makes it.
func (d *Daemon) setupRun(ctx context.Context, meta *v1.RequestMeta) (*v1.Run, error) {
	runs, err := d.store.Read(ctx).ListRuns(false)
	if err != nil {
		return nil, err
	}
	for _, run := range runs {
		if run.Setup && run.Status == v1.RunStatus_RUN_STATUS_OPEN {
			return run, nil
		}
	}
	out := &v1.StartRunResponse{}
	// The run is not written under the request's mutation id: SpawnAgent below is, and one id can't serve both.
	_, err = d.write(ctx, nil, agentmeshv1connect.MeshServiceStartSetupProcedure, out, func(t *store.Tx) error {
		run := &v1.Run{Id: ids.New("r", 6), Title: setupRunTitle, Status: v1.RunStatus_RUN_STATUS_OPEN,
			CreatedAt: tsOf(t.Now()), CreatedBy: t.Actor, Setup: true}
		if err := t.PutRun(run); err != nil {
			return err
		}
		if err := d.ensureRunDir(run.Id); err != nil {
			return err
		}
		if _, err := t.AppendEvent(&v1.Event{RunId: run.Id, Payload: &v1.Event_RunStarted{RunStarted: &v1.RunStarted{Run: run}}}); err != nil {
			return err
		}
		out.Run = run
		return nil
	})
	return out.Run, err
}

// setupRepo makes the scratch git repo the setup agent's worktree comes from (<state>/setup), once: the worktree
// machinery needs a repo, and the agent has no project of its own.
func (d *Daemon) setupRepo(ctx context.Context) (string, error) {
	dir := filepath.Join(d.paths.Dir, "setup")
	// Ready only with a commit to branch from, in a repo of its own: a first try that was cut off after `git init` is
	// finished, not trusted, and a folder with no .git is not ready because an enclosing repo (a dotfiles repo
	// around the state dir) has a HEAD.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		if _, err := git(ctx, dir, "rev-parse", "--verify", "-q", "HEAD^{commit}"); err == nil {
			return dir, nil
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if _, err := git(ctx, dir, "init", "-q"); err != nil {
		return "", connect.NewError(connect.CodeFailedPrecondition, err)
	}
	// The user's own git settings (signing, hooks) are for their repos, not this one.
	if _, err := git(ctx, dir, "-c", "user.name=agent-mesh", "-c", "user.email=meshd@localhost", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "--no-verify", "-m", "setup agent's scratch repo"); err != nil {
		return "", connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return dir, nil
}
