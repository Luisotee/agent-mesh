package daemon

import (
	"context"
	"fmt"
	"os"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// ArchiveAgent retires an agent and removes its worktree, keeping the branch: the work stays in git, the disk is freed,
// and a message to the agent (or ResumeAgent) makes the worktree again from the branch (`ensureWorktree`), with the files of
// `.worktreeinclude` copied back. It never discards work: a worktree with uncommitted changes always refuses, and
// commits no remote branch has refuse unless `force` (the branch is kept either way, so `force` loses nothing a branch
// holds, but a branch that is never pushed lives only on this machine).
//
// Only a worktree of the daemon's own (under its worktrees folder) is removed: never the user's own checkout, which a
// root adopted from a Claude Code session runs in.
func (d *Daemon) ArchiveAgent(ctx context.Context, req *connect.Request[v1.ArchiveAgentRequest]) (*connect.Response[v1.ArchiveAgentResponse], error) {
	actor := auth.ActorFrom(ctx)
	found, err := d.FindAgent(d.store.Read(ctx), actor, req.Msg.Agent)
	if err != nil {
		return nil, err
	}
	if found.Peer || found.AdoptedPid != 0 || found.Worktree == "" || found.Repo == "" || !within(found.Worktree, d.worktrees()) {
		return nil, failedPrecondition("%s has no worktree of the daemon's to archive", found.Name)
	}
	if found.Archived {
		return nil, failedPrecondition("%s is archived already", found.Name)
	}
	if err := archiveRefusal(agentChanges(ctx, found), found.Name, req.Msg.Force); err != nil {
		return nil, err
	}
	mu := d.agentLock(found.Id)
	mu.Lock()
	defer mu.Unlock()
	out := &v1.ArchiveAgentResponse{}
	replayed, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceArchiveAgentProcedure, out, func(tx *store.Tx) error {
		a, err := tx.GetAgent(found.Id)
		if err != nil {
			return err
		}
		if _, err := retireTx(tx, a, "archived"); err != nil {
			return err
		}
		out.Agent = a
		return nil
	})
	if err != nil {
		return nil, err
	}
	if replayed {
		return connect.NewResponse(out), nil
	}
	if err := d.finishRetire(ctx, found.Id); err != nil {
		return nil, err
	}
	// Its process is gone: what it did while the request was waiting for its lock is in the worktree now, so look again.
	cur, err := d.store.Read(ctx).GetAgent(found.Id)
	if err != nil {
		return nil, err
	}
	ch := agentChanges(ctx, cur)
	if err := archiveRefusal(ch, cur.Name, req.Msg.Force); err != nil {
		return nil, fmt.Errorf("%w (it is retired; its worktree stays)", err)
	}
	if err := d.removeKeepingBranch(ctx, cur); err != nil {
		return nil, failedPrecondition("%s is retired, but its worktree stays: %v", cur.Name, err)
	}
	err = d.store.Write(context.WithoutCancel(ctx), actor, "", func(tx *store.Tx) error {
		a, err := tx.GetAgent(cur.Id)
		if err != nil {
			return err
		}
		a.Archived = true
		if err := tx.PutAgent(a); err != nil {
			return err
		}
		out.Agent = a
		_, err = tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_AgentArchived{AgentArchived: &v1.AgentArchived{
			Worktree: a.Worktree, Branch: a.Branch, Unpushed: ch.Unpushed, Forced: req.Msg.Force && ch.Unpushed > 0}}})
		return err
	})
	if err != nil {
		return nil, err
	}
	out.Unpushed = ch.Unpushed
	return connect.NewResponse(out), nil
}

// archiveRefusal says why an agent's work must not be archived yet: uncommitted changes always, unpushed commits unless forced.
func archiveRefusal(ch *v1.GetAgentChangesResponse, name string, force bool) error {
	switch {
	case ch.Uncommitted > 0:
		return failedPrecondition("%s has %d file(s) with uncommitted changes: commit or discard them first (archiving never deletes uncommitted work)", name, ch.Uncommitted)
	case ch.Unpushed > 0 && !force:
		return failedPrecondition("%s's branch has %d commit(s) no remote branch has: push it first, or archive anyway (its branch is kept, on this machine only)", name, ch.Unpushed)
	}
	return nil
}

// removeKeepingBranch removes an agent's worktree with `git worktree remove` (never forced: git refuses one with untracked
// files) and keeps its branch. A worktree already gone is only pruned.
func (d *Daemon) removeKeepingBranch(ctx context.Context, a *v1.Agent) error {
	mu := d.worktreeLock(a.Worktree)
	mu.Lock()
	defer mu.Unlock()
	if _, err := os.Stat(a.Worktree); os.IsNotExist(err) {
		_, _ = git(ctx, a.Repo, "worktree", "prune")
		return nil
	}
	if _, err := git(ctx, a.Repo, "worktree", "remove", a.Worktree); err != nil {
		return fmt.Errorf("git would not remove it: %s", firstLine(err.Error()))
	}
	return nil
}
