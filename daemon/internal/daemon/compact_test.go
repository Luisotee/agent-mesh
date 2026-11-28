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
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// compactWorld is a daemon whose headless compactions run the fake claude in print mode, recorded.
type compactWorld struct {
	*reopenWorld
	mu       sync.Mutex
	runs     [][]string
	runEnvs  []map[string]string
	runFails bool
}

func startCompact(t *testing.T, tweaks ...func(*daemon.AgentConfig)) *compactWorld {
	t.Helper()
	cw := &compactWorld{}
	all := append([]func(*daemon.AgentConfig){func(c *daemon.AgentConfig) {
		c.CompactQuietWait, c.CompactStartWait, c.CompactTimeout, c.ContextEvery = 400*time.Millisecond, 800*time.Millisecond, 10*time.Second, 100*time.Millisecond
		c.RunCLI = cw.run
	}}, tweaks...)
	cw.reopenWorld = startReopen(t, all...)
	return cw
}

// run is the daemon's RunCLI: it records the command and really runs it (the fake claude in print mode).
func (cw *compactWorld) run(ctx context.Context, argv []string, env map[string]string, cwd string) (string, error) {
	cw.mu.Lock()
	cw.runs = append(cw.runs, argv)
	cw.runEnvs = append(cw.runEnvs, env)
	fails := cw.runFails
	cw.mu.Unlock()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if fails {
		cmd.Env = append(cmd.Env, "FAKECLAUDE_PRINT_FAIL=1")
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (cw *compactWorld) compactions() [][]string {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	return append([][]string(nil), cw.runs...)
}

// size writes a transcript for the agent's conversation whose newest reply holds tokens, its cache written with
// the TTL named (ephemeral_1h_input_tokens or ephemeral_5m_input_tokens), last written age ago.
func (cw *compactWorld) size(t *testing.T, a *v1.Agent, tokens int, ttlKey string, age time.Duration) {
	t.Helper()
	path := claude.TranscriptPath(map[string]string{"HOME": cw.home}, a.Worktree, a.SessionId)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"type":"assistant","message":{"usage":{"input_tokens":%d,"cache_read_input_tokens":0,"cache_creation_input_tokens":10,"cache_creation":{%q:10}}}}`+"\n", tokens, ttlKey)
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// compacted lists the agent's AgentCompacted events.
func (cw *compactWorld) compacted(t *testing.T, run, agent string) []*v1.AgentCompacted {
	t.Helper()
	evs, err := cw.d.Store().Read(context.Background()).EventsAfter(0, run, 10000)
	if err != nil {
		t.Fatal(err)
	}
	var out []*v1.AgentCompacted
	for _, e := range evs {
		if c := e.GetAgentCompacted(); c != nil && e.AgentId == agent {
			out = append(out, c)
		}
	}
	return out
}

// seq is the sequence number of the agent's first event that match says is the one, 0 when none.
func (cw *compactWorld) seq(t *testing.T, run, agent string, match func(*v1.Event) bool) int64 {
	t.Helper()
	evs, err := cw.d.Store().Read(context.Background()).EventsAfter(0, run, 10000)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.AgentId == agent && match(e) {
			return e.Seq
		}
	}
	return 0
}

// stopped spawns an agent, sizes its conversation, stops it, and returns it as it was (idle) with its run.
func (cw *compactWorld) stopped(t *testing.T, slug, task string, role v1.AgentRole, tokens int, ttlKey string, age time.Duration) (*v1.Run, *v1.Agent) {
	t.Helper()
	run := startRun(t, cw.device.Mesh, "")
	res, err := cw.device.Mesh.SpawnAgent(context.Background(), connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
		RunId: run.Id, Repo: gitRepo(t), Slug: slug, Task: task, Tier: "mid", Role: role}))
	if err != nil {
		t.Fatal(err)
	}
	before := cw.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	cw.size(t, before, tokens, ttlKey, age)
	cw.typeInto(before, "/exit\r")
	cw.waitState(before.Id, v1.AgentState_AGENT_STATE_EXITED)
	return run, before
}

func (cw *compactWorld) resume(t *testing.T, a *v1.Agent, req *v1.ResumeAgentRequest) *v1.Agent {
	t.Helper()
	req.Agent = a.Id
	if _, err := cw.device.Mesh.ResumeAgent(context.Background(), connect.NewRequest(req)); err != nil {
		t.Fatal(err)
	}
	return cw.waitReopened(t, a.Id, a.TerminalId)
}

const (
	hour = "ephemeral_1h_input_tokens"
	five = "ephemeral_5m_input_tokens"
)

func TestResumeCompactsABigStoppedConversationBeforeItsReopen(t *testing.T) {
	w := startCompact(t)
	run, before := w.stopped(t, "back", "", v1.AgentRole_AGENT_ROLE_WORKER, 400000, hour, 0)
	after := w.resume(t, before, &v1.ResumeAgentRequest{Task: "add the feature\n\nwith tests"})
	runs := w.compactions()
	if len(runs) != 1 {
		t.Fatalf("compactions %v", runs)
	}
	argv := strings.Join(runs[0], "\x00")
	for _, want := range []string{"-p\x00--resume\x00" + before.SessionId, "--dangerously-skip-permissions", `"promptCacheTtl":"5m"`} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv lacks %q: %q", want, runs[0])
		}
	}
	if last := runs[0][len(runs[0])-1]; !strings.HasPrefix(last, "/compact Keep what the next task needs: add the feature. Keep the repo facts") {
		t.Errorf("prompt %q", last)
	}
	for k := range w.runEnvs[0] {
		if strings.HasPrefix(k, "AGENT_MESH_") {
			t.Errorf("the compaction carries an identity: %s", k)
		}
	}
	cs := w.compacted(t, run.Id, before.Id)
	if len(cs) != 1 || cs[0].Why != "resume" || cs[0].Via != "headless" || cs[0].Outcome != "done" || cs[0].TokensBefore != 400010 || cs[0].TokensAfter != 0 {
		t.Fatalf("events %v", cs)
	}
	if after.PendingCompaction != nil || after.SessionId != before.SessionId {
		t.Errorf("after %v", after)
	}
	compact := w.seq(t, run.Id, before.Id, func(e *v1.Event) bool { return e.GetAgentCompacted() != nil })
	reopened := w.seq(t, run.Id, before.Id, func(e *v1.Event) bool { return e.GetAgentReopened() != nil })
	if compact == 0 || reopened < compact {
		t.Errorf("compacted at %d, reopened at %d", compact, reopened)
	}
	// Once only: nothing asks again.
	time.Sleep(500 * time.Millisecond)
	if n := len(w.compactions()); n != 1 {
		t.Errorf("%d compactions for one resume", n)
	}
}

func TestResumeLeavesASmallConversationAlone(t *testing.T) {
	w := startCompact(t)
	run, before := w.stopped(t, "small", "", v1.AgentRole_AGENT_ROLE_WORKER, 250000, hour, 0)
	w.resume(t, before, &v1.ResumeAgentRequest{Task: "more"})
	if n := len(w.compactions()); n != 0 {
		t.Errorf("compactions %v", w.compactions())
	}
	if cs := w.compacted(t, run.Id, before.Id); len(cs) != 0 {
		t.Errorf("events %v", cs)
	}
}

func TestAColdCacheLowersTheThreshold(t *testing.T) {
	// 250k is under compact.claude (300k) but over compact.claude_cold (200k): only a cold conversation compacts.
	w := startCompact(t)
	_, warm := w.stopped(t, "warm", "", v1.AgentRole_AGENT_ROLE_WORKER, 250000, five, time.Minute)
	w.resume(t, warm, &v1.ResumeAgentRequest{Task: "more"})
	if n := len(w.compactions()); n != 0 {
		t.Fatalf("a warm 5-minute cache after one minute: %v", w.compactions())
	}
	_, cold := w.stopped(t, "cold", "", v1.AgentRole_AGENT_ROLE_WORKER, 250000, five, 10*time.Minute)
	w.resume(t, cold, &v1.ResumeAgentRequest{Task: "more"})
	if n := len(w.compactions()); n != 1 {
		t.Fatalf("a cold cache: %v", w.compactions())
	}
	// An hour's cache is still warm after ten minutes.
	_, hot := w.stopped(t, "hot", "", v1.AgentRole_AGENT_ROLE_WORKER, 250000, hour, 10*time.Minute)
	w.resume(t, hot, &v1.ResumeAgentRequest{Task: "more"})
	if n := len(w.compactions()); n != 1 {
		t.Errorf("a one-hour cache after ten minutes: %v", w.compactions())
	}
}

func TestAReadOnlyAgentGetsTwiceTheThreshold(t *testing.T) {
	w := startCompact(t)
	_, asker := w.stopped(t, "ask", "", v1.AgentRole_AGENT_ROLE_ASKER, 400000, hour, 0)
	w.resume(t, asker, &v1.ResumeAgentRequest{Task: "Question (read-only): and now?"})
	if n := len(w.compactions()); n != 0 {
		t.Fatalf("400k for a question: %v", w.compactions())
	}
	_, asker2 := w.stopped(t, "ask2", "", v1.AgentRole_AGENT_ROLE_ASKER, 700000, hour, 0)
	w.resume(t, asker2, &v1.ResumeAgentRequest{Task: "Question (read-only): and now?"})
	if n := len(w.compactions()); n != 1 {
		t.Errorf("700k for a question: %v", w.compactions())
	}
}

func TestCompactYesAndNoOverrideTheSizeAndFocusIsTheCallers(t *testing.T) {
	w := startCompact(t)
	_, small := w.stopped(t, "yes", "", v1.AgentRole_AGENT_ROLE_WORKER, 1000, hour, 0)
	w.resume(t, small, &v1.ResumeAgentRequest{Task: "more", Compact: "yes", Focus: "Keep only the branch name"})
	runs := w.compactions()
	if len(runs) != 1 || runs[0][len(runs[0])-1] != "/compact Keep only the branch name" {
		t.Fatalf("--compact yes: %v", runs)
	}
	_, big := w.stopped(t, "no", "", v1.AgentRole_AGENT_ROLE_WORKER, 900000, hour, 0)
	w.resume(t, big, &v1.ResumeAgentRequest{Task: "more", Compact: "no"})
	if n := len(w.compactions()); n != 1 {
		t.Errorf("--compact no: %v", w.compactions())
	}
	if _, err := w.device.Mesh.ResumeAgent(context.Background(), connect.NewRequest(&v1.ResumeAgentRequest{Agent: big.Id, Compact: "maybe"})); code(err) != connect.CodeInvalidArgument {
		t.Errorf("a bad mode: %v", err)
	}
}

func TestACompactionThatFailsStillResumesTheAgent(t *testing.T) {
	w := startCompact(t)
	w.runFails = true
	run, before := w.stopped(t, "fail", "", v1.AgentRole_AGENT_ROLE_WORKER, 500000, hour, 0)
	after := w.resume(t, before, &v1.ResumeAgentRequest{Task: "more"})
	if after.State != v1.AgentState_AGENT_STATE_IDLE {
		t.Fatalf("after %v", after)
	}
	cs := w.compacted(t, run.Id, before.Id)
	if len(cs) != 1 || !strings.HasPrefix(cs[0].Outcome, "compaction failed, so it goes on as it was") {
		t.Fatalf("events %v", cs)
	}
	if n := len(w.compactions()); n != 1 {
		t.Errorf("a failed compaction is not retried: %d runs", n)
	}
}

func TestAHeadlessCompactionStartedBeforeARestartIsNotRunAgain(t *testing.T) {
	w := startCompact(t)
	run, before := w.stopped(t, "restart", "", v1.AgentRole_AGENT_ROLE_WORKER, 500000, hour, 0)
	// What meshd left in the store when it died with the compaction's CLI running: the request, marked started.
	err := w.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		a, err := tx.GetAgent(before.Id)
		if err != nil {
			return err
		}
		a.PendingCompaction = &v1.PendingCompaction{Id: "c-restart", Why: "resume", Always: true, RequestedAt: timestamppb.Now(),
			StartedAt: timestamppb.Now(), TokensBefore: 500010}
		return tx.PutAgent(a)
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(w.compacted(t, run.Id, before.Id)) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	cs := w.compacted(t, run.Id, before.Id)
	if len(cs) != 1 || !strings.HasPrefix(cs[0].Outcome, "meshd restarted while it was compacting") || cs[0].TokensBefore != 500010 {
		t.Fatalf("events %v", cs)
	}
	if n := len(w.compactions()); n != 0 {
		t.Errorf("the compaction ran again: %v", w.compactions())
	}
	if got := w.agent(before.Id); got.PendingCompaction != nil {
		t.Errorf("the request is still pending: %v", got.PendingCompaction)
	}
}

func TestACompactionThatCannotBeTypedEndsInsteadOfRetrying(t *testing.T) {
	w := startCompact(t)
	run, worker, _, _ := followUpWorld(t, w)
	w.size(t, worker, 400000, hour, 0)
	// The agent's terminal can't be written to: the line is never typed.
	err := w.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		a, err := tx.GetAgent(worker.Id)
		if err != nil {
			return err
		}
		a.TerminalId = "gone-terminal"
		a.PendingCompaction = &v1.PendingCompaction{Id: "c-type", Why: "follow-up", Always: true, RequestedAt: timestamppb.Now()}
		return tx.PutAgent(a)
	})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the compaction ended", func() bool { return w.agent(worker.Id).PendingCompaction == nil })
	cs := w.compacted(t, run.Id, worker.Id)
	if len(cs) != 1 || !strings.HasPrefix(cs[0].Outcome, "could not type /compact: ") {
		t.Fatalf("events %v", cs)
	}
	time.Sleep(400 * time.Millisecond)
	if n := len(w.compacted(t, run.Id, worker.Id)); n != 1 {
		t.Errorf("%d events: it was asked again", n)
	}
}

// Approving a planner that sent no plan: the compaction's focus names no plan file (there is none to keep).
func TestApproveWithNoPlanCompactsWithoutNamingAPlanFile(t *testing.T) {
	var log string
	w := startCompact(t, func(c *daemon.AgentConfig) {
		log = filepath.Join(filepath.Dir(c.Env["HOME"]), "focus.log")
		c.Env["FAKECLAUDE_COMPACT_LOG"] = log
	})
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker", Repo: gitRepo(t), Slug: "fix",
		Tier: "strong", Plan: true, Task: "plan the fix"}))
	if err != nil {
		t.Fatal(err)
	}
	worker := res.Msg.Agent
	before := w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.size(t, before, 300000, hour, 0)
	readInbox(t, w.as(w.token(t, worker.Id)))
	if _, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: worker.Id, Tier: "mid"})); err != nil {
		t.Fatal(err)
	}
	w.waitReopened(t, worker.Id, before.TerminalId)
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "plan phase was skipped") || strings.Contains(string(b), "plans") || strings.Contains(string(b), "approved plan") {
		t.Errorf("typed focus %q: want the skipped-plan focus, naming no plan", b)
	}
}

func TestApproveCompactsALivePlannerInItsTerminalBeforeTheReopen(t *testing.T) {
	var log string
	w := startCompact(t, func(c *daemon.AgentConfig) {
		log = filepath.Join(filepath.Dir(c.Env["HOME"]), "focus.log")
		c.Env["FAKECLAUDE_COMPACT_LOG"] = log
	})
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker", Repo: gitRepo(t), Slug: "fix",
		Tier: "strong", Plan: true, Task: "plan the fix"}))
	if err != nil {
		t.Fatal(err)
	}
	worker := res.Msg.Agent
	before := w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.size(t, before, 300000, hour, 0)
	asWorker := w.as(w.token(t, worker.Id))
	readInbox(t, asWorker)
	if _, err := asWorker.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent",
		Type: v1.MessageType_MESSAGE_TYPE_PLAN, Body: "the plan"})); err != nil {
		t.Fatal(err)
	}
	ap, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: worker.Id, Tier: "mid"}))
	if err != nil {
		t.Fatal(err)
	}
	after := w.waitReopened(t, worker.Id, before.TerminalId)
	if n := len(w.compactions()); n != 0 {
		t.Errorf("a live planner is compacted in its terminal, not headless: %v", w.compactions())
	}
	b, _ := os.ReadFile(log)
	plan := filepath.Join(w.state, "runs", run.Id, "plans", before.Name+".md")
	if !strings.Contains(string(b), "Your plan was just approved") || !strings.Contains(string(b), "("+plan+")") {
		t.Errorf("typed focus %q (want the plan %s)", b, plan)
	}
	cs := w.compacted(t, run.Id, worker.Id)
	if len(cs) != 1 || cs[0].Why != "approve" || cs[0].Via != "live" || cs[0].Outcome != "done" || cs[0].TokensAfter != 0 {
		t.Fatalf("events %v", cs)
	}
	compact := w.seq(t, run.Id, worker.Id, func(e *v1.Event) bool { return e.GetAgentCompacted() != nil })
	reopened := w.seq(t, run.Id, worker.Id, func(e *v1.Event) bool { return e.GetAgentReopened() != nil })
	if compact == 0 || reopened < compact || after.Model != "sonnet" {
		t.Errorf("compacted at %d, reopened at %d, model %s", compact, reopened, after.Model)
	}
	waitFor(t, "the approval delivered", func() bool { return w.delivered(t, worker.Id, ap.Msg.Message.Id) })
}

// approveBig starts a planner whose transcript is big, sends its plan and approves it onto the mid tier.
func approveBig(t *testing.T, w *compactWorld) (run *v1.Run, before *v1.Agent) {
	t.Helper()
	ctx := context.Background()
	run = startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker", Repo: gitRepo(t), Slug: "fix",
		Tier: "strong", Plan: true, Task: "plan the fix"}))
	if err != nil {
		t.Fatal(err)
	}
	before = w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.size(t, before, 300000, hour, 0)
	asWorker := w.as(w.token(t, before.Id))
	readInbox(t, asWorker)
	if _, err := asWorker.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent",
		Type: v1.MessageType_MESSAGE_TYPE_PLAN, Body: "the plan"})); err != nil {
		t.Fatal(err)
	}
	if _, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: before.Id, Tier: "mid"})); err != nil {
		t.Fatal(err)
	}
	return run, before
}

func transcriptEnds(w *compactWorld, a *v1.Agent) string {
	b, _ := os.ReadFile(claude.TranscriptPath(map[string]string{"HOME": w.home}, a.Worktree, a.SessionId))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}

// Claude Code fires SessionStart (compact) before the compacted transcript is on disk. The reopen of an approve
// stops the process, so a compaction reported done at the hook was lost whenever the stop came first: found live,
// two approves at once both left their workers whole (the "left a worker at 215k tokens" of bin/mesh).
func TestApproveWaitsForTheCompactedTranscriptBeforeItStopsTheProcess(t *testing.T) {
	w := startCompact(t, func(c *daemon.AgentConfig) {
		c.Env["FAKECLAUDE_BOUNDARY_DELAY"] = "700ms"
		c.CompactSettle = 10 * time.Second
	})
	run, before := approveBig(t, w)
	w.waitReopened(t, before.Id, before.TerminalId)
	if last := transcriptEnds(w, before); !strings.Contains(last, "compact_boundary") {
		t.Fatalf("the process was stopped before the compaction reached the transcript; its last line: %s", last)
	}
	cs := w.compacted(t, run.Id, before.Id)
	if len(cs) != 1 || cs[0].Outcome != "done" || cs[0].TokensBefore != 300010 || cs[0].TokensAfter != 0 {
		t.Fatalf("events %v", cs)
	}
}

// A compaction the CLI reports and the transcript never shows is not "done": the agent goes on after the settle
// time, and the event says so.
func TestAReportedCompactionThatNeverReachesTheTranscriptIsNotDone(t *testing.T) {
	w := startCompact(t, func(c *daemon.AgentConfig) {
		c.Env["FAKECLAUDE_BOUNDARY_DELAY"] = "1h"
		c.CompactSettle = 700 * time.Millisecond
	})
	run, before := approveBig(t, w)
	w.waitReopened(t, before.Id, before.TerminalId)
	cs := w.compacted(t, run.Id, before.Id)
	if len(cs) != 1 || !strings.Contains(cs[0].Outcome, "showed no smaller size") || cs[0].TokensAfter != 300010 {
		t.Fatalf("events %v", cs)
	}
}

// An approve that reopens nothing (a pinned agent, which has no phase and no deny list) asks for no compaction:
// the compaction is the reopen's, and without a reopen the conversation just goes on with the answer.
func TestApproveWithoutAReopenCompactsNothing(t *testing.T) {
	w := startCompact(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker", Repo: gitRepo(t), Slug: "pinned",
		Model: "opus", Task: "Direct task (no plan): do it"}))
	if err != nil {
		t.Fatal(err)
	}
	pinned := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.size(t, pinned, 300000, hour, 0)
	asWorker := w.as(w.token(t, pinned.Id))
	readInbox(t, asWorker)
	if _, err := asWorker.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent",
		Type: v1.MessageType_MESSAGE_TYPE_PLAN, Body: "a plan"})); err != nil {
		t.Fatal(err)
	}
	ap, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: pinned.Id}))
	if err != nil || ap.Msg.Reopen != nil {
		t.Fatalf("approving: %v, %v", ap, err)
	}
	waitFor(t, "the approval delivered", func() bool { return w.delivered(t, pinned.Id, ap.Msg.Message.Id) })
	if got := w.agent(pinned.Id); got.PendingCompaction != nil {
		t.Errorf("a compaction is pending: %v", got.PendingCompaction)
	}
	if cs := w.compacted(t, run.Id, pinned.Id); len(cs) != 0 || len(w.compactions()) != 0 {
		t.Errorf("compacted: %v %v", cs, w.compactions())
	}
}

// followUpWorld starts a root and a direct-task worker that has gone idle, with the worker's own client.
func followUpWorld(t *testing.T, w *compactWorld) (*v1.Run, *v1.Agent, agentmeshv1connect.MeshServiceClient, agentmeshv1connect.MeshServiceClient) {
	t.Helper()
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker", Repo: gitRepo(t), Slug: "fix",
		Tier: "mid", Task: "Direct task (no plan): fix it"}))
	if err != nil {
		t.Fatal(err)
	}
	worker := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	asWorker := w.as(w.token(t, worker.Id))
	readInbox(t, asWorker)
	return run, worker, asLead, asWorker
}

func TestAFollowUpRoundCompactsAFinishedWorkerBeforeTheMessageRings(t *testing.T) {
	w := startCompact(t)
	ctx := context.Background()
	run, worker, asLead, asWorker := followUpWorld(t, w)
	round := func(tokens int, body string) *v1.Message {
		w.size(t, worker, tokens, hour, 0)
		if _, err := asWorker.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent",
			Type: v1.MessageType_MESSAGE_TYPE_DONE, Body: "done"})); err != nil {
			t.Fatal(err)
		}
		m, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: worker.Name,
			Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: body}))
		if err != nil {
			t.Fatal(err)
		}
		// The ring waits for the live compaction (up to CompactTimeout, 10 s, which a loaded host can nearly use up), so
		// the wait carries that and a margin: 15 s timed out once at load 8.
		waitForWithinSaying(t, 45*time.Second, "the follow-up delivered", func() bool { return w.delivered(t, worker.Id, m.Msg.Message.Id) },
			func() string {
				a := w.agent(worker.Id)
				return fmt.Sprintf("agent %v (%q), pending compaction %v, reopen %v, compactions %v, runs %d", a.State, a.StateDetail,
					a.PendingCompaction, a.PendingReopen, w.compacted(t, run.Id, worker.Id), len(w.compactions()))
			})
		readInbox(t, asWorker)
		return m.Msg.Message
	}
	round(100000, "a small follow-up")
	if cs := w.compacted(t, run.Id, worker.Id); len(cs) != 0 {
		t.Fatalf("100k is under compact.followup: %v", cs)
	}
	m := round(300000, "the second round")
	cs := w.compacted(t, run.Id, worker.Id)
	if len(cs) != 1 || cs[0].Why != "follow-up" || cs[0].Via != "live" || cs[0].Outcome != "done" {
		t.Fatalf("events %v", cs)
	}
	compact := w.seq(t, run.Id, worker.Id, func(e *v1.Event) bool { return e.GetAgentCompacted() != nil })
	rung := w.seq(t, run.Id, worker.Id, func(e *v1.Event) bool { return e.GetMessageDelivered().GetMessageId() == m.Id })
	if compact == 0 || rung < compact {
		t.Errorf("compacted at %d, the follow-up rang at %d", compact, rung)
	}
}

func TestALiveCompactionThatNeverStartsIsGivenUp(t *testing.T) {
	w := startCompact(t, func(c *daemon.AgentConfig) { c.Env["FAKECLAUDE_COMPACT"] = "none" })
	ctx := context.Background()
	run, worker, asLead, asWorker := followUpWorld(t, w)
	w.size(t, worker, 400000, hour, 0)
	if _, err := asWorker.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent",
		Type: v1.MessageType_MESSAGE_TYPE_DONE, Body: "done"})); err != nil {
		t.Fatal(err)
	}
	m, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: worker.Name,
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "next round"}))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the follow-up delivered anyway", func() bool { return w.delivered(t, worker.Id, m.Msg.Message.Id) })
	cs := w.compacted(t, run.Id, worker.Id)
	if len(cs) != 1 || cs[0].Outcome != "the CLI did not start compacting" || w.agent(worker.Id).PendingCompaction != nil {
		t.Fatalf("events %v", cs)
	}
}

func TestATaskForAWorkerThatHasNotReportedIsNeverCompacted(t *testing.T) {
	w := startCompact(t)
	ctx := context.Background()
	run, worker, asLead, _ := followUpWorld(t, w)
	w.size(t, worker, 900000, hour, 0)
	m, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: worker.Name,
		Type: v1.MessageType_MESSAGE_TYPE_ANSWER, Body: "go on"}))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the answer delivered", func() bool { return w.delivered(t, worker.Id, m.Msg.Message.Id) })
	if cs := w.compacted(t, run.Id, worker.Id); len(cs) != 0 || w.agent(worker.Id).PendingCompaction != nil {
		t.Errorf("a mid-task answer compacted: %v", cs)
	}
}

// A headless compaction still running when meshd stops is ended by the stop, and Serve returns: close() waits for
// the compaction's goroutine (it is in bgWG) instead of closing the store under it, and the stop is not a
// reason to wait the compaction's own timeout.
func TestAHeadlessCompactionStillRunningWhenMeshdStopsIsEndedByTheStop(t *testing.T) {
	started, ended := make(chan struct{}), make(chan error, 1)
	var once sync.Once
	w := startCompact(t, func(c *daemon.AgentConfig) {
		c.CompactTimeout = 10 * time.Minute
		c.RunCLI = func(ctx context.Context, argv []string, env map[string]string, cwd string) (string, error) {
			once.Do(func() { close(started) })
			<-ctx.Done() // a compaction that never finishes by itself
			ended <- ctx.Err()
			return "", ctx.Err()
		}
	})
	_, before := w.stopped(t, "stopping", "", v1.AgentRole_AGENT_ROLE_WORKER, 500000, hour, 0)
	if _, err := w.device.Mesh.ResumeAgent(context.Background(), connect.NewRequest(&v1.ResumeAgentRequest{Agent: before.Id, Task: "more"})); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("the compaction never started")
	}
	begin := time.Now()
	w.stop() // fails the test when Serve does not return within 10 s
	if took := time.Since(begin); took > 9*time.Second {
		t.Errorf("stopping took %v with a compaction running", took)
	}
	select {
	case err := <-ended:
		if err == nil {
			t.Error("the compaction's context was not ended")
		}
	default:
		t.Error("the compaction was still running when Serve returned")
	}
}
