package daemon

// Retiring: an agent that has nothing left to do stops its process and keeps its conversation (design §18). Its
// state is RETIRED, its token revoked, its worktree and branch kept; a message to it, or ResumeAgent, reopens
// the same conversation. Retire is called by hand (RetireAgent), by StopRun for a whole run, and by the daemon
// itself for an agent idle after its report.

import (
	"context"
	"errors"
	"os/exec"
	"sort"
	"strings"
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
		return false, nil
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

// finishRetire stops a retired agent's process and lets the terminal go, under the agent's lock. An exit the
// host reports meanwhile finds the agent retired and only acknowledges the terminal.
func (d *Daemon) finishRetire(ctx context.Context, agentID string) error {
	if d.rt.term == nil {
		return nil
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
	_, err = d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceRetireAgentProcedure, out, func(tx *store.Tx) error {
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
		if a.State != v1.AgentState_AGENT_STATE_IDLE || a.Role == v1.AgentRole_AGENT_ROLE_ROOT || a.PendingReopen != nil ||
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
		if err := d.autoRetire(ctx, a.Id, report.Id); err != nil && ctx.Err() == nil {
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
func (d *Daemon) autoRetire(ctx context.Context, agentID, reportID string) error {
	mu := d.agentLock(agentID)
	mu.Lock()
	defer mu.Unlock()
	retired := false
	err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		a, err := tx.GetAgent(agentID)
		if err != nil {
			return err
		}
		if a.State != v1.AgentState_AGENT_STATE_IDLE || a.PendingReopen != nil {
			return nil
		}
		if report, err := lastReport(tx.Reader, a); err != nil || report == nil || report.Id != reportID {
			return err
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
// the branch has a commit of its own or an upstream; a worktree with uncommitted changes stays, with its branch.
func (d *Daemon) StopRun(ctx context.Context, req *connect.Request[v1.StopRunRequest]) (*connect.Response[v1.StopRunResponse], error) {
	out := &v1.StopRunResponse{}
	// Every agent of the run is locked first, in id order, so a reopen or a doorbell in flight finishes before the
	// run is closed, and none starts after (the reopen finds its request dropped).
	before, err := d.store.Read(ctx).ListAgents(req.Msg.RunId)
	if err != nil {
		return nil, err
	}
	sort.Slice(before, func(i, j int) bool { return before[i].Id < before[j].Id })
	for _, a := range before {
		mu := d.agentLock(a.Id)
		mu.Lock()
		defer mu.Unlock()
	}
	var agents []*v1.Agent
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
	if err != nil {
		return nil, err
	}
	// Outside the transaction: stopping a process waits for it.
	notStopped := map[string]string{}
	for _, a := range agents {
		if err := d.finishRetire(ctx, a.Id); err != nil {
			d.log.Warn("stopping an agent of a stopped run", "agent", a.Name, "err", err)
			notStopped[a.Id] = err.Error()
		}
	}
	// A replayed call has no agents: its stored response already says what happened (without the worktrees, which
	// are only reported by the call that removed them).
	for _, a := range agents {
		if why, bad := notStopped[a.Id]; bad {
			out.Worktrees = append(out.Worktrees, &v1.WorktreeOutcome{AgentId: a.Id, Worktree: a.Worktree, Branch: a.Branch,
				BranchKept: true, Reason: "its process could not be stopped, so its worktree stays: " + firstLine(why)})
			continue
		}
		out.Worktrees = append(out.Worktrees, d.removeWorktree(ctx, a))
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
	status, err := git(ctx, a.Worktree, "status", "--porcelain")
	switch {
	case err != nil && !strings.Contains(err.Error(), "No such file"):
		o.Reason = "can't read its status: " + firstLine(err.Error())
		return o
	case err != nil:
		o.WorktreeRemoved = true // already gone
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
	if has, why := branchHasWork(ctx, a.Repo, a.Branch); has {
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

// branchHasWork reports whether a branch is worth keeping: a commit no other ref has (its own work), or a remote
// branch of its name (it was pushed, so it may back a PR).
func branchHasWork(ctx context.Context, repo, branch string) (bool, string) {
	// A remote branch of its name means it was pushed. (Not "has an upstream": a worktree started from origin/main
	// has that one for free.)
	if refs, err := git(ctx, repo, "for-each-ref", "--format=%(refname)", "refs/remotes/*/"+branch); err == nil && refs != "" {
		return true, "its branch was pushed: kept"
	}
	// Commits reachable from the branch and from no other branch or remote.
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
