package daemon_test

import (
	"context"
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
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
	"github.com/Luisotee/agent-mesh/daemon/internal/termclient"
)

// resetWorld is a compactWorld whose fake claude logs every line typed into it, and which can make a fresh launch
// fail (the file named by FAKECLAUDE_FAIL_FILE exists).
type resetWorld struct {
	*compactWorld
	inputLog, failFile, hangFile, crashFile string
}

func startReset(t *testing.T, timings daemon.ResetTimings, tweaks ...func(*daemon.AgentConfig)) *resetWorld {
	t.Helper()
	rw := &resetWorld{}
	dir, err := os.MkdirTemp("", "reset-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	rw.inputLog, rw.failFile = filepath.Join(dir, "input.log"), filepath.Join(dir, "fail")
	rw.hangFile, rw.crashFile = filepath.Join(dir, "hang"), filepath.Join(dir, "clearcrash")
	all := append([]func(*daemon.AgentConfig){func(c *daemon.AgentConfig) {
		c.Env["FAKECLAUDE_INPUT_LOG"], c.Env["FAKECLAUDE_FAIL_FILE"] = rw.inputLog, rw.failFile
		c.Env["FAKECLAUDE_HANG_FILE"], c.Env["FAKECLAUDE_CLEAR_CRASH_FILE"] = rw.hangFile, rw.crashFile
		c.Reset = timings
	}}, tweaks...)
	rw.compactWorld = startCompact(t, all...)
	return rw
}

// typed is every line typed into any fake claude so far.
func (rw *resetWorld) typed() string {
	b, _ := os.ReadFile(rw.inputLog)
	return string(b)
}

func (rw *resetWorld) setSetting(t *testing.T, key string, value any) {
	t.Helper()
	if _, err := rw.device.Mesh.SetSetting(context.Background(), connect.NewRequest(&v1.SetSettingRequest{Key: key, Value: val(t, value)})); err != nil {
		t.Fatal(err)
	}
}

func (rw *resetWorld) waitReset(t *testing.T, id, outcome string) *v1.Agent {
	t.Helper()
	var a *v1.Agent
	waitFor(t, "the reset to end "+outcome, func() bool {
		a = rw.agent(id)
		return a.PendingReset == nil && a.LastReset != nil && a.LastReset.Outcome == outcome
	})
	return a
}

// resetEvents lists the root's reset events by kind, in order.
func (rw *resetWorld) resetEvents(t *testing.T, run, agent string) []string {
	t.Helper()
	evs, err := rw.d.Store().Read(context.Background()).EventsAfter(0, run, 10000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		switch {
		case e.AgentId != agent:
		case e.GetRootResetRequested() != nil:
			out = append(out, "requested")
		case e.GetRootResetFinished() != nil:
			out = append(out, e.GetRootResetFinished().RootReset.Outcome)
		}
	}
	return out
}

func TestAResetRelaunchesTheRootFreshAndTellsItToRunBrief(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	before := w.agent(lead.Id)
	w.size(t, before, 200000, five, 10*time.Minute)

	res, err := asLead.ResetRoot(ctx, connect.NewRequest(&v1.ResetRootRequest{Next: "finish the PR"}))
	if err != nil {
		t.Fatal(err)
	}
	if r := res.Msg.RootReset; r.Via != "command" || r.Next != "finish the PR" || r.Tokens != 200010 {
		t.Fatalf("reset %v", r)
	}
	if _, err := asLead.ResetRoot(ctx, connect.NewRequest(&v1.ResetRootRequest{})); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("a second reset while one is pending: %v", err)
	}
	after := w.waitReset(t, lead.Id, "done")
	if after.TerminalId == before.TerminalId || after.SessionId == "" || after.SessionId == before.SessionId {
		t.Fatalf("after %v", after)
	}
	lr := after.LastReset
	if lr.Mode != "relaunch" || lr.OldSession != before.SessionId || lr.NewSession != after.SessionId || lr.Next != "finish the PR" {
		t.Errorf("last reset %v", lr)
	}
	launches := w.launches(t, after.SessionId)
	if len(launches) != 1 {
		t.Fatalf("launches %v", launches)
	}
	if f, _ := flags(launches[0]); f["--session-id"] != after.SessionId || f["--resume"] != "" {
		t.Errorf("a fresh conversation, not a resume: %v", launches[0])
	}
	waitFor(t, "the bootstrap typed", func() bool { return strings.Contains(w.typed(), "mesh reset` just gave you a fresh session") })
	line := ""
	for _, l := range strings.Split(w.typed(), "\n") {
		if strings.HasPrefix(l, "📬 [mesh] You are "+lead.Name) {
			line = l
		}
	}
	for _, want := range []string{"the root of agent-mesh run " + run.Id, "Run `mesh brief`", "Then: finish the PR"} {
		if !strings.Contains(line, want) {
			t.Errorf("bootstrap %q lacks %q", line, want)
		}
	}
	if got := strings.Join(w.resetEvents(t, run.Id, lead.Id), ","); got != "requested,done" {
		t.Errorf("events %s", got)
	}
	// The old token is gone with the old process; the fresh root works under its new one.
	if _, err := w.as(w.token(t, lead.Id)).Hello(ctx, connect.NewRequest(&v1.HelloRequest{})); err != nil {
		t.Errorf("the new token: %v", err)
	}
}

func TestAResetWithRelaunchOffClearsTheSameProcess(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	w.setSetting(t, "root_reset.relaunch", false)
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	before := w.agent(lead.Id)
	if _, err := asLead.ResetRoot(ctx, connect.NewRequest(&v1.ResetRootRequest{})); err != nil {
		t.Fatal(err)
	}
	after := w.waitReset(t, lead.Id, "done")
	if after.TerminalId != before.TerminalId || after.SessionId != before.SessionId+"-cleared" {
		t.Fatalf("a /clear keeps the process and gives a new session: %v", after)
	}
	if lr := after.LastReset; lr.Mode != "clear" || lr.NewSession != after.SessionId {
		t.Errorf("last reset %v", lr)
	}
	typed := w.typed()
	if !strings.Contains(typed, "/clear\n") || strings.Index(typed, "/clear\n") > strings.Index(typed, "mesh reset` just gave you a fresh session") ||
		!strings.Contains(typed, "Then: carry on from the next step in your notes") {
		t.Errorf("typed %q", typed)
	}
}

func TestACompactingResetTypesCompactAndKeepsTheConversation(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	before := w.agent(lead.Id)
	w.size(t, before, 200000, hour, 0)
	if _, err := asLead.ResetRoot(ctx, connect.NewRequest(&v1.ResetRootRequest{Compact: true, Focus: "keep the notes"})); err != nil {
		t.Fatal(err)
	}
	after := w.waitReset(t, lead.Id, "done")
	if after.TerminalId != before.TerminalId || after.SessionId != before.SessionId || after.LastReset.Mode != "compact" {
		t.Fatalf("after %v", after)
	}
	typed := w.typed()
	if !strings.Contains(typed, "/compact keep the notes\n") || !strings.Contains(typed, "mesh reset` just compacted your session") {
		t.Errorf("typed %q", typed)
	}
	if cs := w.compacted(t, run.Id, lead.Id); len(cs) != 1 || cs[0].Why != "reset" || cs[0].Via != "live" {
		t.Errorf("events %v", cs)
	}
}

func TestAResetRefusesWhileAPlanWaitsAndCancelsOneThatArrives(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: gitRepo(t), Slug: "fix", Tier: "mid",
		Task: "Direct task (no plan): fix it"}))
	if err != nil {
		t.Fatal(err)
	}
	worker := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	asWorker := w.as(w.token(t, worker.Id))
	readInbox(t, asWorker)

	// The root is in a turn; the reset waits for it to end.
	rootNow := w.agent(lead.Id)
	w.typeInto(rootNow, "/hold\r")
	w.waitState(lead.Id, v1.AgentState_AGENT_STATE_WORKING)
	if _, err := asLead.ResetRoot(ctx, connect.NewRequest(&v1.ResetRootRequest{})); err != nil {
		t.Fatal(err)
	}
	if _, err := asWorker.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent",
		Type: v1.MessageType_MESSAGE_TYPE_PLAN, Body: "the plan"})); err != nil {
		t.Fatal(err)
	}
	w.typeInto(w.agent(lead.Id), "\r") // the turn ends
	after := w.waitReset(t, lead.Id, "cancelled")
	if !strings.Contains(after.LastReset.Reason, "a plan from") || after.TerminalId != rootNow.TerminalId {
		t.Fatalf("after %v", after)
	}
	// And a reset asked while the plan is unread is refused.
	w.waitState(lead.Id, v1.AgentState_AGENT_STATE_IDLE)
	if _, err := asLead.ResetRoot(ctx, connect.NewRequest(&v1.ResetRootRequest{})); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("a reset with a plan unread: %v", err)
	}
	if got := strings.Join(w.resetEvents(t, run.Id, lead.Id), ","); got != "requested,cancelled" {
		t.Errorf("events %s", got)
	}
}

func TestAResetGivesUpWhenTheRootNeverGoesQuiet(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	w.setSetting(t, "root_reset.wait", 0.5)
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	w.typeInto(w.agent(lead.Id), "/hold\r")
	w.waitState(lead.Id, v1.AgentState_AGENT_STATE_WORKING)
	if _, err := asLead.ResetRoot(ctx, connect.NewRequest(&v1.ResetRootRequest{})); err != nil {
		t.Fatal(err)
	}
	after := w.waitReset(t, lead.Id, "failed")
	if !strings.Contains(after.LastReset.Reason, "gave up after waiting") {
		t.Errorf("reason %q", after.LastReset.Reason)
	}
	// The failure is shown to the root once, in the hint header of its next command.
	hint := func() string {
		res, err := asLead.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Header().Get(daemon.HintHeader)
	}
	if h := hint(); !strings.Contains(h, "the last `mesh reset` failed") || !strings.Contains(h, "gave up after waiting") {
		t.Errorf("hint %q", h)
	}
	if h := hint(); h != "" {
		t.Errorf("the failure was said twice: %q", h)
	}
}

func TestAFreshLaunchThatFailsResumesTheOldSessionSoTheRootKeepsAProcess(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	before := w.agent(lead.Id)
	if err := os.WriteFile(w.failFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := asLead.ResetRoot(ctx, connect.NewRequest(&v1.ResetRootRequest{})); err != nil {
		t.Fatal(err)
	}
	after := w.waitReset(t, lead.Id, "failed")
	if !strings.Contains(after.LastReset.Reason, "the old one was resumed") {
		t.Errorf("reason %q", after.LastReset.Reason)
	}
	var cur *v1.Agent
	waitFor(t, "the old session running again", func() bool {
		cur = w.agent(lead.Id)
		return cur.State == v1.AgentState_AGENT_STATE_IDLE && cur.PendingReopen == nil && cur.SessionId == before.SessionId &&
			cur.TerminalId != "" && cur.TerminalId != before.TerminalId
	})
	if all := w.launches(t, before.SessionId); len(all) < 2 {
		t.Errorf("launches of the old session %v", all)
	}
}

func TestAFreshLaunchTooSlowToReachItsPromptIsStoppedAndTheOldSessionResumed(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{Confirm: 800 * time.Millisecond})
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	before := w.agent(lead.Id)
	if err := os.WriteFile(w.hangFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := asLead.ResetRoot(ctx, connect.NewRequest(&v1.ResetRootRequest{})); err != nil {
		t.Fatal(err)
	}
	after := w.waitReset(t, lead.Id, "failed")
	if !strings.Contains(after.LastReset.Reason, "did not come up at its prompt in time") {
		t.Errorf("reason %q", after.LastReset.Reason)
	}
	var cur *v1.Agent
	waitFor(t, "the old session running again, the slow process gone", func() bool {
		cur = w.agent(lead.Id)
		return cur.State == v1.AgentState_AGENT_STATE_IDLE && cur.PendingReopen == nil && cur.SessionId == before.SessionId &&
			cur.TerminalId != "" && cur.TerminalId != before.TerminalId
	})
	tc, err := termclient.New(statedir.Paths{Dir: w.state}.Term())
	if err != nil {
		t.Fatal(err)
	}
	live := 0
	waitFor(t, "the slow process stopped", func() bool {
		list, err := tc.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		live = 0
		for _, term := range list.Msg.Terminals {
			if !term.Exited {
				live++
			}
		}
		return live == 1
	})
	if live != 1 {
		t.Errorf("%d live terminals: the slow process was left running unbriefed", live)
	}
	if strings.Contains(w.typed(), "just gave you a fresh session") {
		t.Errorf("a bootstrap was typed into a root that was resumed: %q", w.typed())
	}
}

func TestAClearedRootThatEndsIsNotHeldByItsReset(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	w.setSetting(t, "root_reset.relaunch", false)
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	if err := os.WriteFile(w.crashFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := asLead.ResetRoot(ctx, connect.NewRequest(&v1.ResetRootRequest{})); err != nil {
		t.Fatal(err)
	}
	after := w.waitReset(t, lead.Id, "failed")
	if !strings.Contains(after.LastReset.Reason, "its process ended before the prompt to run `mesh brief` was typed") {
		t.Errorf("reason %q", after.LastReset.Reason)
	}
	if after.PendingReset != nil {
		t.Errorf("still held: %v", after.PendingReset)
	}
}

func TestABootstrapHeldBackByUserTypingIsGivenUpAndSaidSo(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	w.setSetting(t, "root_reset.relaunch", false)
	w.setSetting(t, "root_reset.wait", 1.0)
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	// A user is attached and silent, so the reset's /clear goes through at once...
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s := w.openTerminal(sctx, w.agent(lead.Id), true, &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_TAIL})
	defer s.Close()
	for {
		f, err := s.Recv(sctx)
		if err != nil {
			t.Fatal(err)
		}
		if f.GetReplayDone() != nil {
			break
		}
	}
	s.Ready(sctx)
	if _, err := asLead.ResetRoot(ctx, connect.NewRequest(&v1.ResetRootRequest{})); err != nil {
		t.Fatal(err)
	}
	// ...and starts typing the moment it is typed, and never stops: the bootstrap line is held back, then given up.
	for !strings.Contains(w.typed(), "/clear\n") && sctx.Err() == nil {
		time.Sleep(2 * time.Millisecond)
	}
	for a := w.agent(lead.Id); a.PendingReset != nil && sctx.Err() == nil; a = w.agent(lead.Id) {
		if err := s.Input(sctx, []byte("x")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	after := w.waitReset(t, lead.Id, "failed")
	if !strings.Contains(after.LastReset.Reason, "was not typed: someone was typing into it") || !strings.Contains(after.LastReset.Reason, "Run `mesh brief` yourself") {
		t.Errorf("reason %q", after.LastReset.Reason)
	}
	if strings.Contains(w.typed(), "just gave you a fresh session") {
		t.Errorf("a bootstrap was typed over a draft: %q", w.typed())
	}
}

func TestTheKeeperResetsABigIdleRootAndNotASmallOneOrOneWithMail(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{Every: 100 * time.Millisecond, Quiet: 300 * time.Millisecond, UserQuiet: 600 * time.Millisecond, Backoff: time.Hour})
	ctx := context.Background()

	small := startRun(t, w.device.Mesh, "")
	smallLead, _ := w.root(t, small.Id)
	w.size(t, w.agent(smallLead.Id), 100000, hour, 0)

	mailed := startRun(t, w.device.Mesh, "")
	mailedLead, _ := w.root(t, mailed.Id)
	w.size(t, w.agent(mailedLead.Id), 200000, hour, 0)
	// An unread message holds the keeper (it acts without the root's say).
	if _, err := w.device.Mesh.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: mailedLead.Id,
		Type: v1.MessageType_MESSAGE_TYPE_INFO, Body: "fyi"})); err != nil {
		t.Fatal(err)
	}

	big := startRun(t, w.device.Mesh, "")
	bigLead, _ := w.root(t, big.Id)
	w.size(t, w.agent(bigLead.Id), 200000, hour, 0)
	started := time.Now()
	after := w.waitReset(t, bigLead.Id, "done")
	if after.LastReset.Via != "keeper" || !strings.Contains(after.LastReset.Next, "carry on supervising") || time.Since(started) < 500*time.Millisecond {
		t.Errorf("last reset %v after %s", after.LastReset, time.Since(started))
	}
	waitFor(t, "the keeper's bootstrap", func() bool {
		return strings.Contains(w.typed(), "ask the user again rather than going on without it")
	})
	time.Sleep(700 * time.Millisecond)
	if got := w.resetEvents(t, small.Id, smallLead.Id); len(got) != 0 {
		t.Errorf("a small root: %v", got)
	}
	if got := w.resetEvents(t, mailed.Id, mailedLead.Id); len(got) != 0 {
		t.Errorf("a root with unread mail: %v", got)
	}
	// The backoff: a second reset isn't asked for at once, though the root is still big.
	w.size(t, w.agent(bigLead.Id), 200000, hour, 0)
	time.Sleep(700 * time.Millisecond)
	if got := strings.Join(w.resetEvents(t, big.Id, bigLead.Id), ","); got != "requested,done" {
		t.Errorf("events %s", got)
	}
}

func TestTheKeeperWaitsLessAfterATurnMeshStarted(t *testing.T) {
	// Quiet is 300 ms after a turn a doorbell started and 30 s otherwise: only the root a worker's report woke is
	// reset within a second.
	w := startReset(t, daemon.ResetTimings{Every: 100 * time.Millisecond, Quiet: 300 * time.Millisecond, UserQuiet: 30 * time.Second})
	ctx := context.Background()
	woken := startRun(t, w.device.Mesh, "")
	wokenLead, asWoken := w.root(t, woken.Id)
	res, err := asWoken.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: gitRepo(t), Slug: "fix", Tier: "mid", Task: "Direct task (no plan): fix it"}))
	if err != nil {
		t.Fatal(err)
	}
	worker := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	asWorker := w.as(w.token(t, worker.Id))
	readInbox(t, asWorker)
	if _, err := asWorker.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{To: "parent", Type: v1.MessageType_MESSAGE_TYPE_DONE, Body: "done"})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the report's doorbell to start a turn", func() bool { return w.agent(wokenLead.Id).LastTurnByMesh })
	readInbox(t, asWoken)
	w.waitState(wokenLead.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.size(t, w.agent(wokenLead.Id), 200000, hour, 0)

	typed := startRun(t, w.device.Mesh, "")
	typedLead, _ := w.root(t, typed.Id)
	w.typeInto(w.agent(typedLead.Id), "what is the state?\r") // the user's turn
	waitFor(t, "the user's turn", func() bool {
		a := w.agent(typedLead.Id)
		return a.State == v1.AgentState_AGENT_STATE_IDLE && a.LastActivity != nil && !a.LastTurnByMesh
	})
	w.size(t, w.agent(typedLead.Id), 200000, hour, 0)

	w.waitReset(t, wokenLead.Id, "done")
	if got := w.resetEvents(t, typed.Id, typedLead.Id); len(got) != 0 {
		t.Errorf("a root the user last talked to was reset: %v", got)
	}
}

func TestARootCommandCarriesTheContextHintAndNobodyElsesDoes(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	hint := func(c agentmeshv1connect.MeshServiceClient) string {
		res, err := c.ListAgents(ctx, connect.NewRequest(&v1.ListAgentsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Header().Get(daemon.HintHeader)
	}
	rootNow := w.agent(lead.Id)
	// Under the threshold: nothing to say.
	w.size(t, rootNow, 100000, hour, 0)
	if h := hint(asLead); h != "" {
		t.Errorf("a small root: %q", h)
	}
	// Big and cold: reset now.
	w.size(t, rootNow, 200000, five, 10*time.Minute)
	h := hint(asLead)
	for _, want := range []string{"root context 200k, cache cold since", "its next turn re-reads all of it at the cache-write price", "`mesh reset --next"} {
		if !strings.Contains(h, want) {
			t.Errorf("cold hint %q lacks %q", h, want)
		}
	}
	// Big and warm: only a spawn (about to wait) says to reset; a very big one says it at a quiet point.
	w.size(t, rootNow, 200000, hour, 0)
	if h := hint(asLead); h != "" {
		t.Errorf("warm: %q", h)
	}
	spawn, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Repo: gitRepo(t), Slug: "fix", Tier: "mid", Task: "Direct task (no plan): fix it"}))
	if err != nil {
		t.Fatal(err)
	}
	if h := spawn.Header().Get(daemon.HintHeader); !strings.Contains(h, "you're about to wait on workers") {
		t.Errorf("about to wait: %q", h)
	}
	w.size(t, rootNow, 400000, hour, 0)
	if h := hint(asLead); !strings.Contains(h, "at your next quiet point") {
		t.Errorf("very big: %q", h)
	}
	// A worker's own commands carry none.
	worker := w.waitState(spawn.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	if h := hint(w.as(w.token(t, worker.Id))); h != "" {
		t.Errorf("a worker's hint: %q", h)
	}
}

func TestACompactingResetFailsWhenTheCompactionNeverStartsAndTypesNoBootstrap(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{}, func(c *daemon.AgentConfig) { c.Env["FAKECLAUDE_COMPACT"] = "none" })
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	w.size(t, w.agent(lead.Id), 200000, hour, 0)
	if _, err := asLead.ResetRoot(ctx, connect.NewRequest(&v1.ResetRootRequest{Compact: true})); err != nil {
		t.Fatal(err)
	}
	after := w.waitReset(t, lead.Id, "failed")
	if after.LastReset.Reason != "the CLI did not start compacting" || strings.Contains(w.typed(), "mesh reset` just") {
		t.Errorf("reason %q, typed %q", after.LastReset.Reason, w.typed())
	}
}
