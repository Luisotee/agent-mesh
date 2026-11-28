package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/agy"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/hook"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// The windows a plan's limits come in: the short one and the weekly one.
const (
	shortWindow  = 5 * time.Hour
	weeklyWindow = 7 * 24 * time.Hour
)

// The unit `mesh usage` compares pools in: a week of the Claude plan (its seven-day window).
const (
	refPool     = "claude:seven_day"
	refPoolName = "a Claude week"
)

const (
	usageSampleEvery = 10 * time.Minute    // a reading nothing moved is kept this often at most
	usageSampleKeep  = 45 * 24 * time.Hour // how long a reading is kept
	sizeLookback     = 30 * 24 * time.Hour // the readings a size estimate looks at
	sizeMinPoints    = 5.0                 // percent a period's readings must have moved for it to count
	paceAhead        = 15.0                // points of use over the share of the window gone that make a warning
	sizeEvery        = 24 * time.Hour
	// A reading further than this from the one before it starts a new period: the percent moved in a long gap is
	// mostly use the mesh's tokens don't show (the user's own sessions), which would make a pool look bigger in
	// percent and so smaller in tokens. A five-hour window's is tighter than a weekly one's (a night).
	shortReadingGap  = 2 * time.Hour
	weeklyReadingGap = 36 * time.Hour
	// maxSizePeriods are the newest reset periods of a pool its size is worked out from (each costs one `mesh cost` scan).
	maxSizePeriods = 8
)

// paceOf is how far through its window a pool is (percent) and, when use runs ahead of that and would reach the
// limit before the reset, a sentence saying so. Without a reset time it knows neither.
func paceOf(used float64, resets time.Time, window time.Duration, now time.Time) (elapsed float64, pace string) {
	if resets.IsZero() || window <= 0 {
		return 0, ""
	}
	left := resets.Sub(now)
	if left < 0 {
		left = 0
	}
	if left > window {
		left = window
	}
	gone := window - left
	elapsed = math.Round(float64(gone)/float64(window)*1000) / 10
	if used < 30 || used-elapsed < paceAhead || gone <= 0 {
		return elapsed, ""
	}
	if used >= 100 {
		return elapsed, fmt.Sprintf("%.0f%% used with %.0f%% of the window gone: it is spent, %s to the reset", used, elapsed, ago(left))
	}
	runsOutIn := time.Duration(float64(gone) * (100 - used) / used)
	if runsOutIn >= left {
		return elapsed, ""
	}
	return elapsed, fmt.Sprintf("%.0f%% used with %.0f%% of the window gone: at this pace it runs out in %s, %s before the reset",
		used, elapsed, ago(runsOutIn), ago(left-runsOutIn))
}

// claudePool is a Claude window's pool name; only the five-hour and seven-day windows are pools of their own (the
// Opus and Sonnet weeklies are slices of the seven-day one).
func claudePool(key string) string {
	if key == "five_hour" || key == "seven_day" {
		return "claude:" + key
	}
	return ""
}

// agyPool names an agy row's pool by its group and label (a group may list a short and a weekly row, or several).
func agyPool(r agy.UsageRow) string { return "agy:" + r.Group + ":" + r.Label }

func windowOf(weekly bool) time.Duration {
	if weekly {
		return weeklyWindow
	}
	return shortWindow
}

// recordUsage keeps the readings a usage report was built from (usage_samples), for the size estimate. It runs
// wherever a reading is consumed (`mesh usage`, a spawn or an ask warming the usage), so it samples when the host
// is working: when tokens are being spent. A failure is only logged: a missing sample is a coarser estimate.
func (d *Daemon) recordUsage(ctx context.Context, snap *hook.ClaudeUsageSnapshot, rows []agy.UsageRow, agyAt time.Time) {
	now := d.nowTime()
	var samples []store.UsageSample
	if snap != nil {
		at := snapshotTime(snap)
		if at.IsZero() {
			at = now
		}
		for key, w := range snap.Windows {
			pool := claudePool(key)
			if pool == "" || claudeWindowStale(w, now) {
				continue
			}
			s := store.UsageSample{Pool: pool, At: at, Used: w.Used}
			if rt, ok := parseClaudeReset(w.Resets); ok {
				s.Resets = rt
			}
			samples = append(samples, s)
		}
	}
	if agyAt.IsZero() {
		rows = nil // a reading with no date would enter the samples as fresh: it is not kept
	}
	for _, row := range rows {
		s := store.UsageSample{Pool: agyPool(row), At: agyAt, Used: float64(100 - row.Left)}
		if rt, ok := parseAgyReset(row.Resets, agyAt); ok {
			s.Resets = rt
		}
		samples = append(samples, s)
	}
	if len(samples) == 0 {
		return
	}
	if err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error {
		for _, s := range samples {
			if err := tx.PutUsageSample(s, usageSampleEvery, usageSampleKeep); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		d.log.Warn("recording usage readings", "err", err)
	}
}

// poolSize is a pool's measured size: the tokens one percent of its window is worth, from `points` percent
// observed over `tokens` counted.
type poolSize struct {
	TokensPerPercent float64   `json:"tokens_per_percent"`
	Points           float64   `json:"points"`
	Tokens           int64     `json:"tokens"`
	At               time.Time `json:"at"`
}

func poolSizeKey(pool string) string { return "usage.size." + pool }

// sizeOf is a pool's stored size, ok false when it was not measured.
func (d *Daemon) sizeOf(r store.Reader, pool string) (poolSize, bool) {
	raw, err := r.Meta(poolSizeKey(pool))
	if err != nil {
		return poolSize{}, false
	}
	var s poolSize
	if json.Unmarshal([]byte(raw), &s) != nil || s.TokensPerPercent <= 0 {
		return poolSize{}, false
	}
	return s, true
}

// worth is what `left` percent of a pool is, in the unit of the Claude week: "≈ 3% of a Claude week", or why it
// can't say.
func (d *Daemon) worth(r store.Reader, pool string, left float64) string {
	if pool == "" {
		return ""
	}
	ref, ok := d.sizeOf(r, refPool)
	if !ok {
		return "not measured yet"
	}
	s, ok := d.sizeOf(r, pool)
	if !ok {
		return "not measured yet"
	}
	v := left * s.TokensPerPercent / ref.TokensPerPercent
	if kindOfPool(pool) == "agy" { // agy's tokens are the CLI's, not split by group: an estimate
		return worthText(v) + " (estimate)"
	}
	return worthText(v)
}

func worthText(v float64) string {
	switch {
	case v >= 10:
		return fmt.Sprintf("≈ %.0f%% of %s", v, refPoolName)
	case v >= 0.1:
		return fmt.Sprintf("≈ %.1f%% of %s", v, refPoolName)
	}
	return fmt.Sprintf("under 0.1%% of %s", refPoolName)
}

// kindOfPool is the CLI whose tokens a pool is spent by.
func kindOfPool(pool string) string {
	kind, _, _ := strings.Cut(pool, ":")
	return kind
}

// estimateSizes works out each pool's tokens per percent from the readings kept: in each reset period (readings
// whose reset times are within 90 minutes of each other) the first and the last reading, if the percent moved at
// least sizeMinPoints, give the points and the tokens `mesh cost` counts for that CLI between them. The periods of
// the last 30 days add up, and the result is stored in meta for `mesh usage`. Only the mesh's own tokens are
// counted, so a plan the user also spends outside the mesh reads smaller than it is (a lower bound), and agy's pools
// share one token count for the CLI (the tokens are not split by model): both are estimates.
func (d *Daemon) estimateSizes(ctx context.Context) { d.estimateSizesWith(ctx, d.costTokens) }

// costTokens is the tokens `mesh cost` counts for a CLI between two times, across every run.
func (d *Daemon) costTokens(ctx context.Context, kind string, from, to time.Time) (int64, bool) {
	costCtx := auth.WithActor(ctx, &v1.Actor{Class: v1.ActorClass_ACTOR_CLASS_DEVICE, Id: "usage-size"})
	res, err := d.GetCost(costCtx, connect.NewRequest(&v1.GetCostRequest{
		Since: from.UTC().Format(time.RFC3339Nano), Until: to.UTC().Format(time.RFC3339Nano)}))
	if err != nil {
		return 0, false
	}
	for _, k := range res.Msg.Kinds {
		if k.Kind == kind {
			return k.Total, true
		}
	}
	return 0, false
}

func (d *Daemon) estimateSizesWith(ctx context.Context, tokensBetween func(ctx context.Context, kind string, from, to time.Time) (int64, bool)) {
	now := d.nowTime()
	r := d.store.Read(ctx)
	pools, err := r.UsagePools()
	if err != nil {
		return
	}
	for _, pool := range pools {
		samples, err := r.UsageSamples(pool, now.Add(-sizeLookback))
		if err != nil || len(samples) < 2 {
			continue
		}
		var points float64
		var tokens int64
		periods := periodsOf(samples, readingGapOf(pool))
		if len(periods) > maxSizePeriods { // each period is a scan of the transcripts: the newest ones tell enough
			periods = periods[len(periods)-maxSizePeriods:]
		}
		for _, per := range periods {
			first, last := per[0], per[len(per)-1]
			if last.Used-first.Used < sizeMinPoints {
				continue
			}
			if n, ok := tokensBetween(ctx, kindOfPool(pool), first.At, last.At); ok {
				tokens += n
				points += last.Used - first.Used
			}
		}
		if points < sizeMinPoints || tokens <= 0 {
			continue
		}
		raw, _ := json.Marshal(poolSize{TokensPerPercent: float64(tokens) / points, Points: points, Tokens: tokens, At: now})
		if err := d.store.Write(ctx, nil, "", func(tx *store.Tx) error { return tx.PutMeta(poolSizeKey(pool), string(raw)) }); err != nil {
			d.log.Warn("storing a pool's size", "pool", pool, "err", err)
		}
	}
}

// periodsOf splits a pool's readings (oldest first) into reset periods: a reading starts a new one when its reset
// time is more than 90 minutes from the period's, its use fell (the window reset), or it is more than maxReadingGap
// after the one before.
func periodsOf(samples []store.UsageSample, maxGap time.Duration) [][]store.UsageSample {
	var out [][]store.UsageSample
	for _, s := range samples {
		if n := len(out); n > 0 {
			cur := out[n-1]
			first, last := cur[0], cur[len(cur)-1]
			sameReset := first.Resets.IsZero() || s.Resets.IsZero() || s.Resets.Sub(first.Resets).Abs() <= 90*time.Minute
			if sameReset && s.Used >= last.Used && s.At.Sub(last.At) <= maxGap {
				out[n-1] = append(cur, s)
				continue
			}
		}
		out = append(out, []store.UsageSample{s})
	}
	return out
}

// sizeLoop measures the pools once a day (and a little after the daemon starts).
func (d *Daemon) sizeLoop(ctx context.Context) {
	t := time.NewTimer(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.estimateSizes(ctx)
			t.Reset(sizeEvery)
		}
	}
}

// readingGapOf is how far apart two readings of a pool may be to count as one stretch of use: by the length of its
// window (a Claude seven-day window or an agy row labelled weekly is a weekly one).
func readingGapOf(pool string) time.Duration {
	if pool == "claude:five_hour" || (kindOfPool(pool) == "agy" && !strings.Contains(strings.ToLower(pool), "week")) {
		return shortReadingGap
	}
	return weeklyReadingGap
}
