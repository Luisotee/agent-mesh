package daemon_test

// The two delivery bugs of the herdr mesh, as tests of the daemon (TODO: "Daemon delivery tests"):
//
//  1. A report must reach the root, whatever its type, and a plan from a worker resumed into planning included;
//     one sent while the root is mid-turn is rung at its next IDLE, never typed into the turn.
//  2. A fresh pane's input-box placeholder must never hold a doorbell. The herdr mesh judged a draft by reading
//     the screen, and read the placeholder as one; the daemon judges typing by client input only.
//
// Adapters get the same two tests as they land: the Claude adapter is the only one so far, tested through the
// fake claude. OpenCode's first (TODO).

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/termclient"
)

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// screen is what an agent's terminal shows now: the host's repaint, as a client attaching sees it.
func (w *reopenWorld) screen(t *testing.T, a *v1.Agent) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := w.openTerminal(ctx, a, false, &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_REPAINT})
	defer s.Close()
	var out strings.Builder
	for {
		f, err := s.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if o := f.GetOutput(); o != nil {
			out.Write(o.Data)
		}
		if sc := f.GetScreen(); sc != nil {
			out.Write(sc.Data)
		}
		if f.GetReplayDone() != nil {
			return out.String()
		}
	}
}

// attach opens a client on an agent's terminal, ready (a client with input rights counts as someone who may type).
func (w *reopenWorld) attach(t *testing.T, a *v1.Agent, input bool) *termclient.Stream {
	t.Helper()
	ctx := context.Background()
	s := w.openTerminal(ctx, a, input, &termv1.Attach{Mode: termv1.AttachMode_ATTACH_MODE_TAIL})
	for {
		f, err := s.Recv(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if f.GetReplayDone() != nil {
			break
		}
	}
	s.Ready(ctx)
	return s
}

// lastClientInput reports whether the host has counted a keystroke of an attached client on an agent's terminal.
func (w *reopenWorld) lastClientInput(t *testing.T, a *v1.Agent) bool {
	t.Helper()
	return !w.lastClientInputAt(t, a).IsZero()
}

// lastClientInputAt is when the host counted the newest keystroke (zero when none).
func (w *reopenWorld) lastClientInputAt(t *testing.T, a *v1.Agent) time.Time {
	t.Helper()
	for _, tm := range w.terminals(t) {
		if tm.Id == w.agent(a.Id).TerminalId && tm.LastClientInput != nil {
			return tm.LastClientInput.AsTime()
		}
	}
	return time.Time{}
}

// deliveredAt reads when a message's doorbell rang (zero while it hasn't).
func (w *reopenWorld) deliveredAt(t *testing.T, agentID, messageID string) time.Time {
	t.Helper()
	msgs, err := w.d.Store().Read(context.Background()).Inbox(agentID, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.Id == messageID && m.DeliveredAt != nil {
			return m.DeliveredAt.AsTime()
		}
	}
	return time.Time{}
}

// send sends a message from a worker to its parent (as the worker) and returns it.
func (w *reopenWorld) sendToParent(t *testing.T, from *v1.Agent, typ v1.MessageType) *v1.Message {
	t.Helper()
	res, err := w.as(w.token(t, from.Id)).SendMessage(context.Background(), connect.NewRequest(&v1.SendMessageRequest{
		Meta: meta(""), To: "parent", Type: typ, Body: "from " + from.Name}))
	if err != nil {
		t.Fatalf("%s sending a %s to its parent: %v", from.Name, typ, err)
	}
	return res.Msg.Message
}

func TestEveryReportReachesTheRootAndRingsItsDoorbell(t *testing.T) {
	w := startReopen(t)
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "reporter", &v1.SpawnAgentRequest{})

	// A wait is not a report: stored for the root to read, it rings no doorbell (PROTOCOL.md).
	wait := w.sendToParent(t, worker, v1.MessageType_MESSAGE_TYPE_WAIT)
	if wait.ToId != lead.Id {
		t.Fatalf("a wait went to %s, not the root", wait.ToId)
	}
	time.Sleep(time.Second) // many doorbell ticks (the test's spool and delivery delay are 100 and 20 ms)
	if screen := w.screen(t, lead); contains(screen, "📬") {
		t.Fatalf("the root's screen shows a doorbell for a wait:\n%s", screen)
	}
	// Every type a worker reports with (an info too: a note that can't wait). Each one is stored for the root and
	// rung: the root is idle.
	for _, typ := range []v1.MessageType{v1.MessageType_MESSAGE_TYPE_PLAN, v1.MessageType_MESSAGE_TYPE_QUESTION,
		v1.MessageType_MESSAGE_TYPE_DONE, v1.MessageType_MESSAGE_TYPE_FAILED, v1.MessageType_MESSAGE_TYPE_INFO} {
		m := w.sendToParent(t, worker, typ)
		if m.ToId != lead.Id {
			t.Fatalf("a %s went to %s, not the root", typ, m.ToId)
		}
		waitFor(t, "the root's doorbell for a "+typ.String(), func() bool { return !w.deliveredAt(t, lead.Id, m.Id).IsZero() })
		// Wait for the turn it caused to end before the next: each report is its own idle moment.
		w.waitState(lead.Id, v1.AgentState_AGENT_STATE_IDLE)
	}
	inbox, err := asLead.ReadInbox(context.Background(), connect.NewRequest(&v1.ReadInboxRequest{Meta: meta(""), Peek: true}))
	if err != nil || len(inbox.Msg.Messages) != 6 {
		t.Fatalf("the root's inbox: %d messages, %v", len(inbox.Msg.Messages), err)
	}
}

func TestAReportSentWhileTheRootIsWorkingRingsAtItsNextIdle(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "early", &v1.SpawnAgentRequest{})

	// The root is mid-turn (a long one: it waits for a line).
	w.typeInto(lead, "/hold\r")
	w.waitState(lead.Id, v1.AgentState_AGENT_STATE_WORKING)
	m := w.sendToParent(t, worker, v1.MessageType_MESSAGE_TYPE_DONE)
	// Stored at once, and not typed into the turn however long it lasts.
	time.Sleep(3 * time.Second)
	if !w.deliveredAt(t, lead.Id, m.Id).IsZero() {
		t.Fatal("a doorbell was typed into the root's working turn")
	}
	if peek, _ := asLead.ReadInbox(ctx, connect.NewRequest(&v1.ReadInboxRequest{Meta: meta(""), Peek: true})); len(peek.Msg.Messages) != 1 {
		t.Fatalf("the report is not in the root's inbox: %v", peek)
	}
	// The turn ends: the next IDLE rings it, with no other trigger (no new message, no retry by hand).
	w.typeInto(lead, "go on\r")
	waitFor(t, "the doorbell at the root's next idle", func() bool { return !w.deliveredAt(t, lead.Id, m.Id).IsZero() })
	if got := w.waitState(lead.Id, v1.AgentState_AGENT_STATE_IDLE); got.State != v1.AgentState_AGENT_STATE_IDLE {
		t.Errorf("the root %v", got.State)
	}
}

// A worker resumed into planning by a follow-up task (the case the herdr mesh lost) reports its plan to the root,
// and it is rung like any report.
func TestAPlanFromAWorkerResumedIntoPlanningReachesTheRoot(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "again", &v1.SpawnAgentRequest{})
	w.sendToParent(t, worker, v1.MessageType_MESSAGE_TYPE_DONE)
	waitFor(t, "the first report rung", func() bool {
		msgs, _ := w.d.Store().Read(ctx).Undelivered(lead.Id)
		return len(msgs) == 0
	})
	w.waitState(lead.Id, v1.AgentState_AGENT_STATE_IDLE)

	// The root retires it, then follows up with a task it plans: the worker comes back planning, under the deny list.
	if _, err := asLead.RetireAgent(ctx, connect.NewRequest(&v1.RetireAgentRequest{Meta: meta(""), Agent: worker.Id})); err != nil {
		t.Fatal(err)
	}
	if _, err := asLead.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Meta: meta(""), Agent: worker.Id,
		Task: "now add the second feature", Plan: true, Tier: "strong"})); err != nil {
		t.Fatal(err)
	}
	back := w.waitReopened(t, worker.Id, "")
	if back.Phase != v1.AgentPhase_AGENT_PHASE_PLANNING || !back.DenyEdits {
		t.Fatalf("resumed as %v", back)
	}
	plan := w.sendToParent(t, back, v1.MessageType_MESSAGE_TYPE_PLAN)
	waitFor(t, "the plan reaching the root", func() bool { return !w.deliveredAt(t, lead.Id, plan.Id).IsZero() })
	// And the root can approve exactly that plan, which moves the worker to its execution tier.
	ap, err := asLead.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{Agent: worker.Id, Tier: "mid"}))
	if err != nil || ap.Msg.Reopen == nil || ap.Msg.Reopen.Model != "sonnet" {
		t.Fatalf("approving the resumed worker's plan: %v, %v", ap, err)
	}
	waitFor(t, "the approval delivered", func() bool { return w.delivered(t, worker.Id, ap.Msg.Message.Id) })
}

// The fake claude draws Claude Code's placeholder after its prompt, and nobody types: the doorbell rings at once,
// well inside the time a typing user would hold it (TypingQuiet), at the agent's first idle and later.
func TestAFreshPanesPlaceholderNeverHoldsADoorbell(t *testing.T) {
	typingQuiet := 3 * time.Second
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Env["FAKECLAUDE_PLACEHOLDER"] = "1"
		c.TypingQuiet = typingQuiet
	})
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "fresh", &v1.SpawnAgentRequest{})

	// The placeholder is on its screen.
	screen := w.screen(t, worker)
	if want := `Try "fix lint errors"`; !contains(screen, want) {
		t.Fatalf("the fake claude drew no placeholder: %q", screen)
	}
	for _, typ := range []v1.MessageType{v1.MessageType_MESSAGE_TYPE_TASK, v1.MessageType_MESSAGE_TYPE_INFO} {
		sent := time.Now()
		res, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: worker.Id, Type: typ, Body: "hello"}))
		if err != nil {
			t.Fatal(err)
		}
		var rang time.Time
		waitFor(t, "the doorbell over a placeholder", func() bool {
			rang = w.deliveredAt(t, worker.Id, res.Msg.Message.Id)
			return !rang.IsZero()
		})
		if held := rang.Sub(sent); held > typingQuiet/2 {
			t.Errorf("a %s was held %s by a placeholder (a typing user would hold it %s)", typ, held, typingQuiet)
		}
		w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	}
	// A user who is attached and types does hold it: the rule is about client input, not about what is drawn.
	user := w.attach(t, worker, true)
	defer user.Close()
	user.Input(ctx, []byte("x\x7f"))
	waitFor(t, "the keystroke counted", func() bool { return w.lastClientInput(t, worker) })
	res, err := asLead.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: worker.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "while you type"}))
	if err != nil {
		t.Fatal(err)
	}
	// Held for the whole quiet period after the keystroke, then rung once the user has stopped typing, though
	// still attached. Measured from the host's own mark of the keystroke, so a slow machine can't make it flaky.
	typed := w.lastClientInputAt(t, worker)
	var rang time.Time
	waitFor(t, "the doorbell once the user stopped typing", func() bool {
		rang = w.deliveredAt(t, worker.Id, res.Msg.Message.Id)
		return !rang.IsZero()
	})
	if held := rang.Sub(typed); held < typingQuiet-200*time.Millisecond {
		t.Errorf("the doorbell rang %s after a keystroke, inside the %s a typing user holds it", held, typingQuiet)
	}
}
