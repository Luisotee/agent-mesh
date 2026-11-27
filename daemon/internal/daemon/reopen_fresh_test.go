package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/testutil/shortdir"
)

// A plain chat agent switched to another effort before anything was typed in it has no conversation to resume (Claude
// Code writes it with the first prompt: `--resume` exits 1): it is reopened fresh under the same session id, stays
// up, and takes the message that was queued meanwhile.
func TestAnAgentReopenedBeforeItsFirstTurnStartsFreshUnderItsSessionAndTakesTheQueuedMessage(t *testing.T) {
	log := filepath.Join(t.TempDir(), "in.log")
	h, a := chatDaemon(t, map[string]string{"FAKECLAUDE_INPUT_LOG": log, "FAKECLAUDE_LAZY_SESSION": "1"})
	ctx := context.Background()
	sid := a.SessionId
	old := a.TerminalId
	if _, err := h.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Meta: meta("sw-1"), Agent: a.Id, Effort: "low"})); err != nil {
		t.Fatal(err)
	}
	_, queueErr := h.device.Mesh.PromptAgent(ctx, connect.NewRequest(&v1.PromptAgentRequest{Meta: meta("q-1"), Agent: a.Id, Text: "hello"}))
	waitFor(t, "the agent reopened and idle", func() bool {
		got, err := h.device.Mesh.GetAgent(ctx, connect.NewRequest(&v1.GetAgentRequest{Agent: a.Id}))
		if err != nil {
			return false
		}
		cur := got.Msg.Agent
		if cur.State == v1.AgentState_AGENT_STATE_EXITED {
			t.Fatalf("the reopened agent exited: %q (code %d)", cur.StateDetail, cur.ExitCode)
		}
		return cur.TerminalId != "" && cur.TerminalId != old && cur.State == v1.AgentState_AGENT_STATE_IDLE
	})
	got, _ := h.device.Mesh.GetAgent(ctx, connect.NewRequest(&v1.GetAgentRequest{Agent: a.Id}))
	if got.Msg.Agent.SessionId != sid {
		t.Errorf("the session id changed: %q to %q", sid, got.Msg.Agent.SessionId)
	}
	if err := h.promptWhenReady(a.Id, "again"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a message typed into the fresh process", func() bool { return hasLine(typedLines(t, log), "again") })
	if queueErr == nil {
		waitFor(t, "the message queued during the reopen", func() bool { return hasLine(typedLines(t, log), "hello") })
	} else {
		t.Logf("the prompt during the reopen was refused, not queued: %v", queueErr)
	}
}

// A CLI that exits at once says why on its screen, and the agent's state says it too, not a bare code.
func TestAnAgentThatExitsAtOnceShowsWhatItPrinted(t *testing.T) {
	state := shortdir.State(t)
	fail := filepath.Join(t.TempDir(), "fail")
	if err := os.WriteFile(fail, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h := startWithAgents(t, state, t.TempDir(), func(c *daemon.AgentConfig) { c.Env["FAKECLAUDE_FAIL_FILE"] = fail })
	t.Cleanup(func() { stopTermHost(t, state) })
	run := startRun(t, h.device.Mesh, "")
	a := h.spawn(run.Id, gitRepo(t), "crash", "")
	got := h.waitState(a.Id, v1.AgentState_AGENT_STATE_EXITED)
	if got.ExitCode != 3 || !strings.Contains(got.StateDetail, "code 3: ") || !strings.Contains(got.StateDetail, "fakeclaude: startup failed") {
		t.Errorf("exit code %d, state detail %q; want it to say what the CLI printed", got.ExitCode, got.StateDetail)
	}
}

// An agent that had a turn and whose conversation Claude Code has since deleted (it expires transcripts after 30 days)
// is reopened fresh under its id, told it has no memory, and its parent is told; one reopened before its first turn
// loses nothing and is told nothing.
func TestAnAgentWhoseConversationWasDeletedIsToldItStartsWithNoMemoryAndSoIsItsParent(t *testing.T) {
	w := startReopen(t, func(c *daemon.AgentConfig) { c.Env["FAKECLAUDE_LAZY_SESSION"] = "1" })
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, root := w.root(t, run.Id)
	res, err := root.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: gitRepo(t), Slug: "wk", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_WORKER}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	// Before its first turn: reopened fresh, told nothing.
	old := a.TerminalId
	if _, err := root.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Meta: meta("sw-a"), Agent: a.Id, Effort: "low"})); err != nil {
		t.Fatal(err)
	}
	a = w.waitReopened(t, a.Id, old)
	if w.infoTo(t, a, "no longer exists") != nil {
		t.Fatal("an agent reopened before its first turn was told its context is gone")
	}
	// After a turn, its conversation deleted: told, and its parent too.
	w.typeInto(a, "do something\r")
	waitFor(t, "its turn recorded", func() bool { got, _ := w.d.Store().Read(ctx).GetAgent(a.Id); return got.HadTurn })
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	gone, _ := filepath.Glob(filepath.Join(w.home, ".claude", "projects", "*", a.SessionId+".jsonl"))
	if len(gone) == 0 {
		t.Fatal("the fake wrote no conversation")
	}
	for _, f := range gone {
		os.Remove(f)
	}
	old = a.TerminalId
	if _, err := root.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Meta: meta("sw-b"), Agent: a.Id, Effort: "medium"})); err != nil {
		t.Fatal(err)
	}
	a = w.waitReopened(t, a.Id, old)
	waitFor(t, "the agent told its context is gone", func() bool { return w.infoTo(t, a, "no longer exists") != nil })
	waitFor(t, "its parent told", func() bool { return w.infoTo(t, lead, "[mesh alert context "+a.Id+"]") != nil })
}
