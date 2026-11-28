package daemon

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

func TestPaceWarnsWhenUseRunsAheadOfTheWindow(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		used     float64
		left     time.Duration
		window   time.Duration
		elapsed  float64
		wantWarn string
	}{
		{"agy 98% used, 6 days to go", 98, 6 * 24 * time.Hour, weeklyWindow, 14.3, "runs out in"},
		{"on pace", 50, weeklyWindow / 2, weeklyWindow, 50, ""},
		{"a little ahead, not enough", 40, weeklyWindow * 7 / 10, weeklyWindow, 30, ""},
		{"little used, early", 20, weeklyWindow * 9 / 10, weeklyWindow, 10, ""},
		{"spent", 100, 3 * time.Hour, shortWindow, 40, "it is spent"},
		{"ahead but the reset comes first", 80, 20 * time.Minute, shortWindow, 93.3, ""},
	} {
		el, pace := paceOf(tc.used, now.Add(tc.left), tc.window, now)
		if el < tc.elapsed-0.1 || el > tc.elapsed+0.1 {
			t.Errorf("%s: elapsed %.1f, want %.1f", tc.name, el, tc.elapsed)
		}
		if (tc.wantWarn == "") != (pace == "") || !strings.Contains(pace, tc.wantWarn) {
			t.Errorf("%s: pace %q, want it to contain %q", tc.name, pace, tc.wantWarn)
		}
	}
	if el, pace := paceOf(90, time.Time{}, weeklyWindow, now); el != 0 || pace != "" {
		t.Errorf("an unknown reset: %v %q", el, pace)
	}
}

func TestPeriodsSplitWhereTheWindowResetsTheUseFallsOrTheGapIsLong(t *testing.T) {
	t0 := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	reset1, reset2 := t0.Add(48*time.Hour), t0.Add(48*time.Hour+7*24*time.Hour)
	s := func(h int, used float64, resets time.Time) store.UsageSample {
		return store.UsageSample{Pool: "claude:seven_day", At: t0.Add(time.Duration(h) * time.Hour), Used: used, Resets: resets}
	}
	got := periodsOf([]store.UsageSample{s(1, 10, reset1), s(5, 18, reset1.Add(time.Minute)), s(14, 40, reset1), s(60, 3, reset2), s(70, 9, reset2), s(120, 12, reset2)}, weeklyReadingGap)
	// The last reading is 50 hours after the one before it, over a weekly pool's 36 hours: a gap that long starts a period of its own.
	if len(got) != 3 || len(got[0]) != 3 || len(got[1]) != 2 || len(got[2]) != 1 {
		t.Fatalf("periods %v", got)
	}
}

func TestWorthSaysHowMuchAPoolIsInAClaudeWeek(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d := &Daemon{store: st}
	ctx := context.Background()
	if got := d.worth(st.Read(ctx), "agy:Gemini:weekly", 50); got != "not measured yet" {
		t.Errorf("before any measure: %q", got)
	}
	put := func(pool string, tpp float64) {
		raw := `{"tokens_per_percent":` + strings.TrimRight(strings.TrimRight(formatFloat(tpp), "0"), ".") + `,"points":10,"tokens":1000}`
		if err := st.Write(ctx, nil, "", func(tx *store.Tx) error { return tx.PutMeta(poolSizeKey(pool), raw) }); err != nil {
			t.Fatal(err)
		}
	}
	put(refPool, 1000000)
	if got := d.worth(st.Read(ctx), "agy:Gemini:weekly", 50); got != "not measured yet" {
		t.Errorf("a pool not measured: %q", got)
	}
	put("agy:Gemini:weekly", 100000)
	if got := d.worth(st.Read(ctx), "agy:Gemini:weekly", 50); got != "≈ 5.0% of a Claude week (estimate)" {
		t.Errorf("agy at half of a plan a tenth the size: %q", got)
	}
	if got := d.worth(st.Read(ctx), refPool, 40); got != "≈ 40% of a Claude week" {
		t.Errorf("the reference itself: %q", got)
	}
	if got := d.worth(st.Read(ctx), "", 40); got != "" {
		t.Errorf("a window with no pool: %q", got)
	}
}

func formatFloat(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) }

// A pool's size is the tokens counted over the percent its readings moved, in the periods that moved at least five
// points; it is stored for `mesh usage` and read back by worth.
func TestEstimateSizesDividesTheTokensByThePercentMoved(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	d := &Daemon{store: st, now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()
	reset := now.Add(24 * time.Hour)
	put := func(pool string, hoursAgo int, used float64) {
		s := store.UsageSample{Pool: pool, At: now.Add(-time.Duration(hoursAgo) * time.Hour), Used: used, Resets: reset}
		if err := st.Write(ctx, nil, "", func(tx *store.Tx) error { return tx.PutUsageSample(s, time.Minute, usageSampleKeep) }); err != nil {
			t.Fatal(err)
		}
	}
	put("claude:seven_day", 20, 10)
	put("claude:seven_day", 10, 14)
	put("claude:seven_day", 2, 20) // 10 points over the period
	put("agy:Gemini:weekly", 20, 50)
	put("agy:Gemini:weekly", 2, 52) // 2 points: too few
	d.estimateSizesWith(ctx, func(_ context.Context, kind string, from, to time.Time) (int64, bool) {
		if kind != "claude" || to.Sub(from) != 18*time.Hour {
			t.Errorf("tokens asked for %s between %v and %v", kind, from, to)
			return 0, false
		}
		return 5_000_000, true
	})
	sz, ok := d.sizeOf(st.Read(ctx), "claude:seven_day")
	if !ok || sz.TokensPerPercent != 500000 || sz.Points != 10 {
		t.Errorf("claude week: %+v %v", sz, ok)
	}
	if _, ok := d.sizeOf(st.Read(ctx), "agy:Gemini:weekly"); ok {
		t.Error("a pool that moved 2 points was measured")
	}
}

func TestTheReadingGapFollowsTheLengthOfTheWindow(t *testing.T) {
	for pool, want := range map[string]time.Duration{
		"claude:five_hour": shortReadingGap, "claude:seven_day": weeklyReadingGap,
		"agy:Gemini 4 Neon:Weekly Limit Remaining": weeklyReadingGap, "agy:Gemini 4 Neon:Neon": shortReadingGap,
	} {
		if got := readingGapOf(pool); got != want {
			t.Errorf("%s: %v, want %v", pool, got, want)
		}
	}
}

func TestOfferedEffortsKeepTheLastListingOfACLIThatFailedThisTime(t *testing.T) {
	prev := map[string]map[string][]string{"agy": {"hydrogen": {"high"}}, "opencode": {"m": {"high"}}}
	got := mergeOffered(prev, map[string]map[string][]string{"opencode": {"m": {"high", "max"}}})
	if len(got["agy"]["hydrogen"]) != 1 || len(got["opencode"]["m"]) != 2 {
		t.Errorf("merged %v", got)
	}
}
