package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/agy"
	"github.com/Luisotee/agent-mesh/daemon/internal/settings"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

func TestAWindowIsHeldAtItsReserveForEveryoneAndAtTheReviewsOnlyLineForAllButAReviewer(t *testing.T) {
	type res struct {
		line float64
		ro   bool
		ok   bool
	}
	held := func(l reserveLimits, used float64, weekly, reviewer bool) res {
		line, ro, ok := l.held(used, weekly, reviewer)
		return res{line, ro, ok}
	}
	l := reserveLimits{short: 85, weekly: 95, reviews: 100}
	for _, tc := range []struct {
		used             float64
		weekly, reviewer bool
		want             res
	}{
		{84, false, false, res{}}, {85, false, false, res{85, false, true}}, {85, false, true, res{85, false, true}},
		{90, true, false, res{}}, {95, true, true, res{95, false, true}},
	} {
		if got := held(l, tc.used, tc.weekly, tc.reviewer); got != tc.want {
			t.Errorf("no reviews-only line, used %v weekly=%v reviewer=%v: %+v, want %+v", tc.used, tc.weekly, tc.reviewer, got, tc.want)
		}
	}
	l.reviews = 70
	for _, tc := range []struct {
		used             float64
		weekly, reviewer bool
		want             res
	}{
		{69, false, false, res{}},
		{70, false, false, res{70, true, true}},  // a task: kept for reviews
		{70, false, true, res{}},                 // a reviewer still starts
		{85, false, false, res{85, false, true}}, // at the reserve a task is held at the reserve, not "reviews only"
		{85, false, true, res{85, false, true}},  // and so is a reviewer
		{90, true, false, res{70, true, true}},
		{95, true, true, res{95, false, true}},
	} {
		if got := held(l, tc.used, tc.weekly, tc.reviewer); got != tc.want {
			t.Errorf("reviews-only 70, used %v weekly=%v reviewer=%v: %+v, want %+v", tc.used, tc.weekly, tc.reviewer, got, tc.want)
		}
	}
	// A reviews-only line above the reserve changes nothing.
	l.reviews = 90
	if got := held(l, 85, false, false); got != (res{85, false, true}) {
		t.Errorf("reviews-only above the reserve: %+v", got)
	}
	// A row at its reserve is reported as that, not as reviews-only, whatever the line (a task and a reviewer alike).
	rows := []agy.UsageRow{{Group: "g", Label: "Five Hour Limit Remaining", Left: 5}, {Group: "g", Label: "Weekly Limit Remaining", Left: 25}}
	lim := reserveLimits{short: 90, weekly: 95, reviews: 70}
	for _, reviewer := range []bool{false, true} {
		row, line, ro, ok := agyRowHit(rows, lim, reviewer)
		if !ok || ro || line != 90 || row.Left != 5 {
			t.Errorf("agyRowHit(reviewer=%v) = %+v, %v, %v, %v; want the five-hour row at its reserve 90", reviewer, row, line, ro, ok)
		}
	}
	if (reserveLimits{short: 100, weekly: 100, reviews: 100}).off() == false || (reserveLimits{short: 100, weekly: 90, reviews: 100}).off() {
		t.Error("off() is true only when no window has a reserve and the reviews-only line is off")
	}
}

func TestATierPickSkipsAnAgyModelAtTheReviewsOnlyLineForATaskNotForAReviewer(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d := &Daemon{
		store: st, paths: statedir.Paths{Dir: dir}, schema: settings.Default(), now: func() time.Time { return now },
		rt: &runtime{cfg: AgentConfig{Usage: &UsageOps{
			ReadAgyModels: func(context.Context) (string, error) {
				return "gemini-4-neon-high\tGemini 4 Neon (High)\ngemini-4-hydrogen-high\tGemini 4 Hydrogen (High)\n", nil
			},
		}}},
	}
	rows := "Gemini 4 Neon\tFive Hour Limit Remaining\t20%\t1h\nGemini 4 Hydrogen\tFive Hour Limit Remaining\t90%\t1h\n"
	if err := os.WriteFile(d.paths.AgyUsage(), []byte(rows), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(d.paths.AgyUsage(), now, now); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if got, _ := d.agyUsageRows(ctx, false, false); len(got) != 2 {
		t.Fatalf("rows = %+v", got)
	}
	d.resolveAgyModelDisplay(ctx, "neon", "high", true)
	d.resolveAgyModelDisplay(ctx, "hydrogen", "high", true)
	chain := []string{"neon", "hydrogen"}
	r := st.Read(ctx)
	if got := d.firstFree(r, "agy", "high", chain, false); got != "neon" {
		t.Fatalf("with no reviews-only line a task takes neon (80%% used, under the reserve): %q", got)
	}
	if err := st.Write(ctx, nil, "", func(tx *store.Tx) error { return tx.PutSetting("quota_reviews_only.agy", "70") }); err != nil {
		t.Fatal(err)
	}
	r = st.Read(ctx)
	if got := d.firstFree(r, "agy", "high", chain, false); got != "hydrogen" {
		t.Errorf("a task's pick = %q, want hydrogen (neon takes only reviews)", got)
	}
	if got := d.firstFree(r, "agy", "high", chain, true); got != "neon" {
		t.Errorf("a reviewer's pick = %q, want neon", got)
	}
	if got := d.nextFree(r, "agy", "high", []string{"x", "neon", "hydrogen"}, 0, false); got != "hydrogen" {
		t.Errorf("a task's next = %q, want hydrogen", got)
	}
	if got := d.nextFree(r, "agy", "high", []string{"x", "neon", "hydrogen"}, 0, true); got != "neon" {
		t.Errorf("a reviewer's next = %q, want neon", got)
	}
	_ = agy.UsageRow{}
}
