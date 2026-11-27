package daemon_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/copilot"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
	"github.com/Luisotee/agent-mesh/daemon/internal/termclient"
	"github.com/Luisotee/agent-mesh/daemon/internal/testutil/shortdir"
)

type fakeCopilotLaunch struct {
	Argv    []string `json:"argv"`
	Session string   `json:"session"`
	Model   string   `json:"model"`
	Deny    []string `json:"deny"`
	Home    string   `json:"home"`
	Token   string   `json:"token"`
	Note    string   `json:"note"`
}

func readCopilotLog(t *testing.T, path string) []fakeCopilotLaunch {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []fakeCopilotLaunch
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var rec fakeCopilotLaunch
		if line != "" && json.Unmarshal([]byte(line), &rec) == nil {
			out = append(out, rec)
		}
	}
	return out
}

func copilotLaunches(t *testing.T, path string) []fakeCopilotLaunch {
	var out []fakeCopilotLaunch
	for _, r := range readCopilotLog(t, path) {
		if r.Note == "" {
			out = append(out, r)
		}
	}
	return out
}

// copilotTweak makes the fake Copilot the daemon's `copilot`: its state folder (where it writes its events) and
// a log of its launches. The returned log and home are the test's to read.
func copilotTweak(t *testing.T, env map[string]string) (tweak func(*daemon.AgentConfig), argvLog, home string) {
	t.Helper()
	dir, err := shortdir.Make("cop-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	argvLog, home = filepath.Join(dir, "argv.jsonl"), filepath.Join(dir, "copilot-home")
	return func(c *daemon.AgentConfig) {
		c.Binaries["copilot"] = filepath.Join(bins, "copilot")
		c.Env["COPILOT_HOME"] = home
		c.Env["FAKECOPILOT_ARGV_LOG"] = argvLog
		for k, v := range env {
			c.Env[k] = v
		}
	}, argvLog, home
}

func (w *reopenWorld) copilotRoot(t *testing.T, run string) (*v1.Agent, agentmeshv1connect.MeshServiceClient) {
	t.Helper()
	res, err := w.device.Mesh.SpawnAgent(context.Background(), connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run, Repo: gitRepo(t), Slug: "cp-lead", Role: v1.AgentRole_AGENT_ROLE_ROOT, Kind: "copilot", Tier: "light"}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	return a, w.as(w.token(t, a.Id))
}

// copilotTurn types a line into a Copilot agent and waits for its turn to end.
func (w *reopenWorld) copilotTurn(t *testing.T, a *v1.Agent, line string) *v1.Agent {
	t.Helper()
	w.typeInto(a, line+"\r")
	waitFor(t, "copilot turn "+line, func() bool {
		s := w.screen(t, a)
		return strings.Contains(s, "done: "+line) || strings.Contains(s, "edit denied") || strings.Contains(s, "wrote blocked.txt") ||
			strings.Contains(s, "git push denied") || strings.Contains(s, "pushed")
	})
	return w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
}

func TestCopilotEveryReportReachesTheRootAndRingsItsDoorbell(t *testing.T) {
	tweak, argvLog, _ := copilotTweak(t, nil)
	w := startReopen(t, fastPass, tweak)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.copilotRoot(t, run.Id)
	worker := w.spawnUnder(t, asLead, "reporter", &v1.SpawnAgentRequest{Kind: "copilot", Tier: "light", Task: "investigate bug"})
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)

	launches := copilotLaunches(t, argvLog)
	if len(launches) != 2 || launches[1].Session == "" || launches[1].Model != "auto" ||
		!slices.Contains(launches[1].Argv, "--auto-tier") || len(launches[1].Deny) != 0 {
		t.Fatalf("copilot worker launch: %+v", launches)
	}

	// A wait is not a report: stored, no doorbell.
	wait := w.sendToParent(t, worker, v1.MessageType_MESSAGE_TYPE_WAIT)
	if wait.ToId != lead.Id {
		t.Fatalf("a wait went to %s, not the root", wait.ToId)
	}
	time.Sleep(time.Second)
	if screen := w.screen(t, lead); contains(screen, "📬") {
		t.Fatalf("the copilot root's screen shows a doorbell for a wait:\n%s", screen)
	}
	for _, typ := range []v1.MessageType{v1.MessageType_MESSAGE_TYPE_PLAN, v1.MessageType_MESSAGE_TYPE_QUESTION,
		v1.MessageType_MESSAGE_TYPE_DONE, v1.MessageType_MESSAGE_TYPE_FAILED, v1.MessageType_MESSAGE_TYPE_INFO} {
		m := w.sendToParent(t, worker, typ)
		waitFor(t, "the copilot root's doorbell for a "+typ.String(), func() bool { return !w.deliveredAt(t, lead.Id, m.Id).IsZero() })
		w.waitState(lead.Id, v1.AgentState_AGENT_STATE_IDLE)
	}

	// A report sent while the root is mid-turn waits for its next idle: the turn is read from Copilot's events.
	readInbox(t, asLead)
	w.typeInto(lead, "/hold\r")
	w.waitState(lead.Id, v1.AgentState_AGENT_STATE_WORKING)
	m := w.sendToParent(t, worker, v1.MessageType_MESSAGE_TYPE_DONE)
	time.Sleep(1500 * time.Millisecond)
	if !w.deliveredAt(t, lead.Id, m.Id).IsZero() {
		t.Fatal("a doorbell was typed into the copilot root's working turn")
	}
	if peek, _ := asLead.ReadInbox(ctx, connect.NewRequest(&v1.ReadInboxRequest{Meta: meta(""), Peek: true})); len(peek.Msg.Messages) != 1 {
		t.Fatalf("the report is not in the copilot root's inbox: %v", peek)
	}
	w.typeInto(lead, "go on\r")
	waitFor(t, "the doorbell at the copilot root's next idle", func() bool { return !w.deliveredAt(t, lead.Id, m.Id).IsZero() })
	w.waitState(lead.Id, v1.AgentState_AGENT_STATE_IDLE)
}

func TestCopilotGutterBoxAndPlaceholderNeverHoldADoorbellButTypingDoes(t *testing.T) {
	typingQuiet := 3 * time.Second
	tweak, _, _ := copilotTweak(t, map[string]string{"FAKECOPILOT_PLACEHOLDER": "1"})
	w := startReopen(t, fastPass, tweak, func(c *daemon.AgentConfig) { c.TypingQuiet = typingQuiet })
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.copilotRoot(t, run.Id)
	worker := w.spawnUnder(t, asLead, "fresh", &v1.SpawnAgentRequest{Kind: "copilot", Tier: "light"})
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)

	screen := w.screen(t, worker)
	if !contains(screen, "╻▄▄") || !contains(screen, "┃") || !contains(screen, "╹▀▀") || !contains(screen, "Type @ to mention files") {
		t.Fatalf("fakecopilot drew no gutter box with placeholder: %q", screen)
	}
	for i, typ := range []v1.MessageType{v1.MessageType_MESSAGE_TYPE_TASK, v1.MessageType_MESSAGE_TYPE_INFO} {
		sent := time.Now()
		res, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: worker.Id, Type: typ, Body: "hello " + string(rune('a'+i))}))
		if err != nil {
			t.Fatal(err)
		}
		var rang time.Time
		waitFor(t, "the doorbell over the placeholder", func() bool {
			rang = w.deliveredAt(t, worker.Id, res.Msg.Message.Id)
			return !rang.IsZero()
		})
		if held := rang.Sub(sent); held > typingQuiet/2 {
			t.Errorf("%s was held %s by the placeholder text", typ, held)
		}
		w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	}

	// A client with input rights typing holds the doorbell for TypingQuiet.
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
	waitFor(t, "the doorbell after the user stopped typing", func() bool {
		rang = w.deliveredAt(t, worker.Id, res.Msg.Message.Id)
		return !rang.IsZero()
	})
	if held := rang.Sub(typed); held < typingQuiet-200*time.Millisecond {
		t.Errorf("the doorbell rang %s after a keystroke, inside %s quiet window", held, typingQuiet)
	}
}

func TestCopilotKeepsItsSessionAndLiftsDenyToolOnApprove(t *testing.T) {
	tweak, argvLog, _ := copilotTweak(t, nil)
	w := startReopen(t, fastPass, tweak)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.copilotRoot(t, run.Id)
	worker := w.spawnUnder(t, asLead, "again", &v1.SpawnAgentRequest{Kind: "copilot", Tier: "light"})
	worker = w.copilotTurn(t, w.agent(worker.Id), "first turn")
	sid := worker.SessionId
	if sid == "" {
		t.Fatal("the session id chosen at launch was not recorded")
	}
	w.sendToParent(t, worker, v1.MessageType_MESSAGE_TYPE_DONE)
	waitFor(t, "first report rung", func() bool {
		msgs, _ := w.d.Store().Read(ctx).Undelivered(lead.Id)
		return len(msgs) == 0
	})
	if _, err := asLead.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: worker.Id})); err != nil {
		t.Fatal(err)
	}
	if _, err := asLead.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Meta: meta(""), Agent: worker.Id, Task: "plan second feature", Plan: true, Tier: "strong"})); err != nil {
		t.Fatal(err)
	}
	back := w.waitReopened(t, worker.Id, worker.TerminalId)
	if back.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING || !back.DenyEdits || back.SessionId != sid {
		t.Fatalf("resumed as %v", back)
	}
	waitFor(t, "resume task delivered", func() bool {
		msgs, _ := w.d.Store().Read(ctx).Inbox(worker.Id, false)
		return len(msgs) > 0 && msgs[len(msgs)-1].DeliveredAt != nil
	})
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)

	// Planning: `write` and publishing are denied, enforced by the CLI.
	w.copilotTurn(t, back, "/edit")
	if _, err := os.Stat(filepath.Join(back.Worktree, "blocked.txt")); err == nil {
		t.Fatal("the planner wrote blocked.txt under --deny-tool")
	}
	w.copilotTurn(t, back, "/push")
	if !strings.Contains(w.screen(t, back), "git push denied") {
		t.Fatalf("the planner's git push was not denied: %q", w.screen(t, back))
	}
	plan := w.sendToParent(t, back, v1.MessageType_MESSAGE_TYPE_PLAN)
	waitFor(t, "the plan reaching the root", func() bool { return !w.deliveredAt(t, lead.Id, plan.Id).IsZero() })

	ap, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: worker.Id, Tier: "light"}))
	if err != nil || ap.Msg.Reopen == nil || ap.Msg.Reopen.DenyEdits {
		t.Fatalf("approving the copilot plan: %v, %v", ap, err)
	}
	after := w.waitReopened(t, worker.Id, back.TerminalId)
	if after.DenyEdits || after.SessionId != sid {
		t.Fatalf("expected DenyEdits lifted in the same session %q, got %v", sid, after)
	}
	waitFor(t, "approval delivered", func() bool { return w.delivered(t, worker.Id, ap.Msg.Message.Id) })
	w.copilotTurn(t, after, "/edit")
	if _, err := os.Stat(filepath.Join(after.Worktree, "blocked.txt")); err != nil {
		t.Fatalf("the approved worker could not write: %v", err)
	}

	launches := copilotLaunches(t, argvLog)
	plan1, exec1 := launches[len(launches)-2], launches[len(launches)-1]
	if plan1.Session != sid || exec1.Session != sid {
		t.Fatalf("the session changed across reopens: %q -> %q (want %q)", plan1.Session, exec1.Session, sid)
	}
	if !slices.Contains(plan1.Deny, "write") || !slices.Contains(plan1.Deny, "shell(git push:*)") || len(exec1.Deny) != 0 {
		t.Fatalf("deny rules: planning=%v executing=%v", plan1.Deny, exec1.Deny)
	}
	for _, a := range plan1.Argv {
		if a == "--session-id" {
			goto ok
		}
	}
	t.Fatalf("no --session-id in %v", plan1.Argv)
ok:
}

func TestCopilotFolderTrustDialogIsAnsweredBeforeAnyDoorbell(t *testing.T) {
	tweak, argvLog, _ := copilotTweak(t, map[string]string{"FAKECOPILOT_TRUST": "1"})
	w := startReopen(t, fastPass, tweak)
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.copilotRoot(t, run.Id)
	worker := w.spawnUnder(t, asLead, "trusting", &v1.SpawnAgentRequest{Kind: "copilot", Tier: "light", Task: "look around"})
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	waitFor(t, "the task doorbell", func() bool {
		msgs, _ := w.d.Store().Read(context.Background()).Inbox(worker.Id, true)
		return len(msgs) > 0 && msgs[0].DeliveredAt != nil
	})
	trusted := 0
	for _, r := range readCopilotLog(t, argvLog) {
		if r.Note == "trust answered" {
			trusted++
		}
	}
	if trusted != 2 { // the root's and the worker's
		t.Fatalf("the trust dialog was answered %d times, want 2", trusted)
	}
}

// A trust dialog the Enter presses do not answer leaves the agent needing input, with the reason, instead of
// starting for good; answered by hand, it goes on to idle.
func TestCopilotAnUnansweredTrustDialogNeedsInputUntilItIsAnswered(t *testing.T) {
	tweak, _, _ := copilotTweak(t, map[string]string{"FAKECOPILOT_TRUST": "stuck"})
	w := startReopen(t, fastPass, tweak)
	run := startRun(t, w.device.Mesh, "")
	res, err := w.device.Mesh.SpawnAgent(context.Background(), connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: gitRepo(t), Slug: "stuck", Kind: "copilot", Tier: "light", Task: "look around"}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_NEEDS_INPUT)
	if !strings.Contains(a.StateDetail, "folder trust dialog not answered") {
		t.Fatalf("detail %q", a.StateDetail)
	}
	tc, err := termclient.New(statedir.Paths{Dir: w.state}.Term())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tc.API.WriteTerminal(context.Background(), connect.NewRequest(&termv1.WriteTerminalRequest{Id: a.TerminalId,
		Data: []byte("trust\r")})); err != nil {
		t.Fatal(err)
	}
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
}

func (w *reopenWorld) modelHold(t *testing.T, kind string) time.Time {
	t.Helper()
	until, _, err := w.d.Store().Read(context.Background()).ModelHold(kind, "auto", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return until
}

func TestCopilotLimitsHoldTheKindFromQuotaPolicyAndScreen(t *testing.T) {
	for _, tc := range []struct {
		line     string
		min, max time.Duration
	}{
		{"/402", 23 * time.Hour, 25 * time.Hour},         // the monthly quota: a day, no reset named
		{"/policy", 50 * time.Minute, 70 * time.Minute},  // the 403 refusal: an hour
		{"/limit", 100 * time.Minute, 130 * time.Minute}, // a rate limit that resets in 2 h
	} {
		t.Run(tc.line, func(t *testing.T) {
			tweak, _, _ := copilotTweak(t, nil)
			w := startReopen(t, fastPass, tweak)
			run := startRun(t, w.device.Mesh, "")
			_, asLead := w.copilotRoot(t, run.Id)
			worker := w.spawnUnder(t, asLead, "limited", &v1.SpawnAgentRequest{Kind: "copilot", Tier: "light"})
			w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
			w.typeInto(worker, tc.line+"\r")
			w.waitState(worker.Id, v1.AgentState_AGENT_STATE_LIMITED)
			held := time.Until(w.modelHold(t, "copilot"))
			if held < tc.min || held > tc.max {
				t.Fatalf("%s holds copilot for %s, want %s..%s", tc.line, held, tc.min, tc.max)
			}
		})
	}
}

func TestCopilotAnsweringAgainLiftsTheHoldAndTheLimit(t *testing.T) {
	tweak, _, _ := copilotTweak(t, nil)
	w := startReopen(t, fastPass, tweak)
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.copilotRoot(t, run.Id)
	worker := w.spawnUnder(t, asLead, "recovers", &v1.SpawnAgentRequest{Kind: "copilot", Tier: "light"})
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.typeInto(worker, "/402\r")
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_LIMITED)
	if w.modelHold(t, "copilot").IsZero() {
		t.Fatal("a 402 left no hold")
	}
	time.Sleep(50 * time.Millisecond)
	// The quota is topped up: the next prompt is answered.
	w.typeInto(worker, "back again\r")
	waitFor(t, "the answer after the 402", func() bool { return w.agent(worker.Id).State == v1.AgentState_AGENT_STATE_IDLE })
	waitFor(t, "the hold lifted", func() bool { return w.modelHold(t, "copilot").IsZero() })
}

func TestCopilotAsAReviewerTakesASlotUntilItsHoldAndIsNotAskedAgain(t *testing.T) {
	tweak, _, _ := copilotTweak(t, nil)
	w := startReviews(t, tweak)
	w.set(t, "review.rounds.large", 3)
	w.pr.push(t, "the feature")
	round1 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "large", Tier: "mid", Reviewers: "claude,copilot"})
	if len(round1.Agents) != 2 {
		t.Fatalf("round 1: %v", round1)
	}
	var cop *v1.Agent
	for _, a := range round1.Agents {
		w.quiet(t, a.Id)
		if a.Kind == "copilot" {
			cop = a
		}
	}
	if cop == nil || cop.Model != "auto" {
		t.Fatalf("no copilot reviewer in %v", round1.Agents)
	}
	for _, a := range round1.Agents {
		w.say(t, a.Id, v1.MessageType_MESSAGE_TYPE_DONE, "finding in "+a.Kind)
	}
	waitFor(t, "round 1's message", func() bool { return len(w.answers(t)) == 1 })

	// A fix commit: the next round skips Copilot (review.re_review.skip).
	w.pr.push(t, "the fix")
	round2 := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "large", Tier: "mid", Reviewers: "auto"})
	for _, a := range round2.Agents {
		if a.Kind == "copilot" {
			t.Fatalf("copilot was asked again: %v", round2.Notes)
		}
	}
	if !strings.Contains(strings.Join(round2.Notes, "\n"), "copilot reviewed it once") {
		t.Errorf("no note on skipping copilot: %v", round2.Notes)
	}
}

func TestCopilotAtItsLimitIsLeftOutOfAutoReviewers(t *testing.T) {
	tweak, _, _ := copilotTweak(t, nil)
	w := startReviews(t, tweak)
	w.set(t, "review.reviewers.large", []any{[]any{"copilot", "claude"}})
	w.pr.push(t, "the feature")
	if err := w.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		return tx.PutModelLimit("copilot", "*", time.Now().Add(6*time.Hour))
	}); err != nil {
		t.Fatal(err)
	}
	res := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "large", Tier: "mid", Reviewers: "auto"})
	for _, a := range res.Agents {
		if a.Kind == "copilot" {
			t.Fatalf("a copilot at its limit was asked: %v", res.Notes)
		}
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "copilot is at its limit") {
		t.Errorf("no note on copilot's limit: %v", res.Notes)
	}
	// Named by hand, it is refused the same way.
	res = w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "large", Tier: "mid", Reviewers: "claude,copilot"})
	if len(res.Agents) != 1 || res.Agents[0].Kind != "claude" {
		t.Fatalf("named copilot at its limit: %v", res.Agents)
	}
}

func TestCopilotTurnsFollowAScreenLimitWhoseLineStaysOnScreen(t *testing.T) {
	tweak, _, _ := copilotTweak(t, nil)
	w := startReopen(t, fastPass, tweak)
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.copilotRoot(t, run.Id)
	worker := w.spawnUnder(t, asLead, "stale", &v1.SpawnAgentRequest{Kind: "copilot", Tier: "light"})
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.typeInto(worker, "/limit\r")
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_LIMITED)
	// A prompt is answered after the limit: its line is still among the last screen lines, yet the turn is
	// followed to its end.
	w.typeInto(worker, "go again\r")
	waitFor(t, "the answered prompt", func() bool { return strings.Contains(w.screen(t, worker), "done: go again") })
	waitFor(t, "idle after the answer, not stuck on the old limit line", func() bool {
		return w.agent(worker.Id).State == v1.AgentState_AGENT_STATE_IDLE
	})
}

// The turn after a limit stays working for several polls with the old limit line still on screen: it must not
// limit the agent again, and a turn that ends is still followed to its end.
func TestCopilotAScreenLimitLineOnScreenDoesNotLimitALongTurnAgain(t *testing.T) {
	tweak, _, _ := copilotTweak(t, nil)
	w := startReopen(t, fastPass, tweak)
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.copilotRoot(t, run.Id)
	worker := w.spawnUnder(t, asLead, "slow", &v1.SpawnAgentRequest{Kind: "copilot", Tier: "light"})
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.typeInto(worker, "/limit\r")
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_LIMITED)
	w.typeInto(worker, "/hold\r")
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_WORKING)
	// Many polls with the turn open and the limit line among the last screen lines.
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := w.agent(worker.Id).State; got != v1.AgentState_AGENT_STATE_WORKING {
			t.Fatalf("the agent left working to %v while its turn was open", got)
		}
		time.Sleep(30 * time.Millisecond)
	}
	w.typeInto(worker, "finish\r\n")
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
}

// A per-model rate limit holds the agent's own model, not every Copilot model; the session limit holds all.
func TestCopilotAPerModelScreenLimitHoldsOnlyItsModel(t *testing.T) {
	tweak, _, _ := copilotTweak(t, nil)
	w := startReopen(t, fastPass, tweak)
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.copilotRoot(t, run.Id)
	worker := w.spawnUnder(t, asLead, "onemodel", &v1.SpawnAgentRequest{Kind: "copilot", Tier: "light"})
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.typeInto(worker, "/modellimit\r")
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_LIMITED)
	holds, err := w.d.Store().Read(context.Background()).ModelLimits("copilot")
	if err != nil {
		t.Fatal(err)
	}
	if len(holds) != 1 || holds[0].Model == "*" || holds[0].Model != w.agent(worker.Id).Model {
		t.Fatalf("a per-model limit holds %+v, want the agent's model %q only", holds, w.agent(worker.Id).Model)
	}
}

// A turn over 256 KiB of events still ends: its prompt is out of the tail the poll reads.
func TestCopilotATurnBiggerThanTheTailStillEnds(t *testing.T) {
	tweak, _, _ := copilotTweak(t, nil)
	w := startReopen(t, fastPass, tweak)
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.copilotRoot(t, run.Id)
	worker := w.spawnUnder(t, asLead, "big", &v1.SpawnAgentRequest{Kind: "copilot", Tier: "light"})
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.typeInto(worker, "/bighold\r")
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_WORKING)
	time.Sleep(200 * time.Millisecond)
	w.typeInto(worker, "go\r")
	waitFor(t, "the big turn's answer", func() bool { return strings.Contains(w.screen(t, worker), "done: big") })
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
}

// A hold is lifted by the answer to a prompt typed after it, not by the answer of a turn that was already running
// when the quota ran out.
func TestCopilotARunningTurnsAnswerDoesNotLiftAFreshHold(t *testing.T) {
	tweak, _, _ := copilotTweak(t, nil)
	w := startReopen(t, fastPass, tweak)
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.copilotRoot(t, run.Id)
	running := w.spawnUnder(t, asLead, "running", &v1.SpawnAgentRequest{Kind: "copilot", Tier: "light"})
	limited := w.spawnUnder(t, asLead, "limited", &v1.SpawnAgentRequest{Kind: "copilot", Tier: "light"})
	w.waitState(running.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.waitState(limited.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.typeInto(running, "/hold\r")
	w.waitState(running.Id, v1.AgentState_AGENT_STATE_WORKING)
	time.Sleep(100 * time.Millisecond)
	w.typeInto(limited, "/402\r")
	w.waitState(limited.Id, v1.AgentState_AGENT_STATE_LIMITED)
	// The 402's day-long hold (the limited agent's own model may hold shorter, for its own reset).
	if held := time.Until(w.modelHold(t, "copilot")); held < 23*time.Hour {
		t.Fatalf("a 402 holds copilot for %s", held)
	}
	w.typeInto(running, "finish\r")
	w.waitState(running.Id, v1.AgentState_AGENT_STATE_IDLE)
	time.Sleep(500 * time.Millisecond) // several polls
	if held := time.Until(w.modelHold(t, "copilot")); held < 23*time.Hour {
		t.Fatalf("the answer of a turn begun before the hold left %s of it", held)
	}
	// A prompt typed after the hold, and answered, does.
	w.typeInto(running, "after the hold\r")
	waitFor(t, "the hold lifted by an answer to a newer prompt", func() bool { return w.modelHold(t, "copilot").IsZero() })
}

// An answer lifts only its own model's hold (and an account-wide one): an agent on another model answering says
// nothing of a model that is rate limited.
func TestCopilotAnAnswerOnOneModelDoesNotLiftAnotherModelsHold(t *testing.T) {
	tweak, _, _ := copilotTweak(t, nil)
	w := startReopen(t, fastPass, tweak)
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.copilotRoot(t, run.Id)
	onX := w.spawnUnder(t, asLead, "onx", &v1.SpawnAgentRequest{Kind: "copilot", Tier: "light", Model: "model-x"})
	onY := w.spawnUnder(t, asLead, "ony", &v1.SpawnAgentRequest{Kind: "copilot", Tier: "light", Model: "model-y"})
	w.waitState(onX.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.waitState(onY.Id, v1.AgentState_AGENT_STATE_IDLE)
	// The hold's remaining time by model. A lifted hold on a limited agent is put back for the short default
	// (settleLimit), so only the length tells a kept hold from a lifted one.
	holds := func() map[string]time.Duration {
		hs, err := w.d.Store().Read(context.Background()).ModelLimits("copilot")
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]time.Duration{}
		for _, h := range hs {
			if left := time.Until(h.Until); left > 0 {
				out[h.Model] = left
			}
		}
		return out
	}
	w.typeInto(onX, "/modellimit\r")
	w.waitState(onX.Id, v1.AgentState_AGENT_STATE_LIMITED)
	if h := holds(); h["model-x"] < time.Hour || len(h) != 1 { // "Resets in 2h"
		t.Fatalf("holds %v, want model-x's, for 2h, only", h)
	}
	time.Sleep(600 * time.Millisecond) // the hold is final: a poll or two may still note it
	// Y answers a prompt typed after X's hold: X's hold stays.
	w.typeInto(onY, "hello\r")
	waitFor(t, "model-y's answer", func() bool { return strings.Contains(w.screen(t, onY), "done: hello") })
	w.waitState(onY.Id, v1.AgentState_AGENT_STATE_IDLE)
	time.Sleep(500 * time.Millisecond) // several polls
	if h := holds(); h["model-x"] < time.Hour {
		t.Fatalf("an answer on model-y lifted model-x's hold: %v", h)
	}
	// X answering a prompt of its own, typed after the hold, does lift it.
	w.typeInto(onX, "hello again\r")
	waitFor(t, "model-x's hold lifted by an answer on model-x", func() bool { return holds()["model-x"] < time.Hour })
}

// A limit line whose countdown ticks is one line: the working agent is not limited again by it.
func TestCopilotLimitKeyIgnoresTheCountdown(t *testing.T) {
	a := copilot.Limit{Message: "You've hit your rate limit, resets in 3h 20m"}
	b := copilot.Limit{Message: "You've hit your rate limit, resets in 3h 19m"}
	if a.Key() != b.Key() || a.Key() == "" {
		t.Errorf("keys %q and %q differ", a.Key(), b.Key())
	}
	if c := (copilot.Limit{Message: "You've hit the rate limit for this model"}); c.Key() == a.Key() {
		t.Errorf("different limits share a key %q", c.Key())
	}
}

// Copilot has no `top` tier: an auto round at that tier passes it over, as bin/mesh's pick_slots does, and says so.
func TestCopilotWithoutTheAskedTierIsLeftOutOfAutoReviewers(t *testing.T) {
	tweak, _, _ := copilotTweak(t, nil)
	w := startReviews(t, tweak)
	w.set(t, "review.reviewers.large", []any{[]any{"copilot", "claude"}})
	w.pr.push(t, "the feature")
	res := w.ask(t, &v1.AskRequest{Repo: w.pr.repo, Review: "7", Size: "large", Tier: "top", Reviewers: "auto"})
	for _, a := range res.Agents {
		if a.Kind == "copilot" {
			t.Fatalf("copilot was asked at a tier it has none of: %v", res.Notes)
		}
	}
	if len(res.Agents) != 1 || res.Agents[0].Kind != "claude" || !strings.Contains(strings.Join(res.Notes, "\n"), "copilot has no top tier") {
		t.Errorf("agents %v notes %v", res.Agents, res.Notes)
	}
}
