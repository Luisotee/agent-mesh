package daemon_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
)

func chatDaemon(t *testing.T, env map[string]string) (*agentsHarness, *v1.Agent) {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	h := startWithAgents(t, state, t.TempDir(), func(c *daemon.AgentConfig) {
		c.Env["FAKECLAUDE_TRANSCRIPT"] = "1"
		c.Env["FAKECLAUDE_BOX"] = "1" // a prompt goes only into a box that reads empty
		for k, v := range env {
			c.Env[k] = v
		}
	})
	t.Cleanup(func() { stopTermHost(t, state) })
	run := startRun(t, h.device.Mesh, "")
	a := h.spawn(run.Id, gitRepo(t), "chat", "")
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	return h, a
}

// promptWhenReady sends a prompt as a user would: when the host says the box isn't empty yet (the agent is
// still drawing its first prompt or the turn before), wait and send again.
func (h *agentsHarness) promptWhenReady(agent, text string) error {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := h.prompt(agent, text)
		if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "input box") || time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (h *agentsHarness) prompt(agent, text string) error {
	_, err := h.device.Mesh.PromptAgent(context.Background(), connect.NewRequest(&v1.PromptAgentRequest{Meta: meta("prompt-" + text), Agent: agent, Text: text}))
	return err
}

func (h *agentsHarness) transcript(agent string, offset int64, backward bool) *v1.ReadTranscriptResponse {
	h.t.Helper()
	res, err := h.device.Mesh.ReadTranscript(context.Background(), connect.NewRequest(&v1.ReadTranscriptRequest{Agent: agent, Offset: offset, Backward: backward}))
	if err != nil {
		h.t.Fatal(err)
	}
	return res.Msg
}

func kindsOf(es []*v1.TranscriptEntry) string {
	var out []string
	for _, e := range es {
		out = append(out, strings.TrimPrefix(e.Kind.String(), "TRANSCRIPT_KIND_"))
	}
	return strings.Join(out, " ")
}

func TestAPromptTypedFromTheChatReachesTheCLIAndShowsInItsTranscript(t *testing.T) {
	h, a := chatDaemon(t, nil)
	// Before anything is said the conversation is empty, not an error, and says which session it is.
	first := h.transcript(a.Id, -1, true)
	if !first.Supported || first.SessionId == "" || len(first.Entries) != 0 || !first.AtStart {
		t.Fatalf("an empty conversation: %v", first)
	}

	if err := h.promptWhenReady(a.Id, "fix the typo"); err != nil {
		t.Fatal(err)
	}
	var tail *v1.ReadTranscriptResponse
	waitFor(t, "the turn in the transcript", func() bool {
		tail = h.transcript(a.Id, -1, true)
		return kindsOf(tail.Entries) == "USER ASSISTANT TOOL_CALL TOOL_RESULT ASSISTANT"
	})
	e := tail.Entries
	if e[0].Text != "fix the typo" || e[1].Text != "Working on: fix the typo" || e[2].Tool != "Bash" || e[2].ToolInput != "echo fix the typo" || e[3].ToolUseId != e[2].ToolUseId || e[4].Text != "Done: fix the typo" {
		t.Errorf("entries %v", e)
	}
	if tail.SessionId != first.SessionId || !tail.AtStart || tail.EndOffset <= 0 {
		t.Errorf("session %q (was %q), atStart %v, end %d", tail.SessionId, first.SessionId, tail.AtStart, tail.EndOffset)
	}

	// Reading on from the end gives only what comes next; the agent is the same process and conversation.
	if err := h.promptWhenReady(a.Id, "and the README"); err != nil {
		t.Fatal(err)
	}
	var more *v1.ReadTranscriptResponse
	waitFor(t, "the next turn", func() bool {
		more = h.transcript(a.Id, tail.EndOffset, false)
		return len(more.Entries) == 5
	})
	if more.Entries[0].Text != "and the README" || more.StartOffset != tail.EndOffset {
		t.Errorf("the next page: %v from %d", more.Entries, more.StartOffset)
	}
	if got := h.agent(a.Id); got.SessionId != tail.SessionId {
		t.Errorf("the prompt changed the conversation: %s -> %s", tail.SessionId, got.SessionId)
	}
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
}

func TestAPromptOfSeveralLinesIsOnePrompt(t *testing.T) {
	h, a := chatDaemon(t, nil)
	if err := h.promptWhenReady(a.Id, "first line\r\nsecond line\x1b[201~\x03"); err != nil {
		t.Fatal(err)
	}
	var got *v1.ReadTranscriptResponse
	waitFor(t, "the prompt in the transcript", func() bool {
		got = h.transcript(a.Id, -1, true)
		return len(got.Entries) > 0
	})
	// One prompt with its newline, and the keystrokes a paste could carry are gone.
	if got.Entries[0].Kind != v1.TranscriptKind_TRANSCRIPT_KIND_USER || got.Entries[0].Text != "first line\nsecond line[201~" {
		t.Errorf("prompt: %q", got.Entries[0].Text)
	}
}

func TestOnlyADeviceThatMayTypeCanPromptAnAgentAndOnlyWhereTypingIsSafe(t *testing.T) {
	h, a := chatDaemon(t, map[string]string{"FAKECLAUDE_BOX": "1"})
	ctx := context.Background()

	// An agent may not, even the run's own lead; a read-only device may not.
	tok, err := fsutil.ReadSecret(statedir.Paths{Dir: h.state}.AgentToken(a.Id))
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.as(tok).PromptAgent(ctx, connect.NewRequest(&v1.PromptAgentRequest{Meta: meta("p-agent"), Agent: a.Id, Text: "hi"}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("an agent prompting: %v", err)
	}
	cp, err := h.device.Mesh.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{Meta: meta("pair-ro"), Name: "ro", ReadOnly: true}))
	if err != nil {
		t.Fatal(err)
	}
	red, err := pairingClient(h.state, "").RedeemPairing(ctx, connect.NewRequest(&v1.RedeemPairingRequest{Token: cp.Msg.Token}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.as(red.Msg.DeviceToken).PromptAgent(ctx, connect.NewRequest(&v1.PromptAgentRequest{Meta: meta("p-ro"), Agent: a.Id, Text: "hi"}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a read-only device prompting: %v", err)
	}
	// The same read-only device may read the conversation.
	if _, err := h.as(red.Msg.DeviceToken).ReadTranscript(ctx, connect.NewRequest(&v1.ReadTranscriptRequest{Agent: a.Id, Offset: -1, Backward: true})); err != nil {
		t.Errorf("a read-only device reading: %v", err)
	}

	// An empty prompt, and one with only control characters, say so.
	for _, text := range []string{"", "  \n ", "\x1b\x03"} {
		if err := h.prompt(a.Id, text); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("prompt %q: %v", text, err)
		}
	}

	// A draft someone left in the box is not typed after.
	h.typeInto(a, "half a thou")
	waitFor(t, "the draft refused", func() bool {
		err := h.prompt(a.Id, "hello")
		return connect.CodeOf(err) == connect.CodeFailedPrecondition && strings.Contains(err.Error(), "draft")
	})
}

func TestAPromptIsRefusedWhereTheLineWouldAnswerAPromptOrThereIsNoTerminal(t *testing.T) {
	h, a := chatDaemon(t, nil)
	h.typeInto(a, "/ask\r")
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_NEEDS_INPUT)
	if err := h.prompt(a.Id, "hello"); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "waits at a prompt") {
		t.Errorf("at a prompt: %v", err)
	}
	h.typeInto(a, "y\r")
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)

	h.typeInto(a, "/exit\r")
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_EXITED)
	if err := h.prompt(a.Id, "hello"); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "exited") {
		t.Errorf("an exited agent: %v", err)
	}
	if _, err := h.device.Mesh.PromptAgent(context.Background(), connect.NewRequest(&v1.PromptAgentRequest{Meta: meta("p-none"), Agent: "nobody", Text: "hi"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("an unknown agent: %v", err)
	}
}

func TestACLIWithoutARecordAnswersUnsupported(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	h := startWithAgents(t, state, t.TempDir())
	t.Cleanup(func() { stopTermHost(t, state) })
	run := startRun(t, h.device.Mesh, "")
	res, err := h.device.Mesh.SpawnAgent(context.Background(), connect.NewRequest(&v1.SpawnAgentRequest{Meta: meta("spawn-agy"), RunId: run.Id, Repo: gitRepo(t), Slug: "agy", Kind: "agy", Tier: "mid"}))
	if err != nil {
		t.Fatal(err)
	}
	got := h.transcript(res.Msg.Agent.Id, -1, true)
	if got.Supported || len(got.Entries) != 0 {
		t.Errorf("agy has no record we read, but %v", got)
	}
	time.Sleep(10 * time.Millisecond)
}

func TestCleanPromptKeepsTextAndDropsKeystrokes(t *testing.T) {
	for in, want := range map[string]string{
		"plain":              "plain",
		"a\r\nb\rc":          "a\nb\nc",
		"tab\there":          "tab\there",
		"esc\x1b[201~gone":   "esc[201~gone",
		"bell\x07 nul\x00 x": "bell nul x",
		"del\x7fete":         "delete",
		"unicode é 日本 🙂":     "unicode é 日本 🙂",
		"c1\u009bcontrol":    "c1control",
	} {
		if got := daemon.CleanPrompt(in); got != want {
			t.Errorf("CleanPrompt(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestARetriedPromptIsTypedOnceAndAnExpiredOneNotAtAll(t *testing.T) {
	h, a := chatDaemon(t, nil)
	ctx := context.Background()
	send := func(m *v1.RequestMeta, text string) error {
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			_, err := h.device.Mesh.PromptAgent(ctx, connect.NewRequest(&v1.PromptAgentRequest{Meta: m, Agent: a.Id, Text: text}))
			if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "input box") || time.Now().After(deadline) {
				return err
			}
		}
	}
	// A request past its expiry types nothing, and doesn't use up its id.
	late := &v1.RequestMeta{MutationId: "late", ExpiresAt: timestamppb.New(time.Now().Add(-time.Second))}
	if err := send(late, "too late"); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("an expired prompt: %v", err)
	}
	// The same request sent twice (the bridge resends when a socket resets after the daemon took it) is one prompt.
	once := meta("once")
	if err := send(once, "only once"); err != nil {
		t.Fatal(err)
	}
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	if err := send(once, "only once"); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	// A later, different prompt still goes in.
	if err := send(meta("next"), "and then"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both prompts, one each", func() bool {
		var users []string
		for _, e := range h.transcript(a.Id, -1, true).Entries {
			if e.Kind == v1.TranscriptKind_TRANSCRIPT_KIND_USER {
				users = append(users, e.Text)
			}
		}
		return strings.Join(users, "|") == "only once|and then"
	})
	time.Sleep(300 * time.Millisecond)
	n := 0
	for _, e := range h.transcript(a.Id, -1, true).Entries {
		if e.Kind == v1.TranscriptKind_TRANSCRIPT_KIND_USER {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d user turns, want 2", n)
	}
}

func TestAPromptIsNotTypedWhereNoEmptyInputBoxIsOnTheScreen(t *testing.T) {
	// A dropped line leaves a dialog where the box was: its Enter would pick an option.
	h, a := chatDaemon(t, map[string]string{"FAKECLAUDE_DROP": "1", "FAKECLAUDE_DIALOG": "1"})
	h.typeInto(a, "📬 [mesh] dropped\r")
	waitFor(t, "the refusal", func() bool {
		err := h.prompt(a.Id, "hello")
		return connect.CodeOf(err) == connect.CodeFailedPrecondition && strings.Contains(err.Error(), "no empty input box")
	})
}
