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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
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
		return nil, failedPrecondition("process %d has no Claude Code session record: `mesh adopt` is for a Claude Code session "+
			"(an agy, Copilot or OpenCode session can't be adopted)", pid)
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
	run, err := r.GetRun(m.RunId)
	if err != nil {
		return nil, notFound("no run %q", m.RunId)
	}
	if run.Status != v1.RunStatus_RUN_STATUS_OPEN {
		return nil, failedPrecondition("run %s is closed", run.Id)
	}
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
	top := sess.Cwd
	if t, err := topOf(ctx, sess.Cwd); err == nil && t != "" {
		top = t
	}
	if err := d.ensureRunDir(run.Id); err != nil {
		return nil, err
	}
	token := auth.NewToken()
	out := &v1.AdoptSessionResponse{}
	var agent *v1.Agent
	if root != nil {
		mu := d.agentLock(root.Id)
		mu.Lock()
		defer mu.Unlock()
		// The run's own root stops first (its conversation stays on disk); its row, mailbox and workers are the
		// adopted session's from here on.
		if err := d.store.Write(ctx, auth.ActorFrom(ctx), "", func(tx *store.Tx) error {
			cur, err := tx.GetAgent(root.Id)
			if err != nil {
				return err
			}
			_, err = retireTx(tx, cur, "adopted by a session")
			return err
		}); err != nil {
			return nil, err
		}
		if err := d.finishRetire(ctx, root.Id); err != nil {
			return nil, err
		}
	}
	_, err = d.write(ctx, m.Meta, agentmeshv1connect.MeshServiceAdoptSessionProcedure, out, func(tx *store.Tx) error {
		now := tx.Now()
		if root != nil {
			cur, err := tx.GetAgent(root.Id)
			if err != nil {
				return err
			}
			from := cur.State
			cur.State, cur.StateDetail, cur.NeedsInput = v1.AgentState_AGENT_STATE_IDLE, "", 0
			cur.TerminalId, cur.SessionId, cur.Kind = "", sess.ID, claude.Kind
			cur.Repo, cur.Worktree, cur.Branch, cur.BaseCommit = top, "", "", ""
			cur.AdoptedPid, cur.AdoptedPidStart = m.Pid, m.PidStartTicks
			cur.PendingReopen, cur.PendingReset, cur.PendingCompaction = nil, nil, nil
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
	if err := d.writeAdoptedFile(agent, token); err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
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
	return os.WriteFile(path, b, 0o600)
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

// deliverAdopted rings an adopted session: one frame into its inbox socket for the messages it hasn't been rung
// about, marked delivered when it was written. There is no ring to follow (the session sends the daemon no
// hooks) and no typed lane: the daemon can't see the session's input box.
func (d *Daemon) deliverAdopted(ctx context.Context, a *v1.Agent) error {
	if stopped(a) {
		return nil
	}
	msgs, err := d.store.Read(ctx).Undelivered(a.Id)
	if err != nil || len(msgs) == 0 {
		return err
	}
	if on, err := d.Setting(d.store.Read(ctx), "delivery.socket"); err != nil || on != true {
		return err
	}
	ad, err := d.rt.adapters.Get(a.Kind)
	if err != nil {
		return nil
	}
	line := Doorbell(msgs, ringToken())
	d.rt.mu.Lock()
	first := !d.rt.introduced[a.Id]
	d.rt.mu.Unlock()
	if first {
		line += d.protocolPointer(a)
	}
	res, err := ad.DeliverNative(ctx, connect.NewRequest(&adapterv1.DeliverNativeRequest{AgentId: a.Id, Pid: int64(a.AdoptedPid),
		PidStartTicks: a.AdoptedPidStart, Cwd: a.Repo, Env: d.claudeEnv(), Text: line}))
	if err != nil {
		return nil
	}
	if !res.Msg.Delivered {
		d.log.Debug("doorbell not written to an adopted session", "agent", a.Name, "reason", res.Msg.Reason)
		return nil // the retry pass looks again
	}
	d.rt.mu.Lock()
	d.rt.introduced[a.Id] = true
	d.rt.mu.Unlock()
	return d.markDelivered(ctx, msgs, "socket", 1)
}
