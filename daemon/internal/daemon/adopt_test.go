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
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/proc"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
	"github.com/Luisotee/agent-mesh/daemon/internal/testutil/shortdir"
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

// startUserSession starts one; sub, when given, is the folder under the repo the session runs in.
func startUserSession(t *testing.T, cfg string, settings string, sub ...string) *userSession {
	t.Helper()
	inbox, err := shortdir.Make("adopt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(inbox) })
	s := &userSession{log: filepath.Join(inbox, "lines.log"), cfg: cfg, cwd: gitRepo(t)}
	for _, d := range sub {
		s.cwd = filepath.Join(s.cwd, d)
		if err := os.MkdirAll(s.cwd, 0o755); err != nil {
			t.Fatal(err)
		}
	}
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
	// It starts a worker, whose report reaches the session over the socket. The session is the user's own
	// process: it fills none of the daemon's agent slots (one slot here is the worker's).
	w.set(t, "caps.agents", 1)
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
	// The frame is written to the socket before the fake session logs it: wait for the line.
	waitFor(t, "the frame in the session", func() bool { return len(lines(t, sess.log)) > 0 })
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
	// The timeline never shows the root as retired and then revived: one state change, from what it was.
	evs, _ := w.d.Store().Read(ctx).ListEvents(run.Id)
	changed := 0
	for _, e := range evs {
		if e.AgentId != lead.Id {
			continue
		}
		if r := e.GetAgentRetired(); r != nil {
			t.Errorf("the takeover appended a retire event: %v", r)
		}
		if c := e.GetAgentStateChanged(); c != nil && strings.Contains(c.Detail, "adopted the Claude Code session") {
			changed++
			if c.From == v1.AgentState_AGENT_STATE_RETIRED || c.To != v1.AgentState_AGENT_STATE_IDLE {
				t.Errorf("the takeover's state change: %v", c)
			}
		}
	}
	if changed != 1 {
		t.Errorf("%d state changes for the takeover", changed)
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

// A repeated `mesh adopt` (the reply was lost; the same mutation id) answers what the first call did and changes
// nothing: the row stays the session's, its token and identity file stand.
func TestAReplayedAdoptionChangesNothing(t *testing.T) {
	w, cfg := adoptWorld(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, _ := w.root(t, run.Id)
	sess := startUserSession(t, cfg, accepting)
	call := func() (*v1.AdoptSessionResponse, error) {
		res, err := w.device.Mesh.AdoptSession(ctx, connect.NewRequest(&v1.AdoptSessionRequest{Meta: meta("adopt-1"),
			RunId: run.Id, Pid: int32(sess.pid), PidStartTicks: sess.start}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	first, err := call()
	if err != nil {
		t.Fatal(err)
	}
	again, err := call()
	if err != nil {
		t.Fatalf("the replay: %v", err)
	}
	if again.Agent.Id != lead.Id || first.Agent.Id != lead.Id {
		t.Errorf("agents %s, %s; want %s", first.Agent.Id, again.Agent.Id, lead.Id)
	}
	if got := w.agent(lead.Id); got.State == v1.AgentState_AGENT_STATE_RETIRED || got.AdoptedPid != int32(sess.pid) {
		t.Errorf("the replay changed the root: %v", got)
	}
	file, err := statedir.Paths{Dir: w.state}.ReadAdopted(sess.pid)
	if err != nil {
		t.Fatalf("the identity file after a replay: %v", err)
	}
	if _, err := w.as(file.Env["AGENT_MESH_TOKEN"]).ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{RunId: run.Id})); err != nil {
		t.Errorf("the token after a replay: %v", err)
	}
}

// A first call whose identity file never got written (a full disk: the store's write had committed) is repaired by the
// same mutation id: the replay writes a new token to the file, and the lost one stops working.
func TestAReplayedAdoptionRewritesAMissingIdentityFile(t *testing.T) {
	w, cfg := adoptWorld(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, _ := w.root(t, run.Id)
	sess := startUserSession(t, cfg, accepting)
	call := func() {
		t.Helper()
		if _, err := w.device.Mesh.AdoptSession(ctx, connect.NewRequest(&v1.AdoptSessionRequest{Meta: meta("adopt-2"),
			RunId: run.Id, Pid: int32(sess.pid), PidStartTicks: sess.start})); err != nil {
			t.Fatal(err)
		}
	}
	call()
	paths := statedir.Paths{Dir: w.state}
	file, err := paths.ReadAdopted(sess.pid)
	if err != nil {
		t.Fatal(err)
	}
	lost := file.Env["AGENT_MESH_TOKEN"]
	if err := os.Remove(paths.Adopted(sess.pid)); err != nil {
		t.Fatal(err)
	}
	call() // the replay repairs it
	file, err = paths.ReadAdopted(sess.pid)
	if err != nil {
		t.Fatalf("no identity file after the replay: %v", err)
	}
	fresh := file.Env["AGENT_MESH_TOKEN"]
	if fresh == "" || fresh == lost || file.Agent != lead.Id {
		t.Fatalf("file %v (lost token %s)", file, lost)
	}
	if _, err := w.as(fresh).ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{RunId: run.Id})); err != nil {
		t.Errorf("the new token: %v", err)
	}
	if _, err := w.as(lost).ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{RunId: run.Id})); code(err) != connect.CodeUnauthenticated {
		t.Errorf("the lost token still works: %v", err)
	}
	// A replay with the file in place rewrites nothing.
	before, _ := os.ReadFile(paths.Adopted(sess.pid))
	call()
	if after, _ := os.ReadFile(paths.Adopted(sess.pid)); string(before) != string(after) {
		t.Errorf("a replay rewrote a good file")
	}
}

// An adoption that is refused inside its write (here: past its expiry) leaves the run's own root as it was: still
// running, its token good. The root is retired only by the write that makes the session its successor.
func TestAFailedAdoptionLeavesTheRunsRootRunning(t *testing.T) {
	w, cfg := adoptWorld(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	before := w.agent(lead.Id)
	sess := startUserSession(t, cfg, accepting)
	_, err := w.device.Mesh.AdoptSession(ctx, connect.NewRequest(&v1.AdoptSessionRequest{
		Meta:  &v1.RequestMeta{ExpiresAt: timestamppb.New(time.Now().Add(-time.Minute))},
		RunId: run.Id, Pid: int32(sess.pid), PidStartTicks: sess.start}))
	if err == nil {
		t.Fatal("an expired adoption went through")
	}
	got := w.agent(lead.Id)
	if got.State == v1.AgentState_AGENT_STATE_RETIRED || got.TerminalId != before.TerminalId || got.AdoptedPid != 0 {
		t.Errorf("the root after a refused adoption: %v", got)
	}
	if _, err := asLead.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{RunId: run.Id})); err != nil {
		t.Errorf("the root's token after a refused adoption: %v", err)
	}
}

// An adopted session is rung only through its socket, so adopting with `delivery.socket` off is refused.
func TestAdoptingNeedsTheSocketDeliveryOn(t *testing.T) {
	w, cfg := adoptWorld(t)
	run := startRun(t, w.device.Mesh, "")
	w.set(t, "delivery.socket", false)
	sess := startUserSession(t, cfg, accepting)
	if _, err := sess.adopt(w, run.Id); code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "delivery.socket") {
		t.Errorf("adopting with the socket off: %v", err)
	}
}

// A session started in a folder under the repo is rung under the policy of that folder (as adopt checked it), not
// of the repo's top: a repo setting of `hold` above it doesn't reach it, and one in its own folder is told.
func TestAnAdoptedSessionIsRungUnderItsOwnFoldersPolicy(t *testing.T) {
	w, cfg := adoptWorld(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	sess := startUserSession(t, cfg, accepting, "services", "api")
	top := filepath.Dir(filepath.Dir(sess.cwd))
	os.MkdirAll(filepath.Join(top, ".claude"), 0o755)
	os.WriteFile(filepath.Join(top, ".claude", "settings.json"), []byte(`{"crossSessionInbound":"hold"}`), 0o644)
	res, err := sess.adopt(w, run.Id)
	if err != nil {
		t.Fatal(err)
	}
	root := res.Agent
	file, _ := statedir.Paths{Dir: w.state}.ReadAdopted(sess.pid)
	asRoot := w.as(file.Env["AGENT_MESH_TOKEN"])
	spawned, err := asRoot.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: gitRepo(t), Slug: "fix", Tier: "mid", Task: "work"}))
	if err != nil {
		t.Fatal(err)
	}
	worker := w.waitState(spawned.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	asWorker := w.as(w.token(t, worker.Id))
	readInbox(t, asWorker)
	msg := send(t, asWorker, "parent", v1.MessageType_MESSAGE_TYPE_DONE, "finished")
	waitFor(t, "the delivery under the session's own folder", func() bool { return !w.deliveredAt(t, root.Id, msg.Id).IsZero() })

	// A hold in the session's own folder is refused, and said: in the log and in the agent's detail.
	os.MkdirAll(filepath.Join(sess.cwd, ".claude"), 0o755)
	os.WriteFile(filepath.Join(sess.cwd, ".claude", "settings.json"), []byte(`{"crossSessionInbound":"hold"}`), 0o644)
	held := send(t, asWorker, "parent", v1.MessageType_MESSAGE_TYPE_INFO, "again")
	waitFor(t, "the refusal told", func() bool { return strings.HasPrefix(w.agent(root.Id).StateDetail, "doorbells not delivered: ") })
	if !w.deliveredAt(t, root.Id, held.Id).IsZero() {
		t.Errorf("a refused doorbell counts as delivered")
	}
	// Once the folder accepts again, the next pass writes the frame and the detail goes.
	os.Remove(filepath.Join(sess.cwd, ".claude", "settings.json"))
	waitFor(t, "the delivery after the hold", func() bool { return !w.deliveredAt(t, root.Id, held.Id).IsZero() })
	waitFor(t, "the detail cleared", func() bool { return w.agent(root.Id).StateDetail == "" })
}

// A session that adopts the row an earlier session left is introduced to the protocol again on its first
// doorbell, as the first was.
func TestASecondSessionOnTheSameRowGetsTheProtocolPointer(t *testing.T) {
	w, cfg := adoptWorld(t)
	run := startRun(t, w.device.Mesh, "")
	ring := func(sess *userSession, body string) {
		res, err := sess.adopt(w, run.Id)
		if err != nil {
			t.Fatal(err)
		}
		file, _ := statedir.Paths{Dir: w.state}.ReadAdopted(sess.pid)
		asRoot := w.as(file.Env["AGENT_MESH_TOKEN"])
		spawned, err := asRoot.SpawnAgent(context.Background(), connect.NewRequest(&v1.SpawnAgentRequest{Repo: gitRepo(t), Slug: "fix" + body, Tier: "mid", Task: "work"}))
		if err != nil {
			t.Fatal(err)
		}
		worker := w.waitState(spawned.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
		asWorker := w.as(w.token(t, worker.Id))
		readInbox(t, asWorker)
		msg := send(t, asWorker, "parent", v1.MessageType_MESSAGE_TYPE_DONE, body)
		waitFor(t, "the delivery", func() bool { return !w.deliveredAt(t, res.Agent.Id, msg.Id).IsZero() })
	}
	first := startUserSession(t, cfg, accepting)
	ring(first, "one")
	first.cmd.Process.Kill()
	first.cmd.Wait()
	waitFor(t, "the exit", func() bool {
		a, err := w.d.Store().Read(context.Background()).ListAgents(run.Id)
		for _, x := range a {
			if x.Role == v1.AgentRole_AGENT_ROLE_ROOT {
				return err == nil && x.State == v1.AgentState_AGENT_STATE_EXITED
			}
		}
		return false
	})
	second := startUserSession(t, cfg, accepting)
	ring(second, "two")
	waitFor(t, "the frame in the second session", func() bool { return len(lines(t, second.log)) > 0 })
	got := lines(t, second.log)
	if len(got) != 1 || !strings.Contains(got[0], "ROOT_CARD.md") {
		t.Errorf("what the second session was given: %q", got)
	}
}
