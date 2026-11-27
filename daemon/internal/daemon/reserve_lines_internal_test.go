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

func TestTheLineOfAWindowIsItsReserveOrForAnyButAReviewerTheReviewsOnlyLine(t *testing.T) {
	l := reserveLimits{short: 85, weekly: 95, reviews: 100}
	for _, tc := range []struct {
		weekly, reviewer bool
		want             float64
		ro               bool
	}{
		{false, false, 85, false}, {true, false, 95, false}, {false, true, 85, false}, {true, true, 95, false},
	} {
		if got, ro := l.line(tc.weekly, tc.reviewer); got != tc.want || ro != tc.ro {
			t.Errorf("line(weekly=%v, reviewer=%v) = %v, %v; want %v, %v", tc.weekly, tc.reviewer, got, ro, tc.want, tc.ro)
		}
	}
	l.reviews = 70
	if got, ro := l.line(false, false); got != 70 || !ro {
		t.Errorf("a task's line = %v, %v; want the reviews-only 70", got, ro)
	}
	if got, ro := l.line(true, true); got != 95 || ro {
		t.Errorf("a reviewer's weekly line = %v, %v; want its reserve 95", got, ro)
	}
	// A reviews-only line above the reserve changes nothing.
	l.reviews = 90
	if got, ro := l.line(false, false); got != 85 || ro {
		t.Errorf("reviews-only above the reserve: line = %v, %v; want 85", got, ro)
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
