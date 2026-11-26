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
