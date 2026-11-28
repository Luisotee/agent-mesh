package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/Luisotee/agent-mesh/daemon/internal/testutil/shortdir"
)

func TestModelLimitsPersistAcrossDaemonRestart(t *testing.T) {
	ctx := context.Background()
	state := shortdir.State(t)
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
	leadRes, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
		RunId: run.Id, Repo: repo, Slug: "lead", Role: v1.AgentRole_AGENT_ROLE_ROOT,
	}))
	if err != nil {
		t.Fatal(err)
	}
	lead := w.waitState(leadRes.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	rootCli := agentClient(t, w, lead)
	worker := w.spawnClaude(t, run, "w", v1.AgentRole_AGENT_ROLE_WORKER)

	// Spawn a planner with rootCli and have it send a plan so we can test ApproveAgent under reserve.
	plannerRes, err := rootCli.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
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

	// Also spawn & retire a worker to test ResumeAgent(--task vs no task) under reserve, plus an ASKER
	// (read-only role must also respect reserve on SendMessage(TASK) and ResumeAgent(--task)).
	retRes, err := rootCli.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
		RunId: run.Id, Repo: repo, Slug: "ret", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_WORKER,
	}))
	if err != nil {
		t.Fatal(err)
	}
	retWorker := w.waitState(retRes.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	retWorker = retireStoppedWorker(t, w, retWorker)

	askerRes, err := rootCli.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
		RunId: run.Id, Repo: repo, Slug: "ask", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_ASKER,
	}))
	if err != nil {
		t.Fatal(err)
	}
	asker := w.waitState(askerRes.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)

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
	if _, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
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
	if _, err := rootCli.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{
		Meta: meta(""), To: asker.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "blocked asker task",
	})); code(err) != connect.CodeResourceExhausted {
		t.Errorf("SendMessage(TASK) to ASKER under reserve: %v, want ResourceExhausted", err)
	}
	stoppedAsker := retireStoppedWorker(t, w, asker)
	if _, err := rootCli.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Meta: meta(""), Agent: stoppedAsker.Id, Task: "blocked asker resume task",
	})); code(err) != connect.CodeResourceExhausted {
		t.Errorf("ResumeAgent(--task) to ASKER under reserve: %v, want ResourceExhausted", err)
	}

	// Answers STILL go through while at reserve, plain ResumeAgent succeeds, and a device task to ROOT succeeds.
	if _, err := w.device.Mesh.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{
		Meta: meta(""), To: worker.Id, Type: v1.MessageType_MESSAGE_TYPE_ANSWER, Body: "allowed answer",
	})); err != nil {
		t.Errorf("SendMessage(ANSWER) blocked by reserve: %v", err)
	}
	if _, err := w.device.Mesh.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{
		Meta: meta(""), To: lead.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "user task to lead",
	})); err != nil {
		t.Errorf("SendMessage(TASK) to ROOT blocked by reserve: %v", err)
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
			"fetchedAtMs": time.Now().UnixMilli(),
			"seven_day":   map[string]any{"utilization": 94.0, "resets_at": time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)},
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

func TestClaudeUsageStaleBoundAndFamilyWindowMatching(t *testing.T) {
	ctx := context.Background()
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Usage = &daemon.UsageOps{
			FetchClaudeUsage: func(context.Context) (*hook.ClaudeUsageSnapshot, error) {
				return nil, os.ErrNotExist
			},
		}
	})
	run := startRun(t, w.device.Mesh, "")

	// Stale claude-usage.json (> 15m old, no resets_at) and stale .claude.json cachedUsageUtilization
	// (> 15m fetchedAtMs) must both be ignored: unreadable quota counts as available.
	staleFetched := float64(time.Now().Add(-20 * time.Minute).Unix())
	diskBytes, _ := json.Marshal(hook.ClaudeUsageSnapshot{
		Windows: map[string]hook.ClaudeUsageWindow{"seven_day": {Used: 96}},
		Fetched: staleFetched,
		Source:  "statusline",
	})
	usagePath := statedir.Paths{Dir: w.state}.ClaudeUsage()
	if err := os.WriteFile(usagePath, diskBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	cacheBytes, _ := json.Marshal(map[string]any{
		"cachedUsageUtilization": map[string]any{
			"fetchedAtMs": time.Now().Add(-25 * time.Minute).UnixMilli(),
			"utilization": map[string]any{
				"seven_day": map[string]any{"utilization": 95.0},
			},
		},
	})
	if err := os.WriteFile(filepath.Join(w.home, ".claude.json"), cacheBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if u, err := w.device.Mesh.GetUsage(ctx, connect.NewRequest(&v1.GetUsageRequest{})); err != nil || u.Msg.Claude != nil {
		t.Fatalf("stale Claude usage not discarded: %+v, err=%v", u, err)
	}
	if err := w.d.ReserveGuard(ctx, w.d.Store().Read(ctx), run.Id, "claude", "sonnet", "high", "test"); err != nil {
		t.Errorf("stale Claude reading blocked ReserveGuard: %v", err)
	}

	// .claude.json without fetchedAtMs must be treated as stale even when file mtime is fresh (bin/mesh:3369).
	noFetchedAt, _ := json.Marshal(map[string]any{
		"cachedUsageUtilization": map[string]any{
			"utilization": map[string]any{
				"seven_day": map[string]any{"utilization": 95.0},
			},
		},
	})
	if err := os.WriteFile(filepath.Join(w.home, ".claude.json"), noFetchedAt, 0o600); err != nil {
		t.Fatal(err)
	}
	if u, err := w.device.Mesh.GetUsage(ctx, connect.NewRequest(&v1.GetUsageRequest{})); err != nil || u.Msg.Claude != nil {
		t.Fatalf(".claude.json without fetchedAtMs not treated as stale: %+v, err=%v", u, err)
	}

	// Family-specific windows (seven_day_opus / seven_day_sonnet) apply only to matching models, never empty model.
	freshBytes, _ := json.Marshal(hook.ClaudeUsageSnapshot{
		Windows: map[string]hook.ClaudeUsageWindow{"seven_day_opus": {Used: 97}},
		Fetched: float64(time.Now().Unix()),
		Source:  "statusline",
	})
	if err := os.WriteFile(usagePath, freshBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.d.ReserveGuard(ctx, w.d.Store().Read(ctx), run.Id, "claude", "", "high", "test"); err != nil {
		t.Errorf("seven_day_opus wrongly blocked empty model: %v", err)
	}
	if err := w.d.ReserveGuard(ctx, w.d.Store().Read(ctx), run.Id, "claude", "sonnet", "high", "test"); err != nil {
		t.Errorf("seven_day_opus wrongly blocked sonnet: %v", err)
	}
	if err := w.d.ReserveGuard(ctx, w.d.Store().Read(ctx), run.Id, "claude", "opus", "high", "test"); code(err) != connect.CodeResourceExhausted {
		t.Errorf("seven_day_opus did not block opus: %v", err)
	}
}

func TestClaudeReserveAlertsOnlyToRunsWithLiveClaudeAgent(t *testing.T) {
	ctx := context.Background()
	reset := time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)
	var atReserve atomic.Bool
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.ReserveEvery = 40 * time.Millisecond
		c.Usage = &daemon.UsageOps{
			ReadAgyUsage: func(context.Context) (string, error) {
				return "Gemini 4 Hydrogen\tHydrogen\t90%\t1h\n", nil
			},
			ReadAgyModels: func(context.Context) (string, error) {
				return "gemini-4-hydrogen-high\tGemini 4 Hydrogen (High)\n", nil
			},
			FetchClaudeUsage: func(context.Context) (*hook.ClaudeUsageSnapshot, error) {
				opusUsed := 20.0
				if atReserve.Load() {
					opusUsed = 96.0
				}
				return &hook.ClaudeUsageSnapshot{
					Windows: map[string]hook.ClaudeUsageWindow{
						"five_hour":      {Used: 50, Resets: reset},
						"seven_day_opus": {Used: opusUsed, Resets: reset},
					},
					Source: "live",
				}, nil
			},
		}
	})
	repo := gitRepo(t)

	// Run 1 has only a Claude root (no non-root Claude workers): must NOT receive Claude alerts.
	runRootOnly := startRun(t, w.device.Mesh, "")
	leadRootOnly := w.waitState(func() string {
		res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
			RunId: runRootOnly.Id, Repo: repo, Slug: "lead-ro", Role: v1.AgentRole_AGENT_ROLE_ROOT,
		}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.Agent.Id
	}(), v1.AgentState_AGENT_STATE_IDLE)

	// Run 2 has a Claude root + a Claude sonnet worker: seven_day_opus does not apply to sonnet.
	runSonnet := startRun(t, w.device.Mesh, "")
	leadSonnet := w.spawnClaude(t, runSonnet, "lead-so", v1.AgentRole_AGENT_ROLE_ROOT)
	sonnetRes, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
		RunId: runSonnet.Id, Repo: repo, Slug: "w-sonnet", Model: "sonnet", Effort: "high", Role: v1.AgentRole_AGENT_ROLE_WORKER,
	}))
	if err != nil {
		t.Fatal(err)
	}
	w.waitState(sonnetRes.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)

	// Run 3 has a Claude root + a Claude opus worker: receives seven_day_opus alert once quota crosses reserve.
	runOpus := startRun(t, w.device.Mesh, "")
	leadOpus := w.spawnClaude(t, runOpus, "lead-op", v1.AgentRole_AGENT_ROLE_ROOT)
	opusRes, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
		RunId: runOpus.Id, Repo: repo, Slug: "w-opus", Model: "opus", Effort: "high", Role: v1.AgentRole_AGENT_ROLE_WORKER,
	}))
	if err != nil {
		t.Fatal(err)
	}
	w.waitState(opusRes.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)

	// Run 4 has only agy agents, no Claude workers.
	runAgy := startRun(t, w.device.Mesh, "")
	leadAgyRes, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
		RunId: runAgy.Id, Repo: repo, Slug: "lead-a", Kind: "agy", Role: v1.AgentRole_AGENT_ROLE_ROOT,
	}))
	if err != nil {
		t.Fatal(err)
	}
	leadAgy := w.waitState(leadAgyRes.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)

	atReserve.Store(true)
	_ = os.Remove(statedir.Paths{Dir: w.state}.ClaudeUsage())
	waitFor(t, "claude opus reserve alert in opus run lead", func() bool {
		return w.infoTo(t, leadOpus, "[mesh alert reserve claude-seven_day_opus]") != nil
	})
	if msg := w.infoTo(t, leadRootOnly, "[mesh alert reserve claude-"); msg != nil {
		t.Errorf("root-only run received Claude reserve alert: %+v", msg)
	}
	if msg := w.infoTo(t, leadSonnet, "[mesh alert reserve claude-seven_day_opus]"); msg != nil {
		t.Errorf("sonnet-only run received seven_day_opus reserve alert: %+v", msg)
	}
	if msg := w.infoTo(t, leadAgy, "[mesh alert reserve claude-"); msg != nil {
		t.Errorf("agy-only run received Claude reserve alert: %+v", msg)
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
	// It says which row the 80% is: the shared pool, not neon's own.
	if strongPick.Group != "Gemini and Claude models" || strongPick.Label != "Shared" {
		t.Errorf("strong tier pick draws on %q / %q, want the shared pool", strongPick.Group, strongPick.Label)
	}

	// Spawning on tier strong skips neon and lands directly on hydrogen; explicit model neon is refused by ReserveGuard.
	res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
		RunId: run.Id, Repo: repo, Slug: "agy-h", Kind: "agy", Tier: "strong", Role: v1.AgentRole_AGENT_ROLE_WORKER,
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	if a.Model != "hydrogen" {
		t.Errorf("spawned agy model = %q, want hydrogen", a.Model)
	}
	if _, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
		RunId: run.Id, Repo: repo, Slug: "agy-n", Kind: "agy", Model: "neon", Effort: "high", Role: v1.AgentRole_AGENT_ROLE_WORKER,
	})); code(err) != connect.CodeResourceExhausted {
		t.Errorf("explicit spent agy model: %v, want ResourceExhausted", err)
	}
}

func TestPeriodicReserveAlertPostedOnceToRunLead(t *testing.T) {
	ctx := context.Background()
	reset := time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)
	var atReserve atomic.Bool
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.ReserveEvery = 40 * time.Millisecond
		c.Usage = &daemon.UsageOps{
			FetchClaudeUsage: func(context.Context) (*hook.ClaudeUsageSnapshot, error) {
				used := 20.0
				if atReserve.Load() {
					used = 96.0
				}
				return &hook.ClaudeUsageSnapshot{
					Windows: map[string]hook.ClaudeUsageWindow{
						"five_hour": {Used: used, Resets: reset},
					},
					Source: "live",
				}, nil
			},
		}
	})
	run := startRun(t, w.device.Mesh, "")
	repo := gitRepo(t)
	leadRes, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
		RunId: run.Id, Repo: repo, Slug: "lead", Role: v1.AgentRole_AGENT_ROLE_ROOT,
	}))
	if err != nil {
		t.Fatal(err)
	}
	lead := w.waitState(leadRes.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	_ = w.spawnClaude(t, run, "w", v1.AgentRole_AGENT_ROLE_WORKER)

	atReserve.Store(true)
	_ = os.Remove(statedir.Paths{Dir: w.state}.ClaudeUsage())
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

func TestAgyStaleUsageAndFailedModelLookupRecovery(t *testing.T) {
	ctx := context.Background()
	var usageResp atomic.Value
	var usageFail atomic.Bool
	var modelsFail atomic.Bool
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Usage = &daemon.UsageOps{
			DisableClaudeNetwork: true,
			ReadAgyUsage: func(context.Context) (string, error) {
				if usageFail.Load() {
					return "", os.ErrNotExist
				}
				v, _ := usageResp.Load().(string)
				return v, nil
			},
			ReadAgyModels: func(context.Context) (string, error) {
				if modelsFail.Load() {
					return "", os.ErrNotExist
				}
				return "gemini-4-neon-high\tGemini 4 Neon (High)\n" +
					"gemini-4-hydrogen-high\tGemini 4 Hydrogen (High)\n", nil
			},
		}
	})
	w.set(t, "tiers.agy.strong.model", []any{"neon", "hydrogen"})
	run := startRun(t, w.device.Mesh, "")
	repo := gitRepo(t)

	// Write a stale agy-usage.txt (> 15m old) showing Neon at 5%: while ReadAgyUsage fails, it must be ignored.
	agyPath := statedir.Paths{Dir: w.state}.AgyUsage()
	if err := os.WriteFile(agyPath, []byte("Gemini 4 Neon\tNeon\t5%\t1h\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * time.Minute)
	if err := os.Chtimes(agyPath, old, old); err != nil {
		t.Fatal(err)
	}
	usageFail.Store(true)
	uStale, err := w.device.Mesh.GetUsage(ctx, connect.NewRequest(&v1.GetUsageRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(uStale.Msg.AgyRows) != 0 {
		t.Errorf("stale agy-usage.txt still trusted: %+v", uStale.Msg.AgyRows)
	}
	if err := w.d.ReserveGuard(ctx, w.d.Store().Read(ctx), run.Id, "agy", "neon", "high", "test"); err != nil {
		t.Errorf("stale agy usage blocked ReserveGuard: %v", err)
	}

	// Transient ReadAgyModels failure must not be cached permanently: once it recovers, ReserveGuard catches neon.
	usageFail.Store(false)
	usageResp.Store("Gemini 4 Neon\tNeon\t5%\t1h\nGemini and Claude models\tShared\t80%\t2h\n")
	modelsFail.Store(true)
	_, _ = w.device.Mesh.GetUsage(ctx, connect.NewRequest(&v1.GetUsageRequest{Refresh: true}))
	modelsFail.Store(false)
	if err := w.d.ReserveGuard(ctx, w.d.Store().Read(ctx), run.Id, "agy", "neon", "high", "test"); code(err) != connect.CodeResourceExhausted {
		t.Errorf("after ReadAgyModels recovery, ReserveGuard(neon) = %v, want ResourceExhausted", err)
	}
	_ = repo
}

func TestWarmUsageSkipsAgyWhenQuotaStopDisabledAndRunInvalid(t *testing.T) {
	ctx := context.Background()
	var agyUsageCalls atomic.Int32
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Usage = &daemon.UsageOps{
			DisableClaudeNetwork: true,
			ReadAgyUsage: func(context.Context) (string, error) {
				agyUsageCalls.Add(1)
				return "Gemini 4 Hydrogen\tHydrogen\t90%\t1h\n", nil
			},
			ReadAgyModels: func(context.Context) (string, error) {
				return "gemini-4-hydrogen-high\tGemini 4 Hydrogen (High)\n", nil
			},
		}
	})
	repo := gitRepo(t)
	run := startRun(t, w.device.Mesh, "")

	// Invalid/missing run stops SpawnAgent before warmUsage calls ReadAgyUsage even with quota_stop < 100.
	if _, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
		RunId: "r_missing", Repo: repo, Slug: "miss", Kind: "agy", Tier: "strong", Role: v1.AgentRole_AGENT_ROLE_WORKER,
	})); code(err) != connect.CodeNotFound {
		t.Fatalf("missing run code = %v, want NotFound", err)
	}
	if agyUsageCalls.Load() != 0 {
		t.Errorf("ReadAgyUsage called %d times on invalid run, want 0", agyUsageCalls.Load())
	}

	// With quota_stop = 100, SpawnAgent/SendMessage/SwitchAgent/ResumeAgent never run `agy -p /usage`.
	w.set(t, "quota_stop", 100)
	res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
		RunId: run.Id, Repo: repo, Slug: "agy-w", Kind: "agy", Tier: "strong", Role: v1.AgentRole_AGENT_ROLE_WORKER,
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	if _, err := w.device.Mesh.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{
		Meta: meta(""), To: a.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "Direct task\ndo work",
	})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "agy session id", func() bool {
		got, err := w.device.Mesh.GetAgent(ctx, connect.NewRequest(&v1.GetAgentRequest{Agent: a.Id}))
		if err != nil || got.Msg.Agent.SessionId == "" || got.Msg.Agent.State != v1.AgentState_AGENT_STATE_IDLE {
			return false
		}
		a = got.Msg.Agent
		return true
	})
	oldTerm := a.TerminalId
	if _, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{
		Meta: meta(""), Agent: a.Id, Effort: "low",
	})); err != nil {
		t.Fatal(err)
	}
	a = w.waitReopened(t, a.Id, oldTerm)
	a = retireStoppedWorker(t, w, a)
	if _, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Meta: meta(""), Agent: a.Id, Task: "more",
	})); err != nil {
		t.Fatal(err)
	}
	if agyUsageCalls.Load() != 0 {
		t.Errorf("ReadAgyUsage called %d times with quota_stop=100, want 0", agyUsageCalls.Load())
	}
}

// An agy model at the reserve is told once per window even when each reading words the reset differently (the
// estimate of when it resets then moves from pass to pass): the lead had the same alert every half hour.
func TestAnAgyReserveAlertIsSentOncePerWindowWhateverTheResetEstimate(t *testing.T) {
	ctx := context.Background()
	var reads atomic.Int32
	var spent atomic.Bool // the agent is started while the model has quota left
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.ReserveEvery = 40 * time.Millisecond
		c.Usage = &daemon.UsageOps{
			DisableClaudeNetwork: true,
			ReadAgyUsage: func(context.Context) (string, error) {
				n := int(reads.Add(1))
				left := 80
				if spent.Load() {
					left = 2
				}
				return fmt.Sprintf("Gemini 4 Hydrogen\tHydrogen\t%d%%\t%dh%dm\n", left, 3+n%2, (n*17)%60), nil
			},
			ReadAgyModels: func(context.Context) (string, error) {
				return "gemini-4-hydrogen-high\tGemini 4 Hydrogen (High)\n", nil
			},
		}
	})
	run := startRun(t, w.device.Mesh, "")
	leadRes, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
		RunId: run.Id, Repo: gitRepo(t), Slug: "lead", Role: v1.AgentRole_AGENT_ROLE_ROOT}))
	if err != nil {
		t.Fatal(err)
	}
	lead := w.waitState(leadRes.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker",
		RunId: run.Id, Repo: gitRepo(t), Slug: "agy-h", Kind: "agy", Model: "hydrogen", Effort: "high", Role: v1.AgentRole_AGENT_ROLE_WORKER}))
	if err != nil {
		t.Fatal(err)
	}
	w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	spent.Store(true)
	_ = os.Remove(statedir.Paths{Dir: w.state}.AgyUsage())
	waitFor(t, "an agy reserve alert", func() bool { return w.infoTo(t, lead, "[mesh alert reserve agy-") != nil })
	for i := 0; i < 8; i++ { // several passes, each with a new reading
		time.Sleep(80 * time.Millisecond)
		_ = os.Remove(statedir.Paths{Dir: w.state}.AgyUsage())
	}
	msgs, err := w.d.Store().Read(ctx).Inbox(lead.Id, true)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range msgs {
		if strings.Contains(m.Body, "[mesh alert reserve agy-") {
			n++
		}
	}
	if n != 1 || reads.Load() < 3 {
		t.Errorf("%d agy reserve alerts over %d readings, want 1", n, reads.Load())
	}
}
