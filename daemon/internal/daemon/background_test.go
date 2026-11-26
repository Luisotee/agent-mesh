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
	old := daemon.SetBackgroundGrace(4 * time.Second)
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
	time.Sleep(3 * time.Second)
	if n := w.nudges(t, wk); n != 1 {
		t.Errorf("nudged %d times about one command, want once", n)
	}
}
