package daemon_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
	"github.com/Luisotee/agent-mesh/daemon/internal/termclient"
)

// set changes a setting as the device.
func (w *reopenWorld) set(t *testing.T, key string, value any) {
	t.Helper()
	if _, err := w.device.Mesh.SetSetting(context.Background(), connect.NewRequest(&v1.SetSettingRequest{
		Meta: meta(""), Key: key, Value: val(t, value)})); err != nil {
		t.Fatal(err)
	}
}

// report sends a report to the agent's parent as the agent itself.
func (w *reopenWorld) report(t *testing.T, a *v1.Agent, typ v1.MessageType) {
	t.Helper()
	if _, err := w.as(w.token(t, a.Id)).SendMessage(context.Background(), connect.NewRequest(&v1.SendMessageRequest{
		To: "parent", Type: typ, Body: "reported"})); err != nil {
		t.Fatal(err)
	}
}

// spawnUnder starts a worker under the root.
func (w *reopenWorld) spawnUnder(t *testing.T, asRoot agentmeshv1connect.MeshServiceClient, slug string, req *v1.SpawnAgentRequest) *v1.Agent {
	t.Helper()
	req.Repo, req.Slug = gitRepo(t), slug
	if req.Tier == "" && req.Model == "" {
		req.Tier = "mid"
	}
	res, err := asRoot.SpawnAgent(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	return w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
}

// terminals lists the host's terminals.
func (w *reopenWorld) terminals(t *testing.T) []*termv1.Terminal {
	t.Helper()
	tc, err := termclient.New(statedir.Paths{Dir: w.state}.Term())
	if err != nil {
		t.Fatal(err)
	}
	l, err := tc.API.ListTerminals(context.Background(), connect.NewRequest(&termv1.ListTerminalsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return l.Msg.Terminals
}

func TestRetireStopsTheProcessKeepsTheConversationAndAMessageResumesIt(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	a := w.spawnUnder(t, asLead, "idle", &v1.SpawnAgentRequest{})
	other := w.spawnUnder(t, asLead, "other", &v1.SpawnAgentRequest{})
	oldTok := w.token(t, a.Id)

	// Only its parent or a device; a sibling can't.
	if _, err := w.as(w.token(t, other.Id)).RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: a.Id})); code(err) != connect.CodePermissionDenied {
		t.Errorf("a sibling retiring: %v", err)
	}
	res, err := asLead.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta("r-1"), Agent: a.Id}))
	if err != nil {
		t.Fatal(err)
	}
	got := res.Msg.Agent
	if got.State != v1.AgentState_AGENT_STATE_RETIRED || got.TerminalId != "" || got.SessionId != a.SessionId || got.Worktree != a.Worktree {
		t.Fatalf("retired %v", got)
	}
	for _, tm := range w.terminals(t) {
		if tm.Id == a.TerminalId {
			t.Errorf("the host still holds its terminal: %v", tm)
		}
	}
	if _, err := w.as(oldTok).Hello(ctx, connect.NewRequest(&v1.HelloRequest{})); code(err) != connect.CodeUnauthenticated {
		t.Errorf("its token: %v", err)
	}
	if _, err := os.Stat(a.Worktree); err != nil {
		t.Errorf("its worktree went: %v", err)
	}
	// No AgentExited: it was stopped on purpose.
	evs, _ := w.d.Store().Read(ctx).EventsAfter(0, run.Id, 1000)
	retired := 0
	for _, e := range evs {
		if e.AgentId != a.Id {
			continue
		}
		if e.GetAgentExited() != nil {
			t.Errorf("a retire recorded an exit: %v", e)
		}
		if r := e.GetAgentRetired(); r != nil && r.Reason == "requested" {
			retired++
		}
	}
	if retired != 1 {
		t.Errorf("%d AgentRetired events", retired)
	}
	// Retiring again is a no-op, and a retry of the first call is its stored answer.
	if again, err := asLead.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Agent: a.Id})); err != nil || again.Msg.Agent.State != v1.AgentState_AGENT_STATE_RETIRED {
		t.Errorf("retiring a retired agent: %v, %v", again, err)
	}

	// An info is only stored: nothing starts.
	info, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: a.Id, Type: v1.MessageType_MESSAGE_TYPE_INFO, Body: "thanks"}))
	if err != nil || info.Msg.Resumed != nil {
		t.Fatalf("info: %v, %v", info, err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(w.launches(t, a.SessionId)); n != 1 || w.agent(a.Id).State != v1.AgentState_AGENT_STATE_RETIRED {
		t.Fatalf("an info resumed it: %d launches", n)
	}
	// A task resumes the same conversation, direct, and rings in the new process.
	task, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: a.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "one more thing"}))
	if err != nil || task.Msg.Resumed == nil || task.Msg.Resumed.Why != "resumed" {
		t.Fatalf("task: %v, %v", task, err)
	}
	back := w.waitReopened(t, a.Id, "")
	if back.SessionId != a.SessionId || back.DenyEdits || back.Phase != v1.AgentPhase_AGENT_PHASE_UNSPECIFIED {
		t.Fatalf("back %v", back)
	}
	all := w.launches(t, a.SessionId)
	if f, deny := flags(all[len(all)-1]); len(all) != 2 || f["--resume"] != a.SessionId || deny {
		t.Errorf("launches %v", all)
	}
	waitFor(t, "the task delivered", func() bool { return w.delivered(t, a.Id, task.Msg.Message.Id) })
	// The info it missed is in its inbox when it reads.
	inbox, err := w.as(w.token(t, a.Id)).ReadInbox(ctx, connect.NewRequest(&v1.ReadInboxRequest{Meta: meta(""), Peek: true}))
	if err != nil || len(inbox.Msg.Messages) != 2 {
		t.Errorf("inbox %v, %v", inbox, err)
	}
}

func TestAnAnswerBringsARetiredPlannerBackPlanningAndATaskKeepsItPlanning(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	a := w.spawnUnder(t, asLead, "plan", &v1.SpawnAgentRequest{Tier: "strong", Plan: true})
	w.report(t, a, v1.MessageType_MESSAGE_TYPE_QUESTION)
	if _, err := asLead.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: a.Id})); err != nil {
		t.Fatal(err)
	}
	ans, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: a.Id, Type: v1.MessageType_MESSAGE_TYPE_ANSWER, Body: "use the second"}))
	if err != nil || ans.Msg.Resumed == nil {
		t.Fatalf("answer: %v, %v", ans, err)
	}
	back := w.waitReopened(t, a.Id, "")
	if back.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING || !back.DenyEdits || back.Model != "opus" {
		t.Fatalf("an answer brought it back as %v", back)
	}
	waitFor(t, "delivered", func() bool { return w.delivered(t, a.Id, ans.Msg.Message.Id) })
	// Retired again; a task to a planner keeps it planning (a planned task needs ResumeAgent's plan flag).
	if _, err := asLead.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: a.Id})); err != nil {
		t.Fatal(err)
	}
	if _, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: a.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "plan it again"})); err != nil {
		t.Fatal(err)
	}
	if again := w.waitReopened(t, a.Id, ""); again.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING || !again.DenyEdits {
		t.Fatalf("a task to a planner: %v", again)
	}
}

func TestAnIdleAgentIsRetiredAfterItsReportUnlessSomethingHoldsItOpen(t *testing.T) {
	w := startReopen(t, func(c *daemon.AgentConfig) { c.RetireEvery = 100 * time.Millisecond })
	ctx := context.Background()
	w.set(t, "idle_retire", 1)
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)

	done := w.spawnUnder(t, asLead, "done", &v1.SpawnAgentRequest{})
	failed := w.spawnUnder(t, asLead, "failed", &v1.SpawnAgentRequest{})
	planned := w.spawnUnder(t, asLead, "planned", &v1.SpawnAgentRequest{})
	asked := w.spawnUnder(t, asLead, "asked", &v1.SpawnAgentRequest{})
	mail := w.spawnUnder(t, asLead, "mail", &v1.SpawnAgentRequest{})
	viewed := w.spawnUnder(t, asLead, "viewed", &v1.SpawnAgentRequest{})
	silent := w.spawnUnder(t, asLead, "silent", &v1.SpawnAgentRequest{})

	w.report(t, done, v1.MessageType_MESSAGE_TYPE_DONE)
	w.report(t, failed, v1.MessageType_MESSAGE_TYPE_FAILED)
	w.report(t, planned, v1.MessageType_MESSAGE_TYPE_PLAN)
	w.report(t, asked, v1.MessageType_MESSAGE_TYPE_QUESTION)
	w.report(t, mail, v1.MessageType_MESSAGE_TYPE_DONE)
	w.report(t, viewed, v1.MessageType_MESSAGE_TYPE_DONE)
	// Unread mail: work is coming, or a doorbell was lost.
	if _, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: mail.Id, Type: v1.MessageType_MESSAGE_TYPE_INFO, Body: "wait"})); err != nil {
		t.Fatal(err)
	}
	// Someone is looking: a client with input rights attached.
	ictx, icancel := context.WithTimeout(ctx, 60*time.Second)
	defer icancel()
	user := w.openTerminal(ictx, viewed, true, &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_TAIL})
	for {
		f, err := user.Recv(ictx)
		if err != nil {
			t.Fatal(err)
		}
		if f.GetReplayDone() != nil {
			break
		}
	}
	user.Ready(ictx)

	w.waitState(done.Id, v1.AgentState_AGENT_STATE_RETIRED)
	w.waitState(failed.Id, v1.AgentState_AGENT_STATE_RETIRED)
	evs, _ := w.d.Store().Read(ctx).EventsAfter(0, run.Id, 1000)
	for _, e := range evs {
		if r := e.GetAgentRetired(); r != nil && e.AgentId == done.Id && r.Reason != "idle after its report" {
			t.Errorf("reason %q", r.Reason)
		}
	}
	// Give the held ones a few passes more.
	time.Sleep(2 * time.Second)
	for name, a := range map[string]*v1.Agent{"a plan": planned, "a question": asked, "unread mail": mail, "a viewer": viewed,
		"no report": silent, "the root": lead} {
		if got := w.agent(a.Id); got.State != v1.AgentState_AGENT_STATE_IDLE {
			t.Errorf("%s: retired anyway (%v)", name, got.State)
		}
	}
	// Once the mail is read and the viewer leaves, they go.
	if _, err := w.as(w.token(t, mail.Id)).ReadInbox(ctx, connect.NewRequest(&v1.ReadInboxRequest{Meta: meta("")})); err != nil {
		t.Fatal(err)
	}
	user.Close()
	w.waitState(mail.Id, v1.AgentState_AGENT_STATE_RETIRED)
	w.waitState(viewed.Id, v1.AgentState_AGENT_STATE_RETIRED)
	// A retired agent isn't retired again, and 0 turns it off.
	w.set(t, "idle_retire", 0)
	w.report(t, silent, v1.MessageType_MESSAGE_TYPE_DONE)
	time.Sleep(time.Second)
	if got := w.agent(silent.Id); got.State != v1.AgentState_AGENT_STATE_IDLE {
		t.Errorf("idle_retire 0 retired %v", got.State)
	}
}

func TestAMessageArrivingJustBeforeAnIdleRetireKeepsTheAgentOpen(t *testing.T) {
	w := startReopen(t, func(c *daemon.AgentConfig) { c.RetireEvery = 50 * time.Millisecond })
	ctx := context.Background()
	w.set(t, "idle_retire", 1)
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	a := w.spawnUnder(t, asLead, "busy", &v1.SpawnAgentRequest{})
	w.report(t, a, v1.MessageType_MESSAGE_TYPE_DONE)
	// A task before the delay is up: it works on it, and the new report restarts the count.
	time.Sleep(600 * time.Millisecond)
	if _, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: a.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "again"})); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if got := w.agent(a.Id); got.State == v1.AgentState_AGENT_STATE_RETIRED {
		t.Fatalf("retired with an unanswered task: %v", got)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestStopRunRetiresEveryAgentClosesTheRunAndKeepsWhatHoldsWork(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	untouched := w.spawnUnder(t, asLead, "untouched", &v1.SpawnAgentRequest{})
	committed := w.spawnUnder(t, asLead, "committed", &v1.SpawnAgentRequest{})
	dirty := w.spawnUnder(t, asLead, "dirty", &v1.SpawnAgentRequest{})
	pushed := w.spawnUnder(t, asLead, "pushed", &v1.SpawnAgentRequest{})
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitIn(t, pushed.Repo, "init", "-q", "--bare", remote)
	gitIn(t, pushed.Repo, "remote", "add", "origin", remote)
	gitIn(t, pushed.Worktree, "push", "-q", "origin", pushed.Branch)
	gone := w.spawnUnder(t, asLead, "gone", &v1.SpawnAgentRequest{})
	os.WriteFile(filepath.Join(committed.Worktree, "done.txt"), []byte("work"), 0o644)
	gitIn(t, committed.Worktree, "add", "done.txt")
	gitIn(t, committed.Worktree, "commit", "-q", "-m", "work")
	os.WriteFile(filepath.Join(dirty.Worktree, "wip.txt"), []byte("half"), 0o644)
	w.typeInto(gone, "/exit\r")
	w.waitState(gone.Id, v1.AgentState_AGENT_STATE_EXITED)

	// Only a device stops a run.
	if _, err := asLead.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta(""), RunId: run.Id})); code(err) != connect.CodePermissionDenied {
		t.Errorf("a root stopping its run: %v", err)
	}
	if _, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta(""), RunId: "nope"})); code(err) != connect.CodeNotFound {
		t.Errorf("an unknown run: %v", err)
	}
	res, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta("stop-1"), RunId: run.Id}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.Run.Status != v1.RunStatus_RUN_STATUS_CLOSED || len(res.Msg.Retired) != 5 {
		t.Fatalf("stopped %v", res.Msg)
	}
	for _, a := range []*v1.Agent{lead, untouched, committed, dirty, pushed} {
		if got := w.agent(a.Id); got.State != v1.AgentState_AGENT_STATE_RETIRED || got.TerminalId != "" {
			t.Errorf("%s: %v", a.Name, got.State)
		}
	}
	if got := w.agent(gone.Id); got.State != v1.AgentState_AGENT_STATE_EXITED {
		t.Errorf("an exited agent became %v", got.State)
	}
	if left := w.terminals(t); len(left) != 0 {
		t.Errorf("terminals left: %v", left)
	}
	out := map[string]*v1.WorktreeOutcome{}
	for _, o := range res.Msg.Worktrees {
		out[o.AgentId] = o
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	branches := gitIn(t, untouched.Repo, "branch", "--list", "mesh/*")
	if o := out[untouched.Id]; !o.WorktreeRemoved || o.BranchKept || exists(untouched.Worktree) || strings.Contains(branches, untouched.Branch) {
		t.Errorf("untouched %v (branches %s)", o, branches)
	}
	if o := out[committed.Id]; !o.WorktreeRemoved || !o.BranchKept || exists(committed.Worktree) {
		t.Errorf("committed %v", o)
	}
	if b := gitIn(t, committed.Repo, "branch", "--list", committed.Branch); !strings.Contains(b, committed.Branch) {
		t.Errorf("the committed branch went: %q", b)
	}
	if o := out[dirty.Id]; o.WorktreeRemoved || !o.BranchKept || !exists(dirty.Worktree) || !strings.Contains(o.Reason, "uncommitted") {
		t.Errorf("dirty %v", o)
	}
	if o := out[pushed.Id]; !o.WorktreeRemoved || !o.BranchKept || !strings.Contains(o.Reason, "pushed") {
		t.Errorf("pushed %v", o)
	}
	if o := out[gone.Id]; !o.WorktreeRemoved || o.BranchKept {
		t.Errorf("the exited agent's %v", o)
	}
	// The run is closed: nothing starts in it, and nothing resumes.
	if _, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{RunId: run.Id, Repo: gitRepo(t), Slug: "late", Tier: "mid"})); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("a spawn in a stopped run: %v", err)
	}
	if _, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Meta: meta(""), Agent: dirty.Id})); code(err) != connect.CodeFailedPrecondition ||
		!strings.Contains(err.Error(), "closed") {
		t.Errorf("a resume in a stopped run: %v", err)
	}
	evs, _ := w.d.Store().Read(ctx).EventsAfter(0, run.Id, 1000)
	stopped := 0
	for _, e := range evs {
		if e.GetRunStopped() != nil {
			stopped++
		}
	}
	if stopped != 1 {
		t.Errorf("%d RunStopped events", stopped)
	}
	// A retry of the same call is its stored answer, and stopping again with a new id changes nothing more.
	if again, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta("stop-1"), RunId: run.Id})); err != nil || len(again.Msg.Retired) != 5 {
		t.Errorf("the retry: %v, %v", again, err)
	}
	if again, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta("stop-2"), RunId: run.Id})); err != nil || len(again.Msg.Retired) != 0 {
		t.Errorf("stopping a stopped run: %v, %v", again, err)
	}
}

func TestTheAgentCapRefusesWhatWouldPassIt(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	w.set(t, "caps.agents", 2)
	run := startRun(t, w.device.Mesh, "")
	repo := gitRepo(t)
	spawn := func(slug string) (*v1.Agent, error) {
		res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Meta: meta("sp-" + slug), RunId: run.Id, Repo: repo,
			Slug: slug, Tier: "mid"}))
		if err != nil {
			return nil, err
		}
		return res.Msg.Agent, nil
	}
	a, err := spawn("one")
	if err != nil {
		t.Fatal(err)
	}
	b, err := spawn("two")
	if err != nil {
		t.Fatal(err)
	}
	_, err = spawn("three")
	if code(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "caps.agents 2") {
		t.Fatalf("a third agent: %v", err)
	}
	// Nothing was left behind: no worktree, no agent, no branch.
	if list, _ := w.device.Mesh.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{RunId: run.Id})); len(list.Msg.Agents) != 2 {
		t.Errorf("%d agents", len(list.Msg.Agents))
	}
	if left, _ := filepath.Glob(filepath.Join(w.home, "..", "state", "worktrees", run.Id, "*three")); len(left) != 0 {
		t.Errorf("worktree left: %v", left)
	}
	// Retire one: room for another. A resume or a message to a retired one counts too, and is refused, not queued.
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	if _, err := w.device.Mesh.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: a.Id})); err != nil {
		t.Fatal(err)
	}
	c, err := spawn("three")
	if err != nil {
		t.Fatalf("after a retire: %v", err)
	}
	if _, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Meta: meta(""), Agent: a.Id})); code(err) != connect.CodeResourceExhausted {
		t.Errorf("a resume at the cap: %v", err)
	}
	before, _ := w.d.Store().Read(ctx).Inbox(a.Id, true)
	if _, err := w.device.Mesh.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: a.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "wake up"})); code(err) != connect.CodeResourceExhausted {
		t.Errorf("a message at the cap: %v", err)
	}
	if after, _ := w.d.Store().Read(ctx).Inbox(a.Id, true); len(after) != len(before) {
		t.Errorf("a refused message was stored")
	}
	// A pending resume counts: with one cap slot left it takes it, and the next is refused.
	w.waitState(c.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.waitState(b.Id, v1.AgentState_AGENT_STATE_IDLE)
	if _, err := w.device.Mesh.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: c.Id})); err != nil {
		t.Fatal(err)
	}
	if _, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Meta: meta(""), Agent: a.Id})); err != nil {
		t.Fatalf("a resume with room: %v", err)
	}
	if _, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Meta: meta(""), Agent: c.Id})); code(err) != connect.CodeResourceExhausted {
		t.Errorf("a second resume: %v", err)
	}
	// Switching a live agent replaces its process: not a new one, never refused.
	if _, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: b.Id, Effort: "low"})); err != nil {
		t.Errorf("a switch at the cap: %v", err)
	}
	w.waitReopened(t, b.Id, b.TerminalId)
	// 0 is no cap.
	w.set(t, "caps.agents", 0)
	if _, err := spawn("four"); err != nil {
		t.Errorf("with no cap: %v", err)
	}
}

func TestSpawnsInABurstCannotAllPassTheCap(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	w.set(t, "caps.agents", 2)
	run := startRun(t, w.device.Mesh, "")
	repo := gitRepo(t)
	var wg sync.WaitGroup
	results := make([]error, 6)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Meta: meta(""), RunId: run.Id, Repo: repo,
				Slug: "burst-" + string(rune('a'+i)), Tier: "mid"}))
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case code(err) != connect.CodeResourceExhausted:
			t.Errorf("a burst spawn: %v", err)
		}
	}
	if ok != 2 {
		t.Errorf("%d of 6 spawns passed a cap of 2", ok)
	}
	list, _ := w.device.Mesh.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{RunId: run.Id}))
	for _, a := range list.Msg.Agents {
		w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	}
}

func writeProc(t *testing.T, dir, name, text string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func meminfoText(availMB, swapFreeMB, totalMB, swapTotalMB int) string {
	return fmt.Sprintf("MemTotal: %d kB\nMemAvailable: %d kB\nSwapTotal: %d kB\nSwapFree: %d kB\n",
		totalMB*1024, availMB*1024, swapTotalMB*1024, swapFreeMB*1024)
}

func TestTheMemoryGuardRefusesALaunchUnderTheFloorOrUnderPressure(t *testing.T) {
	dir := t.TempDir()
	mem := writeProc(t, dir, "meminfo", meminfoText(1500, 0, 16000, 4000))
	psi := writeProc(t, dir, "pressure", "some avg10=0.00 avg60=0.00 avg300=0.00 total=0\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")
	w := startReopen(t, func(c *daemon.AgentConfig) { c.MeminfoPath, c.PressurePath = mem, psi })
	ctx := context.Background()
	w.set(t, "min_free_mb", 1000)
	run := startRun(t, w.device.Mesh, "")
	repo := gitRepo(t)
	spawn := func(slug string) error {
		_, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Meta: meta(""), RunId: run.Id, Repo: repo, Slug: slug, Tier: "mid"}))
		return err
	}
	// 1500 MB free over a 1000 MB floor: room for the first two; each launched agent counts 400 MB until it has
	// grown into the free figure, so the third reads 700 and is refused (four spawns in a row can't all pass).
	for _, slug := range []string{"one", "two"} {
		if err := spawn(slug); err != nil {
			t.Fatalf("%s: %v", slug, err)
		}
	}
	err := spawn("three")
	if code(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "2 agent(s) still starting") ||
		!strings.Contains(err.Error(), "under the 1000 MB floor") {
		t.Fatalf("the third: %v", err)
	}
	// Swap counts: free memory plus free swap is what the floor is read against.
	os.WriteFile(mem, []byte(meminfoText(1500, 1000, 16000, 4000)), 0o600)
	if err := spawn("three"); err != nil {
		t.Errorf("with swap free: %v", err)
	}
	// 0 turns the guard off.
	os.WriteFile(mem, []byte(meminfoText(10, 0, 16000, 4000)), 0o600)
	if err := spawn("four"); code(err) != connect.CodeResourceExhausted {
		t.Errorf("under the floor: %v", err)
	}
	w.set(t, "min_free_mb", 0)
	if err := spawn("four"); err != nil {
		t.Errorf("with the guard off: %v", err)
	}
	// "auto": 10% of RAM plus 10% of swap plus 1 GB, at least 2 GB: 16000 + 4000 MB → 3024 MB.
	w.set(t, "min_free_mb", "auto")
	os.WriteFile(mem, []byte(meminfoText(2900, 0, 16000, 4000)), 0o600)
	if err := spawn("five"); code(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "under the 3024 MB floor") {
		t.Errorf("auto floor: %v", err)
	}
	os.WriteFile(mem, []byte(meminfoText(1000, 0, 1000, 0)), 0o600) // a small machine: the 2 GB least applies
	if err := spawn("five"); code(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "under the 2048 MB floor") {
		t.Errorf("auto floor, small machine: %v", err)
	}
	// Pressure: over the limit refuses whatever is free; 0 turns it off.
	os.WriteFile(mem, []byte(meminfoText(60000, 0, 64000, 0)), 0o600)
	os.WriteFile(psi, []byte("some avg10=9.00 avg60=9.00 avg300=1.00 total=1\nfull avg10=20.00 avg60=14.50 avg300=3.00 total=9\n"), 0o600)
	if err := spawn("five"); code(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "memory pressure is high") ||
		!strings.Contains(err.Error(), "14.5%") {
		t.Errorf("under pressure: %v", err)
	}
	w.set(t, "memory_pressure_max", 0)
	if err := spawn("five"); err != nil {
		t.Errorf("pressure guard off: %v", err)
	}
	// No /proc files at all: nothing to tell by, so nothing refused.
	os.Remove(mem)
	os.Remove(psi)
	w.set(t, "memory_pressure_max", 10)
	if err := spawn("six"); err != nil {
		t.Errorf("without /proc: %v", err)
	}
	// The guard also checks a resume.
	list, _ := w.device.Mesh.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{RunId: run.Id}))
	victim := list.Msg.Agents[0]
	w.waitState(victim.Id, v1.AgentState_AGENT_STATE_IDLE)
	if _, err := w.device.Mesh.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: victim.Id})); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(mem, []byte(meminfoText(10, 0, 16000, 4000)), 0o600)
	w.set(t, "min_free_mb", 1000)
	if _, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Meta: meta(""), Agent: victim.Id})); code(err) != connect.CodeResourceExhausted {
		t.Errorf("a resume under the floor: %v", err)
	}
	for _, a := range list.Msg.Agents {
		if got := w.agent(a.Id); got.State != v1.AgentState_AGENT_STATE_RETIRED {
			w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
		}
	}
}
