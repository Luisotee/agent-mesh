package daemon

// Retiring: an agent that has nothing left to do stops its process and keeps its conversation (design §18). Its
// state is RETIRED, its token revoked, its worktree and branch kept; a message to it, or ResumeAgent, reopens
// the same conversation. Retire is called by hand (RetireAgent), by StopRun for a whole run, and by the daemon
// itself for an agent idle after its report.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// retireTx marks an agent retired: the state, a pending reopen dropped, the token revoked, and the event. Its
// process is stopped afterwards (finishRetire), so that anything the dying process says finds a retired agent.
// It reports whether anything changed.
func retireTx(tx *store.Tx, a *v1.Agent, reason string) (bool, error) {
	if stopped(a) {
		// Already stopped, but a resume may be on its way (stored, not launched yet): retiring ends that too, or
		// it would launch into a retired agent, or a closed run.
		if a.PendingReopen == nil {
			return false, nil
		}
		if err := cancelReopen(tx, a, "retired"); err != nil {
			return false, err
		}
		return false, tx.PutAgent(a)
	}
	if err := cancelReopen(tx, a, "retired"); err != nil {
		return false, err
	}
	a.State, a.NeedsInput, a.StateDetail = v1.AgentState_AGENT_STATE_RETIRED, 0, ""
	a.StateBeforeCompact = v1.AgentState_AGENT_STATE_UNSPECIFIED
	if err := tx.RevokeTokens(a.Id); err != nil {
		return false, err
	}
	if _, err := tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id, Payload: &v1.Event_AgentRetired{
		AgentRetired: &v1.AgentRetired{Reason: reason}}}); err != nil {
		return false, err
	}
	if err := tx.PutAgent(a); err != nil {
		return false, err
	}
	return true, nil
}

// retireWithHelpersTx is retireTx and its reviewers too, in the same write
// (stopping a run retires every agent of it on its own, so it uses retireTx).
func retireWithHelpersTx(tx *store.Tx, a *v1.Agent, reason string) (bool, error) {
	changed, err := retireTx(tx, a, reason)
	if err != nil {
		return false, err
	}
	return changed, retireHelpersTx(tx, a) // also for one retired before: helpers it left running go now
}

// helpersOf are the reviewers a worker spawned that are still running: they answer to it and have no use once it is
// gone. (An asker may carry on a conversation with a parent that is resumed, so it stays.)
func helpersOf(r store.Reader, parent *v1.Agent) ([]*v1.Agent, error) {
	if parent.Role == v1.AgentRole_AGENT_ROLE_REVIEWER {
		return nil, nil
	}
	all, err := r.ListAgents(parent.RunId)
	if err != nil {
		return nil, err
	}
	var out []*v1.Agent
	for _, c := range all {
		if c.ParentId == parent.Id && !c.Peer && c.Role == v1.AgentRole_AGENT_ROLE_REVIEWER && (!stopped(c) || c.PendingReopen != nil) {
			out = append(out, c)
		}
	}
	return out, nil
}

// retireHelpersTx retires a retired worker's reviewers in the same write; their processes are stopped
// after it (stopRetiredHelpers).
func retireHelpersTx(tx *store.Tx, parent *v1.Agent) error {
	hs, err := helpersOf(tx.Reader, parent)
	if err != nil {
		return err
	}
	for _, h := range hs {
		if _, err := retireTx(tx, h, "its parent "+parent.Name+" was retired"); err != nil {
			return err
		}
	}
	return nil
}

// stopRetiredHelpers stops the processes of the reviewers a retire of parent retired with it.
func (d *Daemon) stopRetiredHelpers(ctx context.Context, parentID string) error {
	r := d.store.Read(ctx)
	all, err := r.ListAgents("")
	if err != nil {
		return err
	}
	var errs []error // one that cannot be stopped does not leave the others running
	for _, c := range all {
		if c.ParentId == parentID && c.State == v1.AgentState_AGENT_STATE_RETIRED && c.TerminalId != "" &&
			!c.Peer && c.Role == v1.AgentRole_AGENT_ROLE_REVIEWER {
			errs = append(errs, d.stopRetiredProcess(ctx, c.Id))
		}
	}
	return errors.Join(errs...)
}

// mayRetire is who may retire an agent: a device, its parent, or the root of its run (a worker's reviewer outlives
// the worker, and its parent is gone or busy; the root answers for the whole run).
func mayRetire(actor *v1.Actor, a *v1.Agent) error {
	if actor.Class == v1.ActorClass_ACTOR_CLASS_ROOT && actor.RunId == a.RunId && a.Role != v1.AgentRole_AGENT_ROLE_ROOT {
		return nil
	}
	return mayMove(actor, a, "retire")
}

// stopRetiredProcess stops a retired agent's process and lets the terminal go, under the agent's lock. An exit
// the host reports meanwhile finds the agent retired and only acknowledges the terminal.
func (d *Daemon) stopRetiredProcess(ctx context.Context, agentID string) error {
	// Its launch no longer counts against the memory guard, and a caller that gives up now would leave the process
	// running under a retired agent: the stop is bounded by StopWait, not by the caller.
	d.rt.guard.release(agentID)
	_ = os.Remove(d.leanMCPFile(agentID))
	ctx = context.WithoutCancel(ctx)
	if a, err := d.store.Read(ctx).GetAgent(agentID); err == nil && isAdopted(a) {
		_ = os.Remove(d.paths.Adopted(int(a.AdoptedPid))) // its token is revoked: the file names nothing
	}
	if d.rt.term == nil {
		return nil // no host: nothing to stop; the terminal id stays, for a boot with one
	}
	a, err := d.store.Read(ctx).GetAgent(agentID)
	if err != nil || a.State != v1.AgentState_AGENT_STATE_RETIRED || a.TerminalId == "" {
		return err
	}
	if err := d.stopTerminal(ctx, a.TerminalId); err != nil {
		return err
	}
	return d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		cur, err := tx.GetAgent(agentID)
		if err != nil || cur.State != v1.AgentState_AGENT_STATE_RETIRED || cur.TerminalId != a.TerminalId {
			return err
		}
		cur.TerminalId = ""
		return tx.PutAgent(cur)
	})
}

// sweepAsync runs sweepWorktrees in a daemon-tracked goroutine that aborts on daemon shutdown (d.stopping),
// keeping RetireAgent, autoRetire, and boot adopt off the sweep's poll wait.
func (d *Daemon) sweepAsync(agents []*v1.Agent) {
	if len(agents) == 0 {
		return
	}
	// Refused once close() began (a retire that finishes during shutdown): the leftover processes are then the
	// next start's to find, not a goroutine reading a store that is closing.
	d.goBG(func() {
		ctx, cancel := d.stopCtx()
		defer cancel()
		d.sweepWorktrees(ctx, agents)
	})
}

// finishRetire stops a retired agent's process synchronously and sweeps any leftover processes in its worktree
// in the background.
func (d *Daemon) finishRetire(ctx context.Context, agentID string) error {
	ctx = context.WithoutCancel(ctx)
	// The reviewers are stopped whether or not the worker's own stop worked: they are retired, token gone, and
	// no later pass would stop them.
	stopErr := errors.Join(d.stopRetiredProcess(ctx, agentID), d.stopRetiredHelpers(ctx, agentID))
	if stopErr != nil {
		return stopErr
	}
	if a, err := d.store.Read(ctx).GetAgent(agentID); err == nil {
		d.sweepAsync([]*v1.Agent{a})
	}
	return nil
}

// RetireAgent retires an agent: its process stops, its conversation stays.
func (d *Daemon) RetireAgent(ctx context.Context, req *connect.Request[v1.RetireAgentRequest]) (*connect.Response[v1.RetireAgentResponse], error) {
	actor := auth.ActorFrom(ctx)
	found, err := d.FindAgent(d.store.Read(ctx), actor, req.Msg.Agent)
	if err != nil {
		return nil, err
	}
	mu := d.agentLock(found.Id)
	mu.Lock()
	defer mu.Unlock()
	out := &v1.RetireAgentResponse{}
	replayed, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceRetireAgentProcedure, out, func(tx *store.Tx) error {
		a, err := tx.GetAgent(found.Id)
		if err != nil {
			return err
		}
		if err := mayRetire(tx.Actor, a); err != nil {
			return err
		}
		if _, err := retireWithHelpersTx(tx, a, "requested"); err != nil {
			return err
		}
		out.Agent = a
		return nil
	})
	if err != nil {
		return nil, err
	}
	if replayed {
		return connect.NewResponse(out), nil // what the first call answered, not the agent as it is now
	}
	if err := d.finishRetire(ctx, found.Id); err != nil {
		return nil, err
	}
	cur, err := d.store.Read(ctx).GetAgent(found.Id)
	if err != nil {
		return nil, err
	}
	out.Agent = cur
	return connect.NewResponse(out), nil
}

// ── idle retire ──

// retireQuiet is how often the retire pass runs by default.
const retireQuiet = 10 * time.Second

// retireIdle retires the agents that reported done or failed and then stayed idle `idle_retire` seconds (0 never), a
// reviewer or an asker after `idle_retire.review` instead when that is the shorter (nothing comes back for it: a
// re-review or a follow-up ask resumes the same conversation). Not: a root; one with a reopen pending; one with
// unread mail (work is coming, or a doorbell was lost); one whose newest report since its last task or answer isn't
// done or failed (a plan or question waits on its parent); or one a client with input rights is attached to (the
// viewer rule: someone is looking).
func (d *Daemon) retireIdle(ctx context.Context) {
	if d.rt.term == nil {
		return
	}
	r := d.store.Read(ctx).WithMessageCache() // a run's messages are read once for the pass, not once per agent
	v, err := d.Setting(r, "idle_retire")
	if err != nil {
		return
	}
	afterWork := time.Duration(v.(float64) * float64(time.Second))
	if afterWork <= 0 {
		return
	}
	afterReview := afterWork
	if v, err := d.Setting(r, "idle_retire.review"); err == nil {
		if f, ok := v.(float64); ok && f > 0 {
			afterReview = min(afterWork, time.Duration(f*float64(time.Second)))
		}
	}
	agents, err := r.ListAgents("")
	if err != nil {
		return
	}
	var viewed map[string]bool
	for _, a := range agents {
		if !atPrompt(a.State) || a.Role == v1.AgentRole_AGENT_ROLE_ROOT || a.PendingReopen != nil ||
			a.TerminalId == "" {
			continue
		}
		report, err := lastReport(r, a)
		if err != nil || report == nil ||
			(report.Type != v1.MessageType_MESSAGE_TYPE_DONE && report.Type != v1.MessageType_MESSAGE_TYPE_FAILED) {
			continue
		}
		after := afterWork
		if a.Role == v1.AgentRole_AGENT_ROLE_REVIEWER || a.Role == v1.AgentRole_AGENT_ROLE_ASKER {
			after = afterReview
		}
		since := report.CreatedAt.AsTime()
		if a.LastActivity != nil && a.LastActivity.AsTime().After(since) {
			since = a.LastActivity.AsTime()
		}
		if d.now().Sub(since) < after {
			continue
		}
		if unread, err := r.Inbox(a.Id, false); err != nil || len(unread) > 0 {
			continue
		}
		if viewed == nil {
			if viewed, err = d.viewedTerminals(ctx); err != nil {
				return
			}
		}
		if viewed[a.TerminalId] {
			continue
		}
		if err := d.autoRetire(ctx, a.Id, report.Id, after); err != nil && ctx.Err() == nil {
			d.log.Warn("idle retire", "agent", a.Name, "err", err)
		}
	}
}

// viewedTerminals are the terminals a client with input rights is attached to.
func (d *Daemon) viewedTerminals(ctx context.Context) (map[string]bool, error) {
	res, err := d.rt.term.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, t := range res.Msg.Terminals {
		out[t.Id] = t.InputClients > 0
	}
	return out, nil
}

// autoRetire retires an agent the pass found due, re-checking under the agent's lock and inside the transaction
// that a message, a reopen or a turn didn't arrive since: either this sees it and the agent stays open, or the
// message finds it retired and resumes it.
func (d *Daemon) autoRetire(ctx context.Context, agentID, reportID string, after time.Duration) error {
	mu := d.agentLock(agentID)
	mu.Lock()
	defer mu.Unlock()
	// The pass read the viewers once, before earlier agents were stopped (which takes a while): ask again.
	if cur, err := d.store.Read(ctx).GetAgent(agentID); err == nil && cur.TerminalId != "" {
		if viewed, err := d.viewedTerminals(ctx); err != nil || viewed[cur.TerminalId] {
			return err
		}
	}
	retired := false
	err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		a, err := tx.GetAgent(agentID)
		if err != nil {
			return err
		}
		if !atPrompt(a.State) || a.PendingReopen != nil {
			return nil
		}
		report, err := lastReport(tx.Reader, a)
		if err != nil || report == nil || report.Id != reportID {
			return err
		}
		// A turn since the pass looked (a person typing into it makes no message) keeps it open.
		since := report.CreatedAt.AsTime()
		if a.LastActivity != nil && a.LastActivity.AsTime().After(since) {
			since = a.LastActivity.AsTime()
		}
		if tx.Now().Sub(since) < after {
			return nil
		}
		if unread, err := tx.Inbox(a.Id, false); err != nil || len(unread) > 0 {
			return err
		}
		retired, err = retireWithHelpersTx(tx, a, "idle after its report")
		return err
	})
	if err != nil || !retired {
		return err
	}
	return d.finishRetire(ctx, agentID)
}

// ── stopping a run ──

// StopRun retires every agent of a run and closes it. A clean worktree is removed, and its branch with it unless
// the branch moved past where it started or was pushed; a worktree with uncommitted changes stays, with its branch.
func (d *Daemon) StopRun(ctx context.Context, req *connect.Request[v1.StopRunRequest]) (*connect.Response[v1.StopRunResponse], error) {
	res, _, err := d.stopRun(ctx, req, false)
	return res, err
}

// stopRun is StopRun. With requireEnded (DeleteRun) it refuses, inside the transaction that holds every agent's lock, a run
// with an agent that has a process, so nothing is retired or stopped for a run that is refused; the second answer is the
// agents whose process could not be stopped, with why.
func (d *Daemon) stopRun(ctx context.Context, req *connect.Request[v1.StopRunRequest], requireEnded bool) (*connect.Response[v1.StopRunResponse], map[string]string, error) {
	out := &v1.StopRunResponse{}
	// Every agent of the run is locked first, in id order, so a reopen or a doorbell in flight finishes before the
	// run is closed, and none starts after (the reopen finds its request dropped). An agent spawned meanwhile
	// shows up inside the transaction, unlocked: the transaction is then undone, every lock is let go and the whole
	// set, with the new agent, is taken again in id order. Locking only the new one while holding the rest could
	// deadlock with a second StopRun that holds it and waits for one of ours.
	locked := map[string]bool{}
	var unlock []func()
	release := func() {
		for _, f := range unlock {
			f()
		}
		unlock = nil
	}
	defer release()
	lock := func(add []string) {
		release()
		for _, id := range add {
			locked[id] = true
		}
		ids := make([]string, 0, len(locked))
		for id := range locked {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			mu := d.agentLock(id)
			mu.Lock()
			unlock = append(unlock, mu.Unlock)
		}
	}
	before, err := d.store.Read(ctx).ListAgents(req.Msg.RunId)
	if err != nil {
		return nil, nil, err
	}
	var ids []string
	for _, a := range before {
		ids = append(ids, a.Id)
	}
	lock(ids)
	if stopRunLockedHook != nil {
		stopRunLockedHook()
	}
	errUnlocked := errors.New("an agent was started meanwhile")
	var agents []*v1.Agent
	var lateIDs []string
	var replayed bool
	for {
		lateIDs = nil
		replayed, err = d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceStopRunProcedure, out, func(tx *store.Tx) error {
			run, err := tx.GetRun(req.Msg.RunId)
			if errors.Is(err, store.ErrNotFound) {
				return notFound("no run %q", req.Msg.RunId)
			}
			if err != nil {
				return err
			}
			all, err := tx.ListAgents(run.Id)
			if err != nil {
				return err
			}
			var late []string
			for _, a := range all {
				if !locked[a.Id] {
					late = append(late, a.Id)
				}
			}
			if len(late) > 0 {
				lateIDs = late
				return errUnlocked
			}
			agents = all
			if requireEnded {
				for _, a := range all {
					if hasProcess(a) {
						return failedPrecondition("%s is still running: stop the run first", a.Name)
					}
				}
			}
			for _, a := range all {
				changed, err := retireTx(tx, a, "run stopped")
				if err != nil {
					return err
				}
				if changed {
					out.Retired = append(out.Retired, a.Id)
				}
			}
			if run.Status != v1.RunStatus_RUN_STATUS_CLOSED {
				run.Status = v1.RunStatus_RUN_STATUS_CLOSED
				if err := tx.UpdateRun(run); err != nil {
					return err
				}
				if _, err := tx.AppendEvent(&v1.Event{RunId: run.Id, Payload: &v1.Event_RunStopped{RunStopped: &v1.RunStopped{}}}); err != nil {
					return err
				}
			}
			out.Run = run
			return nil
		})
		if errors.Is(err, errUnlocked) {
			lock(lateIDs)
			continue
		}
		break
	}
	if err != nil {
		return nil, nil, err
	}
	// Outside the transaction: stopping a process waits for it. All at once, so that the locks held meanwhile
	// are held for one stop (StopWait at most), not one per agent.
	notStopped := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, a := range agents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.stopRetiredProcess(ctx, a.Id); err != nil {
				d.log.Warn("stopping an agent of a stopped run", "agent", a.Name, "err", err)
				mu.Lock()
				notStopped[a.Id] = err.Error()
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if replayed {
		// A replayed call's stored response already reports its stop; avoid repeating filesystem/sweep work or appending outcomes.
		return connect.NewResponse(out), notStopped, nil
	}
	// Sweep leftover processes workers left in their worktrees (already retired or exited ones too) before
	// removing those worktrees, skipping any worktree held by a live agent or another open run.
	busyWorktrees := map[string]bool{}
	openRuns := map[string]bool{}
	sr := d.store.Read(ctx)
	if runs, rerr := sr.ListRuns(false); rerr == nil {
		for _, r := range runs {
			openRuns[r.Id] = true
		}
	}
	if all, aerr := sr.AllAgents(); aerr == nil {
		for _, other := range all {
			if other.RunId != req.Msg.RunId && other.Worktree != "" &&
				(hasProcess(other) || openRuns[other.RunId]) {
				busyWorktrees[other.Worktree] = true
			}
		}
	}
	var sweepAgents []*v1.Agent
	seenSweep := map[string]bool{}
	for _, a := range agents {
		if a.Worktree != "" {
			seenSweep[a.Worktree] = true
		}
		if notStopped[a.Id] == "" && !busyWorktrees[a.Worktree] {
			sweepAgents = append(sweepAgents, a)
		}
	}
	wts, werr := sr.ListWorktrees(req.Msg.RunId)
	if werr == nil {
		for _, wt := range wts {
			if wt.Path != "" && !seenSweep[wt.Path] && !busyWorktrees[wt.Path] {
				seenSweep[wt.Path] = true
				sweepAgents = append(sweepAgents, &v1.Agent{
					Id:       wt.Id,
					RunId:    wt.RunId,
					Role:     v1.AgentRole_AGENT_ROLE_WORKER,
					Worktree: wt.Path,
				})
			}
		}
	}
	d.sweepWorktrees(context.WithoutCancel(ctx), sweepAgents)
	seenWT := map[string]bool{}
	for _, a := range agents {
		if a.Worktree != "" {
			seenWT[a.Worktree] = true
		}
		if why, bad := notStopped[a.Id]; bad {
			out.Worktrees = append(out.Worktrees, &v1.WorktreeOutcome{AgentId: a.Id, Worktree: a.Worktree, Branch: a.Branch,
				BranchKept: true, Reason: "its process could not be stopped, so its worktree stays: " + firstLine(why)})
			continue
		}
		if busyWorktrees[a.Worktree] {
			out.Worktrees = append(out.Worktrees, &v1.WorktreeOutcome{AgentId: a.Id, Worktree: a.Worktree, Branch: a.Branch,
				BranchKept: true, Reason: "worktree is held by another run (open or with a live agent)"})
			continue
		}
		out.Worktrees = append(out.Worktrees, d.lockedRemoveWorktree(ctx, a.Id, a.Repo, a.Worktree, a.Branch, a.BaseCommit, a.Peer, a.Plain))
	}
	if werr != nil {
		d.log.Warn("listing standalone worktrees on stop", "run", req.Msg.RunId, "err", werr)
		out.Worktrees = append(out.Worktrees, &v1.WorktreeOutcome{
			BranchKept: true,
			Reason:     "couldn't list standalone worktrees: " + firstLine(werr.Error()),
		})
	} else {
		for _, wt := range wts {
			if seenWT[wt.Path] {
				continue
			}
			seenWT[wt.Path] = true
			if busyWorktrees[wt.Path] {
				out.Worktrees = append(out.Worktrees, &v1.WorktreeOutcome{Worktree: wt.Path, Branch: wt.Branch,
					BranchKept: true, Reason: "worktree is held by another run (open or with a live agent)"})
				continue
			}
			outcome := d.lockedRemoveWorktree(ctx, "", wt.Repo, wt.Path, wt.Branch, wt.BaseCommit, false, false)
			if outcome.WorktreeRemoved {
				if derr := d.store.Write(ctx, auth.ActorFrom(ctx), "", func(tx *store.Tx) error {
					return tx.DeleteWorktree(wt.Id)
				}); derr != nil {
					d.log.Warn("deleting removed worktree row", "run", wt.RunId, "worktree", wt.Id, "err", derr)
				}
			}
			out.Worktrees = append(out.Worktrees, outcome)
		}
	}
	return connect.NewResponse(out), notStopped, nil
}

func (d *Daemon) lockedRemoveWorktree(ctx context.Context, agentID, repo, worktree, branch, baseCommit string, peer, plain bool) *v1.WorktreeOutcome {
	if worktree == "" {
		return d.removeWorktreePath(ctx, agentID, repo, worktree, branch, baseCommit, peer, plain)
	}
	wmu := d.worktreeLock(worktree)
	wmu.Lock()
	defer wmu.Unlock()
	return d.removeWorktreePath(ctx, agentID, repo, worktree, branch, baseCommit, peer, plain)
}

// removeWorktreePath removes a worktree when it is clean and deletes its branch unless it holds work. Nothing
// is ever forced: a dirty worktree, or one git refuses to remove, stays. A plain agent's branch also goes once what it
// holds is in the repo's default branch (it was merged: its run and branch are made at each "New agent", and nothing
// else would ever clear them).
func (d *Daemon) removeWorktreePath(ctx context.Context, agentID, repo, worktree, branch, baseCommit string, peer, plain bool) *v1.WorktreeOutcome {
	o := &v1.WorktreeOutcome{AgentId: agentID, Worktree: worktree, Branch: branch, BranchKept: true}
	if worktree == "" || repo == "" {
		o.Reason = "no worktree recorded"
		return o
	}
	if peer {
		o.Reason = "a peer's worktree is kept: it is the peer's, not this run's"
		return o
	}
	var status string
	var err error
	_, statErr := os.Stat(worktree)
	gone := errors.Is(statErr, os.ErrNotExist) // not the text of git's error: it follows the locale
	if !gone {
		status, err = git(ctx, worktree, "status", "--porcelain")
	}
	switch {
	case err != nil:
		o.Reason = "can't read its status: " + firstLine(err.Error())
		return o
	case gone:
		// Git still lists it, and would not delete the branch of a worktree it lists.
		git(ctx, repo, "worktree", "prune")
		o.WorktreeRemoved = true
		o.Reason = "its worktree was already gone"
	case status != "":
		o.Reason = "uncommitted changes: the worktree and its branch stay"
		return o
	default:
		if _, err := git(ctx, repo, "worktree", "remove", worktree); err != nil {
			o.Reason = "git would not remove it: " + firstLine(err.Error())
			return o
		}
		o.WorktreeRemoved = true
	}
	if branch == "" {
		return o
	}
	// Its commits being in the default branch, deleting it loses nothing (a branch the default has not got stays).
	// A branch that is gone already (a run stopped before) has nothing to keep.
	// (Only when the repo itself reads: if it moved, the branch may be there.)
	_, gitDirErr := git(ctx, repo, "rev-parse", "--git-dir")
	if _, err := git(ctx, repo, "rev-parse", "--verify", "-q", "refs/heads/"+branch); err != nil && gone && gitDirErr == nil {
		o.BranchKept = false
		o.Reason = "its branch was already gone"
		return o
	}
	if !plain || !branchMerged(ctx, repo, branch) {
		if has, why := branchHasWork(ctx, repo, branch, baseCommit); has {
			o.Reason = why
			return o
		}
	}
	if _, err := git(ctx, repo, "branch", "-D", branch); err != nil {
		o.Reason = "couldn't delete its branch: " + firstLine(err.Error())
		return o
	}
	o.BranchKept = false
	return o
}

// branchMerged says whether a branch holds nothing a default branch of a remote of the repo (`origin/HEAD`, `origin/main`
// or `origin/master`, as the repo last saw them: any of those that exist) has not: every commit of it is in the default
// branch, or merging it into the default branch changes nothing (`merge-tree`: the merge's tree is the default's own),
// which is what a squash or a rebase merge leaves, as the branch's own commits are not in the default branch then.
func branchMerged(ctx context.Context, repo, branch string) bool {
	tip, err := git(ctx, repo, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	if err != nil || tip == "" {
		return false
	}
	for _, ref := range []string{"refs/remotes/origin/HEAD", "refs/remotes/origin/main", "refs/remotes/origin/master"} {
		if _, err := git(ctx, repo, "rev-parse", "--verify", "-q", ref); err != nil {
			continue
		}
		if gitIsAncestor(ctx, repo, tip, ref) || mergeAddsNothing(ctx, repo, ref, tip) {
			return true
		}
	}
	return false
}

// treeOf is the tree a commit has ("" when git can't say).
func treeOf(ctx context.Context, repo, commit string) string {
	out, err := gitRO(ctx, repo, "rev-parse", "--verify", "-q", commit+"^{tree}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// mergeAddsNothing says whether merging `tip` into `ref` would leave `ref`'s tree as it is: the change the branch made is
// already there (a conflict, or a git too old for `merge-tree --write-tree`, says no).
func mergeAddsNothing(ctx context.Context, repo, ref, tip string) bool {
	// A branch whose commits change no file (empty ones) has no change that could be "already there": its commits are its
	// own work, kept as before.
	if base, err := gitRO(ctx, repo, "merge-base", ref, tip); err != nil || base == "" || treeOf(ctx, repo, base) == treeOf(ctx, repo, tip) {
		return false
	}
	merged, err := gitRO(ctx, repo, "merge-tree", "--write-tree", ref, tip)
	if err != nil {
		return false
	}
	own := treeOf(ctx, repo, ref)
	return own != "" && strings.TrimSpace(strings.SplitN(merged, "\n", 2)[0]) == own
}

// branchHasWork reports whether a branch is worth keeping: it moved past the commit it started at (its own work,
// even when another branch stacked on it reaches the same commits), or a remote branch of its name exists (it was
// pushed, so it may back a PR).
func branchHasWork(ctx context.Context, repo, branch, baseCommit string) (bool, string) {
	// A remote branch of its name means it was pushed. (Not "has an upstream": a worktree started from origin/main
	// has that one for free.)
	if refs, err := git(ctx, repo, "for-each-ref", "--format=%(refname)", "refs/remotes/*/"+branch); err == nil && refs != "" {
		return true, "its branch was pushed: kept"
	}
	tip, err := git(ctx, repo, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	if err != nil || tip == "" {
		return true, "can't read its branch: kept"
	}
	if baseCommit != "" {
		if tip != baseCommit {
			return true, "its branch has commits of its own: kept"
		}
		return false, ""
	}
	// An agent recorded without its start: commits reachable from the branch and from no other branch or remote.
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "rev-list", "--count", "refs/heads/"+branch,
		"--not", "--exclude="+branch, "--branches", "--remotes")
	if b, err := cmd.Output(); err != nil || strings.TrimSpace(string(b)) != "0" {
		return true, "its branch has commits of its own: kept"
	}
	return false, ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// retireReviewersOfPR retires the reviewers of a PR that merged or closed: the review is over, and nothing will
// ask them again. A reviewer still on a turn, or with mail unread, is left: it is looked at again when the PR is
// next read.
func (d *Daemon) retireReviewersOfPR(ctx context.Context, runID, prURL, state string) {
	asks, err := prAsksByKey(d.store.Read(ctx), prURL)
	if err != nil {
		return
	}
	done := map[string]bool{}
	for _, ask := range asks {
		if ask.RunId != runID {
			continue
		}
		for _, m := range ask.Members {
			if m.AgentId == "" || done[m.AgentId] {
				continue
			}
			done[m.AgentId] = true
			if cur, err := d.store.Read(ctx).GetAgent(m.AgentId); err != nil || stopped(cur) {
				continue // the common case once a PR has merged: nothing to write
			}
			if err := d.retireReviewer(ctx, m.AgentId, "its PR "+state); err != nil && ctx.Err() == nil {
				d.log.Warn("retiring a reviewer of a finished PR", "agent", m.Name, "err", err)
			}
		}
	}
}

// retireReviewer retires one idle reviewer, re-checking under the agent's lock and inside the write that nothing
// arrived for it since (the rule autoRetire keeps).
func (d *Daemon) retireReviewer(ctx context.Context, agentID, reason string) error {
	mu := d.agentLock(agentID)
	mu.Lock()
	defer mu.Unlock()
	retired := false
	err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		a, err := tx.GetAgent(agentID)
		if errors.Is(err, store.ErrNotFound) {
			return nil // gone
		}
		if err != nil {
			return err
		}
		if a.Role != v1.AgentRole_AGENT_ROLE_REVIEWER || a.Peer || !atPrompt(a.State) || a.PendingReopen != nil {
			return nil
		}
		if unread, err := tx.Inbox(a.Id, false); err != nil || len(unread) > 0 {
			return err
		}
		retired, err = retireTx(tx, a, reason)
		return err
	})
	if err != nil || !retired {
		return err
	}
	return d.finishRetire(ctx, agentID)
}
