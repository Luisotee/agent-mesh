package daemon

// `mesh adopt`: a user's own Claude Code session becomes a run's root, in place (docs/apps.md, "Entry point"). The
// session has no terminal of the daemon's, no worktree and no hooks, so the daemon knows less of it than of a root
// it started: it rings it through its inbox socket (one frame per message, no retries: it can't read the box),
// follows it by the process and the session record Claude Code keeps (`sessions/<pid>.json`), and never signals
// it. Its `mesh` calls act as the root through a token kept in `<state>/adopted/<pid>.json`, which the CLI finds
// by walking up its own process tree (cli/adopt.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// isAdopted reports whether an agent is a user's own session the daemon adopted.
func isAdopted(a *v1.Agent) bool { return a.AdoptedPid > 0 }

// claudeEnv is the environment the daemon reads Claude Code's own files by (its config folder).
func (d *Daemon) claudeEnv() map[string]string {
	env := map[string]string{}
	for _, k := range []string{"HOME", "CLAUDE_CONFIG_DIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	for k, v := range d.rt.cfg.Env {
		env[k] = v
	}
	return env
}

// AdoptSession makes a running Claude Code session the root of a run.
func (d *Daemon) AdoptSession(ctx context.Context, req *connect.Request[v1.AdoptSessionRequest]) (*connect.Response[v1.AdoptSessionResponse], error) {
	m := req.Msg
	// A paired phone must not register this machine's processes as roots: the host's own device only.
	if actor := auth.ActorFrom(ctx); actor == nil || actor.Id != localDeviceID {
		return nil, denied("only the host's own device may adopt a session")
	}
	if m.RunId == "" {
		return nil, invalid("name the run (--run-id)")
	}
	if m.Pid <= 1 || m.PidStartTicks == 0 {
		return nil, invalid("no session to adopt: run `mesh adopt` from inside the Claude Code session (its Bash tool)")
	}
	pid := int(m.Pid)
	if !d.rt.sys.ProcAlive(pid, m.PidStartTicks) {
		return nil, failedPrecondition("process %d is not running (or has become another process)", pid)
	}
	env := d.claudeEnv()
	sess, ok := claude.ReadSession(env, pid, m.PidStartTicks)
	if !ok {
		return nil, failedPrecondition("process %d has no Claude Code session record under %s (the folder meshd reads: CLAUDE_CONFIG_DIR of its own "+
			"environment, else ~/.claude): `mesh adopt` is for a Claude Code session (an agy, Copilot or OpenCode session can't be adopted), "+
			"and one that keeps its config elsewhere needs meshd started with the same CLAUDE_CONFIG_DIR", pid, filepath.Join(claude.ConfigHome(env), "sessions"))
	}
	if sess.SocketPath == "" {
		return nil, failedPrecondition("the session has no inbox socket: it can't be rung")
	}
	if value, source := claude.InboundPolicy(env, sess.Cwd, pid); value != claude.InboundAccept {
		return nil, failedPrecondition("the session takes cross-session messages under %q (%s settings), so the daemon's doorbells can't reach it: "+
			"add `\"crossSessionInbound\": \"accept\"` to your own Claude settings (~/.claude/settings.json), restart the session and adopt again. "+
			"mesh never edits your settings", value, source)
	}
	r := d.store.Read(ctx)
	// An adopted session has no typed lane: only the inbox socket rings it.
	if on, err := d.Setting(r, "delivery.socket"); err != nil {
		return nil, err
	} else if on != true {
		return nil, failedPrecondition("`delivery.socket` is off, and an adopted session can only be rung through its inbox socket " +
			"(it has no terminal of the daemon's to type to): turn it on (`mesh set --key delivery.socket --value true`) and adopt again")
	}
	run, err := r.GetRun(m.RunId)
	if err != nil {
		return nil, notFound("no run %q", m.RunId)
	}
	if run.Status != v1.RunStatus_RUN_STATUS_OPEN {
		return nil, failedPrecondition("run %s is closed", run.Id)
	}
	top := sess.Cwd // (a git subprocess: before the locks, so a slow one holds no other adoption up)
	if t, err := topOf(ctx, sess.Cwd); err == nil && t != "" {
		top = t
	}
	// One adoption of a run at a time, from the scan of its agents to the identity file: two sessions adopting at once
	// then see each other (one root, the second refused), a retry with the same mutation id finds the file the first
	// call wrote or repairs a missing one, and never mints a token that call is about to overwrite. Keyed by the run,
	// whether it has a root yet or not.
	amu := d.agentLock("adopt:" + run.Id)
	amu.Lock()
	defer amu.Unlock()
	// And one run at a time for a session: the same process adopted into two runs at once would write one identity file
	// for two roots.
	pmu := d.agentLock(fmt.Sprintf("adopt-pid:%d", pid))
	pmu.Lock()
	defer pmu.Unlock()
	all, err := r.AllAgents()
	if err != nil {
		return nil, err
	}
	var root *v1.Agent
	for _, a := range all {
		if a.SessionId == sess.ID && a.TerminalId != "" && hasProcess(a) {
			return nil, failedPrecondition("that session is the daemon's own agent %s: adopt a session you started yourself", a.Name)
		}
		if isAdopted(a) && !stopped(a) && a.AdoptedPid == m.Pid && a.AdoptedPidStart == m.PidStartTicks && a.RunId != run.Id {
			return nil, failedPrecondition("that session is the root of run %s: end that run's use of it first", a.RunId)
		}
		if a.RunId == run.Id && a.Role == v1.AgentRole_AGENT_ROLE_ROOT && root == nil {
			root = a
		}
	}
	if root != nil && isAdopted(root) && !stopped(root) && (root.AdoptedPid != m.Pid || root.AdoptedPidStart != m.PidStartTicks) &&
		d.rt.sys.ProcAlive(int(root.AdoptedPid), root.AdoptedPidStart) {
		return nil, failedPrecondition("the run's root is another adopted session (process %d): end it first", root.AdoptedPid)
	}
	if err := d.ensureRunDir(run.Id); err != nil {
		return nil, err
	}
	token := auth.NewToken()
	out := &v1.AdoptSessionResponse{}
	var agent *v1.Agent
	var old *v1.Agent // the run's own root as it was, when this call took its row over
	if root != nil {
		mu := d.agentLock(root.Id)
		mu.Lock()
		defer mu.Unlock()
	}
	replayed, err := d.write(ctx, m.Meta, agentmeshv1connect.MeshServiceAdoptSessionProcedure, out, func(tx *store.Tx) error {
		// The run may have been stopped since it was read: a session is no root of a closed run.
		if cur, err := tx.GetRun(run.Id); err != nil || cur.Status != v1.RunStatus_RUN_STATUS_OPEN {
			return failedPrecondition("run %s is closed", run.Id)
		}
		now := tx.Now()
		if root != nil {
			cur, err := tx.GetAgent(root.Id)
			if err != nil {
				return err
			}
			// The run's own root stops in the same write that makes its row the adopted session's (its
			// conversation stays on disk; its row, mailbox and workers are the session's from here on): a write
			// that fails leaves the root as it was. Its process is stopped once this commits.
			old = proto.Clone(cur).(*v1.Agent)
			from := cur.State
			// Not retireTx: the row never shows as retired (the timeline would read "retired", then "revived"),
			// only as the root's state changing to the session's. What it does besides is done here: its
			// pending reopen ends and its token stops working.
			if err := cancelReopen(tx, cur, "adopted by a session"); err != nil {
				return err
			}
			if err := tx.RevokeTokens(cur.Id); err != nil {
				return err
			}
			cur.State, cur.StateDetail, cur.NeedsInput = v1.AgentState_AGENT_STATE_IDLE, "", 0
			cur.TerminalId, cur.SessionId, cur.Kind = "", sess.ID, claude.Kind
			cur.Repo, cur.Worktree, cur.Branch, cur.BaseCommit = top, "", "", ""
			cur.AdoptedPid, cur.AdoptedPidStart = m.Pid, m.PidStartTicks
			cur.PendingReopen, cur.PendingReset, cur.PendingCompaction = nil, nil, nil
			cur.StateBeforeCompact = v1.AgentState_AGENT_STATE_UNSPECIFIED
			cur.LastActivity = timestamppb.New(now)
			if err := tx.PutAgent(cur); err != nil {
				return err
			}
			if err := tx.PutToken(auth.Hash(token), store.TokenRecord{Class: ClassOf(cur.Role), SubjectID: cur.Id, Name: cur.Name, RunID: run.Id}); err != nil {
				return err
			}
			if _, err := tx.AppendEvent(&v1.Event{RunId: run.Id, AgentId: cur.Id, Payload: &v1.Event_AgentStateChanged{
				AgentStateChanged: &v1.AgentStateChanged{From: from, To: cur.State, Detail: fmt.Sprintf("adopted the Claude Code session of process %d", pid)}}}); err != nil {
				return err
			}
			agent = cur
			out.Agent = cur
			out.Note = fmt.Sprintf("%s (the run's root) is now this session: its own process was stopped, its workers and mailbox are yours", cur.Name)
			return nil
		}
		name := filepath.Base(top) + "-lead"
		if same, err := tx.AgentsNamed(run.Id, name); err != nil {
			return err
		} else if len(same) > 0 {
			name += "-here"
		}
		agent = &v1.Agent{Id: ids.New("a", 10), RunId: run.Id, Name: name, Role: v1.AgentRole_AGENT_ROLE_ROOT, Kind: claude.Kind,
			Repo: top, SessionId: sess.ID, State: v1.AgentState_AGENT_STATE_IDLE, AdoptedPid: m.Pid, AdoptedPidStart: m.PidStartTicks,
			LastActivity: timestamppb.New(now), CreatedAt: timestamppb.New(now)}
		if err := tx.PutAgent(agent); err != nil {
			return err
		}
		if err := tx.PutToken(auth.Hash(token), store.TokenRecord{Class: ClassOf(agent.Role), SubjectID: agent.Id, Name: name, RunID: run.Id}); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(&v1.Event{RunId: run.Id, AgentId: agent.Id, Payload: &v1.Event_AgentSpawned{
			AgentSpawned: &v1.AgentSpawned{Agent: agent}}}); err != nil {
			return err
		}
		out.Agent = agent
		out.Note = fmt.Sprintf("%s is now the run's root", name)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if replayed {
		// The first call did all of it, and its file stands; if the file write failed after the store's commit (a full
		// disk), the stored answer is all there is and the token was lost with it: mint another and write it. The
		// run's adoption lock is held (above) from before the write to here, so a replay never mints a token while the first call writes its file, and two replays do not both mint.
		if err := d.repairAdoptedFile(ctx, out.GetAgent().GetId(), m); err != nil {
			return nil, err
		}
		return connect.NewResponse(out), nil
	}
	if old != nil {
		d.endTakenOverRoot(ctx, old, agent, out)
	}
	d.rt.mu.Lock()
	delete(d.rt.introduced, agent.Id) // a session that takes this row over is introduced to the protocol afresh
	d.rt.mu.Unlock()
	if err := d.writeAdoptedFile(agent, token); err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

// repairAdoptedFile rewrites the identity file of an adopted session, when the call that adopted it left none: the
// session of this process is still the agent's, and `<state>/adopted/<pid>.json` is missing. The agent's tokens go (the
// lost one was never delivered, and the file was the only place a token lived) and a new one is written to it.
func (d *Daemon) repairAdoptedFile(ctx context.Context, agentID string, m *v1.AdoptSessionRequest) error {
	if agentID == "" {
		return nil
	}
	cur, err := d.store.Read(ctx).GetAgent(agentID)
	if err != nil || !isAdopted(cur) || stopped(cur) || cur.AdoptedPid != m.Pid || cur.AdoptedPidStart != m.PidStartTicks {
		return nil // another session's row now, or gone: the replay's answer is all that is owed
	}
	if _, err := os.Stat(d.paths.Adopted(int(cur.AdoptedPid))); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err // unreadable, not missing: no token is revoked for it
	}
	token := auth.NewToken()
	err = d.store.Write(ctx, meshActor, "", func(tx *store.Tx) error {
		if err := tx.RevokeTokens(cur.Id); err != nil {
			return err
		}
		return tx.PutToken(auth.Hash(token), store.TokenRecord{Class: ClassOf(cur.Role), SubjectID: cur.Id, Name: cur.Name, RunID: cur.RunId})
	})
	if err != nil {
		return err
	}
	return d.writeAdoptedFile(cur, token)
}

// endTakenOverRoot stops what the run's own root left running once its row has become an adopted session's: its
// terminal (the host's exit report finds no agent claiming it, and only forgets it), its launch's memory, its
// lean MCP file, a doorbell typed to it, and the identity file of an adopted session it was before. A process
// that will not stop is said in the response: the adoption stands.
func (d *Daemon) endTakenOverRoot(ctx context.Context, old, now *v1.Agent, out *v1.AdoptSessionResponse) {
	ctx = context.WithoutCancel(ctx)
	d.rt.guard.release(old.Id)
	_ = os.Remove(d.leanMCPFile(old.Id))
	d.setRing(old.Id, nil)
	if isAdopted(old) && old.AdoptedPid != now.AdoptedPid {
		_ = os.Remove(d.paths.Adopted(int(old.AdoptedPid)))
	}
	if old.TerminalId == "" || d.rt.term == nil {
		return
	}
	if err := d.stopTerminal(ctx, old.TerminalId); err != nil {
		d.log.Warn("stopping the root an adopted session replaced", "agent", old.Name, "terminal", old.TerminalId, "err", err)
		out.Note += fmt.Sprintf(" (its own process did not stop: %v; stop terminal %s yourself)", err, old.TerminalId)
	}
}

// writeAdoptedFile keeps the adopted session's identity where its `mesh` finds it (0600, like the device token).
func (d *Daemon) writeAdoptedFile(a *v1.Agent, token string) error {
	env := map[string]string{}
	for k, v := range d.agentEnv(a, token) {
		if strings.HasPrefix(k, "AGENT_MESH_") {
			env[k] = v
		}
	}
	b, err := json.Marshal(statedir.AdoptedFile{Pid: int(a.AdoptedPid), Start: a.AdoptedPidStart, Agent: a.Id, Env: env})
	if err != nil {
		return err
	}
	path := d.paths.Adopted(int(a.AdoptedPid))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, b, 0o600) // a failed write leaves no half file, which a repair would take for a good one
}

// adoptedPass follows the adopted sessions: one whose process is gone is exited (its token revoked, its file
// removed); one that is alive takes the state its session record shows.
func (d *Daemon) adoptedPass(ctx context.Context) {
	agents, err := d.store.Read(ctx).ListAgents("")
	if err != nil {
		return
	}
	env := d.claudeEnv()
	for _, a := range agents {
		if !isAdopted(a) || stopped(a) {
			continue
		}
		if !d.rt.sys.ProcAlive(int(a.AdoptedPid), a.AdoptedPidStart) {
			d.adoptedGone(ctx, a)
			continue
		}
		sess, ok := claude.ReadSession(env, int(a.AdoptedPid), a.AdoptedPidStart)
		if !ok {
			continue
		}
		to := a.State
		switch sess.Status {
		case "idle":
			to = v1.AgentState_AGENT_STATE_IDLE
		case "busy", "shell":
			to = v1.AgentState_AGENT_STATE_WORKING
		case "waiting":
			to = v1.AgentState_AGENT_STATE_NEEDS_INPUT
		}
		if to == a.State {
			continue
		}
		if err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
			cur, err := tx.GetAgent(a.Id)
			if err != nil || stopped(cur) || cur.State == to {
				return err
			}
			from := cur.State
			cur.State, cur.LastActivity = to, timestamppb.New(tx.Now())
			if err := tx.PutAgent(cur); err != nil {
				return err
			}
			_, err = tx.AppendEvent(&v1.Event{RunId: cur.RunId, AgentId: cur.Id, Payload: &v1.Event_AgentStateChanged{
				AgentStateChanged: &v1.AgentStateChanged{From: from, To: to}}})
			return err
		}); err != nil && ctx.Err() == nil {
			d.log.Warn("following an adopted session", "agent", a.Name, "err", err)
		}
	}
}

// adoptedGone records that an adopted session's process ended: the agent exited, its token is revoked and its
// file removed. It is never resumed by the daemon (its conversation is the user's own).
func (d *Daemon) adoptedGone(ctx context.Context, a *v1.Agent) {
	mu := d.agentLock(a.Id)
	mu.Lock()
	defer mu.Unlock()
	err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		cur, err := tx.GetAgent(a.Id)
		if err != nil || stopped(cur) {
			return err
		}
		cur.State, cur.NeedsInput, cur.StateDetail = v1.AgentState_AGENT_STATE_EXITED, 0, ""
		if err := tx.RevokeTokens(cur.Id); err != nil {
			return err
		}
		if err := tx.PutAgent(cur); err != nil {
			return err
		}
		_, err = tx.AppendEvent(&v1.Event{RunId: cur.RunId, AgentId: cur.Id, Payload: &v1.Event_AgentExited{
			AgentExited: &v1.AgentExited{Reason: "lost"}}})
		return err
	})
	if err != nil {
		if ctx.Err() == nil {
			d.log.Warn("marking an adopted session exited", "agent", a.Name, "err", err)
		}
		return
	}
	_ = os.Remove(d.paths.Adopted(int(a.AdoptedPid)))
}

// refusedPrefix starts the StateDetail of an adopted session whose doorbells are refused.
const refusedPrefix = "doorbells not delivered: "

// deliverAdopted rings an adopted session: one frame into its inbox socket for the messages it hasn't been rung
// about, marked delivered when it was written. There is no ring to follow (the session sends the daemon no
// hooks) and no typed lane: the daemon can't see the session's input box. So a message counts as delivered
// once the frame is written, not once the session took it (daemon/CLAUDE.md, "Doorbells"); a frame it drops
// is not rung again, and the root finds the message with `mesh inbox` (`mesh brief` lists unread mail). A
// refusal is told: logged at Warn, and shown in the agent's detail until a frame is written.
func (d *Daemon) deliverAdopted(ctx context.Context, a *v1.Agent) error {
	if stopped(a) {
		return nil
	}
	msgs, err := d.store.Read(ctx).Undelivered(a.Id)
	if err != nil || len(msgs) == 0 {
		return err
	}
	if on, err := d.Setting(d.store.Read(ctx), "delivery.socket"); err != nil {
		return err
	} else if on != true {
		d.adoptedRefused(ctx, a, "`delivery.socket` is off")
		return nil
	}
	ad, err := d.rt.adapters.Get(a.Kind)
	if err != nil {
		return nil
	}
	env := d.claudeEnv()
	// The policy is read where the session runs (repo settings may only tighten it), as AdoptSession read it:
	// the row's repo is the git top, which a session started in a subfolder doesn't share.
	cwd := a.Repo
	if sess, ok := claude.ReadSession(env, int(a.AdoptedPid), a.AdoptedPidStart); ok && sess.Cwd != "" {
		cwd = sess.Cwd
	}
	line := Doorbell(msgs, ringToken())
	d.rt.mu.Lock()
	first := !d.rt.introduced[a.Id]
	d.rt.mu.Unlock()
	if first {
		line += d.protocolPointer(a)
	}
	res, err := ad.DeliverNative(ctx, connect.NewRequest(&adapterv1.DeliverNativeRequest{AgentId: a.Id, Pid: int64(a.AdoptedPid),
		PidStartTicks: a.AdoptedPidStart, Cwd: cwd, Env: env, Text: line}))
	if err != nil {
		return nil
	}
	if !res.Msg.Delivered {
		d.adoptedRefused(ctx, a, res.Msg.Reason)
		return nil // the retry pass looks again
	}
	d.rt.mu.Lock()
	d.rt.introduced[a.Id] = true
	d.rt.mu.Unlock()
	if strings.HasPrefix(a.StateDetail, refusedPrefix) {
		d.setDetail(ctx, a.Id, refusedPrefix, "")
	}
	return d.markDelivered(ctx, msgs, "socket", 1)
}

// adoptedRefused tells that an adopted session's doorbell was not written: once per reason (the retry pass
// comes back every few seconds), at Warn and in the agent's detail, where the apps show it.
func (d *Daemon) adoptedRefused(ctx context.Context, a *v1.Agent, reason string) {
	detail := refusedPrefix + reason
	if a.StateDetail == detail {
		return
	}
	d.log.Warn("doorbell not written to an adopted session", "agent", a.Name, "reason", reason)
	d.setDetail(ctx, a.Id, "", detail)
}

// setDetail sets an agent's StateDetail when it now starts with only (any detail when only is empty).
func (d *Daemon) setDetail(ctx context.Context, id, only, detail string) {
	err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		cur, err := tx.GetAgent(id)
		if err != nil || stopped(cur) || cur.StateDetail == detail || (only != "" && !strings.HasPrefix(cur.StateDetail, only)) {
			return err
		}
		cur.StateDetail = detail
		return tx.PutAgent(cur)
	})
	if err != nil && ctx.Err() == nil {
		d.log.Warn("setting an adopted session's detail", "agent", id, "err", err)
	}
}
