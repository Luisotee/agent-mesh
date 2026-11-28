package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

func TestReserveAlertDedupeSurvivesPastOneHour(t *testing.T) {
	d := &Daemon{rt: &runtime{}}
	t0 := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	resetAt := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	key := "claude:seven_day:2026-12-01T00:00Z"

	if !d.shouldSendReserveAlert("r1", key, t0) {
		t.Fatal("first alert should send")
	}
	d.markReserveAlertSent("r1", key, resetAt)

	if d.shouldSendReserveAlert("r1", key, t0.Add(2*time.Hour)) {
		t.Error("same alert key sent again after 2h; want once per reset window")
	}
	if d.shouldSendReserveAlert("r1", key, t0.Add(24*time.Hour)) {
		t.Error("same alert key sent again after 24h; want once per reset window")
	}
	if !d.shouldSendReserveAlert("r1", "claude:seven_day:2026-12-08T00:00Z", t0.Add(24*time.Hour)) {
		t.Error("new reset window should alert")
	}
	// Once the reset passes, the old key is pruned and allowed again.
	if !d.shouldSendReserveAlert("r1", key, resetAt) {
		t.Error("expired alert key should prune once reset time is reached")
	}
}

func TestReserveAlertDedupeResetNormalizationAndExpiry(t *testing.T) {
	d := &Daemon{rt: &runtime{}}
	t0 := time.Date(2026, 11, 26, 10, 0, 10, 0, time.UTC)

	// Claude resets_at differing only by seconds / microseconds / TZ offset share the same minute key.
	k1, exp1 := claudeAlertKeyAndExpiry("five_hour", "2026-11-26T15:12:34.123456Z", t0)
	k2, _ := claudeAlertKeyAndExpiry("five_hour", "2026-11-26T12:12:01-03:00", t0)
	if k1 != "claude:five_hour:2026-11-26T15:12Z" || k1 != k2 {
		t.Fatalf("claude keys mismatched: k1=%q k2=%q", k1, k2)
	}
	if !exp1.Equal(time.Date(2026, 11, 26, 15, 12, 34, 123456000, time.UTC)) {
		t.Fatalf("exp1 = %v", exp1)
	}

	// Agy relative resets resolved against reading time stay stable across passes.
	ak1, aexp1 := agyAlertKeyAndExpiry("Claude", "in 4h", t0, t0)
	ak2, _ := agyAlertKeyAndExpiry("Claude", "3h45m", t0.Add(15*time.Minute+12*time.Second), t0.Add(15*time.Minute+12*time.Second))
	if ak1 != "agy:Claude:2026-11-26T14:00Z" || ak1 != ak2 {
		t.Fatalf("relative agy keys mismatched: ak1=%q ak2=%q", ak1, ak2)
	}
	if !d.shouldSendReserveAlert("r1", ak1, t0) {
		t.Fatal("first agy alert should send")
	}
	d.markReserveAlertSent("r1", ak1, aexp1)
	if d.shouldSendReserveAlert("r1", ak2, t0.Add(15*time.Minute)) {
		t.Error("subsequent relative reset in same window was not deduped")
	}

	// Recurring wall-clock reset ("12:00") alerts on day 1, prunes once passed, and alerts again on day 2.
	wk1, wexp1 := agyAlertKeyAndExpiry("Neon", "12:00 (UTC)", t0, t0)
	if wk1 != "agy:Neon:2026-11-26T12:00Z" {
		t.Fatalf("day1 wall-clock key = %q", wk1)
	}
	d.markReserveAlertSent("r1", wk1, wexp1)
	if d.shouldSendReserveAlert("r1", wk1, t0.Add(time.Hour)) {
		t.Error("same day wall-clock key repeated before reset")
	}
	tDay2 := t0.Add(24 * time.Hour)
	wk2, _ := agyAlertKeyAndExpiry("Neon", "12:00 (UTC)", tDay2, tDay2)
	if wk2 != "agy:Neon:2026-11-27T12:00Z" {
		t.Fatalf("day2 wall-clock key = %q", wk2)
	}
	if !d.shouldSendReserveAlert("r1", wk2, tDay2) {
		t.Error("next day recurring wall-clock reset should alert")
	}
	// Calling shouldSendReserveAlert on day 2 pruned the expired day 1 key.
	if !d.shouldSendReserveAlert("r1", wk1, tDay2) {
		t.Error("day1 wall-clock key should have pruned after its reset passed")
	}

	// Unparseable / empty reset falls back to reserveAlertCooldown (1h).
	uk, uexp := agyAlertKeyAndExpiry("Neon", "soon", t0, t0)
	if uk != "agy:Neon:soon" || !uexp.Equal(t0.Add(reserveAlertCooldown)) {
		t.Fatalf("unparseable key/exp = (%q, %v)", uk, uexp)
	}
	d.markReserveAlertSent("r1", uk, uexp)
	if d.shouldSendReserveAlert("r1", uk, t0.Add(30*time.Minute)) {
		t.Error("unparseable reset alerted within 1h cooldown")
	}
	if !d.shouldSendReserveAlert("r1", uk, t0.Add(reserveAlertCooldown)) {
		t.Error("unparseable reset did not expire after 1h cooldown")
	}
}

func TestAgyResolveExpiredTTLUsesStaleEntryWithoutExecAndFreshDiskUsesModTime(t *testing.T) {
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	d := &Daemon{
		paths: statedir.Paths{Dir: dir},
		now:   func() time.Time { return now },
		rt: &runtime{cfg: AgentConfig{Usage: &UsageOps{
			ReadAgyModels: func(context.Context) (string, error) {
				return "gemini-4-neon-high\tGemini 4 Neon (High)\n", nil
			},
		}}},
	}

	// Write a fresh-disk agy-usage.txt modified 3m ago (within agyUsageTTL=5m).
	usagePath := d.paths.AgyUsage()
	if err := os.WriteFile(usagePath, []byte("Gemini 4 Neon\tNeon\t5%\t1h\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileMod := now.Add(-3 * time.Minute)
	if err := os.Chtimes(usagePath, fileMod, fileMod); err != nil {
		t.Fatal(err)
	}
	rows, fetchedAt := d.agyUsageRows(context.Background(), false, false)
	if len(rows) != 1 || !fetchedAt.Equal(fileMod) {
		t.Fatalf("fresh-disk agyUsageRows = (%+v, %v), want fetchedAt=%v", rows, fetchedAt, fileMod)
	}

	// Resolve neon at t=now (caches display name), then advance past agyResolveTTL (5m).
	id, disp := d.resolveAgyModelDisplay(context.Background(), "neon", "high", true)
	if id != "gemini-4-neon-high" || disp != "Gemini 4 Neon (High)" {
		t.Fatalf("initial resolve = (%q, %q)", id, disp)
	}
	now = now.Add(agyResolveTTL + 2*time.Minute) // +7m (fileMod is now 10m ago, still <= 15m maxAge)

	staleID, staleDisp := d.resolveAgyModelDisplay(context.Background(), "neon", "high", false)
	if staleID != "gemini-4-neon-high" || staleDisp != "Gemini 4 Neon (High)" {
		t.Fatalf("expired TTL with allowExec=false = (%q, %q), want cached display", staleID, staleDisp)
	}
	hit := d.agyModelAtReserveCached("neon", "high", reserveLimits{short: 90, weekly: 90, reviews: 100}, false)
	if hit == nil || hit.left != 5 {
		t.Fatalf("agyModelAtReserveCached after agyResolveTTL = %+v, want Neon 5%% hit", hit)
	}

	// Advancing past agyUsageMaxAge from st.ModTime() expires the rows instead of extending by a full TTL.
	_ = os.Remove(usagePath)
	now = fileMod.Add(agyUsageMaxAge + time.Second)
	if expiredRows, _ := d.cachedAgyUsageRows(now); len(expiredRows) != 0 {
		t.Errorf("in-memory agy rows outlived agyUsageMaxAge relative to st.ModTime(): %+v", expiredRows)
	}
}

func TestCanResumeMissingSourceRunReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	actor := &v1.Actor{Class: v1.ActorClass_ACTOR_CLASS_DEVICE, Id: "dev"}
	agent := &v1.Agent{Id: "a1", Name: "repo-fix", RunId: "missing-run", Role: v1.AgentRole_AGENT_ROLE_WORKER}
	rerr := canResume(st.Read(ctx), actor, agent, "target-run")
	if connect.CodeOf(rerr) != connect.CodeNotFound {
		t.Fatalf("canResume with missing source run = %v (code %v), want NotFound", rerr, connect.CodeOf(rerr))
	}
}
