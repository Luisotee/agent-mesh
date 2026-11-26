package daemon_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/hook"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
)

func TestModelLimitsPersistAcrossDaemonRestart(t *testing.T) {
	ctx := context.Background()
	state := t.TempDir()
	home := t.TempDir()
	h := startWithAgents(t, state, home, func(c *daemon.AgentConfig) {
		c.RetireEvery = 50 * time.Millisecond
		c.Usage = &daemon.UsageOps{DisableClaudeNetwork: true}
	})
	w := &reopenWorld{agentsHarness: h}
	w.set(t, "tiers.claude.mid.model", []any{"sonnet", "haiku"})
	run := startRun(t, w.device.Mesh, "")
	a := w.spawnClaude(t, run, "persist", v1.AgentRole_AGENT_ROLE_WORKER)
	sid, oldTerm := a.SessionId, a.TerminalId

	// Limit sonnet with auto=off first so limitPass doesn't switch yet; then restart daemon.
	w.set(t, "limits.auto", "off")
	w.typeInto(a, "/limit You've hit your Sonnet limit · resets 11pm (UTC)\r")
	_ = w.waitState(a.Id, v1.AgentState_AGENT_STATE_LIMITED)

	// Verify model_limits row is in SQLite and stop daemon.
	lims, err := w.d.Store().Read(ctx).ModelLimits("claude")
	if err != nil || len(lims) != 1 || lims[0].Model != "sonnet" {
		t.Fatalf("ModelLimits = %+v, err=%v", lims, err)
	}
	h.stop()

	// Restart daemon on the same state: pick() must still skip held sonnet, and limitPass (with auto=switch)
	// must move the stopped/limited agent onto haiku in the same conversation.
	h2 := startWithAgents(t, state, home, func(c *daemon.AgentConfig) {
		c.RetireEvery = 50 * time.Millisecond
		c.Usage = &daemon.UsageOps{DisableClaudeNetwork: true}
	})
	w2 := &reopenWorld{agentsHarness: h2}
	b := w2.spawnClaude(t, run, "second", v1.AgentRole_AGENT_ROLE_WORKER)
	if b.Model != "haiku" {
		t.Errorf("second worker after restart picked %q, want haiku (sonnet held in SQLite)", b.Model)
	}
	w2.set(t, "limits.auto", "switch")
	re := w2.waitReopened(t, a.Id, oldTerm)
	if re.Model != "haiku" || re.SessionId != sid {
		t.Errorf("after restart limitPass: model=%q session=%q, want haiku in %q", re.Model, re.SessionId, sid)
	}
}

func TestClaudeStatusLineFeedsReserveGuardGetUsageAndSetReserve(t *testing.T) {
	ctx := context.Background()
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Usage = &daemon.UsageOps{DisableClaudeNetwork: true}
	})
	run := startRun(t, w.device.Mesh, "")
	repo := gitRepo(t)
	leadRes, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: repo, Slug: "lead", Role: v1.AgentRole_AGENT_ROLE_ROOT,
	}))
	if err != nil {
		t.Fatal(err)
	}
	lead := w.waitState(leadRes.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	rootCli := agentClient(t, w, lead)
	worker := w.spawnClaude(t, run, "w", v1.AgentRole_AGENT_ROLE_WORKER)

	// Spawn a planner with rootCli and have it send a plan so we can test ApproveAgent under reserve.
	plannerRes, err := rootCli.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: repo, Slug: "plan", Tier: "light", ExecTier: "strong", Plan: true, Role: v1.AgentRole_AGENT_ROLE_WORKER,
	}))
	if err != nil {
		t.Fatal(err)
	}
	planner := w.waitState(plannerRes.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	plannerCli := agentClient(t, w, planner)
	if _, err := plannerCli.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{
		Meta: meta(""), To: "parent", Type: v1.MessageType_MESSAGE_TYPE_PLAN, Body: "my plan",
	})); err != nil {
		t.Fatal(err)
	}

	// Also spawn & retire a worker to test ResumeAgent(--task vs no task) under reserve.
	retRes, err := rootCli.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: repo, Slug: "ret", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_WORKER,
	}))
	if err != nil {
		t.Fatal(err)
	}
	retWorker := w.waitState(retRes.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	retWorker = retireStoppedWorker(t, w, retWorker)

	// Trigger fakeclaude's statusLine command (`mesh statusline --state ...`) via /ratelimits.
	reset := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	w.typeInto(worker, `/ratelimits {"five_hour":{"used_percentage":95,"resets_at":"`+reset+`"}}`+"\r")
	waitFor(t, "claude-usage.json", func() bool {
		_, err := os.Stat(statedir.Paths{Dir: w.state}.ClaudeUsage())
		return err == nil
	})

	// GetUsage reads the recorded statusline snapshot.
	u, err := w.device.Mesh.GetUsage(ctx, connect.NewRequest(&v1.GetUsageRequest{}))
	if err != nil {
		t.Fatalf("GetUsage: %v", err)
	}
	if u.Msg.Claude == nil || u.Msg.Claude.Source != "statusline" || len(u.Msg.Claude.AtReserve) != 1 {
		t.Fatalf("GetUsage claude = %+v", u.Msg.Claude)
	}

	// ReserveGuard refuses exported call, SpawnAgent, ApproveAgent, ResumeAgent(--task), SendMessage(TASK).
	if err := w.d.ReserveGuard(ctx, w.d.Store().Read(ctx), run.Id, "claude", "sonnet", "high", "test"); code(err) != connect.CodeResourceExhausted {
		t.Errorf("exported ReserveGuard code = %v, want ResourceExhausted", err)
	}
	if _, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: repo, Slug: "blocked", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_WORKER,
	})); code(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "five-hour window") {
		t.Errorf("SpawnAgent under reserve: %v", err)
	}
	if _, err := rootCli.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{
		Meta: meta(""), Agent: planner.Id,
	})); code(err) != connect.CodeResourceExhausted {
		t.Errorf("ApproveAgent under reserve: %v", err)
	}
	if _, err := rootCli.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Meta: meta(""), Agent: retWorker.Id, Task: "new task blocked",
	})); code(err) != connect.CodeResourceExhausted {
		t.Errorf("ResumeAgent(--task) under reserve: %v", err)
	}
	if _, err := w.device.Mesh.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{
		Meta: meta(""), To: worker.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "blocked task",
	})); code(err) != connect.CodeResourceExhausted {
		t.Errorf("SendMessage(TASK) under reserve: %v", err)
	}

	// Answers STILL go through while at reserve, and plain ResumeAgent (without task) succeeds.
	if _, err := w.device.Mesh.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{
		Meta: meta(""), To: worker.Id, Type: v1.MessageType_MESSAGE_TYPE_ANSWER, Body: "allowed answer",
	})); err != nil {
		t.Errorf("SendMessage(ANSWER) blocked by reserve: %v", err)
	}
	if _, err := rootCli.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Meta: meta(""), Agent: retWorker.Id,
	})); err != nil {
		t.Errorf("ResumeAgent without task blocked by reserve: %v", err)
	}

	// SetReserve off lifts reserve for this run; SetReserve on re-enables it.
	offRes, err := rootCli.SetReserve(ctx, connect.NewRequest(&v1.SetReserveRequest{
		Meta: meta(""), Mode: "off",
	}))
	if err != nil || !offRes.Msg.OverReserve || !offRes.Msg.Run.OverReserve {
		t.Fatalf("SetReserve(off) = %+v, err=%v", offRes, err)
	}
	if _, err := rootCli.ApproveAgent(ctx, connect.NewRequest(&v1.ApproveAgentRequest{
		Meta: meta(""), Agent: planner.Id,
	})); err != nil {
		t.Fatalf("ApproveAgent with reserve off: %v", err)
	}
	if err := w.d.ReserveGuard(ctx, w.d.Store().Read(ctx), run.Id, "claude", "sonnet", "high", "test"); err != nil {
		t.Errorf("ReserveGuard failed with over_reserve=true: %v", err)
	}

	onRes, err := rootCli.SetReserve(ctx, connect.NewRequest(&v1.SetReserveRequest{
		Meta: meta(""), Mode: "on",
	}))
	if err != nil || onRes.Msg.OverReserve {
		t.Fatalf("SetReserve(on) = %+v, err=%v", onRes, err)
	}
	if err := w.d.ReserveGuard(ctx, w.d.Store().Read(ctx), run.Id, "claude", "sonnet", "high", "test"); code(err) != connect.CodeResourceExhausted {
		t.Errorf("ReserveGuard after SetReserve(on) = %v, want ResourceExhausted", err)
	}
}

func TestClaudeCacheAndOAuthFallbacks(t *testing.T) {
	ctx := context.Background()
	var oauthCalls atomic.Int32
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Usage = &daemon.UsageOps{
			FetchClaudeUsage: func(context.Context) (*hook.ClaudeUsageSnapshot, error) {
				if oauthCalls.Add(1) == 1 {
					return nil, os.ErrNotExist // first call fails -> falls back to .claude.json cache
				}
				return &hook.ClaudeUsageSnapshot{
					Windows: map[string]hook.ClaudeUsageWindow{
						"five_hour": {Used: 88, Resets: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
					},
					Source: "live",
				}, nil
			},
		}
	})
	cacheBytes, _ := json.Marshal(map[string]any{
		"cachedUsageUtilization": map[string]any{
			"seven_day": map[string]any{"utilization": 94.0, "resets_at": time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)},
		},
	})
	if err := os.WriteFile(filepath.Join(w.home, ".claude.json"), cacheBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	u1, err := w.device.Mesh.GetUsage(ctx, connect.NewRequest(&v1.GetUsageRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if u1.Msg.Claude == nil || u1.Msg.Claude.Source != "cache" || len(u1.Msg.Claude.AtReserve) != 1 {
		t.Fatalf("cache fallback = %+v", u1.Msg.Claude)
	}

	u2, err := w.device.Mesh.GetUsage(ctx, connect.NewRequest(&v1.GetUsageRequest{Refresh: true}))
	if err != nil {
		t.Fatal(err)
	}
	if u2.Msg.Claude == nil || u2.Msg.Claude.Source != "live" || len(u2.Msg.Claude.AtReserve) != 0 {
		t.Fatalf("live refresh = %+v", u2.Msg.Claude)
	}
}

func TestAgyUsageReserveSkipsSpentModelsOnChain(t *testing.T) {
	ctx := context.Background()
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Env["FAKEAGY_USAGE"] = strings.Join([]string{
			"Gemini 4 Neon\tNeon\t5%\t1h",
			"Gemini and Claude models\tShared\t80%\t2h",
		}, "\n")
		c.Usage = &daemon.UsageOps{DisableClaudeNetwork: true}
	})
	w.set(t, "tiers.agy.strong.model", []any{"neon", "hydrogen"})
	run := startRun(t, w.device.Mesh, "")
	repo := gitRepo(t)

	// GetUsage reads `fakeagy -p /usage` and shows tier strong picking hydrogen (skipping neon at 5% left <= 10%).
	u, err := w.device.Mesh.GetUsage(ctx, connect.NewRequest(&v1.GetUsageRequest{Refresh: true}))
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Msg.AgyRows) != 2 {
		t.Fatalf("AgyRows = %+v", u.Msg.AgyRows)
	}
	var strongPick *v1.AgyTierPick
	for _, tp := range u.Msg.AgyTiers {
		if tp.Tier == "strong" {
			strongPick = tp
		}
	}
	if strongPick == nil || strongPick.Model != "hydrogen" || strongPick.Left != 80 {
		t.Fatalf("strong tier pick = %+v, want hydrogen (80%%)", strongPick)
	}

	// Spawning on tier strong skips neon and lands directly on hydrogen; explicit model neon is refused by ReserveGuard.
	res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: repo, Slug: "agy-h", Kind: "agy", Tier: "strong", Role: v1.AgentRole_AGENT_ROLE_WORKER,
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	if a.Model != "hydrogen" {
		t.Errorf("spawned agy model = %q, want hydrogen", a.Model)
	}
	if _, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: repo, Slug: "agy-n", Kind: "agy", Model: "neon", Effort: "high", Role: v1.AgentRole_AGENT_ROLE_WORKER,
	})); code(err) != connect.CodeResourceExhausted {
		t.Errorf("explicit spent agy model: %v, want ResourceExhausted", err)
	}
}

func TestPeriodicReserveAlertPostedOnceToRunLead(t *testing.T) {
	ctx := context.Background()
	reset := time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.ReserveEvery = 40 * time.Millisecond
		c.Usage = &daemon.UsageOps{
			FetchClaudeUsage: func(context.Context) (*hook.ClaudeUsageSnapshot, error) {
				return &hook.ClaudeUsageSnapshot{
					Windows: map[string]hook.ClaudeUsageWindow{
						"five_hour": {Used: 96, Resets: reset},
					},
					Source: "live",
				}, nil
			},
		}
	})
	run := startRun(t, w.device.Mesh, "")
	repo := gitRepo(t)
	leadRes, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: repo, Slug: "lead", Role: v1.AgentRole_AGENT_ROLE_ROOT,
	}))
	if err != nil {
		t.Fatal(err)
	}
	lead := w.waitState(leadRes.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)

	waitFor(t, "reserve alert in lead inbox", func() bool {
		return w.infoTo(t, lead, "[mesh alert reserve claude-five_hour]") != nil
	})
	// Wait several reserve alert intervals and verify the alert is deduped per reset window.
	time.Sleep(150 * time.Millisecond)
	msgs, err := w.d.Store().Read(ctx).Inbox(lead.Id, true)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, m := range msgs {
		if strings.Contains(m.Body, "[mesh alert reserve claude-five_hour]") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("got %d reserve alerts, want 1 (deduped)", count)
	}
}
