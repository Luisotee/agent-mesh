package daemon_test

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/proc"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
)

// userSession is a Claude Code session the user started by hand: a process of its own (the fake claude, outside the
// terminal host) with a session record and an inbox socket, launched under `crossSessionInbound: accept` or not.
type userSession struct {
	pid   int
	start uint64
	log   string
	cmd   *exec.Cmd
	cfg   string
	cwd   string
}

func startUserSession(t *testing.T, cfg string, settings string) *userSession {
	t.Helper()
	inbox, err := os.MkdirTemp("", "adopt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(inbox) })
	s := &userSession{log: filepath.Join(inbox, "lines.log"), cfg: cfg, cwd: gitRepo(t)}
	args := []string{}
	if settings != "" {
		args = append(args, "--settings", settings)
	}
	s.cmd = exec.Command(filepath.Join(bins, "claude"), args...)
	s.cmd.Dir = s.cwd
	s.cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+cfg, "FAKECLAUDE_INBOX="+inbox, "FAKECLAUDE_LINES_LOG="+s.log)
	in, err := s.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	s.cmd.Stdout, s.cmd.Stderr = io.Discard, io.Discard
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); s.cmd.Process.Kill(); s.cmd.Wait() })
	s.pid = s.cmd.Process.Pid
	waitFor(t, "the session's record", func() bool {
		_, err := os.Stat(filepath.Join(cfg, "sessions", strconvI(s.pid)+".json"))
		return err == nil
	})
	if s.start, err = proc.StartTicks(s.pid); err != nil {
		t.Fatal(err)
	}
	return s
}

func strconvI(n int) string { return strconv.Itoa(n) }

const accepting = `{"crossSessionInbound":"accept"}`

func (s *userSession) adopt(w *reopenWorld, run string) (*v1.AdoptSessionResponse, error) {
	res, err := w.device.Mesh.AdoptSession(context.Background(), connect.NewRequest(&v1.AdoptSessionRequest{Meta: meta(""),
		RunId: run, Pid: int32(s.pid), PidStartTicks: s.start}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

func adoptWorld(t *testing.T) (*reopenWorld, string) {
	t.Helper()
	cfg := t.TempDir()
	share, err := filepath.Abs(filepath.Join("..", "..", "share"))
	if err != nil {
		t.Fatal(err)
	}
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Share = share
		c.Env["CLAUDE_CONFIG_DIR"] = cfg
		c.Sys.ProcAlive = proc.Same // the harness fakes it: an adopted session is a real process
		c.AccountEvery = 100 * time.Millisecond
	})
	return w, cfg
}

// A session the user started becomes the run's root where it is: it has the root's identity (a token in the
// state dir), a worker's report is written into its inbox socket and counts as delivered, and when the process
// ends the agent is exited and the identity is gone.
func TestAnAdoptedSessionIsTheRootAndIsRungThroughItsSocket(t *testing.T) {
	w, cfg := adoptWorld(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	sess := startUserSession(t, cfg, accepting)
	res, err := sess.adopt(w, run.Id)
	if err != nil {
		t.Fatal(err)
	}
	root := res.Agent
	if root.Role != v1.AgentRole_AGENT_ROLE_ROOT || root.AdoptedPid != int32(sess.pid) || root.TerminalId != "" || root.Worktree != "" ||
		root.Kind != "claude" || root.State != v1.AgentState_AGENT_STATE_IDLE || !strings.HasSuffix(root.Name, "-lead") {
		t.Fatalf("the adopted root: %v", root)
	}
	file, err := statedir.Paths{Dir: w.state}.ReadAdopted(sess.pid)
	if err != nil || file.Env["AGENT_MESH_TOKEN"] == "" || file.Agent != root.Id || file.Env["AGENT_MESH_RUN"] != run.Id || file.Start != sess.start {
		t.Fatalf("the identity file: %+v, %v", file, err)
	}
	if info, _ := os.Stat(statedir.Paths{Dir: w.state}.Adopted(sess.pid)); info.Mode().Perm() != 0o600 {
		t.Errorf("the identity file's mode %v", info.Mode())
	}
	asRoot := w.as(file.Env["AGENT_MESH_TOKEN"])
	if who, err := asRoot.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{RunId: run.Id})); err != nil || len(who.Msg.Agents) != 1 {
		t.Fatalf("the root lists its run: %v, %v", who, err)
	}
	// It starts a worker, whose report reaches the session over the socket.
	spawned, err := asRoot.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: gitRepo(t), Slug: "fix", Tier: "mid", Task: "work"}))
	if err != nil {
		t.Fatal(err)
	}
	worker := w.waitState(spawned.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	if worker.ParentId != root.Id {
		t.Errorf("the worker's parent is %s, not the adopted root", worker.ParentId)
	}
	asWorker := w.as(w.token(t, worker.Id))
	readInbox(t, asWorker)
	msg := send(t, asWorker, "parent", v1.MessageType_MESSAGE_TYPE_DONE, "finished")
	waitFor(t, "the delivery", func() bool { return !w.deliveredAt(t, root.Id, msg.Id).IsZero() })
	got := lines(t, sess.log)
	if len(got) != 1 || !strings.HasPrefix(got[0], "frame: 📬 [mesh] done from") || !strings.Contains(got[0], "ROOT_CARD.md") {
		t.Fatalf("what the session was given: %q", got)
	}
	if d := w.deliveries(t, run.Id, msg.Id); len(d) != 1 || d[0].Via != "socket" {
		t.Errorf("delivery events %+v", d)
	}
	// Its record's status shows in the agent's state.
	rec := filepath.Join(cfg, "sessions", strconvI(sess.pid)+".json")
	b, _ := os.ReadFile(rec)
	os.WriteFile(rec, []byte(strings.Replace(string(b), `"status":"idle"`, `"status":"busy"`, 1)), 0o600)
	waitFor(t, "the busy state", func() bool { return w.agent(root.Id).State == v1.AgentState_AGENT_STATE_WORKING })
	// Excluded: the daemon neither resets nor opens nor stops it.
	if _, err := asRoot.ResetRoot(ctx, connect.NewRequest(&v1.ResetRootRequest{Meta: meta(""), RunId: run.Id})); code(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "your own Claude Code session") {
		t.Errorf("a reset: %v", err)
	}
	if _, err := w.device.Mesh.StopAgent(ctx, connect.NewRequest(&v1.StopAgentRequest{Meta: meta(""), Agent: root.Id})); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("a stop: %v", err)
	}
	if _, err := w.device.Mesh.OpenAgent(ctx, connect.NewRequest(&v1.OpenAgentRequest{Agent: root.Id})); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("an open: %v", err)
	}
	// The session ends: the agent exits, its identity goes, and it is not the daemon's to resume.
	sess.cmd.Process.Kill()
	sess.cmd.Wait()
	waitFor(t, "the exit", func() bool { return w.agent(root.Id).State == v1.AgentState_AGENT_STATE_EXITED })
	if _, err := os.Stat(statedir.Paths{Dir: w.state}.Adopted(sess.pid)); !os.IsNotExist(err) {
		t.Errorf("the identity file stays: %v", err)
	}
	if _, err := asRoot.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{RunId: run.Id})); err == nil {
		t.Errorf("the token still works")
	}
	late, err := asWorker.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent", Type: v1.MessageType_MESSAGE_TYPE_QUESTION, Body: "anyone?"}))
	if err != nil || late.Msg.Resumed != nil || !strings.Contains(late.Msg.ResumeError, "your own Claude Code session") {
		t.Errorf("a message to the ended session: %v, %v", late, err)
	}
}

// A run that already has a root of its own gives it up to the adopted session: same agent, name, mailbox and
// workers; its process is stopped.
func TestAdoptingTakesOverTheRunsOwnRoot(t *testing.T) {
	w, cfg := adoptWorld(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "fix", &v1.SpawnAgentRequest{})
	asWorker := w.as(w.token(t, worker.Id))
	pending := send(t, asWorker, "parent", v1.MessageType_MESSAGE_TYPE_QUESTION, "which way?")
	sess := startUserSession(t, cfg, accepting)
	res, err := sess.adopt(w, run.Id)
	if err != nil {
		t.Fatal(err)
	}
	if res.Agent.Id != lead.Id || res.Agent.Name != lead.Name || res.Agent.AdoptedPid != int32(sess.pid) || res.Agent.TerminalId != "" ||
		res.Agent.State != v1.AgentState_AGENT_STATE_IDLE || !strings.Contains(res.Note, "run's root") {
		t.Fatalf("takeover: %v (%s)", res.Agent, res.Note)
	}
	if term := w.agent(lead.Id).TerminalId; term != "" {
		t.Errorf("the old process is still the root's terminal: %s", term)
	}
	if _, err := asLead.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{RunId: run.Id})); err == nil {
		t.Errorf("the spawned root's token still works")
	}
	// The worker's question, sent before, is in the same mailbox; a new message reaches the session.
	if in, _ := w.d.Store().Read(ctx).Inbox(lead.Id, true); len(in) == 0 || in[0].Id != pending.Id {
		t.Errorf("the mailbox: %v", in)
	}
	msg := send(t, asWorker, "parent", v1.MessageType_MESSAGE_TYPE_INFO, "still here")
	waitFor(t, "the delivery", func() bool { return !w.deliveredAt(t, lead.Id, msg.Id).IsZero() })
	if w.agent(worker.Id).ParentId != lead.Id {
		t.Errorf("the worker lost its parent")
	}
	// A second session can't take it while the first lives.
	other := startUserSession(t, cfg, accepting)
	if _, err := other.adopt(w, run.Id); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "another adopted session") {
		t.Errorf("a second adoption: %v", err)
	}
}

func TestAdoptingRefusesWhatCannotBeRung(t *testing.T) {
	w, cfg := adoptWorld(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	call := func(pid int, start uint64, runID string) error {
		_, err := w.device.Mesh.AdoptSession(ctx, connect.NewRequest(&v1.AdoptSessionRequest{Meta: meta(""), RunId: runID, Pid: int32(pid), PidStartTicks: start}))
		return err
	}
	if err := call(0, 0, run.Id); code(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "from inside the Claude Code session") {
		t.Errorf("no session: %v", err)
	}
	self := os.Getpid()
	ticks, _ := proc.StartTicks(self)
	if err := call(self, ticks, run.Id); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "no Claude Code session record") {
		t.Errorf("a process that is no session: %v", err)
	}
	holding := startUserSession(t, cfg, "")
	err := call(holding.pid, holding.start, run.Id)
	if code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), `"crossSessionInbound": "accept"`) ||
		!strings.Contains(err.Error(), "mesh never edits your settings") {
		t.Errorf("a session that does not accept: %v", err)
	}
	ok := startUserSession(t, cfg, accepting)
	if err := call(ok.pid, ok.start+1, run.Id); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "not running") {
		t.Errorf("another start time: %v", err)
	}
	if _, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta(""), RunId: run.Id})); err != nil {
		t.Fatal(err)
	}
	if err := call(ok.pid, ok.start, run.Id); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "closed") {
		t.Errorf("a closed run: %v", err)
	}
	// An agent can't adopt: it is the user's own shell's call.
	run2 := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run2.Id)
	if _, err := asLead.AdoptSession(ctx, connect.NewRequest(&v1.AdoptSessionRequest{Meta: meta(""), RunId: run2.Id, Pid: int32(ok.pid), PidStartTicks: ok.start})); code(err) != connect.CodePermissionDenied {
		t.Errorf("a root adopting: %v", err)
	}
	// Stopping a run leaves an adopted session's own folder and process alone.
	if _, err := ok.adopt(w, run2.Id); err == nil {
		// the run has a daemon root, which the adoption takes over: the session is its root now
	}
	if _, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta(""), RunId: run2.Id})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ok.cwd); err != nil {
		t.Errorf("the session's folder is gone: %v", err)
	}
	if err := ok.cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("the session was signalled: %v", err)
	}
}
