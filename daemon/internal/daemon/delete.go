package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/hook"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// DeleteRun forgets an ended run: its agents, messages, events and the rest the store keeps of it, and its folders under
// the state dir. It is refused while an agent of the run has a process (stop the run first), and it never removes work
// from git: the worktrees go through StopRun's removal first, so a worktree with changes, or a branch with commits no
// remote has, stays on disk (listed in the answer) when the host no longer knows it.
//
// The run is checked twice: before the removal, so a live run is refused before anything is touched, and inside the
// write that deletes the rows, so an agent reopened in between is not deleted under its process.
func (d *Daemon) DeleteRun(ctx context.Context, req *connect.Request[v1.DeleteRunRequest]) (*connect.Response[v1.DeleteRunResponse], error) {
	runID := req.Msg.RunId
	out := &v1.DeleteRunResponse{}
	actor := auth.ActorFrom(ctx)
	// A retry of a call that finished gets its first answer, though the run is gone by now.
	if mid := req.Msg.Meta.GetMutationId(); mid != "" && actor != nil {
		if proc, resp, err := d.store.Read(ctx).Mutation(actor.Id, mid); err == nil {
			if proc != agentmeshv1connect.MeshServiceDeleteRunProcedure {
				return nil, reusedMutation(mid, proc) // before anything is removed
			}
			if err := proto.Unmarshal(resp, out); err != nil {
				return nil, err
			}
			return connect.NewResponse(out), nil
		}
	}
	// Before anything is removed, as the write's own check comes after StopRun's removal.
	if err := expired(req.Msg.Meta, d.now()); err != nil {
		return nil, err
	}
	if err := d.runEnded(ctx, d.store.Read(ctx), runID); err != nil {
		return nil, err
	}
	// StopRun's removal on an ended run: the ended check is made again inside its locked write, so an agent resumed since
	// the first check refuses the delete before anything is retired or stopped.
	stopped, notStopped, err := d.stopRun(ctx, connect.NewRequest(&v1.StopRunRequest{RunId: runID}), true)
	if err != nil {
		return nil, err
	}
	for _, why := range notStopped {
		// The run is stopped and its clean worktrees are gone by now; its rows and files stay.
		return nil, failedPrecondition("the run was stopped but not deleted: an agent's process could not be stopped (%s)", firstLine(why))
	}
	out.Worktrees = stopped.Msg.Worktrees
	var agents []string
	if _, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceDeleteRunProcedure, out, func(tx *store.Tx) error {
		if err := d.runEnded(ctx, tx, runID); err != nil {
			return err
		}
		var err error
		if agents, err = tx.DeleteRun(runID); err != nil {
			return err
		}
		// A peer that was last asked from this run, or runs in it, no longer has a run or an answers file to point at.
		peers, err := tx.ListPeers("")
		if err != nil {
			return err
		}
		for _, p := range peers {
			if p.LastRun != runID && p.RunningRun != runID {
				continue
			}
			if p.LastRun == runID {
				p.LastRun, p.LastAgent, p.LastAnswers = "", "", ""
			}
			if p.RunningRun == runID {
				p.RunningRun, p.RunningAgent = "", ""
			}
			if err := tx.PutPeer(p); err != nil {
				return err
			}
		}
		_, err = tx.AppendEvent(&v1.Event{RunId: runID, Payload: &v1.Event_RunDeleted{RunDeleted: &v1.RunDeleted{}}})
		return err
	}); err != nil {
		return nil, err
	}
	// The files go after the rows: the rows are the truth, and a folder that could not be removed is only disk to clean.
	d.removeDeletedRunFiles(runID, agents)
	return connect.NewResponse(out), nil
}

// removeDeletedRunFiles removes a deleted run's folder and its agents' (token, MCP config), and the run's worktree
// folder when nothing stayed in it. Ids come from the store, but a name that is not one path element is never joined.
func (d *Daemon) removeDeletedRunFiles(runID string, agents []string) {
	one := func(id string) bool { return id != "" && id != "." && id != ".." && filepath.Base(id) == id }
	if !one(runID) {
		return
	}
	for _, p := range []string{d.paths.RunDir(runID)} {
		if err := os.RemoveAll(p); err != nil {
			d.log.Warn("removing a deleted run's folder", "run", runID, "path", p, "err", err)
		}
	}
	for _, id := range agents {
		if !one(id) {
			continue
		}
		if err := os.RemoveAll(d.paths.Agent(id)); err != nil {
			d.log.Warn("removing a deleted agent's folder", "agent", id, "err", err)
		}
		// Not while a drain of its spool runs: it would apply what is left to an agent that is gone.
		unlock, lerr := hook.LockSpool(d.paths, id)
		if err := os.RemoveAll(d.paths.Spool(id)); err != nil {
			d.log.Warn("removing a deleted agent's spool", "agent", id, "err", err)
		}
		if lerr == nil {
			unlock()
		}
	}
	if err := os.Remove(filepath.Join(d.worktrees(), runID)); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
		d.log.Warn("removing a deleted run's worktree folder", "run", runID, "err", err)
	}
}

// runEnded checks that a run exists and none of its agents has a process, or is to get one (a reopen pending).
func (d *Daemon) runEnded(_ context.Context, r interface {
	GetRun(string) (*v1.Run, error)
	ListAgents(string) ([]*v1.Agent, error)
}, runID string) error {
	if _, err := r.GetRun(runID); errors.Is(err, store.ErrNotFound) {
		return notFound("no run %q", runID)
	} else if err != nil {
		return err
	}
	agents, err := r.ListAgents(runID)
	if err != nil {
		return err
	}
	for _, a := range agents {
		if hasProcess(a) {
			return failedPrecondition("%s is still running: stop the run first", a.Name)
		}
	}
	return nil
}
