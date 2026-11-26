package daemon_test

// The delivery guarantees of bin/mesh's doorbell (design §3 and §11), as tests of the daemon: a doorbell is
// delivered only when the agent is known to have taken it, never typed into a human's draft or at a dialog, and
// written into Claude's inbox socket where it takes one. Run against the fake claude's input box
// (FAKECLAUDE_BOX), which draws what a draft and a doorbell left in the box look like on the screen.

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/agy"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/opencode"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
)

// boxWorld starts a world whose fake claude draws the input box and logs the lines typed (and frames written)
// to it, and returns the log's path.
func boxWorld(t *testing.T, env map[string]string, tweaks ...func(*daemon.AgentConfig)) (*reopenWorld, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "lines.jsonl")
	w := startReopen(t, append([]func(*daemon.AgentConfig){func(c *daemon.AgentConfig) {
		c.Env["FAKECLAUDE_BOX"] = "1"
		c.Env["FAKECLAUDE_LINES_LOG"] = log
		c.TypingQuiet = 200 * time.Millisecond
		for k, v := range env {
			c.Env[k] = v
		}
	}}, tweaks...)...)
	return w, log
}

// lines are what was typed to (or written into) the fake claude, oldest first.
func lines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l string
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatal(err)
		}
		out = append(out, l)
	}
	return out
}

// events are a run's events so far.
func (w *reopenWorld) events(t *testing.T, run string) []*v1.Event {
	t.Helper()
	evs, err := w.d.Store().Read(context.Background()).EventsAfter(0, run, 100000)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// deliveries are a message's MessageDelivered events.
func (w *reopenWorld) deliveries(t *testing.T, run, messageID string) []*v1.MessageDelivered {
	var out []*v1.MessageDelivered
	for _, ev := range w.events(t, run) {
		if md := ev.GetMessageDelivered(); md != nil && md.MessageId == messageID {
			out = append(out, md)
		}
	}
	return out
}

func (w *reopenWorld) count(t *testing.T, run string, match func(*v1.Event) bool) int {
	n := 0
	for _, ev := range w.events(t, run) {
		if match(ev) {
			n++
		}
	}
	return n
}

func doorbells(ls []string) []string {
	var out []string
	for _, l := range ls {
		if strings.HasPrefix(l, "📬") {
			out = append(out, l)
		}
	}
	return out
}

func tokenOf(t *testing.T, doorbell string) string {
	t.Helper()
	_, rest, ok := strings.Cut(doorbell, "(ref ")
	if !ok {
		t.Fatalf("no ref in %q", doorbell)
	}
	token, _, _ := strings.Cut(rest, ")")
	token, _, _ = strings.Cut(token, ":")
	return token
}

func send(t *testing.T, as interface {
	SendMessage(context.Context, *connect.Request[v1.SendMessageRequest]) (*connect.Response[v1.SendMessageResponse], error)
}, to string, typ v1.MessageType, body string) *v1.Message {
	t.Helper()
	res, err := as.SendMessage(context.Background(), connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: to, Type: typ, Body: body}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.Message
}

func TestADraftLeftInTheBoxByAUserWhoDetachedHoldsTheDoorbell(t *testing.T) {
	w, _ := boxWorld(t, nil)
	w.set(t, "delivery.draft_hold_alert", 1)
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "drafty", &v1.SpawnAgentRequest{})

	// The user types half a thought, with no Enter, and detaches: the host sees no client any more.
	// Non-ASCII, and longer than the 80 characters of it an event keeps: the cut must fall on a character.
	draft := "meio pensamento çãõ " + strings.Repeat("ç", 90)
	w.typeInto(worker, draft)
	waitFor(t, "the draft on the screen", func() bool { return strings.Contains(w.screen(t, worker), "meio pensamento") })
	time.Sleep(500 * time.Millisecond) // past the typing-quiet window: only the screen can hold it now

	msg := send(t, asLead, worker.Id, v1.MessageType_MESSAGE_TYPE_TASK, "do the thing")
	waitFor(t, "a DoorbellHeld event for the draft", func() bool {
		return w.count(t, run.Id, func(ev *v1.Event) bool { return ev.GetDoorbellHeld() != nil }) > 0
	})
	if !w.deliveredAt(t, worker.Id, msg.Id).IsZero() {
		t.Fatal("the doorbell was typed into a human's draft")
	}
	if screen := w.screen(t, worker); strings.Contains(screen, "📬") || !strings.Contains(screen, "meio pensamento") {
		t.Fatalf("the draft was disturbed:\n%s", screen)
	}
	for _, ev := range w.events(t, run.Id) {
		if h := ev.GetDoorbellHeld(); h != nil && (!utf8.ValidString(h.Draft) || !strings.HasPrefix(draft, h.Draft) || len(h.MessageIds) != 1) {
			t.Errorf("DoorbellHeld %+v", h)
		}
	}

	// The user clears it: the doorbell rings, and the held event isn't repeated.
	w.typeInto(worker, strings.Repeat("\x7f", len(draft)))
	waitFor(t, "the doorbell once the draft is gone", func() bool { return !w.deliveredAt(t, worker.Id, msg.Id).IsZero() })
	if n := w.count(t, run.Id, func(ev *v1.Event) bool { return ev.GetDoorbellHeld() != nil }); n != 1 {
		t.Errorf("%d DoorbellHeld events, want 1", n)
	}
}

func TestAPlaceholderInTheBoxNeverHoldsADoorbell(t *testing.T) {
	w, _ := boxWorld(t, map[string]string{"FAKECLAUDE_PLACEHOLDER": "1"})
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "fresh", &v1.SpawnAgentRequest{})
	if screen := w.screen(t, worker); !strings.Contains(screen, `Try "fix lint errors"`) {
		t.Fatalf("no placeholder drawn:\n%s", screen)
	}
	for _, typ := range []v1.MessageType{v1.MessageType_MESSAGE_TYPE_TASK, v1.MessageType_MESSAGE_TYPE_INFO} {
		sent := time.Now()
		msg := send(t, asLead, worker.Id, typ, "hello")
		waitFor(t, "the doorbell over a placeholder", func() bool { return !w.deliveredAt(t, worker.Id, msg.Id).IsZero() })
		if held := w.deliveredAt(t, worker.Id, msg.Id).Sub(sent); held > time.Second {
			t.Errorf("a %s was held %s by a placeholder", typ, held)
		}
		w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	}
}

func TestADoorbellTheAgentDroppedIsTypedAgainWithANewToken(t *testing.T) {
	w, log := boxWorld(t, map[string]string{"FAKECLAUDE_DROP": "1"})
	w.set(t, "delivery.confirm_after", 1)
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "dropper", &v1.SpawnAgentRequest{})
	// A turn of its own first: activity from before the doorbell must not confirm it.
	w.typeInto(worker, "warm up\r")
	waitFor(t, "the warm-up turn", func() bool { return strings.Contains(w.screen(t, worker), "done: warm up") })
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)

	msg := send(t, asLead, worker.Id, v1.MessageType_MESSAGE_TYPE_INFO, "fyi")
	waitFor(t, "the delivery", func() bool { return !w.deliveredAt(t, worker.Id, msg.Id).IsZero() })
	rung := doorbells(lines(t, log))
	if len(rung) != 2 {
		t.Fatalf("doorbells typed: %q, want the dropped one and its retry", rung)
	}
	if a, b := tokenOf(t, rung[0]), tokenOf(t, rung[1]); a == b || len(a) != 6 {
		t.Errorf("tokens %q and %q: the retry needs a fresh one", a, b)
	}
	if got := w.deliveries(t, run.Id, msg.Id); len(got) != 1 || got[0].Tries != 2 || got[0].Via != "turn" {
		t.Errorf("delivery events %+v, want one via turn after 2 tries", got)
	}
}

func TestADoorbellLeftInTheBoxIsSubmittedWithEnterAlone(t *testing.T) {
	w, log := boxWorld(t, map[string]string{"FAKECLAUDE_STUCK": "1"})
	w.set(t, "delivery.confirm_after", 1)
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "stuck", &v1.SpawnAgentRequest{})
	msg := send(t, asLead, worker.Id, v1.MessageType_MESSAGE_TYPE_INFO, "fyi")
	waitFor(t, "the doorbell left in the box", func() bool { return strings.Contains(w.screen(t, worker), "📬") })
	if !w.deliveredAt(t, worker.Id, msg.Id).IsZero() {
		t.Fatal("a doorbell Enter didn't submit counted as delivered")
	}
	waitFor(t, "the delivery", func() bool { return !w.deliveredAt(t, worker.Id, msg.Id).IsZero() })
	// One doorbell was typed: the retry sent Enter, not a second doorbell fused to the first.
	if rung := doorbells(lines(t, log)); len(rung) != 1 {
		t.Errorf("doorbells typed: %q", rung)
	}
	if got := w.deliveries(t, run.Id, msg.Id); len(got) != 1 || got[0].Tries != 2 {
		t.Errorf("delivery events %+v", got)
	}
}

func TestAnAgentAtADialogGetsNoSecondEnterAndTheDoorbellIsGivenUp(t *testing.T) {
	w, log := boxWorld(t, map[string]string{"FAKECLAUDE_DROP": "1", "FAKECLAUDE_DIALOG": "1"})
	w.set(t, "delivery.confirm_after", 1)
	w.set(t, "delivery.ring_tries", 3)
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "dialog", &v1.SpawnAgentRequest{})
	msg := send(t, asLead, worker.Id, v1.MessageType_MESSAGE_TYPE_INFO, "fyi")
	waitFor(t, "the dialog", func() bool { return strings.Contains(w.screen(t, worker), "Do you want to proceed?") })
	waitFor(t, "the doorbell given up", func() bool {
		return w.count(t, run.Id, func(ev *v1.Event) bool { return ev.GetDoorbellFailed() != nil }) > 0
	})
	time.Sleep(2500 * time.Millisecond) // well past another try: nothing more is typed at a dialog
	if rung := doorbells(lines(t, log)); len(rung) != 1 {
		t.Errorf("doorbells typed at a dialog: %q", rung)
	}
	if n := w.count(t, run.Id, func(ev *v1.Event) bool { return ev.GetDoorbellFailed() != nil }); n != 1 {
		t.Errorf("%d DoorbellFailed events, want 1", n)
	}
	for _, ev := range w.events(t, run.Id) {
		if f := ev.GetDoorbellFailed(); f != nil && (f.Tries != 3 || len(f.MessageIds) != 1 || f.MessageIds[0] != msg.Id) {
			t.Errorf("DoorbellFailed %+v", f)
		}
	}
	if !w.deliveredAt(t, worker.Id, msg.Id).IsZero() {
		t.Error("the message counts as delivered")
	}
}

// ── the inbox socket ──

// inboxWorld is a world whose fake claude binds a cross-session inbox.
func inboxWorld(t *testing.T, env map[string]string, tweaks ...func(*daemon.AgentConfig)) (*reopenWorld, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ibx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	e := map[string]string{"FAKECLAUDE_INBOX": dir}
	for k, v := range env {
		e[k] = v
	}
	return boxWorld(t, e, tweaks...)
}

func TestAClaudeAgentIsRungThroughItsInboxSocketNotItsTerminal(t *testing.T) {
	w, log := inboxWorld(t, nil)
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "native", &v1.SpawnAgentRequest{})
	// A user's draft doesn't hold a frame: it never touches the input box (nothing is typed: the log below).
	w.typeInto(worker, "a draft")
	waitFor(t, "the draft on the screen", func() bool { return strings.Contains(w.screen(t, worker), "a draft") })
	time.Sleep(400 * time.Millisecond)

	msg := send(t, asLead, worker.Id, v1.MessageType_MESSAGE_TYPE_TASK, "over the socket")
	waitFor(t, "the delivery", func() bool { return !w.deliveredAt(t, worker.Id, msg.Id).IsZero() })
	got := lines(t, log)
	if len(got) != 1 || !strings.HasPrefix(got[0], "frame: 📬 [mesh] task from") || !strings.Contains(got[0], "(ref ") {
		t.Fatalf("what the agent was given: %q", got)
	}
	if d := w.deliveries(t, run.Id, msg.Id); len(d) != 1 || d[0].Via != "socket" || d[0].Tries != 1 {
		t.Errorf("delivery events %+v", d)
	}
	// Its launch carries the setting that lets the frame in.
	argv := w.launches(t, worker.SessionId)
	if len(argv) != 1 || !strings.Contains(strings.Join(argv[0], " "), `"crossSessionInbound":"accept"`) {
		t.Errorf("the launch: %v", argv)
	}
}

func TestTheSocketLaneStepsAsideForASettingOrAHeldFrame(t *testing.T) {
	t.Run("delivery.socket off types the doorbell", func(t *testing.T) {
		w, log := inboxWorld(t, nil)
		w.set(t, "delivery.socket", false)
		run := startRun(t, w.device.Mesh, "")
		_, asLead := w.root(t, run.Id)
		worker := w.spawnUnder(t, asLead, "typed", &v1.SpawnAgentRequest{})
		msg := send(t, asLead, worker.Id, v1.MessageType_MESSAGE_TYPE_INFO, "fyi")
		waitFor(t, "the delivery", func() bool { return !w.deliveredAt(t, worker.Id, msg.Id).IsZero() })
		if got := lines(t, log); len(got) != 1 || strings.HasPrefix(got[0], "frame:") {
			t.Errorf("what the agent was given: %q", got)
		}
		if d := w.deliveries(t, run.Id, msg.Id); len(d) != 1 || d[0].Via != "turn" {
			t.Errorf("delivery events %+v", d)
		}
	})
	t.Run("managed policy that holds types the doorbell", func(t *testing.T) {
		managed := t.TempDir()
		if err := os.WriteFile(filepath.Join(managed, "managed-settings.json"), []byte(`{"crossSessionInbound":"hold"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		w, log := inboxWorld(t, nil, func(c *daemon.AgentConfig) {
			ca := claude.New()
			ca.ManagedDir = managed
			c.Adapters = []adapter.Adapter{ca, agy.New(), opencode.New()}
		})
		run := startRun(t, w.device.Mesh, "")
		_, asLead := w.root(t, run.Id)
		worker := w.spawnUnder(t, asLead, "held-by-policy", &v1.SpawnAgentRequest{})
		msg := send(t, asLead, worker.Id, v1.MessageType_MESSAGE_TYPE_INFO, "fyi")
		waitFor(t, "the delivery", func() bool { return !w.deliveredAt(t, worker.Id, msg.Id).IsZero() })
		if got := lines(t, log); len(got) != 1 || strings.HasPrefix(got[0], "frame:") {
			t.Errorf("what the agent was given: %q", got)
		}
	})
	t.Run("a frame the session holds is typed after confirm_after, and later doorbells are typed at once", func(t *testing.T) {
		w, log := inboxWorld(t, map[string]string{"FAKECLAUDE_INBOX_HOLD": "1"})
		w.set(t, "delivery.confirm_after", 1)
		run := startRun(t, w.device.Mesh, "")
		_, asLead := w.root(t, run.Id)
		worker := w.spawnUnder(t, asLead, "held-frame", &v1.SpawnAgentRequest{})
		first := send(t, asLead, worker.Id, v1.MessageType_MESSAGE_TYPE_INFO, "one")
		waitFor(t, "the first delivery", func() bool { return !w.deliveredAt(t, worker.Id, first.Id).IsZero() })
		w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
		second := send(t, asLead, worker.Id, v1.MessageType_MESSAGE_TYPE_INFO, "two")
		waitFor(t, "the second delivery", func() bool { return !w.deliveredAt(t, worker.Id, second.Id).IsZero() })
		got := lines(t, log)
		var frames, typed int
		for _, l := range got {
			if strings.HasPrefix(l, "frame:") {
				frames++
			} else if strings.HasPrefix(l, "📬") {
				typed++
			}
		}
		if frames != 1 || typed != 2 {
			t.Errorf("one frame (held) and two typed doorbells expected: %q", got)
		}
		if d := w.deliveries(t, run.Id, first.Id); len(d) != 1 || d[0].Via != "turn" || d[0].Tries != 2 {
			t.Errorf("first: %+v", d)
		}
		if d := w.deliveries(t, run.Id, second.Id); len(d) != 1 || d[0].Tries != 1 {
			t.Errorf("second: %+v", d)
		}
	})
}

func TestABusySessionIsGivenLongerBeforeItsFrameIsTakenForLost(t *testing.T) {
	// A session that holds the frame but keeps drawing looks busy: the daemon waits up to three times
	// confirm_after before typing a second doorbell into it.
	w, log := inboxWorld(t, map[string]string{"FAKECLAUDE_INBOX_HOLD": "1", "FAKECLAUDE_SPIN": "1"})
	w.set(t, "delivery.confirm_after", 1)
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "spinning", &v1.SpawnAgentRequest{})
	sent := time.Now()
	msg := send(t, asLead, worker.Id, v1.MessageType_MESSAGE_TYPE_INFO, "fyi")
	waitFor(t, "the delivery", func() bool { return !w.deliveredAt(t, worker.Id, msg.Id).IsZero() })
	if waited := w.deliveredAt(t, worker.Id, msg.Id).Sub(sent); waited < 2500*time.Millisecond {
		t.Errorf("the frame was taken for lost after %s, while the session was drawing", waited)
	}
	if got := lines(t, log); len(got) != 2 || !strings.HasPrefix(got[0], "frame:") || !strings.HasPrefix(got[1], "📬") {
		t.Errorf("what the agent was given: %q", got)
	}
}

func TestRingTriesCapsTheSocketWriteAndItsTypedFallbackTogether(t *testing.T) {
	// One try allowed and the frame was written: a frame the session never takes ends the tries, so no
	// doorbell is typed after it.
	w, log := inboxWorld(t, map[string]string{"FAKECLAUDE_INBOX_HOLD": "1"})
	w.set(t, "delivery.confirm_after", 1)
	w.set(t, "delivery.ring_tries", 1)
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "capped", &v1.SpawnAgentRequest{})
	msg := send(t, asLead, worker.Id, v1.MessageType_MESSAGE_TYPE_INFO, "fyi")
	waitFor(t, "the doorbell given up", func() bool {
		return w.count(t, run.Id, func(ev *v1.Event) bool { return ev.GetDoorbellFailed() != nil }) > 0
	})
	time.Sleep(2500 * time.Millisecond)
	got := lines(t, log)
	if len(got) != 1 || !strings.HasPrefix(got[0], "frame:") {
		t.Errorf("what the agent was given: %q, want the one frame", got)
	}
	if n := w.count(t, run.Id, func(ev *v1.Event) bool { return ev.GetDoorbellFailed() != nil }); n != 1 {
		t.Errorf("%d DoorbellFailed events, want 1", n)
	}
	if !w.deliveredAt(t, worker.Id, msg.Id).IsZero() {
		t.Error("the message counts as delivered")
	}
}

func TestAMessageTheAgentReadsBeforeAnyOtherSignIsDeliveredViaRead(t *testing.T) {
	w, log := boxWorld(t, map[string]string{"FAKECLAUDE_DROP": "1"})
	w.set(t, "delivery.confirm_after", 30)
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "reader", &v1.SpawnAgentRequest{})
	msg := send(t, asLead, worker.Id, v1.MessageType_MESSAGE_TYPE_INFO, "fyi")
	waitFor(t, "the doorbell typed", func() bool { return len(doorbells(lines(t, log))) == 1 })
	// The agent never acts on the doorbell, but reads its inbox (a hook, or it saw the mail another way).
	if _, err := w.as(w.token(t, worker.Id)).ReadInbox(context.Background(), connect.NewRequest(&v1.ReadInboxRequest{})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the delivery", func() bool { return !w.deliveredAt(t, worker.Id, msg.Id).IsZero() })
	if got := w.deliveries(t, run.Id, msg.Id); len(got) != 1 || got[0].Via != "read" || got[0].Tries != 1 {
		t.Errorf("delivery events %+v, want one via read after 1 try", got)
	}
	if rung := doorbells(lines(t, log)); len(rung) != 1 {
		t.Errorf("doorbells typed: %q", rung)
	}
}
