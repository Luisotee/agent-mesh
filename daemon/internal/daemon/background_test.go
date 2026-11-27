package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/proc"
)

// nudges are the nudges an agent got.
func (w *reviewWorld) nudges(t *testing.T, a *v1.Agent) int {
	t.Helper()
	in, err := w.d.Store().Read(context.Background()).Inbox(a.Id, true)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range in {
		if strings.HasPrefix(m.Body, "[mesh nudge]") {
			n++
		}
	}
	return n
}

// A Claude worker that ends its turn while a background command runs is waiting on it, whether or not it said so:
// no nudge while the command runs, and the usual one once it has ended and the worker still reported nothing.
func TestAWorkerWithABackgroundCommandRunningIsNotNudged(t *testing.T) {
	stop := filepath.Join(t.TempDir(), "stop")
	w := startReviews(t, func(c *daemon.AgentConfig) {
		c.Env["FAKECLAUDE_BG_STOP"] = stop
		c.Sys.ProcTable = proc.Table // the processes of this machine: the fake's job is a real child
	})
	w.set(t, "accountability.nudge_after", 3)
	wk := w.worker(t, "wk")
	if _, err := w.asRoot.SendMessage(context.Background(), connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: wk.Id,
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "Direct task (no plan): check it"})); err != nil {
		t.Fatal(err)
	}
	w.quiet(t, wk.Id)                     // took its task and ended the turn
	readInbox(t, w.as(w.token(t, wk.Id))) // read it, and reported nothing
	w.typeInto(w.agent(wk.Id), "/bg\r")
	waitFor(t, "the command started", func() bool { return strings.Contains(w.screen(t, wk), "background command started") })
	w.waitState(wk.Id, v1.AgentState_AGENT_STATE_IDLE)

	time.Sleep(8 * time.Second) // well past nudge_after
	if n := w.nudges(t, wk); n != 0 {
		t.Fatalf("a worker with a background command running was nudged %d times", n)
	}

	if err := os.WriteFile(stop, nil, 0o600); err != nil { // the command ends
		t.Fatal(err)
	}
	waitFor(t, "the nudge once the command is over", func() bool { return w.nudges(t, wk) == 1 })
}

// A background command the agent forgot (a dev server, a watcher) is not waited on for hours: past the grace
// from the agent's last activity it is nudged once, with a nudge that names the command, though it still runs.
func TestAWorkerWithAForgottenBackgroundCommandIsNudgedOnceAboutIt(t *testing.T) {
	old := daemon.SetBackgroundGrace(14 * time.Second)
	t.Cleanup(func() { daemon.SetBackgroundGrace(old) })
	stop := filepath.Join(t.TempDir(), "stop")
	w := startReviews(t, func(c *daemon.AgentConfig) {
		c.Env["FAKECLAUDE_BG_STOP"] = stop
		c.Sys.ProcTable = proc.Table
	})
	t.Cleanup(func() { _ = os.WriteFile(stop, nil, 0o600) })
	w.set(t, "accountability.nudge_after", 3)
	wk := w.worker(t, "wk")
	if _, err := w.asRoot.SendMessage(context.Background(), connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: wk.Id,
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "Direct task (no plan): check it"})); err != nil {
		t.Fatal(err)
	}
	w.quiet(t, wk.Id)
	readInbox(t, w.as(w.token(t, wk.Id)))
	w.typeInto(w.agent(wk.Id), "/bg\r")
	waitFor(t, "the command started", func() bool { return strings.Contains(w.screen(t, wk), "background command started") })
	w.waitState(wk.Id, v1.AgentState_AGENT_STATE_IDLE)

	waitFor(t, "the nudge about the command", func() bool { return w.nudges(t, wk) >= 1 })
	in, _ := w.d.Store().Read(context.Background()).Inbox(wk.Id, true)
	found := false
	for _, m := range in {
		if strings.HasPrefix(m.Body, "[mesh nudge]") && strings.Contains(m.Body, "a background command you started still runs") {
			found = true
		}
	}
	if !found {
		t.Errorf("the nudge does not name the background command: %v", in)
	}
	if !strings.Contains(in[len(in)-1].Body, "reports/"+wk.Name+".md") && !anyContains(in, "reports/"+wk.Name+".md") {
		t.Errorf("the nudge does not carry the agent's name: %v", in)
	}
	// The worker ignores it: the silent alert follows nudge_after after its turn ends, not another grace (14 s)
	// later because that turn moved its last activity.
	nudgedAt := time.Now()
	waitFor(t, "the silent alert to the root", func() bool { return w.infoTo(t, w.lead, "[mesh alert silent") != nil })
	if d := time.Since(nudgedAt); d > 10*time.Second {
		t.Errorf("the silent alert came %s after the nudge: the grace held it again", d)
	}
	if n := w.nudges(t, wk); n != 1 {
		t.Errorf("nudged %d times about one command, want once", n)
	}
}

func anyContains(msgs []*v1.Message, s string) bool {
	for _, m := range msgs {
		if strings.HasPrefix(m.Body, "[mesh nudge]") && strings.Contains(m.Body, s) {
			return true
		}
	}
	return false
}

// A declared wait on a background command the daemon can see (a Claude agent's `run_in_background` command or
// Monitor: its end wakes the agent) holds for wait_max, not wait_unseen; once the command has ended with nothing
// reported, the wait is over and the agent is nudged, with the text for a wait.
func TestAWaitOnAVisibleBackgroundCommandHoldsPastWaitUnseen(t *testing.T) {
	// The grace a running command is let run without a wait is shorter than the test: a wait holds past it.
	old := daemon.SetBackgroundGrace(2 * time.Second)
	t.Cleanup(func() { daemon.SetBackgroundGrace(old) })
	stop := filepath.Join(t.TempDir(), "stop")
	w := startReviews(t, func(c *daemon.AgentConfig) {
		c.Env["FAKECLAUDE_BG_STOP"] = stop
		c.Sys.ProcTable = proc.Table
	})
	t.Cleanup(func() { _ = os.WriteFile(stop, nil, 0o600) })
	w.set(t, "accountability.nudge_after", 2)
	w.set(t, "accountability.wait_unseen", 3)
	wk := w.worker(t, "wk")
	if _, err := w.asRoot.SendMessage(context.Background(), connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: wk.Id,
		Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "Direct task (no plan): check it"})); err != nil {
		t.Fatal(err)
	}
	w.quiet(t, wk.Id)
	asWorker := w.as(w.token(t, wk.Id))
	readInbox(t, asWorker)
	w.typeInto(w.agent(wk.Id), "/bg\r")
	waitFor(t, "the command started", func() bool { return strings.Contains(w.screen(t, wk), "background command started") })
	w.waitState(wk.Id, v1.AgentState_AGENT_STATE_IDLE)
	if _, err := asWorker.SendMessage(context.Background(), connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: "parent",
		Type: v1.MessageType_MESSAGE_TYPE_WAIT, Body: "the gate"})); err != nil {
		t.Fatal(err)
	}

	time.Sleep(8 * time.Second) // well past wait_unseen and nudge_after, the command still runs
	if n := w.nudges(t, wk); n != 0 {
		t.Fatalf("a wait on a running background command was nudged %d times", n)
	}
	if err := os.WriteFile(stop, nil, 0o600); err != nil { // the command ends: nobody woke the fake, as in the root's case
		t.Fatal(err)
	}
	waitFor(t, "the nudge once the command is over", func() bool { return w.nudges(t, wk) == 1 })
	in, _ := w.d.Store().Read(context.Background()).Inbox(wk.Id, true)
	if !anyContains(in, "Your declared wait has run its course") {
		t.Errorf("the nudge is not the one for a wait: %v", in)
	}
}
