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

// stopRetiredProcess stops a retired agent's process and lets the terminal go, under the agent's lock. An exit
// the host reports meanwhile finds the agent retired and only acknowledges the terminal.
func (d *Daemon) stopRetiredProcess(ctx context.Context, agentID string) error {
	// Its launch no longer counts against the memory guard, and a caller that gives up now would leave the process
	// running under a retired agent: the stop is bounded by StopWait, not by the caller.
	d.rt.guard.release(agentID)
	ctx = context.WithoutCancel(ctx)
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
	d.bgWG.Add(1)
	go func() {
		defer d.bgWG.Done()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-d.stopping:
				cancel()
			case <-ctx.Done():
			}
		}()
		d.sweepWorktrees(ctx, agents)
	}()
}

// finishRetire stops a retired agent's process synchronously and sweeps any leftover processes in its worktree
// in the background.
func (d *Daemon) finishRetire(ctx context.Context, agentID string) error {
	ctx = context.WithoutCancel(ctx)
	if err := d.stopRetiredProcess(ctx, agentID); err != nil {
		return err
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
		if err := mayMove(tx.Actor, a, "retire"); err != nil {
			return err
		}
		if _, err := retireTx(tx, a, "requested"); err != nil {
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

// retireIdle retires the agents that reported done or failed and then stayed idle `idle_retire` seconds (0 never).
// Not: a root; one with a reopen pending; one with unread mail (work is coming, or a doorbell was lost); one whose
// newest report since its last task or answer isn't done or failed (a plan or question waits on its parent); or
// one a client with input rights is attached to (the viewer rule: someone is looking).
func (d *Daemon) retireIdle(ctx context.Context) {
	if d.rt.term == nil {
		return
	}
	r := d.store.Read(ctx)
	v, err := d.Setting(r, "idle_retire")
	if err != nil {
		return
	}
	after := time.Duration(v.(float64) * float64(time.Second))
	if after <= 0 {
		return
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
		retired, err = retireTx(tx, a, "idle after its report")
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
		return nil, err
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
	for {
		lateIDs = nil
		_, err = d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceStopRunProcedure, out, func(tx *store.Tx) error {
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
		return nil, err
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
	// Sweep leftover processes workers left in their worktrees (already retired or exited ones too) before
	// removing those worktrees, skipping any worktree held by a live agent or another open run.
	busyWorktrees := map[string]bool{}
	openRuns := map[string]bool{}
	sr := d.store.Read(ctx)
	if runs, rerr := sr.ListRuns(true); rerr == nil {
		for _, r := range runs {
			openRuns[r.Id] = true
		}
	}
	if all, aerr := sr.AllAgents(); aerr == nil {
		for _, other := range all {
			if other.RunId != req.Msg.RunId && other.Worktree != "" && (hasProcess(other) || openRuns[other.RunId]) {
				busyWorktrees[other.Worktree] = true
			}
		}
	}
	var sweepAgents []*v1.Agent
	for _, a := range agents {
		if notStopped[a.Id] == "" && !busyWorktrees[a.Worktree] {
			sweepAgents = append(sweepAgents, a)
		}
	}
	d.sweepWorktrees(context.WithoutCancel(ctx), sweepAgents)
	// A replayed call has no agents: its stored response already says what happened (without the worktrees, which
	// are only reported by the call that removed them).
	for _, a := range agents {
		if why, bad := notStopped[a.Id]; bad {
			out.Worktrees = append(out.Worktrees, &v1.WorktreeOutcome{AgentId: a.Id, Worktree: a.Worktree, Branch: a.Branch,
				BranchKept: true, Reason: "its process could not be stopped, so its worktree stays: " + firstLine(why)})
			continue
		}
		if busyWorktrees[a.Worktree] {
			out.Worktrees = append(out.Worktrees, &v1.WorktreeOutcome{AgentId: a.Id, Worktree: a.Worktree, Branch: a.Branch,
				BranchKept: true, Reason: "worktree is held by a live agent in another run"})
			continue
		}
		var wo *v1.WorktreeOutcome
		if a.Worktree != "" {
			wmu := d.worktreeLock(a.Worktree)
			wmu.Lock()
			wo = d.removeWorktree(ctx, a)
			wmu.Unlock()
		} else {
			wo = d.removeWorktree(ctx, a)
		}
		out.Worktrees = append(out.Worktrees, wo)
	}
	return connect.NewResponse(out), nil
}

// removeWorktree removes an agent's worktree when it is clean and deletes its branch unless it holds work. Nothing
// is ever forced: a dirty worktree, or one git refuses to remove, stays.
func (d *Daemon) removeWorktree(ctx context.Context, a *v1.Agent) *v1.WorktreeOutcome {
	o := &v1.WorktreeOutcome{AgentId: a.Id, Worktree: a.Worktree, Branch: a.Branch, BranchKept: true}
	if a.Worktree == "" || a.Repo == "" {
		o.Reason = "no worktree recorded"
		return o
	}
	var status string
	var err error
	_, statErr := os.Stat(a.Worktree)
	gone := errors.Is(statErr, os.ErrNotExist) // not the text of git's error: it follows the locale
	if !gone {
		status, err = git(ctx, a.Worktree, "status", "--porcelain")
	}
	switch {
	case err != nil:
		o.Reason = "can't read its status: " + firstLine(err.Error())
		return o
	case gone:
		// Git still lists it, and would not delete the branch of a worktree it lists.
		git(ctx, a.Repo, "worktree", "prune")
		o.WorktreeRemoved = true
		o.Reason = "its worktree was already gone"
	case status != "":
		o.Reason = "uncommitted changes: the worktree and its branch stay"
		return o
	default:
		if _, err := git(ctx, a.Repo, "worktree", "remove", a.Worktree); err != nil {
			o.Reason = "git would not remove it: " + firstLine(err.Error())
			return o
		}
		o.WorktreeRemoved = true
	}
	if a.Branch == "" {
		return o
	}
	if has, why := branchHasWork(ctx, a); has {
		o.Reason = why
		return o
	}
	if _, err := git(ctx, a.Repo, "branch", "-D", a.Branch); err != nil {
		o.Reason = "couldn't delete its branch: " + firstLine(err.Error())
		return o
	}
	o.BranchKept = false
	return o
}

// branchHasWork reports whether a branch is worth keeping: it moved past the commit it started at (its own work,
// even when another branch stacked on it reaches the same commits), or a remote branch of its name exists (it was
// pushed, so it may back a PR).
func branchHasWork(ctx context.Context, a *v1.Agent) (bool, string) {
	// A remote branch of its name means it was pushed. (Not "has an upstream": a worktree started from origin/main
	// has that one for free.)
	if refs, err := git(ctx, a.Repo, "for-each-ref", "--format=%(refname)", "refs/remotes/*/"+a.Branch); err == nil && refs != "" {
		return true, "its branch was pushed: kept"
	}
	tip, err := git(ctx, a.Repo, "rev-parse", "--verify", "-q", "refs/heads/"+a.Branch)
	if err != nil || tip == "" {
		return true, "can't read its branch: kept"
	}
	if a.BaseCommit != "" {
		if tip != a.BaseCommit {
			return true, "its branch has commits of its own: kept"
		}
		return false, ""
	}
	// An agent recorded without its start: commits reachable from the branch and from no other branch or remote.
	cmd := exec.CommandContext(ctx, "git", "-C", a.Repo, "rev-list", "--count", "refs/heads/"+a.Branch,
		"--not", "--exclude="+a.Branch, "--branches", "--remotes")
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
