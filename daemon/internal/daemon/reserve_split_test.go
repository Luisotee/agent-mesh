package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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

// claudeWindows writes the statusline reading ReserveGuard reads.
func claudeWindows(t *testing.T, w *reopenWorld, used map[string]float64) {
	t.Helper()
	snap := hook.ClaudeUsageSnapshot{Windows: map[string]hook.ClaudeUsageWindow{}, Fetched: float64(time.Now().Unix()), Source: "statusline"}
	for k, u := range used {
		snap.Windows[k] = hook.ClaudeUsageWindow{Used: u}
	}
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statedir.Paths{Dir: w.state}.ClaudeUsage(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func guard(w *reopenWorld, runID, kind, model string, reviewer bool) error {
	ctx := context.Background()
	return w.d.ReserveGuard(ctx, w.d.Store().Read(ctx), runID, kind, model, "high", "test", reviewer)
}

func startClaudeUsageWorld(t *testing.T) (*reopenWorld, *v1.Run) {
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Usage = &daemon.UsageOps{FetchClaudeUsage: func(context.Context) (*hook.ClaudeUsageSnapshot, error) { return nil, os.ErrNotExist }}
	})
	return w, startRun(t, w.device.Mesh, "")
}

// The five-hour and the weekly windows each have their own reserve, and each refusal names its setting.
func TestTheFiveHourAndTheWeeklyReserveAreSeparate(t *testing.T) {
	w, run := startClaudeUsageWorld(t)
	w.set(t, "quota_stop", 85)
	w.set(t, "quota_stop_weekly", 95)

	claudeWindows(t, w, map[string]float64{"five_hour": 80, "seven_day": 90})
	if err := guard(w, run.Id, "claude", "sonnet", false); err != nil {
		t.Errorf("5h 80 under 85 and weekly 90 under 95 must start: %v", err)
	}
	claudeWindows(t, w, map[string]float64{"five_hour": 86, "seven_day": 50})
	if err := guard(w, run.Id, "claude", "sonnet", false); code(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "quota_stop 85%") {
		t.Errorf("5h 86 over 85: %v, want a refusal naming quota_stop 85%%", err)
	}
	claudeWindows(t, w, map[string]float64{"five_hour": 10, "seven_day": 96})
	if err := guard(w, run.Id, "claude", "sonnet", false); code(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "quota_stop_weekly 95%") {
		t.Errorf("weekly 96 over 95: %v, want a refusal naming quota_stop_weekly 95%%", err)
	}
	// A weekly reserve above the five-hour one lets 90% through that the 5h reserve alone would have held.
	claudeWindows(t, w, map[string]float64{"five_hour": 10, "seven_day": 90})
	if err := guard(w, run.Id, "claude", "sonnet", false); err != nil {
		t.Errorf("weekly 90 under its own 95 was held by the 5h reserve: %v", err)
	}
}

// Above quota_reviews_only.claude (off by default) Claude starts PR reviewers only; at the reserve nothing starts.
func TestAModelPastItsReviewsOnlyLineTakesOnlyPRReviews(t *testing.T) {
	w, run := startClaudeUsageWorld(t)
	claudeWindows(t, w, map[string]float64{"five_hour": 80})
	if err := guard(w, run.Id, "claude", "sonnet", false); err != nil {
		t.Fatalf("a reviews-only line is off by default: %v", err)
	}

	w.set(t, "quota_reviews_only.claude", 75)
	err := guard(w, run.Id, "claude", "sonnet", false)
	if code(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "only PR reviews") || !strings.Contains(err.Error(), "quota_reviews_only.claude 75%") {
		t.Errorf("a task past the reviews-only line: %v", err)
	}
	if err := guard(w, run.Id, "claude", "sonnet", true); err != nil {
		t.Errorf("a reviewer past the reviews-only line is refused: %v", err)
	}
	// Another CLI's line is its own.
	if err := guard(w, run.Id, "agy", "neon", false); err != nil {
		t.Errorf("claude's line held agy: %v", err)
	}

	claudeWindows(t, w, map[string]float64{"five_hour": 92})
	if err := guard(w, run.Id, "claude", "sonnet", true); code(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "quota_stop 90%") {
		t.Errorf("a reviewer at the reserve: %v, want the reserve refusal", err)
	}
	// The model's own window: a Sonnet weekly line does not hold Opus.
	claudeWindows(t, w, map[string]float64{"seven_day_sonnet": 80})
	if err := guard(w, run.Id, "claude", "opus", false); err != nil {
		t.Errorf("sonnet's weekly window held opus: %v", err)
	}
	if err := guard(w, run.Id, "claude", "sonnet", false); code(err) != connect.CodeResourceExhausted {
		t.Errorf("sonnet's weekly window past the line: %v", err)
	}

	// `mesh reserve off` lifts both.
	_, root := w.root(t, run.Id)
	if _, err := root.SetReserve(context.Background(), connect.NewRequest(&v1.SetReserveRequest{Meta: meta(""), Mode: "off"})); err != nil {
		t.Fatal(err)
	}
	if err := guard(w, run.Id, "claude", "sonnet", false); err != nil {
		t.Errorf("reserve off left the reviews-only line: %v", err)
	}
}

// agy's weekly rows follow quota_stop_weekly, its five-hour rows quota_stop, and the reviews-only line holds a row
// of either; GetUsage says which rows are at which line.
func TestAgyWeeklyRowsHaveTheWeeklyReserve(t *testing.T) {
	ctx := context.Background()
	var rows atomic.Value
	rows.Store("")
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Usage = &daemon.UsageOps{
			DisableClaudeNetwork: true,
			ReadAgyUsage:         func(context.Context) (string, error) { v, _ := rows.Load().(string); return v, nil },
			ReadAgyModels: func(context.Context) (string, error) {
				return "gemini-4-hydrogen-high\tGemini 4 Hydrogen (High)\n", nil
			},
		}
	})
	run := startRun(t, w.device.Mesh, "")
	read := func(five, week int) {
		rows.Store(fmt.Sprintf("Gemini 4 Hydrogen\tFive Hour Limit Remaining\t%d%%\t1h\nGemini 4 Hydrogen\tWeekly Limit Remaining\t%d%%\t3d\n", five, week))
		_ = os.Remove(statedir.Paths{Dir: w.state}.AgyUsage())
		if _, err := w.device.Mesh.GetUsage(ctx, connect.NewRequest(&v1.GetUsageRequest{Refresh: true})); err != nil {
			t.Fatal(err)
		}
	}
	w.set(t, "quota_stop", 90)
	w.set(t, "quota_stop_weekly", 95)

	read(50, 8) // 92% of the week used: under 95
	if err := guard(w, run.Id, "agy", "hydrogen", false); err != nil {
		t.Errorf("weekly 92 under 95: %v", err)
	}
	read(50, 4) // 96% used
	if err := guard(w, run.Id, "agy", "hydrogen", false); code(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "quota_stop_weekly 95%") {
		t.Errorf("weekly 96 over 95: %v, want a refusal naming quota_stop_weekly", err)
	}
	read(8, 50) // the five-hour row at 92% used: over quota_stop 90
	if err := guard(w, run.Id, "agy", "hydrogen", false); code(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "quota_stop 90%") {
		t.Errorf("five-hour 92 over 90: %v, want a refusal naming quota_stop", err)
	}

	w.set(t, "quota_reviews_only.agy", 70)
	read(20, 50) // 80% of the five-hour window used: past 70, under 90
	if err := guard(w, run.Id, "agy", "hydrogen", false); code(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "only PR reviews") {
		t.Errorf("a task past agy's reviews-only line: %v", err)
	}
	if err := guard(w, run.Id, "agy", "hydrogen", true); err != nil {
		t.Errorf("a reviewer past agy's reviews-only line: %v", err)
	}
	u, err := w.device.Mesh.GetUsage(ctx, connect.NewRequest(&v1.GetUsageRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if u.Msg.QuotaStopWeekly != 95 || u.Msg.ReviewsOnlyAgy != 70 || u.Msg.ReviewsOnlyClaude != 100 {
		t.Errorf("GetUsage lines = weekly %d, agy %d, claude %d", u.Msg.QuotaStopWeekly, u.Msg.ReviewsOnlyAgy, u.Msg.ReviewsOnlyClaude)
	}
	for _, r := range u.Msg.AgyRows {
		if strings.HasPrefix(r.Label, "Five") && (r.AtReserve || !r.ReviewsOnly) {
			t.Errorf("the five-hour row at 80%% = %+v, want reviews-only", r)
		}
	}
}

// The lead is told once when a model crosses its reviews-only line, in words that say what it now takes.
func TestAReviewsOnlyLineAlertIsSentOnce(t *testing.T) {
	ctx := context.Background()
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.ReserveEvery = 40 * time.Millisecond
		c.Usage = &daemon.UsageOps{FetchClaudeUsage: func(context.Context) (*hook.ClaudeUsageSnapshot, error) { return nil, os.ErrNotExist }}
	})
	run := startRun(t, w.device.Mesh, "")
	w.set(t, "quota_reviews_only.claude", 75)
	leadAgent, _ := w.root(t, run.Id)
	w.spawnClaude(t, run, "worker", v1.AgentRole_AGENT_ROLE_WORKER)
	claudeWindows(t, w, map[string]float64{"five_hour": 80})
	waitFor(t, "a reviews-only alert", func() bool {
		m := w.infoTo(t, leadAgent, "[mesh alert reserve claude-five_hour]")
		return m != nil && strings.Contains(m.Body, "only PR reviews") && strings.Contains(m.Body, "quota_reviews_only.claude 75%")
	})
	time.Sleep(300 * time.Millisecond)
	msgs, err := w.d.Store().Read(ctx).Inbox(leadAgent.Id, true)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range msgs {
		if strings.Contains(m.Body, "[mesh alert reserve claude-") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d reserve alerts, want 1", n)
	}
}

// The spawn path passes the role: a worker is refused past the reviews-only line, a reviewer is not.
func TestSpawnPassesTheRoleToTheReviewsOnlyLine(t *testing.T) {
	ctx := context.Background()
	w, run := startClaudeUsageWorld(t)
	w.set(t, "quota_reviews_only.claude", 75)
	claudeWindows(t, w, map[string]float64{"five_hour": 80})
	repo := gitRepo(t)
	if _, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: repo, Slug: "task", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_WORKER,
	})); code(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "only PR reviews") {
		t.Errorf("a worker spawn past the reviews-only line: %v", err)
	}
	res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: repo, Slug: "review", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_REVIEWER,
	}))
	if err != nil {
		t.Fatalf("a reviewer spawn past the reviews-only line: %v", err)
	}
	w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
}

// A CLI with no reviews-only line is never called reviews-only, whatever another CLI's line says.
func TestAClaudeWindowIsNotReviewsOnlyWhenOnlyAgyHasALine(t *testing.T) {
	ctx := context.Background()
	w, _ := startClaudeUsageWorld(t)
	w.set(t, "quota_stop", 100)
	w.set(t, "quota_stop_weekly", 100)
	w.set(t, "quota_reviews_only.agy", 80)
	claudeWindows(t, w, map[string]float64{"five_hour": 100})
	u, err := w.device.Mesh.GetUsage(ctx, connect.NewRequest(&v1.GetUsageRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if u.Msg.Claude == nil || len(u.Msg.Claude.ReviewsOnly) != 0 || len(u.Msg.Claude.AtReserve) != 0 {
		t.Errorf("claude = %+v, want no reserve and no reviews-only mark", u.Msg.Claude)
	}
}

// A model that only reviewers use is not told it now takes only reviews: nothing is held back from them.
func TestNoReviewsOnlyAlertForAModelOnlyReviewersUse(t *testing.T) {
	ctx := context.Background()
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.ReserveEvery = 40 * time.Millisecond
		c.Usage = &daemon.UsageOps{FetchClaudeUsage: func(context.Context) (*hook.ClaudeUsageSnapshot, error) { return nil, os.ErrNotExist }}
	})
	run := startRun(t, w.device.Mesh, "")
	w.set(t, "quota_reviews_only.claude", 75)
	lead, _ := w.root(t, run.Id)
	res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: gitRepo(t), Slug: "review", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_REVIEWER,
	}))
	if err != nil {
		t.Fatal(err)
	}
	w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	claudeWindows(t, w, map[string]float64{"five_hour": 80})
	time.Sleep(500 * time.Millisecond)
	msgs, err := w.d.Store().Read(ctx).Inbox(lead.Id, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if strings.Contains(m.Body, "[mesh alert reserve") {
			t.Errorf("a reviewer-only model was alerted: %s", m.Body)
		}
	}
}

// A change task to an idle reviewer is held to the reviews-only line as the task it is (it turns the reviewer into a
// worker); a read-only one, a review's own kind of work, still goes through. At the reserve a task is told the
// reserve, not the reviews-only line.
func TestAChangeTaskToAReviewerIsHeldToTheReviewsOnlyLine(t *testing.T) {
	ctx := context.Background()
	w, run := startClaudeUsageWorld(t)
	_, root := w.root(t, run.Id)
	res, err := root.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: gitRepo(t), Slug: "rev", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_REVIEWER,
	}))
	if err != nil {
		t.Fatal(err)
	}
	rev := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.set(t, "quota_reviews_only.claude", 75)
	claudeWindows(t, w, map[string]float64{"five_hour": 80})
	send := func(body string) error {
		_, err := root.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{Meta: meta(""), To: rev.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: body}))
		return err
	}
	if err := send("Direct task (no plan): fix the thing"); code(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "only PR reviews") {
		t.Errorf("a change task to a reviewer past the line: %v", err)
	}
	if got, _ := w.d.Store().Read(ctx).GetAgent(rev.Id); got.Role != v1.AgentRole_AGENT_ROLE_REVIEWER {
		t.Errorf("the refused task still made the reviewer a %v", got.Role)
	}
	// A read-only task is a review's own kind of work: it goes through, and the agent stays a reviewer.
	if err := send("Question (read-only): which files does the change touch?"); err != nil {
		t.Errorf("a read-only task to a reviewer past the line: %v", err)
	}
	if got, _ := w.d.Store().Read(ctx).GetAgent(rev.Id); got.Role != v1.AgentRole_AGENT_ROLE_REVIEWER {
		t.Errorf("the read-only task made the reviewer a %v", got.Role)
	}
	// A stopped reviewer resumed with a change task is held as well (`mesh resume --task`).
	w.waitState(rev.Id, v1.AgentState_AGENT_STATE_IDLE)
	retired := retireStoppedWorker(t, w, rev)
	if _, err := root.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{Meta: meta(""), Agent: retired.Id, Task: "Direct task (no plan): fix the thing"})); code(err) != connect.CodeResourceExhausted {
		t.Errorf("a stopped reviewer resumed with a change task past the line: %v", err)
	}
	claudeWindows(t, w, map[string]float64{"five_hour": 92})
	if err := guard(w, run.Id, "claude", "sonnet", false); code(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "quota_stop 90%") || strings.Contains(err.Error(), "only PR reviews") {
		t.Errorf("a task at the reserve with a reviews-only line set: %v, want the reserve refusal", err)
	}
}
