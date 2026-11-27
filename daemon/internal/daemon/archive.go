package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// ArchiveAgent retires a worker and removes its worktree, keeping the branch: the work stays in git, the disk is freed,
// and a message to the agent (or ResumeAgent) makes the worktree again from the branch (`ensureWorktree`), with the files of
// `.worktreeinclude` copied back. It never discards work: a worktree with uncommitted changes always refuses, and
// commits no remote branch has refuse unless `force` (the branch is kept either way, so `force` loses nothing a branch
// holds, but a branch that is never pushed lives only on this machine).
//
// Only a worker's worktree is removed, and only one of the daemon's own (under its worktrees folder): never the user's own
// checkout, which a root adopted from a Claude Code session runs in, and never a root's, asker's or reviewer's, which are
// not made again when they are resumed. A worktree another agent shares (a follow-up run resumed the agent in it) stays while
// that agent is alive or its run is open.
//
// The call is recorded (its mutation, `archived`, the event) in one write, after the worktree is gone: a call that fails
// half way is retired, not archived, and a retry carries on; a retry of one that finished gets its first answer.
func (d *Daemon) ArchiveAgent(ctx context.Context, req *connect.Request[v1.ArchiveAgentRequest]) (*connect.Response[v1.ArchiveAgentResponse], error) {
	actor := auth.ActorFrom(ctx)
	m := req.Msg
	found, err := d.FindAgent(d.store.Read(ctx), actor, m.Agent)
	if err != nil {
		return nil, err
	}
	out := &v1.ArchiveAgentResponse{}
	mu := d.agentLock(found.Id)
	mu.Lock()
	defer mu.Unlock()
	// Looked for under the lock, not before it: a retry that waited for the call it repeats gets that call's answer. An
	// applied call's agent is archived now, which the checks below would refuse; and a call past its expiry is refused here,
	// before anything is retired or removed (the last write can't be, with the worktree gone).
	if mid := m.GetMeta().GetMutationId(); mid != "" {
		proc, resp, err := d.store.Read(ctx).Mutation(actor.Id, mid)
		switch {
		case err == nil && proc == agentmeshv1connect.MeshServiceArchiveAgentProcedure:
			if err := proto.Unmarshal(resp, out); err != nil {
				return nil, err
			}
			return connect.NewResponse(out), nil
		case err == nil:
			return nil, reusedMutation(mid, proc)
		case !errors.Is(err, store.ErrNotFound):
			return nil, err
		}
	}
	if err := expired(m.Meta, d.now()); err != nil {
		return nil, err
	}
	cur, err := d.getAgent(d.store.Read(ctx), found.Id)
	if err != nil {
		return nil, err
	}
	if err := d.archivable(ctx, cur); err != nil {
		return nil, err
	}
	if err := archiveRefusal(agentChanges(ctx, cur), cur.Name, m.Force); err != nil {
		return nil, err
	}
	if err := d.store.Write(ctx, actor, "", func(tx *store.Tx) error {
		a, err := tx.GetAgent(cur.Id)
		if err != nil {
			return err
		}
		_, err = retireTx(tx, a, "archived")
		return err
	}); err != nil {
		return nil, err
	}
	// Its process is gone, and what it left running in the worktree with it, before the worktree is looked at again: what it did
	// while this call was waiting for its lock is in the worktree now.
	ctx = context.WithoutCancel(ctx)
	if err := d.stopRetiredProcess(ctx, cur.Id); err != nil {
		return nil, failedPrecondition("%s is retired, but its process could not be stopped, so its worktree stays: %v", cur.Name, err)
	}
	if cur, err = d.getAgent(d.store.Read(ctx), cur.Id); err != nil {
		return nil, err
	}
	d.sweepWorktrees(ctx, []*v1.Agent{cur})
	// From here the worktree's lock is held (the order is the agent's, then the worktree's), so a resume or a message that
	// would make the agent run again, or another agent that takes the worktree, waits for the removal and finds it gone:
	// what was checked above is checked again, under the lock, and stays true until the removal is recorded.
	wmu := d.worktreeLock(cur.Worktree)
	wmu.Lock()
	defer wmu.Unlock()
	if cur, err = d.getAgent(d.store.Read(ctx), cur.Id); err != nil {
		return nil, err
	}
	retiredStill := cur.State == v1.AgentState_AGENT_STATE_RETIRED && cur.PendingReopen == nil
	if !retiredStill {
		return nil, failedPrecondition("%s was resumed while it was being archived, so its worktree stays", cur.Name)
	}
	if err := d.archivable(ctx, cur); err != nil {
		return nil, failedPrecondition("%v (it is retired; its worktree stays)", errMessage(err))
	}
	ch := agentChanges(ctx, cur)
	if err := archiveRefusal(ch, cur.Name, m.Force); err != nil {
		return nil, failedPrecondition("%s (it is retired; its worktree stays)", errMessage(err))
	}
	if err := d.removeKeepingBranchLocked(ctx, cur); err != nil {
		return nil, failedPrecondition("%s is retired, but its worktree stays: %v", cur.Name, err)
	}
	// Only the mutation id: the expiry was checked before anything was done, and a write the removal has happened for must not
	// be refused for it.
	_, err = d.write(ctx, &v1.RequestMeta{MutationId: m.GetMeta().GetMutationId()}, agentmeshv1connect.MeshServiceArchiveAgentProcedure, out, func(tx *store.Tx) error {
		a, err := tx.GetAgent(cur.Id)
		if err != nil {
			return err
		}
		a.Archived = true
		if err := tx.PutAgent(a); err != nil {
			return err
		}
		out.Agent, out.Unpushed = a, ch.Unpushed
		_, err = tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_AgentArchived{AgentArchived: &v1.AgentArchived{
			Worktree: a.Worktree, Branch: a.Branch, Unpushed: ch.Unpushed, Forced: m.Force && ch.Unpushed > 0}}})
		return err
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

// archivable says why an agent's worktree must not be removed ("" error when it may): not a worker's, not the daemon's own,
// archived already, or shared with an agent that is alive or in an open run.
func (d *Daemon) archivable(ctx context.Context, a *v1.Agent) error {
	switch {
	case a.Role != v1.AgentRole_AGENT_ROLE_WORKER:
		return failedPrecondition("%s is not a worker: only a worker's worktree is archived (a root, asker or reviewer isn't made again when it is resumed)", a.Name)
	case a.Peer || a.AdoptedPid != 0 || a.Worktree == "" || a.Repo == "" || !within(a.Worktree, d.worktrees()):
		return failedPrecondition("%s has no worktree of the daemon's to archive", a.Name)
	case a.Archived:
		return failedPrecondition("%s is archived already", a.Name)
	}
	r := d.store.Read(ctx)
	open := map[string]bool{}
	if runs, err := r.ListRuns(false); err == nil {
		for _, run := range runs {
			open[run.Id] = true
		}
	} else {
		return err
	}
	all, err := r.AllAgents()
	if err != nil {
		return err
	}
	for _, other := range all {
		if other.Id != a.Id && other.Worktree == a.Worktree && (hasProcess(other) || open[other.RunId]) {
			return failedPrecondition("%s shares its worktree with %s, which is alive or in an open run: archive that one first", a.Name, other.Name)
		}
	}
	return nil
}

// archiveRefusal says why an agent's work must not be archived yet: uncommitted changes always, unpushed commits unless forced.
func archiveRefusal(ch *v1.GetAgentChangesResponse, name string, force bool) error {
	switch {
	case !ch.Available:
		return failedPrecondition("%s's work can't be read (%s): nothing is archived on a guess", name, ch.Reason)
	case ch.Uncommitted > 0:
		return failedPrecondition("%s has %d file(s) with uncommitted changes: commit or discard them first (archiving never deletes uncommitted work)", name, ch.Uncommitted)
	case ch.Unpushed > 0 && !force:
		return failedPrecondition("%s's branch has %d commit(s) no remote branch has: push it first, or archive anyway (its branch is kept, on this machine only)", name, ch.Unpushed)
	}
	return nil
}

// removeKeepingBranchLocked removes an agent's worktree with `git worktree remove` (never forced: git refuses one with untracked
// files) and keeps its branch. A worktree already gone is only pruned. The caller holds the worktree's lock.
func (d *Daemon) removeKeepingBranchLocked(ctx context.Context, a *v1.Agent) error {
	if _, err := os.Stat(a.Worktree); os.IsNotExist(err) {
		_, _ = git(ctx, a.Repo, "worktree", "prune")
		return nil
	}
	if _, err := git(ctx, a.Repo, "worktree", "remove", a.Worktree); err != nil {
		return fmt.Errorf("git would not remove it: %s", firstLine(err.Error()))
	}
	return nil
}
