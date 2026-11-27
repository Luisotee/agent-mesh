package daemon_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
)

func (h *agentsHarness) withdraw(agent, id, mid string) (*v1.QueuedPrompt, error) {
	res, err := h.device.Mesh.WithdrawPrompt(context.Background(), connect.NewRequest(&v1.WithdrawPromptRequest{Meta: meta(mid), Agent: agent, PromptId: id}))
	if err != nil {
		return nil, err
	}
	return res.Msg.Prompt, nil
}

// typedLines is what the fake typed into: each line the agent's CLI took, from $FAKECLAUDE_INPUT_LOG.
func typedLines(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

func hasLine(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestAPromptMidTurnIsQueuedAndTypedAtTheNextIdle(t *testing.T) {
	log := filepath.Join(t.TempDir(), "in.log")
	h, a := chatDaemon(t, map[string]string{"FAKECLAUDE_INPUT_LOG": log})
	ctx := context.Background()
	h.typeInto(a, "/hold\r")
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_WORKING)

	// Mid-turn the send is queued, never refused, and the queue says what it waits for.
	res, err := h.device.Mesh.PromptAgent(ctx, connect.NewRequest(&v1.PromptAgentRequest{Meta: meta("q-first"), Agent: a.Id, Text: "first"}))
	if err != nil || res.Msg.Queued == nil || res.Msg.Queued.Text != "first" {
		t.Fatalf("a prompt mid-turn: %v, %v", res, err)
	}
	// A retry of the same send is the same queued prompt, not a second one.
	again, err := h.device.Mesh.PromptAgent(ctx, connect.NewRequest(&v1.PromptAgentRequest{Meta: meta("q-first"), Agent: a.Id, Text: "first"}))
	if err != nil || again.Msg.Queued.GetId() != res.Msg.Queued.Id {
		t.Fatalf("the retry: %v, %v", again, err)
	}
	if err := h.prompt(a.Id, "second"); err != nil {
		t.Fatal(err)
	}
	q := h.prompts(a.Id)
	if len(q) != 2 || q[0].Text != "first" || q[1].Text != "second" || q[0].Held != "working" {
		t.Fatalf("queue = %v", q)
	}
	if lines := typedLines(t, log); hasLine(lines, "first") {
		t.Fatalf("typed mid-turn: %v", lines)
	}

	// The turn ends: they are typed in order, one per idle.
	h.typeInto(a, "\r")
	waitFor(t, "the queue typed", func() bool { return len(h.prompts(a.Id)) == 0 })
	h.waitTranscript(a.Id, "second")
	lines := typedLines(t, log)
	if i, j := indexOf(lines, "first"), indexOf(lines, "second"); i < 0 || j < i {
		t.Errorf("typed in the wrong order: %v", lines)
	}

}

func indexOf(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return -1
}

func TestAQueuedPromptIsWithdrawnUntilTyped(t *testing.T) {
	log := filepath.Join(t.TempDir(), "in.log")
	h, a := chatDaemon(t, map[string]string{"FAKECLAUDE_INPUT_LOG": log})
	h.typeInto(a, "/hold\r")
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_WORKING)
	for _, text := range []string{"keep", "drop"} {
		if err := h.prompt(a.Id, text); err != nil {
			t.Fatal(err)
		}
	}
	// No id: the newest, as the chat's Up arrow takes.
	p, err := h.withdraw(a.Id, "", "w-1")
	if err != nil || p.Text != "drop" {
		t.Fatalf("withdraw the newest = %v, %v", p, err)
	}
	if _, err := h.withdraw(a.Id, p.Id, "w-2"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("withdrawing it twice: %v", err)
	}
	// A device that may not type may not withdraw either.
	tok, err := fsutil.ReadSecret(statedir.Paths{Dir: h.state}.AgentToken(a.Id))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.as(tok).WithdrawPrompt(context.Background(), connect.NewRequest(&v1.WithdrawPromptRequest{Meta: meta("w-agent"), Agent: a.Id})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("an agent withdrawing: %v", err)
	}
	h.typeInto(a, "\r")
	h.waitTranscript(a.Id, "keep")
	q := h.prompts(a.Id)
	if len(q) != 0 {
		t.Errorf("left in the queue: %v", q)
	}
	time.Sleep(300 * time.Millisecond)
	if lines := typedLines(t, log); hasLine(lines, "drop") {
		t.Errorf("a withdrawn prompt was typed: %v", lines)
	}
	// Nothing queued: there is nothing to take back.
	if _, err := h.withdraw(a.Id, "", "w-3"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("withdrawing from an empty queue: %v", err)
	}
}

// The delivery claims a prompt before typing it and the withdrawal takes it only while queued: whichever runs first
// wins, and the prompt is typed exactly when the withdrawal failed.
func TestWithdrawRacingDeliveryEndsInOneOfThem(t *testing.T) {
	log := filepath.Join(t.TempDir(), "in.log")
	h, a := chatDaemon(t, map[string]string{"FAKECLAUDE_INPUT_LOG": log})
	for i := 0; i < 6; i++ {
		text := fmt.Sprintf("race%d", i)
		h.typeInto(a, "/hold\r")
		h.waitState(a.Id, v1.AgentState_AGENT_STATE_WORKING)
		if err := h.prompt(a.Id, text); err != nil {
			t.Fatal(err)
		}
		// End the turn (the delivery starts at the agent's idle) and withdraw at once, from another goroutine.
		var wg sync.WaitGroup
		var werr error
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i*40) * time.Millisecond)
			_, werr = h.withdraw(a.Id, "", "race-w-"+text)
		}()
		h.typeInto(a, "\r")
		wg.Wait()
		if werr != nil && connect.CodeOf(werr) != connect.CodeFailedPrecondition {
			t.Fatalf("withdraw %s: %v", text, werr)
		}
		withdrawn := werr == nil
		t.Logf("%s: withdrawn %v", text, withdrawn)
		waitFor(t, "the queue empty", func() bool { return len(h.prompts(a.Id)) == 0 })
		if withdrawn {
			time.Sleep(400 * time.Millisecond) // a wrong delivery would type it now
		} else {
			h.waitTranscript(a.Id, text)
		}
		h.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
		if typed := hasLine(typedLines(t, log), text); typed == withdrawn {
			t.Fatalf("%s: withdrawn %v, typed %v", text, withdrawn, typed)
		}
	}
}

func TestInterruptStopsAClaudeTurnAndTheQueueGoesOn(t *testing.T) {
	log := filepath.Join(t.TempDir(), "in.log")
	h, a := chatDaemon(t, map[string]string{"FAKECLAUDE_ESC": "1", "FAKECLAUDE_INPUT_LOG": log})
	ctx := context.Background()
	interrupt := func(mid string) (*v1.InterruptAgentResponse, error) {
		res, err := h.device.Mesh.InterruptAgent(ctx, connect.NewRequest(&v1.InterruptAgentRequest{Meta: meta(mid), Agent: a.Id}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	// At its prompt there is nothing to interrupt, and no key is sent (a second Esc would open Claude's rewind menu).
	if res, err := interrupt("i-idle"); err != nil || !res.Stopped {
		t.Fatalf("interrupting an idle agent: %v, %v", res, err)
	}

	h.typeInto(a, "/hold\r")
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_WORKING)
	if err := h.prompt(a.Id, "next"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1600 * time.Millisecond) // the typing of typeInto above is quiet again
	res, err := interrupt("i-work")
	if err != nil || !res.Stopped {
		t.Fatalf("interrupting a turn: %v, %v", res, err)
	}
	// The fake fires no Stop hook for an interrupted turn, as Claude Code doesn't: the daemon recorded the turn's end,
	// and the queued prompt went in after it.
	h.waitTranscript(a.Id, "next")
	if lines := typedLines(t, log); !hasLine(lines, "next") {
		t.Errorf("the queued prompt after the interrupt: %v", lines)
	}
	// A retry is answered from the record, with no second key.
	if again, err := interrupt("i-work"); err != nil || !again.Stopped {
		t.Errorf("a retried interrupt: %v, %v", again, err)
	}

	// A new turn soon after: the Esc sent for the last one doesn't hold this one back (the agent did something since).
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	h.typeInto(a, "/hold\r")
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_WORKING)
	time.Sleep(1600 * time.Millisecond)
	if res, err := interrupt("i-again"); err != nil || !res.Stopped {
		t.Fatalf("interrupting the next turn: %v, %v", res, err)
	}
	h.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)

}

func (h *agentsHarness) sendAnyway(agent, id, draft, mid string) (*v1.SendPromptAnywayResponse, error) {
	res, err := h.device.Mesh.SendPromptAnyway(context.Background(), connect.NewRequest(&v1.SendPromptAnywayRequest{Meta: meta(mid), Agent: agent, PromptId: id, Draft: draft}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// heldByDraft waits for the agent's queue to say it waits for a draft, and returns the prompt with the draft it read.
func (h *agentsHarness) heldByDraft(agent string) *v1.QueuedPrompt {
	h.t.Helper()
	var p *v1.QueuedPrompt
	waitFor(h.t, "the queue to say it waits for the draft", func() bool {
		q := h.prompts(agent)
		if len(q) == 1 && q[0].Held == "draft" {
			p = q[0]
		}
		return p != nil
	})
	return p
}

func TestASendOverAConfirmedDraftClearsItAndTypesThePrompt(t *testing.T) {
	h, a := chatDaemon(t, map[string]string{"FAKECLAUDE_BOX": "1"})
	h.typeInto(a, "half a   thou")
	res, err := h.device.Mesh.PromptAgent(context.Background(), connect.NewRequest(&v1.PromptAgentRequest{Meta: meta("p-over"), Agent: a.Id, Text: "hello there"}))
	if err != nil || res.Msg.Queued == nil || res.Msg.Queued.Held != "draft" || res.Msg.Queued.HeldText != "half a thou" {
		t.Fatalf("a prompt at a draft: %v, %v", res, err)
	}
	p := h.heldByDraft(a.Id)
	if p.HeldText != "half a thou" {
		t.Fatalf("the draft the queue shows: %q", p.HeldText)
	}
	out, err := h.sendAnyway(a.Id, p.Id, p.HeldText, "over-1")
	if err != nil || out.Discarded != "half a thou" {
		t.Fatalf("send anyway: %v, %v", out, err)
	}
	if q := h.prompts(a.Id); len(q) != 0 {
		t.Errorf("the prompt is still queued: %v", q)
	}
	h.waitTranscript(a.Id, "hello there")
	// Only the prompt reached the agent: not the draft in front of it.
	tr, err := h.device.Mesh.ReadTranscript(context.Background(), connect.NewRequest(&v1.ReadTranscriptRequest{Agent: a.Id, Offset: -1, Backward: true}))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range tr.Msg.Entries {
		if e.Kind == v1.TranscriptKind_TRANSCRIPT_KIND_USER && strings.Contains(e.Text, "half") {
			t.Errorf("the draft was sent with it: %q", e.Text)
		}
	}
	// A retry with the same mutation id is the first answer, not a second send.
	again, err := h.sendAnyway(a.Id, p.Id, p.HeldText, "over-1")
	if err != nil || again.Discarded != "" { // the stored answer does not keep the draft
		t.Errorf("the retry: %v, %v", again, err)
	}
}

func TestASendOverADraftThatChangedIsRefusedAndTheDraftKept(t *testing.T) {
	h, a := chatDaemon(t, map[string]string{"FAKECLAUDE_BOX": "1"})
	h.typeInto(a, "half a thou")
	if err := h.prompt(a.Id, "hello"); err != nil {
		t.Fatal(err)
	}
	p := h.heldByDraft(a.Id)
	// The user confirmed an older draft; the box holds something else now.
	_, err := h.sendAnyway(a.Id, p.Id, "something that was there before", "over-3")
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "changed") || !strings.Contains(err.Error(), "half a thou") {
		t.Fatalf("a draft that changed: %v", err)
	}
	if q := h.prompts(a.Id); len(q) != 1 || q[0].Held != "draft" || q[0].HeldText != "half a thou" {
		t.Errorf("the prompt and the draft should be untouched: %v", q)
	}
}

func TestASendOverADraftWhileSomeoneTypesAndOtherRefusals(t *testing.T) {
	h, a := chatDaemon(t, map[string]string{"FAKECLAUDE_BOX": "1"})
	ctx := context.Background()
	h.typeInto(a, "half a thou")
	if err := h.prompt(a.Id, "hello"); err != nil {
		t.Fatal(err)
	}
	p := h.heldByDraft(a.Id)

	// A prompt that is not waiting (never was, or taken back) is refused.
	if _, err := h.sendAnyway(a.Id, "half a thou", "qnope", "over-4"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("an unknown prompt: %v", err)
	}
	// A device without input rights, and an agent, may not.
	tok, err := fsutil.ReadSecret(statedir.Paths{Dir: h.state}.AgentToken(a.Id))
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.as(tok).SendPromptAnyway(ctx, connect.NewRequest(&v1.SendPromptAnywayRequest{Meta: meta("over-5"), Agent: a.Id, PromptId: p.Id, Draft: "half a thou"}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("an agent: %v", err)
	}

	// Someone with the terminal open who typed a moment ago: not now, and the draft stays.
	user := h.openTerminal(ctx, a, true, &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_TAIL})
	defer user.Close()
	user.Ready(ctx)
	user.Input(ctx, []byte("x\x7f"))
	waitFor(t, "the keystroke counted", func() bool {
		_, err := h.sendAnyway(a.Id, p.Id, "half a thou", "over-6-"+time.Now().String())
		return err != nil && strings.Contains(err.Error(), "someone is typing")
	})
	if q := h.prompts(a.Id); len(q) != 1 || q[0].Id != p.Id {
		t.Errorf("the prompt should still wait: %v", q)
	}
	user.Close()
	// With nobody typing any more, the confirmed draft goes.
	waitFor(t, "sent once nobody types", func() bool {
		_, err := h.sendAnyway(a.Id, p.Id, "half a thou", "over-7-"+time.Now().String())
		return err == nil
	})
	h.waitTranscript(a.Id, "hello")
}

func TestASendOverADraftIsForTheOldestPromptAndNotForALongDraft(t *testing.T) {
	h, a := chatDaemon(t, map[string]string{"FAKECLAUDE_BOX": "1"})
	h.typeInto(a, "half a thou")
	for _, text := range []string{"first", "second"} {
		if err := h.prompt(a.Id, text); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "both queued behind the draft", func() bool {
		q := h.prompts(a.Id)
		return len(q) == 2 && q[0].Held == "draft" && q[1].HeldText == "half a thou"
	})
	q := h.prompts(a.Id)
	// The second would be typed before the first: refused, nothing is cleared.
	if _, err := h.sendAnyway(a.Id, q[1].Id, "half a thou", "over-8"); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "older prompt") {
		t.Errorf("the second prompt: %v", err)
	}
	if _, err := h.sendAnyway(a.Id, q[0].Id, "half a thou", "over-9"); err != nil {
		t.Fatal(err)
	}
	h.waitTranscript(a.Id, "first")
}
