package daemon_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
	"github.com/Luisotee/agent-mesh/daemon/internal/termclient"
)

// reopenWorld is a daemon whose fake claude logs every launch's argv.
type reopenWorld struct {
	*agentsHarness
	state, home, argvLog string
}

func startReopen(t *testing.T) *reopenWorld {
	t.Helper()
	// Its own folder, removed with retries after the terminal host is stopped: an agent's last hook (a spooled
	// file) can land while a plain t.TempDir() is being removed, which fails the test for nothing.
	root, err := os.MkdirTemp("", "reopen-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for try := 0; try < 100; try++ {
			if os.RemoveAll(root) == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Errorf("could not remove %s", root)
	})
	w := &reopenWorld{state: filepath.Join(root, "state"), home: filepath.Join(root, "home")}
	os.MkdirAll(w.home, 0o700)
	w.argvLog = filepath.Join(root, "argv.jsonl")
	w.agentsHarness = w.restart(t)
	t.Cleanup(func() { stopTermHost(t, w.state) })
	return w
}

func (w *reopenWorld) restart(t *testing.T) *agentsHarness {
	return startWithAgents(t, w.state, w.home, func(c *daemon.AgentConfig) {
		c.Env["FAKECLAUDE_ARGV_LOG"] = w.argvLog
	})
}

// launches lists the argv of every launch of a conversation, oldest first.
func (w *reopenWorld) launches(t *testing.T, session string) [][]string {
	t.Helper()
	f, err := os.Open(w.argvLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][]string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var argv []string
		if json.Unmarshal(sc.Bytes(), &argv) == nil && strings.Contains(strings.Join(argv, " "), " "+session+" ") {
			out = append(out, argv)
		}
	}
	return out
}

// flags reads an argv's value flags and whether it has a deny list.
func flags(argv []string) (map[string]string, bool) {
	out, deny := map[string]string{}, false
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "--session-id", "--resume", "--model", "--effort":
			out[argv[i]] = argv[i+1]
			i++
		case "--disallowedTools":
			deny = true
		}
	}
	return out, deny
}

func (w *reopenWorld) token(t *testing.T, agentID string) string {
	t.Helper()
	tok, err := fsutil.ReadSecret(statedir.Paths{Dir: w.state}.AgentToken(agentID))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// root starts a root agent, whose token then starts and approves workers.
func (w *reopenWorld) root(t *testing.T, run string) (*v1.Agent, agentmeshv1connect.MeshServiceClient) {
	t.Helper()
	res, err := w.device.Mesh.SpawnAgent(context.Background(), connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run, Repo: gitRepo(t), Slug: "lead", Role: v1.AgentRole_AGENT_ROLE_ROOT, Tier: "strong"}))
	if err != nil {
		t.Fatal(err)
	}
	w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	return res.Msg.Agent, w.as(w.token(t, res.Msg.Agent.Id))
}

// waitReopened waits until the agent runs in another terminal than before and is idle there.
func (w *reopenWorld) waitReopened(t *testing.T, id, oldTerminal string) *v1.Agent {
	t.Helper()
	var a *v1.Agent
	waitFor(t, "the reopen", func() bool {
		a = w.agent(id)
		return a.TerminalId != "" && a.TerminalId != oldTerminal && a.PendingReopen == nil &&
			a.State == v1.AgentState_AGENT_STATE_IDLE
	})
	return a
}

func (w *reopenWorld) delivered(t *testing.T, agentID, messageID string) bool {
	msgs, err := w.d.Store().Read(context.Background()).Inbox(agentID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.Id == messageID {
			return m.DeliveredAt != nil
		}
	}
	return false
}

func TestApproveReopensThePlannersConversationOnItsExecutionTier(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: gitRepo(t), Slug: "fix",
		Tier: "strong", Plan: true, Task: "plan the fix"}))
	if err != nil {
		t.Fatal(err)
	}
	worker := res.Msg.Agent
	if worker.ParentId != lead.Id || worker.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING || !worker.DenyEdits ||
		worker.Model != "opus" || worker.Effort != "high" || worker.Tier != "strong" {
		t.Fatalf("spawned %v", worker)
	}
	before := w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	sid := before.SessionId
	first := w.launches(t, sid)
	if len(first) != 1 {
		t.Fatalf("launches %v", first)
	}
	if f, deny := flags(first[0]); f["--session-id"] != sid || f["--model"] != "opus" || f["--effort"] != "high" || !deny {
		t.Errorf("the planner's launch %v", first[0])
	}

	// Nothing to approve before a plan; the worker can't approve itself.
	oldTok := w.token(t, worker.Id)
	asWorker := w.as(oldTok)
	if _, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: worker.Id, Tier: "mid"})); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("approve with no plan: %v", err)
	}
	if _, err := asWorker.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent",
		Type: v1.MessageType_MESSAGE_TYPE_PLAN, Body: "the plan"})); err != nil {
		t.Fatal(err)
	}
	if _, err := asWorker.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: worker.Id, Tier: "mid"})); code(err) != connect.CodePermissionDenied {
		t.Errorf("self-approval: %v", err)
	}
	if _, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: worker.Id})); code(err) != connect.CodeInvalidArgument {
		t.Errorf("approve with no execution tier: %v", err)
	}

	ap, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: worker.Id, Tier: "mid",
		Why: "the plan leaves nothing open"}))
	if err != nil {
		t.Fatal(err)
	}
	if r := ap.Msg.Reopen; r == nil || r.Model != "sonnet" || r.Effort != "high" || r.DenyEdits || r.Why != "plan approved" {
		t.Fatalf("reopen %v", r)
	}
	if ap.Msg.Message.Body != "approved" || ap.Msg.Message.Type != v1.MessageType_MESSAGE_TYPE_ANSWER {
		t.Errorf("answer %v", ap.Msg.Message)
	}

	after := w.waitReopened(t, worker.Id, before.TerminalId)
	if after.Id != worker.Id || after.Name != worker.Name || after.Worktree != worker.Worktree || after.SessionId != sid ||
		after.Model != "sonnet" || after.DenyEdits || after.Phase != v1.AgentPhase_AGENT_PHASE_EXECUTING || after.ExecTier != "mid" {
		t.Fatalf("after the reopen %v", after)
	}
	all := w.launches(t, sid)
	if len(all) != 2 {
		t.Fatalf("launches %v", all)
	}
	if f, deny := flags(all[1]); f["--resume"] != sid || f["--session-id"] != "" || f["--model"] != "sonnet" || f["--effort"] != "high" || deny {
		t.Errorf("the reopen's launch %v", all[1])
	}
	// The answer rings in the new process, not the old one; the old one's exit is not the agent's.
	waitFor(t, "the approval delivered", func() bool { return w.delivered(t, worker.Id, ap.Msg.Message.Id) })
	evs, _ := w.d.Store().Read(ctx).EventsAfter(0, run.Id, 1000)
	var reopened, rung int64
	for _, e := range evs {
		switch {
		case e.AgentId != worker.Id:
		case e.GetAgentReopened() != nil:
			reopened = e.Seq
		case e.GetMessageDelivered().GetMessageId() == ap.Msg.Message.Id:
			rung = e.Seq
		case e.GetAgentExited() != nil:
			t.Errorf("the reopen recorded an exit: %v", e)
		}
	}
	if reopened == 0 || rung < reopened {
		t.Errorf("reopened at %d, the approval rung at %d", reopened, rung)
	}
	// The old token is revoked, the new one works.
	if _, err := asWorker.Hello(ctx, connect.NewRequest(&v1.HelloRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Errorf("the old token: %v", err)
	}
	if _, err := w.as(w.token(t, worker.Id)).Hello(ctx, connect.NewRequest(&v1.HelloRequest{})); err != nil {
		t.Errorf("the new token: %v", err)
	}
	// Approved already: the newest task or answer is the approval.
	if _, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: worker.Id, Tier: "mid"})); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("a second approval: %v", err)
	}
}

func TestASwitchMidTurnWaitsForIdleEvenAcrossARestart(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	a := w.spawn(run.Id, gitRepo(t), "long", "")
	before := w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.typeInto(before, "/hold\r")
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_WORKING)

	sw, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: a.Id, Effort: "low"}))
	if err != nil {
		t.Fatal(err)
	}
	if r := sw.Msg.Reopen; r.Model != "sonnet" || r.Effort != "low" || r.Why != "moved" {
		t.Fatalf("reopen %v", r)
	}
	time.Sleep(500 * time.Millisecond)
	if got := w.agent(a.Id); got.PendingReopen == nil || got.TerminalId != before.TerminalId || got.State != v1.AgentState_AGENT_STATE_WORKING {
		t.Fatalf("a turn was cut short: %v", got)
	}
	if n := len(w.launches(t, before.SessionId)); n != 1 {
		t.Fatalf("%d launches mid-turn", n)
	}

	// The daemon restarts; the turn ends while it is down (the hooks spool), and the stored reopen runs once
	// the restarted daemon sees the agent idle.
	w.stop()
	tc, err := termclient.New(statedir.Paths{Dir: w.state}.Term())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tc.API.WriteTerminal(ctx, connect.NewRequest(&termv1.WriteTerminalRequest{Id: before.TerminalId,
		Data: []byte("go on\r")})); err != nil {
		t.Fatal(err)
	}
	w.agentsHarness = w.restart(t)
	after := w.waitReopened(t, a.Id, before.TerminalId)
	if after.Effort != "low" || after.Model != "sonnet" || after.SessionId != before.SessionId {
		t.Fatalf("after %v", after)
	}
	all := w.launches(t, before.SessionId)
	if len(all) != 2 {
		t.Fatalf("launches %v", all)
	}
	if f, _ := flags(all[1]); f["--resume"] != before.SessionId || f["--effort"] != "low" {
		t.Errorf("reopen %v", all[1])
	}
	waitFor(t, "the switch's message delivered", func() bool { return w.delivered(t, a.Id, sw.Msg.Message.Id) })
	if !strings.Contains(sw.Msg.Message.Body, "You are now on sonnet at low effort") {
		t.Errorf("body %q", sw.Msg.Message.Body)
	}
}

func TestSwitchNextWalksTheTiersChainAndStopsAtItsEnd(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	if _, err := w.device.Mesh.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{Key: "tiers.claude.mid.model",
		Value: val(t, []any{"sonnet", "haiku"})})); err != nil {
		t.Fatal(err)
	}
	run := startRun(t, w.device.Mesh, "")
	a := w.spawn(run.Id, gitRepo(t), "quota", "")
	before := w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	if _, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: a.Id, Model: "sonnet"})); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("a switch to what it runs on: %v", err)
	}
	if _, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: a.Id, Next: true, Tier: "mid"})); code(err) != connect.CodeInvalidArgument {
		t.Errorf("next and a tier: %v", err)
	}
	sw, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: a.Id, Next: true}))
	if err != nil {
		t.Fatal(err)
	}
	if sw.Msg.Reopen.Model != "haiku" || sw.Msg.Reopen.Effort != "high" || sw.Msg.Reopen.Why != "quota" ||
		!strings.Contains(sw.Msg.Message.Body, "Your previous model (sonnet) hit its quota") {
		t.Fatalf("%v", sw.Msg)
	}
	after := w.waitReopened(t, a.Id, before.TerminalId)
	if after.Model != "haiku" {
		t.Fatalf("after %v", after)
	}
	if _, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: a.Id, Next: true})); code(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "last model") {
		t.Errorf("past the chain's end: %v", err)
	}
}

func TestResumeReopensAStoppedAgentForATaskItPlans(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	a := w.spawn(run.Id, gitRepo(t), "back", "")
	before := w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	if _, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Agent: a.Id})); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("resuming a running agent: %v", err)
	}
	w.typeInto(before, "/exit\r")
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_EXITED)

	res, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Agent: a.Id,
		Task: "add the feature", Plan: true, Tier: "strong"}))
	if err != nil {
		t.Fatal(err)
	}
	if r := res.Msg.Reopen; r.Model != "opus" || !r.DenyEdits || r.Why != "resumed" {
		t.Fatalf("reopen %v", r)
	}
	after := w.waitReopened(t, a.Id, before.TerminalId)
	if after.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING || !after.DenyEdits || after.Tier != "strong" || after.SessionId != before.SessionId {
		t.Fatalf("after %v", after)
	}
	all := w.launches(t, before.SessionId)
	if f, deny := flags(all[len(all)-1]); len(all) != 2 || f["--resume"] != before.SessionId || f["--model"] != "opus" || !deny {
		t.Errorf("launches %v", all)
	}
	waitFor(t, "the task delivered", func() bool { return w.delivered(t, a.Id, res.Msg.Message.Id) })
}

func TestAPlanFromAnAgentWithNoPhaseEntersOneButNotForANamedModel(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	spawn := func(slug string, req *v1.SpawnAgentRequest) *v1.Agent {
		req.Repo, req.Slug = gitRepo(t), slug
		res, err := asLead.SpawnAgent(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatal(err)
		}
		return w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	}
	direct := spawn("direct", &v1.SpawnAgentRequest{Tier: "mid"})
	pinned := spawn("pinned", &v1.SpawnAgentRequest{Model: "opus"})
	if direct.Phase != v1.AgentPhase_AGENT_PHASE_UNSPECIFIED || direct.Pinned || direct.DenyEdits || !pinned.Pinned || pinned.Effort != "high" {
		t.Fatalf("direct %v, pinned %v", direct, pinned)
	}
	for _, a := range []*v1.Agent{direct, pinned} {
		if _, err := w.as(w.token(t, a.Id)).SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent",
			Type: v1.MessageType_MESSAGE_TYPE_PLAN, Body: "a plan"})); err != nil {
			t.Fatal(err)
		}
	}
	if got := w.agent(direct.Id); got.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING || got.DenyEdits {
		t.Errorf("a plan sent: %v", got)
	}
	if got := w.agent(pinned.Id); got.Phase != v1.AgentPhase_AGENT_PHASE_UNSPECIFIED {
		t.Errorf("a pinned agent entered a phase: %v", got)
	}
	// The pinned one is approved as it is: no tier, no reopen.
	if _, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: pinned.Id, Tier: "light"})); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("an execution tier for a pinned agent: %v", err)
	}
	ap, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: pinned.Id}))
	if err != nil || ap.Msg.Reopen != nil {
		t.Fatalf("approving a pinned agent: %v, %v", ap, err)
	}
	// Its turn on the answer ends before the test does (its hooks would spool into a removed state dir).
	waitFor(t, "the pinned agent's answer", func() bool { return w.delivered(t, pinned.Id, ap.Msg.Message.Id) })
	// The other moves to its execution tier; it never had the deny list, and the reopen doesn't add one.
	ap, err = asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: direct.Id, Tier: "light"}))
	if err != nil {
		t.Fatal(err)
	}
	after := w.waitReopened(t, direct.Id, direct.TerminalId)
	if after.Model != "haiku" || after.Effort != "medium" || after.DenyEdits || after.Phase != v1.AgentPhase_AGENT_PHASE_EXECUTING {
		t.Errorf("after %v", after)
	}
	waitFor(t, "the approval delivered", func() bool { return w.delivered(t, direct.Id, ap.Msg.Message.Id) })
	w.waitState(pinned.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.waitState(direct.Id, v1.AgentState_AGENT_STATE_IDLE)
}

func TestStoppingAnAgentDropsItsPendingReopen(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	a := w.spawn(run.Id, gitRepo(t), "halt", "")
	before := w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.typeInto(before, "/hold\r")
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_WORKING)
	if _, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: a.Id, Tier: "strong"})); err != nil {
		t.Fatal(err)
	}
	if _, err := w.device.Mesh.StopAgent(ctx, connect.NewRequest(&v1.StopAgentRequest{Agent: a.Id})); err != nil {
		t.Fatal(err)
	}
	got := w.waitState(a.Id, v1.AgentState_AGENT_STATE_EXITED)
	if got.PendingReopen != nil {
		t.Errorf("pending after a stop: %v", got)
	}
	time.Sleep(500 * time.Millisecond)
	if n := len(w.launches(t, before.SessionId)); n != 1 {
		t.Errorf("relaunched after a stop: %d launches", n)
	}
	evs, _ := w.d.Store().Read(ctx).EventsAfter(0, run.Id, 1000)
	dropped := false
	for _, e := range evs {
		if d := e.GetAgentReopenDropped(); d != nil && d.Reason == "stopped" {
			dropped = true
		}
	}
	if !dropped {
		t.Error("no AgentReopenDropped event")
	}
}

// A daemon that died after starting a reopen's terminal but before recording it finds the terminal at boot by
// its reopen label and records it, instead of launching the conversation a second time.
func TestAReopenInterruptedByADaemonCrashIsAdoptedNotRelaunched(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	a := w.spawn(run.Id, gitRepo(t), "crash", "")
	before := w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.stop()

	tc, err := termclient.New(statedir.Paths{Dir: w.state}.Term())
	if err != nil {
		t.Fatal(err)
	}
	// What the crashed reopen had done: the old process stopped, the new one started, nothing recorded after.
	if _, err := tc.API.KillTerminal(ctx, connect.NewRequest(&termv1.KillTerminalRequest{Id: before.TerminalId})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the old terminal exited", func() bool {
		l, _ := tc.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
		return len(l.Msg.Terminals) == 1 && l.Msg.Terminals[0].Exited
	})
	tc.API.AckExit(ctx, connect.NewRequest(&termv1.AckExitRequest{Id: before.TerminalId}))
	st, err := store.Open(statedir.Paths{Dir: w.state}.DB())
	if err != nil {
		t.Fatal(err)
	}
	err = st.Write(ctx, nil, "", func(tx *store.Tx) error {
		ag, err := tx.GetAgent(a.Id)
		if err != nil {
			return err
		}
		ag.TerminalId = ""
		ag.PendingReopen = &v1.Reopen{Id: "o-crashed", Model: "haiku", Effort: "low", Why: "moved"}
		return tx.PutAgent(ag)
	})
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	term, err := tc.API.CreateTerminal(ctx, connect.NewRequest(&termv1.CreateTerminalRequest{
		Argv: []string{filepath.Join(bins, "claude"), "--resume", before.SessionId}, Env: map[string]string{"HOME": w.home},
		Cwd: before.Worktree, Cols: 80, Rows: 24, Labels: map[string]string{"agent": a.Id, "reopen": "o-crashed"}}))
	if err != nil {
		t.Fatal(err)
	}

	w.agentsHarness = w.restart(t)
	var got *v1.Agent
	waitFor(t, "the reopen's terminal adopted", func() bool {
		got = w.agent(a.Id)
		return got.TerminalId == term.Msg.Terminal.Id && got.PendingReopen == nil
	})
	if got.Model != "haiku" || got.Effort != "low" {
		t.Errorf("adopted %v", got)
	}
	time.Sleep(500 * time.Millisecond)
	if n := len(w.launches(t, before.SessionId)); n != 1 {
		t.Errorf("the conversation was launched again: %d launches", n)
	}
}

func TestASpawnNeedsATierOrANamedModel(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	repo := gitRepo(t)
	for _, c := range []struct {
		req  *v1.SpawnAgentRequest
		want string
	}{
		{&v1.SpawnAgentRequest{}, "give a tier"},
		{&v1.SpawnAgentRequest{Tier: "huge"}, `no "huge" tier: one of light, mid, strong, top`},
		{&v1.SpawnAgentRequest{Tier: "mid", ExecTier: "light"}, "exec_tier is for a task with a plan phase"},
		{&v1.SpawnAgentRequest{Tier: "mid", Plan: true, ExecTier: "huge"}, `no "huge" tier`},
		{&v1.SpawnAgentRequest{Tier: "mid", Effort: "extreme"}, "effort"},
	} {
		c.req.RunId, c.req.Repo, c.req.Slug = run.Id, repo, "x"
		if _, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(c.req)); code(err) != connect.CodeInvalidArgument ||
			!strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want %q", c.req, err, c.want)
		}
	}
}

// A planner switched before it is approved is still approved on its plan; a stopped one is resumed first.
func TestAPlannerSwitchedBeforeApprovalIsStillApprovedAndAStoppedOneIsNot(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: gitRepo(t), Slug: "plan",
		Tier: "strong", Plan: true}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	if _, err := w.as(w.token(t, a.Id)).SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent",
		Type: v1.MessageType_MESSAGE_TYPE_PLAN, Body: "the plan"})); err != nil {
		t.Fatal(err)
	}
	sw, err := asLead.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: a.Id, Effort: "max"}))
	if err != nil {
		t.Fatal(err)
	}
	if sw.Msg.Message.Type != v1.MessageType_MESSAGE_TYPE_INFO {
		t.Errorf("the switch's notice is a %v", sw.Msg.Message.Type)
	}
	switched := w.waitReopened(t, a.Id, a.TerminalId)
	if !switched.DenyEdits || switched.Effort != "max" {
		t.Fatalf("a switch lifted the deny list or kept the effort: %v", switched)
	}
	// Typed only once the notice's doorbell is in, or the two would share a line.
	waitFor(t, "the notice delivered", func() bool { return w.delivered(t, a.Id, sw.Msg.Message.Id) })
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.typeInto(switched, "/exit\r")
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_EXITED)
	if _, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: a.Id, Tier: "mid"})); code(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "resume it") {
		t.Fatalf("approving a stopped planner: %v", err)
	}
	if _, err := asLead.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Agent: a.Id})); err != nil {
		t.Fatal(err)
	}
	back := w.waitReopened(t, a.Id, switched.TerminalId)
	if !back.DenyEdits || back.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING {
		t.Fatalf("resumed as it was: %v", back)
	}
	ap, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: a.Id, Tier: "mid"}))
	if err != nil || ap.Msg.Reopen == nil || ap.Msg.Reopen.DenyEdits {
		t.Fatalf("approve after the switch and the resume: %v, %v", ap, err)
	}
}

func TestASwitchToANamedModelLeavesNoChainForNext(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	a := w.spawn(run.Id, gitRepo(t), "named", "")
	before := w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	sw, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: a.Id, Model: "opus"}))
	if err != nil {
		t.Fatal(err)
	}
	after := w.waitReopened(t, a.Id, before.TerminalId)
	waitFor(t, "the notice delivered", func() bool { return w.delivered(t, a.Id, sw.Msg.Message.Id) })
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	if after.Tier != "" || after.Model != "opus" {
		t.Fatalf("after %v", after)
	}
	if _, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: a.Id, Next: true})); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("next from a named model: %v", err)
	}
	w.typeInto(after, "/exit\r")
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_EXITED)
	if _, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Agent: a.Id, Task: "fix it",
		Effort: "hihg"})); code(err) != connect.CodeInvalidArgument {
		t.Errorf("a bad effort with a task: %v", err)
	}
}
