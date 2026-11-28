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
)

// fastPass makes the daemon's background passes (retire, limits) run every 50 ms.
func fastPass(c *daemon.AgentConfig) { c.RetireEvery = 50 * time.Millisecond }

func (w *reopenWorld) spawnClaude(t *testing.T, run *v1.Run, slug string, role v1.AgentRole) *v1.Agent {
	t.Helper()
	res, err := w.device.Mesh.SpawnAgent(context.Background(), connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
		RunId: run.Id, Repo: gitRepo(t), Slug: slug, Tier: "mid", Role: role}))
	if err != nil {
		t.Fatal(err)
	}
	return w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
}

// infoTo is the info messages stored for an agent whose body holds text.
func (w *reopenWorld) infoTo(t *testing.T, a *v1.Agent, text string) *v1.Message {
	t.Helper()
	msgs, err := w.d.Store().Read(context.Background()).Inbox(a.Id, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.Type == v1.MessageType_MESSAGE_TYPE_INFO && strings.Contains(m.Body, text) {
			return m
		}
	}
	return nil
}

// Claude shows a folder-trust dialog for a folder it hasn't seen, and no hook fires before it is answered: the
// worktree is trusted in ~/.claude.json before the launch, so the agent never waits at it.
func TestClaudeWorktreeIsTrustedBeforeItsLaunch(t *testing.T) {
	w := startReopen(t, func(c *daemon.AgentConfig) { c.Env["FAKECLAUDE_TRUST_DIALOG"] = "1" })
	config := filepath.Join(w.home, ".claude.json")
	if err := os.WriteFile(config, []byte(`{"numStartups": 3, "projects": {"/elsewhere": {"hasTrustDialogAccepted": true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run := startRun(t, w.device.Mesh, "")
	a := w.spawnClaude(t, run, "trust", v1.AgentRole_AGENT_ROLE_WORKER) // IDLE: the first hook came, with nobody at the dialog
	b, _ := os.ReadFile(config)
	real, err := filepath.EvalSymlinks(a.Worktree)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"`+real+`"`) || !strings.Contains(string(b), `"numStartups": 3`) ||
		!strings.Contains(string(b), "/elsewhere") {
		t.Errorf("the config after the launch: %s", b)
	}
	if strings.Contains(w.screen(t, a), "Quick safety check") {
		t.Error("the dialog was shown")
	}
}

// A model's own limit moves the agent to the next model of its chain with quota left, in its own conversation, and
// a model at a limit is passed over when the next agent of the tier starts.
func TestClaudeModelLimitSwitchesToTheNextModelAndPicksSkipHeldModels(t *testing.T) {
	w := startReopen(t, fastPass)
	w.set(t, "tiers.claude.mid.model", []any{"sonnet", "haiku", "opus"})
	run := startRun(t, w.device.Mesh, "")
	a := w.spawnClaude(t, run, "lim", v1.AgentRole_AGENT_ROLE_WORKER)
	if a.Model != "sonnet" {
		t.Fatalf("started on %q", a.Model)
	}
	sid, oldTerm := a.SessionId, a.TerminalId
	w.typeInto(a, "/limit You've hit your Sonnet limit · resets 11pm (UTC)\r")
	re := w.waitReopened(t, a.Id, oldTerm)
	if re.Model != "haiku" || re.SessionId != sid {
		t.Fatalf("after the limit: model %q session %q, want haiku in %q", re.Model, re.SessionId, sid)
	}
	launches := w.launches(t, sid)
	if len(launches) != 2 {
		t.Fatalf("launches %v", launches)
	}
	if f, _ := flags(launches[1]); f["--resume"] != sid || f["--model"] != "haiku" {
		t.Errorf("the reopen %v", launches[1])
	}
	if m := w.infoTo(t, re, "hit its usage limit"); m == nil || !strings.Contains(m.Body, "sonnet") || !strings.Contains(m.Body, "haiku") {
		t.Errorf("no note on the move: %v", m)
	}
	// Sonnet is held until 11pm: the next agent of the tier starts on the next model.
	next := w.spawnClaude(t, run, "next", v1.AgentRole_AGENT_ROLE_WORKER)
	if next.Model != "haiku" {
		t.Errorf("a new agent of the tier started on %q, not past the held sonnet", next.Model)
	}
	// And a switch --next skips a held one: from haiku, opus is free.
	sw, err := w.device.Mesh.SwitchAgent(context.Background(), connect.NewRequest(&v1.SwitchAgentRequest{Agent: re.Id, Next: true}))
	if err != nil || sw.Msg.Reopen.Model != "opus" {
		t.Errorf("switch --next: %v %v", sw, err)
	}
}

// A limit on the whole account (or one that names no reset) moves no agent down the chain: it is reopened on its
// own model once the limit is over (here the hold of a limit that named no reset).
func TestClaudeAccountLimitContinuesAtTheReset(t *testing.T) {
	w := startReopen(t, fastPass)
	w.set(t, "tiers.claude.mid.model", []any{"sonnet", "haiku"})
	w.set(t, "limits.hold", 1.0)
	run := startRun(t, w.device.Mesh, "")
	a := w.spawnClaude(t, run, "acct", v1.AgentRole_AGENT_ROLE_WORKER)
	sid, oldTerm := a.SessionId, a.TerminalId
	w.typeInto(a, "/limit You've hit your session limit\r")
	re := w.waitReopened(t, a.Id, oldTerm)
	if re.Model != "sonnet" || re.SessionId != sid {
		t.Fatalf("after the limit: model %q session %q", re.Model, re.SessionId)
	}
	if m := w.infoTo(t, re, "usage limit has reset"); m == nil {
		t.Error("no note to carry on")
	}
	if f, _ := flags(w.launches(t, sid)[1]); f["--model"] != "sonnet" || f["--resume"] != sid {
		t.Errorf("the reopen %v", w.launches(t, sid)[1])
	}
}

// limits.auto: continue never switches model, and off leaves the agent at its limit.
func TestLimitsAutoContinueAndOff(t *testing.T) {
	w := startReopen(t, fastPass)
	w.set(t, "tiers.claude.mid.model", []any{"sonnet", "haiku"})
	w.set(t, "limits.hold", 1.0)
	run := startRun(t, w.device.Mesh, "")

	w.set(t, "limits.auto", "off")
	a := w.spawnClaude(t, run, "off", v1.AgentRole_AGENT_ROLE_WORKER)
	w.typeInto(a, "/limit You've hit your Sonnet limit\r")
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_LIMITED)
	time.Sleep(1800 * time.Millisecond) // past the hold, with several passes
	if cur := w.agent(a.Id); cur.State != v1.AgentState_AGENT_STATE_LIMITED || cur.PendingReopen != nil || len(w.launches(t, a.SessionId)) != 1 {
		t.Errorf("off: %v pending %v launches %d", cur.State, cur.PendingReopen, len(w.launches(t, a.SessionId)))
	}

	w.set(t, "limits.auto", "continue")
	re := w.waitReopened(t, a.Id, a.TerminalId) // the pass now continues it at the end of its hold
	if re.Model != "sonnet" {
		t.Errorf("continue switched to %q", re.Model)
	}

	b := w.spawnClaude(t, run, "cont", v1.AgentRole_AGENT_ROLE_WORKER)
	w.typeInto(b, "/limit You've hit your Opus limit\r")
	rb := w.waitReopened(t, b.Id, b.TerminalId)
	if rb.Model != b.Model {
		t.Errorf("continue moved %q to %q", b.Model, rb.Model)
	}
}

// A root's model is the user's: the pass leaves it at its limit.
func TestLimitPassLeavesARootAlone(t *testing.T) {
	w := startReopen(t, fastPass)
	w.set(t, "limits.hold", 1.0)
	run := startRun(t, w.device.Mesh, "")
	lead, _ := w.root(t, run.Id)
	w.typeInto(lead, "/limit You've hit your limit\r")
	w.waitState(lead.Id, v1.AgentState_AGENT_STATE_LIMITED)
	time.Sleep(1800 * time.Millisecond)
	if cur := w.agent(lead.Id); cur.State != v1.AgentState_AGENT_STATE_LIMITED || cur.PendingReopen != nil {
		t.Errorf("the root was moved: %v %v", cur.State, cur.PendingReopen)
	}
}

// A reviewer's PreToolUse hook denies a publish command a --disallowedTools prefix would let through, and lets
// a read run; a worker is not checked.
func TestClaudeReviewerPublishCommandsAreDenied(t *testing.T) {
	w := startReopen(t)
	run := startRun(t, w.device.Mesh, "")
	rev := w.spawnClaude(t, run, "rev", v1.AgentRole_AGENT_ROLE_REVIEWER)
	for _, c := range []struct{ line, want string }{
		{"/bash git -C . push origin HEAD", `denied: read-only/planning agent may not run "git push"`},
		{`/bash bash -c "gh pr --repo o/r merge 5"`, `may not run "gh pr merge"`},
		{"/bash gh pr create -t x", `may not run "gh pr create"`},
		{"/bash git status", "ran"},
		{"/bash gh pr view 5 --comments", "ran"},
	} {
		w.typeInto(rev, c.line+"\r")
		waitFor(t, c.line, func() bool { return strings.Contains(w.screen(t, rev), c.want) })
		w.waitState(rev.Id, v1.AgentState_AGENT_STATE_IDLE)
	}
	wk := w.spawnClaude(t, run, "wk", v1.AgentRole_AGENT_ROLE_WORKER)
	w.typeInto(wk, "/bash git -C . push\r")
	waitFor(t, "a worker's push is not checked", func() bool { return strings.Contains(w.screen(t, wk), "ran") })
}
