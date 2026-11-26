package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/opencode"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

type fakeOpenCodeLaunch struct {
	Argv       []string `json:"argv"`
	Agent      string   `json:"agent"`
	Model      string   `json:"model"`
	Session    string   `json:"session"`
	Config     string   `json:"config"`
	Permission string   `json:"permission"`
	Autoupdate string   `json:"autoupdate"`
}

func readOpenCodeLaunches(t *testing.T, logPath string) []fakeOpenCodeLaunch {
	t.Helper()
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var out []fakeOpenCodeLaunch
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var rec fakeOpenCodeLaunch
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode launch log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func (w *reopenWorld) waitInfoDelivered(t *testing.T, agentID, substr string) {
	t.Helper()
	waitFor(t, "info doorbell "+substr, func() bool {
		msgs, _ := w.d.Store().Read(context.Background()).Inbox(agentID, true)
		for _, m := range msgs {
			if m.Type == v1.MessageType_MESSAGE_TYPE_INFO && strings.Contains(m.Body, substr) && m.DeliveredAt != nil {
				return true
			}
		}
		return false
	})
	w.waitState(agentID, v1.AgentState_AGENT_STATE_IDLE)
}

func (w *reopenWorld) opencodeRoot(t *testing.T, run string) (*v1.Agent, agentmeshv1connect.MeshServiceClient) {
	t.Helper()
	res, err := w.device.Mesh.SpawnAgent(context.Background(), connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run, Repo: gitRepo(t), Slug: "oc-lead", Role: v1.AgentRole_AGENT_ROLE_ROOT,
		Kind: "opencode", Tier: "light",
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	return a, w.as(w.token(t, a.Id))
}

func (w *reopenWorld) opencodeTurn(t *testing.T, a *v1.Agent, line string) *v1.Agent {
	t.Helper()
	w.typeInto(a, line+"\r")
	if line == "/hold" {
		w.waitState(a.Id, v1.AgentState_AGENT_STATE_WORKING)
		return w.agent(a.Id)
	}
	waitFor(t, "opencode turn "+line, func() bool {
		return strings.Contains(w.screen(t, a), "done: "+line) ||
			strings.Contains(w.screen(t, a), "edit denied") ||
			strings.Contains(w.screen(t, a), "wrote blocked.txt") ||
			strings.Contains(w.screen(t, a), "git push denied") ||
			strings.Contains(w.screen(t, a), "pushed")
	})
	return w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
}

func TestOpenCodeEveryReportReachesTheRootAndRingsItsDoorbell(t *testing.T) {
	w := startReopen(t, fastPass)
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.opencodeRoot(t, run.Id)
	worker := w.spawnUnder(t, asLead, "reporter", &v1.SpawnAgentRequest{Kind: "opencode", Tier: "light", Task: "investigate bug"})
	inboxMsgs, err := w.d.Store().Read(context.Background()).Inbox(worker.Id, true)
	if err != nil || len(inboxMsgs) == 0 || !strings.Contains(inboxMsgs[0].Body, "--body @") {
		t.Fatalf("opencode worker task lacks --body @file instruction: msgs=%v err=%v", inboxMsgs, err)
	}

	// WAIT is not a report: stored for root, rings no doorbell.
	wait := w.sendToParent(t, worker, v1.MessageType_MESSAGE_TYPE_WAIT)
	if wait.ToId != lead.Id {
		t.Fatalf("a wait went to %s, not the root", wait.ToId)
	}
	time.Sleep(time.Second)
	if screen := w.screen(t, lead); contains(screen, "📬") {
		t.Fatalf("the opencode root's screen shows a doorbell for a wait:\n%s", screen)
	}

	for _, typ := range []v1.MessageType{v1.MessageType_MESSAGE_TYPE_PLAN, v1.MessageType_MESSAGE_TYPE_QUESTION,
		v1.MessageType_MESSAGE_TYPE_DONE, v1.MessageType_MESSAGE_TYPE_FAILED, v1.MessageType_MESSAGE_TYPE_INFO} {
		m := w.sendToParent(t, worker, typ)
		if m.ToId != lead.Id {
			t.Fatalf("a %s went to %s, not the root", typ, m.ToId)
		}
		waitFor(t, "the opencode root's doorbell for a "+typ.String(), func() bool { return !w.deliveredAt(t, lead.Id, m.Id).IsZero() })
		w.waitState(lead.Id, v1.AgentState_AGENT_STATE_IDLE)
	}
}

func TestOpenCodeReportSentWhileRootIsWorkingRingsAtNextIdle(t *testing.T) {
	w := startReopen(t, fastPass)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.opencodeRoot(t, run.Id)
	worker := w.spawnUnder(t, asLead, "early", &v1.SpawnAgentRequest{Kind: "opencode", Tier: "light"})

	w.typeInto(lead, "/hold\r")
	w.waitState(lead.Id, v1.AgentState_AGENT_STATE_WORKING)
	m := w.sendToParent(t, worker, v1.MessageType_MESSAGE_TYPE_DONE)
	time.Sleep(1500 * time.Millisecond)
	if !w.deliveredAt(t, lead.Id, m.Id).IsZero() {
		t.Fatal("a doorbell was typed into the opencode root's working turn")
	}
	if peek, _ := asLead.ReadInbox(ctx, connect.NewRequest(&v1.ReadInboxRequest{Meta: meta(""), Peek: true})); len(peek.Msg.Messages) != 1 {
		t.Fatalf("the report is not in the opencode root's inbox: %v", peek)
	}
	w.typeInto(lead, "go on\r")
	waitFor(t, "the doorbell at the opencode root's next idle", func() bool { return !w.deliveredAt(t, lead.Id, m.Id).IsZero() })
	w.waitState(lead.Id, v1.AgentState_AGENT_STATE_IDLE)
}

func TestOpenCodeResumedPlanningWorkerSendsPlanAndApproveLiftsPermission(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "oc-argv.jsonl")
	w := startReopen(t, fastPass, func(c *daemon.AgentConfig) { c.Env["FAKEOPENCODE_ARGV_LOG"] = argvLog })
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.opencodeRoot(t, run.Id)
	worker := w.spawnUnder(t, asLead, "again", &v1.SpawnAgentRequest{Kind: "opencode", Tier: "light"})

	// First turn creates the session in opencode.db; pollOpenCode records SessionId on IDLE.
	worker = w.opencodeTurn(t, worker, "first turn")
	waitFor(t, "opencode worker session id recorded", func() bool {
		worker = w.agent(worker.Id)
		return worker.SessionId != "" && worker.State == v1.AgentState_AGENT_STATE_IDLE
	})
	sid := worker.SessionId
	w.sendToParent(t, worker, v1.MessageType_MESSAGE_TYPE_DONE)
	waitFor(t, "first report rung", func() bool {
		msgs, _ := w.d.Store().Read(ctx).Undelivered(lead.Id)
		return len(msgs) == 0
	})

	if _, err := asLead.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: worker.Id})); err != nil {
		t.Fatal(err)
	}
	if _, err := asLead.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Meta: meta(""), Agent: worker.Id, Task: "plan second feature", Plan: true, Tier: "strong",
	})); err != nil {
		t.Fatal(err)
	}
	back := w.waitReopened(t, worker.Id, worker.TerminalId)
	if back.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING || !back.DenyEdits || back.SessionId != sid {
		t.Fatalf("resumed as %v", back)
	}
	// Let ResumeAgent's task doorbell finish ringing before typing test commands into the PTY.
	waitFor(t, "resume task delivered", func() bool {
		msgs, _ := w.d.Store().Read(ctx).Inbox(worker.Id, false)
		return len(msgs) > 0 && msgs[len(msgs)-1].DeliveredAt != nil
	})
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)

	// While planning, OPENCODE_PERMISSION denies edits and git push.
	w.opencodeTurn(t, back, "/edit")
	if _, err := os.Stat(filepath.Join(back.Worktree, "blocked.txt")); err == nil {
		t.Fatal("planner wrote blocked.txt while planning OPENCODE_PERMISSION was active")
	}
	w.opencodeTurn(t, back, "/push")
	if !strings.Contains(w.screen(t, back), "git push denied") {
		t.Fatalf("planner /push was not denied: %q", w.screen(t, back))
	}

	plan := w.sendToParent(t, back, v1.MessageType_MESSAGE_TYPE_PLAN)
	waitFor(t, "the plan reaching the root", func() bool { return !w.deliveredAt(t, lead.Id, plan.Id).IsZero() })

	// Approve onto light (opencode/big-pickle at low) so the execution launch uses a distinct --agent and lifts permissions.
	ap, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: worker.Id, Tier: "light"}))
	if err != nil || ap.Msg.Reopen == nil || ap.Msg.Reopen.DenyEdits {
		t.Fatalf("approving opencode plan: %v, %v", ap, err)
	}
	after := w.waitReopened(t, worker.Id, back.TerminalId)
	if after.DenyEdits || after.SessionId != sid {
		t.Fatalf("expected DenyEdits lifted and same session %q, got %v", sid, after)
	}
	waitFor(t, "approval delivered", func() bool { return w.delivered(t, worker.Id, ap.Msg.Message.Id) })

	// After approve, OPENCODE_PERMISSION is lifted so /edit writes blocked.txt in the same session.
	w.opencodeTurn(t, after, "/edit")
	if _, err := os.Stat(filepath.Join(after.Worktree, "blocked.txt")); err != nil {
		t.Fatalf("approved worker blocked.txt not written: %v", err)
	}

	launches := readOpenCodeLaunches(t, argvLog)
	if len(launches) < 4 {
		t.Fatalf("expected at least 4 launches (root, worker, resume-plan, approve), got %d", len(launches))
	}
	planLaunch := launches[len(launches)-2]
	execLaunch := launches[len(launches)-1]
	if planLaunch.Session != sid || execLaunch.Session != sid {
		t.Fatalf("expected same session %q across reopen, got %q -> %q", sid, planLaunch.Session, execLaunch.Session)
	}
	if planLaunch.Permission == "" || execLaunch.Permission != "" {
		t.Fatalf("permission mismatch: planning=%q exec=%q", planLaunch.Permission, execLaunch.Permission)
	}
	if planLaunch.Agent == execLaunch.Agent || planLaunch.Autoupdate != "1" || execLaunch.Autoupdate != "1" || execLaunch.Config == "" {
		t.Fatalf("expected distinct --agent, OPENCODE_DISABLE_AUTOUPDATE=1, OPENCODE_CONFIG_CONTENT: plan=%+v exec=%+v", planLaunch, execLaunch)
	}
}

func TestOpenCodePlaceholderAndWrappedStatusNeverHoldADoorbellAcrossWidths(t *testing.T) {
	typingQuiet := 3 * time.Second
	w := startReopen(t, fastPass, func(c *daemon.AgentConfig) {
		c.Env["FAKEOPENCODE_PLACEHOLDER"] = "1"
		c.Env["FAKEOPENCODE_STATUS_WRAP"] = "1"
		c.TypingQuiet = typingQuiet
	})
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.opencodeRoot(t, run.Id)
	worker := w.spawnUnder(t, asLead, "fresh", &v1.SpawnAgentRequest{Kind: "opencode", Tier: "light"})

	screen := w.screen(t, worker)
	if want := "Ask anything\u2026"; !contains(screen, want) || !contains(screen, "DeepSeek") {
		t.Fatalf("fakeopencode drew no placeholder/status row: %q", screen)
	}

	// Across 50..90 column panes (including wrapped/padded status rows), neither TASK nor INFO waits on TypingQuiet.
	for i, cols := range []uint32{50, 62, 70, 80, 90} {
		// Resize via read-only attach with an initial Attach size? Only input clients resize; open briefly with Resize then close.
		st := w.openTerminal(ctx, worker, true, &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_TAIL, Cols: cols, Rows: 30})
		for {
			f, err := st.Recv(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if f.GetReplayDone() != nil {
				break
			}
		}
		if err := st.Send(ctx, &termv1.TermFrame{Frame: &termv1.TermFrame_Resize{Resize: &termv1.Resize{Cols: cols, Rows: 30}}}); err != nil {
			t.Fatal(err)
		}
		st.Close()
		// Wait briefly for detached client cleanup, then force prompt redraw at the new width.
		w.typeInto(worker, "\r")
		w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)

		typ := v1.MessageType_MESSAGE_TYPE_TASK
		if i%2 == 1 {
			typ = v1.MessageType_MESSAGE_TYPE_INFO
		}
		sent := time.Now()
		res, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: worker.Id, Type: typ, Body: fmt.Sprintf("hello-%d", cols)}))
		if err != nil {
			t.Fatal(err)
		}
		var rang time.Time
		waitFor(t, fmt.Sprintf("doorbell at %d cols over placeholder/status", cols), func() bool {
			rang = w.deliveredAt(t, worker.Id, res.Msg.Message.Id)
			return !rang.IsZero()
		})
		if held := rang.Sub(sent); held > typingQuiet/2 {
			t.Errorf("%d cols: %s held %s by placeholder/status", cols, typ, held)
		}
		w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	}

	// An attached client with input rights typing holds the doorbell for TypingQuiet.
	user := w.attach(t, worker, true)
	defer user.Close()
	user.Input(ctx, []byte("x\x7f"))
	waitFor(t, "keystroke counted", func() bool { return w.lastClientInput(t, worker) })
	res, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: worker.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "while typing"}))
	if err != nil {
		t.Fatal(err)
	}
	typed := w.lastClientInputAt(t, worker)
	var rang time.Time
	waitFor(t, "doorbell after user stopped typing", func() bool {
		rang = w.deliveredAt(t, worker.Id, res.Msg.Message.Id)
		return !rang.IsZero()
	})
	if held := rang.Sub(typed); held < typingQuiet-200*time.Millisecond {
		t.Errorf("doorbell rang %s after keystroke, inside %s quiet window", held, typingQuiet)
	}
}

func TestOpenCodeLimitsHoldModelsAndRotateInSameSession(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "oc-limits.jsonl")
	w := startReopen(t, fastPass, func(c *daemon.AgentConfig) { c.Env["FAKEOPENCODE_ARGV_LOG"] = argvLog })
	ctx := context.Background()
	// Chain starts on concrete unavailable model -> free expands newest-first (big-pickle, deepseek, flint, beam, tidepool).
	w.set(t, "tiers.opencode.mid.model", []any{"openai/gpt-5.2", "free"})
	run := startRun(t, w.device.Mesh, "")

	sp, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: gitRepo(t), Slug: "oc-lim", Kind: opencode.Kind, Tier: "mid",
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(sp.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	if a.Model != "openai/gpt-5.2" {
		t.Fatalf("started on %q, want openai/gpt-5.2", a.Model)
	}
	oldTerm := a.TerminalId

	// 1. Model unavailable (statusCode 400): limitPass switches to newest free model opencode/big-pickle in the same -s session.
	w.typeInto(a, "/unavailable\r")
	re1 := w.waitReopened(t, a.Id, oldTerm)
	if re1.Model != "opencode/big-pickle" || re1.SessionId == "" {
		t.Fatalf("after /unavailable: model=%q session=%q, want opencode/big-pickle", re1.Model, re1.SessionId)
	}
	sid := re1.SessionId
	w.waitInfoDelivered(t, re1.Id, "big-pickle")

	// Held model (openai/gpt-5.2) is skipped when spawning a new agent on the same tier.
	next, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: gitRepo(t), Slug: "oc-skip", Kind: opencode.Kind, Tier: "mid",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if next.Msg.Agent.Model != "opencode/big-pickle" {
		t.Fatalf("new spawn picked %q, did not skip held openai/gpt-5.2", next.Msg.Agent.Model)
	}

	// 2. Free usage limit triggers rotation to next expanded free model (opencode/deepseek-v4.1-flash-free).
	oldTerm = re1.TerminalId
	w.typeInto(re1, "/limit-free\r")
	re2 := w.waitReopened(t, re1.Id, oldTerm)
	if re2.Model != "opencode/deepseek-v4.1-flash-free" || re2.SessionId != sid {
		t.Fatalf("after /limit-free: model=%q session=%q, want deepseek in %q", re2.Model, re2.SessionId, sid)
	}
	w.waitInfoDelivered(t, re2.Id, "deepseek-v4.1-flash-free")

	// 3. 429 triggers rotation to opencode/flint-40b-free.
	oldTerm = re2.TerminalId
	w.typeInto(re2, "/limit-429\r")
	re3 := w.waitReopened(t, re2.Id, oldTerm)
	if re3.Model != "opencode/flint-40b-free" || re3.SessionId != sid {
		t.Fatalf("after /limit-429: model=%q session=%q, want flint in %q", re3.Model, re3.SessionId, sid)
	}
	w.waitInfoDelivered(t, re3.Id, "flint-40b-free")

	// 4. With limits.auto=off, /limit-pane moves agent to LIMITED until explicit SwitchAgent --next -> beam-free.
	w.set(t, "limits.auto", "off")
	oldTerm = re3.TerminalId
	w.typeInto(re3, "/limit-pane\r")
	w.waitState(re3.Id, v1.AgentState_AGENT_STATE_LIMITED)
	sw, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: re3.Id, Next: true, Why: "pane retry"}))
	if err != nil || sw.Msg.Reopen == nil || sw.Msg.Reopen.Model != "opencode/beam-free" {
		t.Fatalf("SwitchAgent --next after pane limit: %v, %v", sw, err)
	}
	re4 := w.waitReopened(t, re3.Id, oldTerm)
	if re4.Model != "opencode/beam-free" || re4.SessionId != sid {
		t.Fatalf("after switch --next: model=%q session=%q", re4.Model, re4.SessionId)
	}
	w.waitInfoDelivered(t, re4.Id, "beam-free")
	// Normal turn still runs in the switched session without re-triggering earlier limits.
	re4 = w.opencodeTurn(t, re4, "still works")
	if re4.State != v1.AgentState_AGENT_STATE_IDLE || re4.Model != "opencode/beam-free" {
		t.Fatalf("unexpected state after follow-up turn: %+v", re4)
	}

	launches := readOpenCodeLaunches(t, argvLog)
	agentsSeen := map[string]bool{}
	for _, l := range launches {
		if l.Session == sid {
			if agentsSeen[l.Agent] {
				t.Errorf("reused --agent %q across rotation", l.Agent)
			}
			agentsSeen[l.Agent] = true
		}
	}
	if len(agentsSeen) < 4 {
		t.Errorf("expected at least 4 distinct --agent flags on resumed session %q, got %d (%v)", sid, len(agentsSeen), agentsSeen)
	}
}

func TestOpenCodeReviewRoundAndHeldModelFallback(t *testing.T) {
	w := startReviews(t)
	ctx := context.Background()
	w.pr.push(t, "the feature")

	// Small auto defaults to [["opencode", "agy"]]: picks opencode when installed, with --body @path instruction.
	res := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "auto", Body: "check limits"})
	if len(res.Agents) != 1 || res.Agents[0].Kind != opencode.Kind {
		t.Fatalf("expected small auto to pick opencode, got %v", res.Agents)
	}
	if !strings.Contains(w.task(t, res.Agents[0].Id), "--body @") {
		t.Errorf("opencode reviewer task lacks --body @ instruction: %s", w.task(t, res.Agents[0].Id))
	}
	w.say(t, res.Agents[0].Id, v1.MessageType_MESSAGE_TYPE_DONE, "opencode: looks good")
	waitFor(t, "round 1 finished", func() bool { return len(w.answers(t)) == 1 })

	// When every model of OpenCode's mid tier is held in model_limits, auto skips opencode with a reset note and falls back to agy.
	until := time.Now().Add(2 * time.Hour)
	if err := w.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error {
		for _, m := range []string{
			"opencode/big-pickle",
			"opencode/deepseek-v4.1-flash-free",
			"opencode/flint-40b-free",
			"opencode/beam-free",
			"opencode/tidepool-alpha-free",
		} {
			if err := tx.PutModelLimit(opencode.Kind, m, until); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Run in a fresh run at the same PR head so round2 picks slots from scratch.
	run2 := startRun(t, w.device.Mesh, "")
	_, asRoot2 := w.root(t, run2.Id)
	res2, err := asRoot2.Ask(ctx, connect.NewRequest(&v1.AskRequest{
		Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "auto",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Msg.Agents) != 1 || res2.Msg.Agents[0].Kind != "agy" {
		t.Fatalf("expected held opencode to fall back to agy, got %v (notes=%v)", res2.Msg.Agents, res2.Msg.Notes)
	}
	if !strings.Contains(strings.Join(res2.Msg.Notes, "\n"), "are held until") {
		t.Errorf("expected reset note for held opencode models, got %v", res2.Msg.Notes)
	}
}

func TestOpenCodeAbsentBinaryFallsBackToAgyInReviewAuto(t *testing.T) {
	w := startReviews(t, func(c *daemon.AgentConfig) {
		delete(c.Binaries, opencode.Kind)
	})
	w.pr.push(t, "the feature")
	res := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "auto"})
	if len(res.Agents) != 1 || res.Agents[0].Kind != "agy" {
		t.Fatalf("expected absent opencode to fall back to agy, got %v", res.Agents)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "opencode is not installed here") {
		t.Errorf("expected missing-binary note for opencode, got %v", res.Notes)
	}
}

func TestOpenCodeRootWithNoModelLaunchesWithoutModelFlag(t *testing.T) {
	argvLog := filepath.Join(t.TempDir(), "oc-root.jsonl")
	w := startReopen(t, fastPass, func(c *daemon.AgentConfig) { c.Env["FAKEOPENCODE_ARGV_LOG"] = argvLog })
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")

	sp, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: gitRepo(t), Slug: "oc-default", Role: v1.AgentRole_AGENT_ROLE_ROOT, Kind: opencode.Kind,
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(sp.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	if a.Model != "" {
		t.Fatalf("root with no tier/model got model %q", a.Model)
	}
	launches := readOpenCodeLaunches(t, argvLog)
	if len(launches) != 1 {
		t.Fatalf("expected 1 launch, got %d", len(launches))
	}
	for _, arg := range launches[0].Argv {
		if arg == "-m" || arg == "--model" {
			t.Fatalf("root launch with no model must omit -m: %v", launches[0].Argv)
		}
	}
	if strings.Contains(launches[0].Config, `"model"`) {
		t.Fatalf("root launch config must omit model key: %s", launches[0].Config)
	}
}

func TestOpenCodePaneBannerFiresAgainAfterSwitchBack(t *testing.T) {
	w := startReopen(t, fastPass)
	ctx := context.Background()
	w.set(t, "limits.auto", "off")
	run := startRun(t, w.device.Mesh, "")

	sp, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: gitRepo(t), Slug: "oc-back", Kind: opencode.Kind, Tier: "mid",
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(sp.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	origModel := a.Model

	// First pane limit hit on origModel -> LIMITED.
	w.typeInto(a, "/limit-pane\r")
	a = w.waitState(a.Id, v1.AgentState_AGENT_STATE_LIMITED)

	// Switch to next model, then switch back to origModel.
	oldTerm := a.TerminalId
	if _, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: a.Id, Next: true})); err != nil {
		t.Fatal(err)
	}
	re1 := w.waitReopened(t, a.Id, oldTerm)
	w.waitInfoDelivered(t, re1.Id, re1.Model)

	oldTerm = re1.TerminalId
	if _, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: re1.Id, Model: origModel})); err != nil {
		t.Fatal(err)
	}
	re2 := w.waitReopened(t, re1.Id, oldTerm)
	w.waitInfoDelivered(t, re2.Id, "now on "+origModel)

	// The exact same pane banner on origModel must fire RateLimited again.
	w.typeInto(re2, "/limit-pane\r")
	w.waitState(re2.Id, v1.AgentState_AGENT_STATE_LIMITED)
}

func TestOpenCodeHeldSkippedInAllExplicitAndReReview(t *testing.T) {
	w := startReviews(t)
	ctx := context.Background()
	w.pr.push(t, "round 1")

	// Round 1 uses opencode (small auto default).
	r1 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "auto"})
	if len(r1.Agents) != 1 || r1.Agents[0].Kind != opencode.Kind {
		t.Fatalf("round 1 picked %v, want opencode", r1.Agents)
	}
	w.say(t, r1.Agents[0].Id, v1.MessageType_MESSAGE_TYPE_DONE, "round 1 ok")
	waitFor(t, "round 1 finished", func() bool { return len(w.answers(t)) == 1 })

	// Hold every expanded opencode free model in mid tier.
	soonest := time.Now().Add(45 * time.Minute).Truncate(time.Second)
	later := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	if err := w.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error {
		for i, m := range []string{
			"opencode/big-pickle",
			"opencode/deepseek-v4.1-flash-free",
			"opencode/flint-40b-free",
			"opencode/beam-free",
			"opencode/tidepool-alpha-free",
		} {
			u := later
			if i == 2 {
				u = soonest
			}
			if err := tx.PutModelLimit(opencode.Kind, m, u); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wantReset := soonest.UTC().Format(time.RFC3339)

	// 1. Re-review on a new head skips held opencode with a reset note and falls back to agy.
	w.pr.push(t, "round 2 fix")
	r2 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "auto"})
	if len(r2.Agents) != 1 || r2.Agents[0].Kind != "agy" {
		t.Fatalf("re-review with held opencode picked %v, want agy (notes=%v)", r2.Agents, r2.Notes)
	}
	if !strings.Contains(strings.Join(r2.Notes, "\n"), wantReset) {
		t.Errorf("re-review notes missing soonest reset %s: %v", wantReset, r2.Notes)
	}

	// 2. mode == "all" skips held opencode with a note, asking only claude + agy.
	runAll := startRun(t, w.device.Mesh, "")
	_, asRootAll := w.root(t, runAll.Id)
	resAll, err := asRootAll.Ask(ctx, connect.NewRequest(&v1.AskRequest{
		Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "all",
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range resAll.Msg.Agents {
		if a.Kind == opencode.Kind {
			t.Fatalf("mode=all picked held opencode: %v", resAll.Msg.Agents)
		}
	}
	if !strings.Contains(strings.Join(resAll.Msg.Notes, "\n"), wantReset) {
		t.Errorf("mode=all notes missing reset %s: %v", wantReset, resAll.Msg.Notes)
	}

	// 3. Explicit list "opencode,claude" skips held opencode with a note and asks claude; "opencode" alone errors.
	runExp := startRun(t, w.device.Mesh, "")
	_, asRootExp := w.root(t, runExp.Id)
	resExp, err := asRootExp.Ask(ctx, connect.NewRequest(&v1.AskRequest{
		Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "opencode,claude",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resExp.Msg.Agents) != 1 || resExp.Msg.Agents[0].Kind != "claude" {
		t.Fatalf("explicit opencode,claude picked %v, want [claude]", resExp.Msg.Agents)
	}
	if !strings.Contains(strings.Join(resExp.Msg.Notes, "\n"), wantReset) {
		t.Errorf("explicit notes missing reset %s: %v", wantReset, resExp.Msg.Notes)
	}

	_, err = asRootExp.Ask(ctx, connect.NewRequest(&v1.AskRequest{
		Repo: w.pr.repo, Review: "7", Size: "small", Tier: "mid", Reviewers: "opencode",
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), wantReset) {
		t.Fatalf("explicit held opencode alone = %v, want FailedPrecondition naming %s", err, wantReset)
	}
}

func TestSwitchAgentNextChainChangedAndAllHeld(t *testing.T) {
	w := startReopen(t, fastPass)
	ctx := context.Background()
	w.set(t, "limits.auto", "off")
	w.set(t, "tiers.opencode.mid.model", []any{"openai/gpt-5.2", "free"})
	run := startRun(t, w.device.Mesh, "")

	sp, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: gitRepo(t), Slug: "oc-allheld", Kind: opencode.Kind, Tier: "mid",
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(sp.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	a = w.opencodeTurn(t, a, "learn session")
	waitFor(t, "session learned", func() bool {
		a = w.agent(a.Id)
		return a.SessionId != "" && a.State == v1.AgentState_AGENT_STATE_IDLE
	})

	// Drop current model openai/gpt-5.2 from the tier chain so SwitchAgent --next hits i < 0.
	w.set(t, "tiers.opencode.mid.model", []any{"free"})
	soonest := time.Now().Add(30 * time.Minute).Truncate(time.Second)
	later := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	if err := w.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error {
		for i, m := range []string{
			"opencode/big-pickle",
			"opencode/deepseek-v4.1-flash-free",
			"opencode/flint-40b-free",
			"opencode/beam-free",
			"opencode/tidepool-alpha-free",
		} {
			u := later
			if i == 3 {
				u = soonest
			}
			if err := tx.PutModelLimit(opencode.Kind, m, u); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Every model in the chain is held: SwitchAgent --next returns an error naming the soonest reset and does not reopen.
	_, err = w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: a.Id, Next: true}))
	wantReset := soonest.UTC().Format(time.RFC3339)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), wantReset) {
		t.Fatalf("all-held SwitchAgent --next = %v, want FailedPrecondition naming %s", err, wantReset)
	}
	if cur := w.agent(a.Id); cur.PendingReopen != nil || cur.Model != "openai/gpt-5.2" {
		t.Fatalf("all-held SwitchAgent --next mutated agent: %+v", cur)
	}

	// Non-OpenCode kinds with i < 0 keep chain[0] even when held.
	cl, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: gitRepo(t), Slug: "cl-chain", Kind: "claude", Tier: "mid",
	}))
	if err != nil {
		t.Fatal(err)
	}
	ca := w.waitState(cl.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.set(t, "tiers.claude.mid.model", []any{"opus", "haiku"})
	if err := w.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error {
		return tx.PutModelLimit("claude", "opus", later)
	}); err != nil {
		t.Fatal(err)
	}
	swCl, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: ca.Id, Next: true}))
	if err != nil || swCl.Msg.Reopen == nil || swCl.Msg.Reopen.Model != "opus" {
		t.Fatalf("claude SwitchAgent --next with i<0 = %v, %v; want chain[0] (opus)", swCl, err)
	}
}
