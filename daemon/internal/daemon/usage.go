package daemon

// Quota usage monitoring, reserve enforcement, and periodic reserve alerts (bin/mesh's claude_usage,
// agy_usage, reserve_guard, reserve_alert, cmd_usage, cmd_reserve).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/agy"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/hook"
	"github.com/Luisotee/agent-mesh/daemon/internal/probe"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

const (
	claudeUsageTTL       = 5 * time.Minute
	claudeUsageStale     = 15 * time.Minute
	agyUsageTTL          = 5 * time.Minute
	agyUsageMaxAge       = 3 * agyUsageTTL
	agyResolveTTL        = agyUsageTTL
	usageFailureTTL      = 2 * time.Minute
	claudeUsageURL       = "https://api.anthropic.com/api/oauth/usage"
	claudeUsageBeta      = "oauth-2025-04-20"
	reserveAlertEvery    = 15 * time.Minute
	reserveAlertCooldown = time.Hour
)

// UsageOps overrides external usage, model listing, and GitHub CLI calls (for tests).
type UsageOps struct {
	// FetchClaudeUsage fetches Claude's live usage windows (for tests; nil calls Anthropic's OAuth endpoint).
	FetchClaudeUsage func(ctx context.Context) (*hook.ClaudeUsageSnapshot, error)
	// DisableClaudeNetwork skips Anthropic's OAuth usage endpoint even if credentials exist (for tests).
	DisableClaudeNetwork bool
	// ReadAgyUsage runs `agy -p /usage` (for tests; nil calls the configured agy binary).
	ReadAgyUsage func(ctx context.Context) (string, error)
	// ReadAgyModels runs `agy models` (for tests; nil calls the configured agy binary / adapter).
	ReadAgyModels func(ctx context.Context) (string, error)
	// RunGH runs `gh` in a directory (for tests; nil invokes `gh` on PATH).
	RunGH func(ctx context.Context, cwd string, args ...string) ([]byte, error)
}

type agyResolveKey struct {
	model  string
	effort string
}

type agyResolvedName struct {
	id         string
	display    string
	resolvedAt time.Time
}

type usageState struct {
	mu          sync.Mutex
	agyRows     []agy.UsageRow
	agyFetched  time.Time
	agyResolved map[agyResolveKey]agyResolvedName
	alerted     map[string]map[string]time.Time // runID -> alertKey -> expiresAt
}

type agyReserveHit struct {
	left      int
	resets    string
	fetchedAt time.Time
	model     string
	display   string
	label     string
	// reviewsOnly: the row is under its reserve but past the reviews-only line, so only a reviewer may start on it.
	reviewsOnly bool
	weekly      bool
	// line is the percent used the row was held at (its reserve, or the reviews-only line).
	line float64
}

type claudeWindowMeta struct {
	key   string
	label string
}

var claudeWindowOrder = []claudeWindowMeta{
	{key: "five_hour", label: "five-hour window"},
	{key: "seven_day", label: "seven-day window"},
	{key: "seven_day_opus", label: "seven-day Opus"},
	{key: "seven_day_sonnet", label: "seven-day Sonnet"},
}

func claudeWindowLabel(key string) string {
	for _, w := range claudeWindowOrder {
		if w.key == key {
			return w.label
		}
	}
	return key
}

// percentSetting reads a Percent setting, or def when it is unreadable.
func (d *Daemon) percentSetting(r store.Reader, key string, def float64) float64 {
	v, err := d.Setting(r, key)
	if err != nil {
		return def
	}
	if f, ok := v.(float64); ok {
		return f
	}
	return def
}

// quotaStop returns the `quota_stop` percentage from settings (default 90): the short (five-hour) windows' reserve.
func (d *Daemon) quotaStop(r store.Reader) float64 { return d.percentSetting(r, "quota_stop", 90) }

// reserveLimits are the lines of one CLI's quota: the percent used at which a short window and a weekly window take
// no new work (`quota_stop`, `quota_stop_weekly`), and the one at which the CLI takes only PR reviews
// (`quota_reviews_only.<kind>`; 100 is off).
type reserveLimits struct {
	short, weekly, reviews float64
}

func (d *Daemon) reserveLimits(r store.Reader, kind string) reserveLimits {
	l := reserveLimits{short: d.quotaStop(r), weekly: d.percentSetting(r, "quota_stop_weekly", 90), reviews: 100}
	if kind == "claude" || kind == "agy" {
		l.reviews = d.percentSetting(r, "quota_reviews_only."+kind, 100)
	}
	return l
}

// off reports there is nothing to enforce: no reserve on any window and no reviews-only line.
func (l reserveLimits) off() bool { return l.short >= 100 && l.weekly >= 100 && l.reviews >= 100 }

// stopLine is the percent used at which a window takes no new work of any sort: its reserve.
func (l reserveLimits) stopLine(weekly bool) float64 {
	if weekly {
		return l.weekly
	}
	return l.short
}

// held says how a window at `used` percent is held for this sort of work: at its reserve (nothing starts), past the
// reviews-only line but under the reserve (a reviewer still starts; `reviewer` false), or not at all. `line` is the
// percent it was held at.
func (l reserveLimits) held(used float64, weekly, reviewer bool) (line float64, reviewsOnly, ok bool) {
	if stop := l.stopLine(weekly); stop < 100 && used >= stop {
		return stop, false, true
	}
	if !reviewer && l.reviews < 100 && used >= l.reviews {
		return l.reviews, true, true
	}
	return 0, false, false
}

func (d *Daemon) usageEnv(key string) string {
	if d != nil && d.rt != nil {
		if v, ok := d.rt.cfg.Env[key]; ok {
			return v
		}
	}
	return os.Getenv(key)
}

func (d *Daemon) cliBaseEnv() map[string]string {
	env := map[string]string{}
	for _, k := range inheritedEnv {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	if d != nil && d.rt != nil {
		for k, v := range d.rt.cfg.Env {
			env[k] = v
		}
	}
	return env
}

func (d *Daemon) claudeUsagePaths() (claudeDir, claudeJSON, creds string) {
	dir := d.usageEnv("CLAUDE_CONFIG_DIR")
	if dir != "" {
		return dir, filepath.Join(dir, ".claude.json"), filepath.Join(dir, ".credentials.json")
	}
	if testing.Testing() && (d == nil || d.rt == nil || (d.rt.cfg.Env["HOME"] == "" && d.rt.cfg.Env["CLAUDE_CONFIG_DIR"] == "")) {
		return "", "", ""
	}
	home := d.usageEnv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return "", "", ""
	}
	dir = filepath.Join(home, ".claude")
	return dir, filepath.Join(home, ".claude.json"), filepath.Join(dir, ".credentials.json")
}

func readClaudeUsageFile(path string) *hook.ClaudeUsageSnapshot {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return nil
	}
	var snap hook.ClaudeUsageSnapshot
	if json.Unmarshal(b, &snap) != nil || len(snap.Windows) == 0 {
		return nil
	}
	if snap.Fetched <= 0 {
		if st, serr := os.Stat(path); serr == nil {
			snap.Fetched = float64(st.ModTime().UnixNano()) / 1e9
		}
	}
	return &snap
}

func snapshotTime(snap *hook.ClaudeUsageSnapshot) time.Time {
	if snap == nil || snap.Fetched <= 0 {
		return time.Time{}
	}
	sec := int64(snap.Fetched)
	nsec := int64((snap.Fetched - float64(sec)) * 1e9)
	return time.Unix(sec, nsec)
}

func parseClaudeReset(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func claudeWindowStale(w hook.ClaudeUsageWindow, now time.Time) bool {
	if t, ok := parseClaudeReset(w.Resets); ok && !now.Before(t) {
		return true
	}
	return false
}

func failedWithin(path string, ttl time.Duration, now time.Time) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	return now.Sub(st.ModTime()) < ttl
}

func noteUsageFailure(path string) {
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = fsutil.WriteAtomic(path, []byte(time.Now().UTC().Format(time.RFC3339)), 0o600)
}

func (d *Daemon) readClaudeCachedFallback(claudeJSON string) *hook.ClaudeUsageSnapshot {
	if claudeJSON == "" {
		return nil
	}
	b, err := os.ReadFile(claudeJSON)
	if err != nil {
		return nil
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(b, &root) != nil {
		return nil
	}
	raw, ok := root["cachedUsageUtilization"]
	if !ok {
		return nil
	}
	var fields map[string]any
	if json.Unmarshal(raw, &fields) != nil || len(fields) == 0 {
		return nil
	}
	windowFields := fields
	if nested, ok := fields["utilization"].(map[string]any); ok && len(nested) > 0 {
		windowFields = nested
	}
	windows := map[string]hook.ClaudeUsageWindow{}
	for _, meta := range claudeWindowOrder {
		v, ok := windowFields[meta.key]
		if !ok || v == nil {
			continue
		}
		switch x := v.(type) {
		case float64:
			windows[meta.key] = hook.ClaudeUsageWindow{Used: x}
		case map[string]any:
			u, ok := x["utilization"].(float64)
			if !ok {
				u, ok = x["used_percentage"].(float64)
			}
			if !ok {
				continue
			}
			res, _ := x["resets_at"].(string)
			windows[meta.key] = hook.ClaudeUsageWindow{Used: u, Resets: res}
		}
	}
	if len(windows) == 0 {
		return nil
	}
	var fetched float64
	if ms, ok := fields["fetchedAtMs"].(float64); ok && ms > 0 {
		fetched = ms / 1000
	}
	return &hook.ClaudeUsageSnapshot{
		Windows: windows,
		Fetched: fetched,
		Source:  "cache",
	}
}

func fetchClaudeOAuthUsage(ctx context.Context, credsPath string, now time.Time) (*hook.ClaudeUsageSnapshot, error) {
	if credsPath == "" {
		return nil, nil
	}
	cb, err := os.ReadFile(credsPath)
	if err != nil {
		return nil, nil
	}
	var creds struct {
		OAuth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(cb, &creds) != nil || strings.TrimSpace(creds.OAuth.AccessToken) == "" {
		return nil, nil
	}
	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, claudeUsageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(creds.OAuth.AccessToken))
	req.Header.Set("anthropic-beta", claudeUsageBeta)
	req.Header.Set("User-Agent", "agent-mesh")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("claude oauth usage: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return nil, fmt.Errorf("claude oauth usage: malformed JSON")
	}
	windows := map[string]hook.ClaudeUsageWindow{}
	for _, meta := range claudeWindowOrder {
		raw, ok := payload[meta.key]
		if !ok || len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
			continue
		}
		var w struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    string   `json:"resets_at"`
		}
		if json.Unmarshal(raw, &w) != nil || w.Utilization == nil {
			continue
		}
		windows[meta.key] = hook.ClaudeUsageWindow{
			Used:   *w.Utilization,
			Resets: w.ResetsAt,
		}
	}
	if len(windows) == 0 {
		return nil, nil
	}
	return &hook.ClaudeUsageSnapshot{
		Windows: windows,
		Fetched: float64(now.UnixNano()) / 1e9,
		Source:  "live",
	}, nil
}

// claudeUsage returns Claude's newest usage reading, preferring a fresh <state>/claude-usage.json (recorded by
// `mesh statusline` or a previous live fetch), then the OAuth usage endpoint when `network` is true, then
// a stale file or ~/.claude.json `cachedUsageUtilization`.
func (d *Daemon) claudeUsage(ctx context.Context, network, force bool) *hook.ClaudeUsageSnapshot {
	now := d.now()
	usageFile := d.paths.ClaudeUsage()
	failedFile := d.paths.ClaudeUsageFailed()
	disk := readClaudeUsageFile(usageFile)
	if !force && disk != nil {
		if ft := snapshotTime(disk); !ft.IsZero() && now.Sub(ft) <= claudeUsageTTL {
			return disk
		}
	}
	_, claudeJSON, credsPath := d.claudeUsagePaths()
	if network && (force || !failedWithin(failedFile, usageFailureTTL, now)) {
		var live *hook.ClaudeUsageSnapshot
		var err error
		if d.rt.cfg.Usage != nil && d.rt.cfg.Usage.FetchClaudeUsage != nil {
			live, err = d.rt.cfg.Usage.FetchClaudeUsage(ctx)
		} else if d.rt.cfg.Usage == nil || !d.rt.cfg.Usage.DisableClaudeNetwork {
			live, err = fetchClaudeOAuthUsage(ctx, credsPath, now)
		}
		if err != nil {
			noteUsageFailure(failedFile)
		} else if live != nil && len(live.Windows) > 0 {
			if live.Fetched == 0 {
				live.Fetched = float64(now.UnixNano()) / 1e9
			}
			if live.Source == "" {
				live.Source = "live"
			}
			if b, err := json.Marshal(live); err == nil {
				_ = os.MkdirAll(filepath.Dir(usageFile), 0o700)
				_ = fsutil.WriteAtomic(usageFile, b, 0o600)
				_ = os.Remove(failedFile)
			}
			return live
		}
	}
	var best *hook.ClaudeUsageSnapshot
	var bestTime time.Time
	for _, cand := range []*hook.ClaudeUsageSnapshot{disk, d.readClaudeCachedFallback(claudeJSON)} {
		if cand == nil {
			continue
		}
		ft := snapshotTime(cand)
		if ft.IsZero() || now.Sub(ft) > claudeUsageStale {
			continue
		}
		if best == nil || ft.After(bestTime) {
			best, bestTime = cand, ft
		}
	}
	return best
}

func (d *Daemon) agyBinary() (string, bool) {
	if bin := d.rt.cfg.Binaries["agy"]; bin != "" {
		_, err := os.Stat(bin)
		return bin, err == nil
	}
	bin, err := lookAgy()
	return bin, err == nil
}

// lookAgy finds agy on PATH. A test never looks (it would find the real agy); one that wants an agy on PATH
// replaces it.
var lookAgy = func() (string, error) {
	if testing.Testing() {
		return "", exec.ErrNotFound
	}
	return exec.LookPath("agy")
}

func (d *Daemon) cachedAgyUsageRows(now time.Time) ([]agy.UsageRow, time.Time) {
	usageFile := d.paths.AgyUsage()
	if b, err := os.ReadFile(usageFile); err == nil && len(bytes.TrimSpace(b)) > 0 {
		if st, serr := os.Stat(usageFile); serr == nil && now.Sub(st.ModTime()) <= agyUsageMaxAge {
			if rows := agy.ParseUsage(string(b)); len(rows) > 0 {
				d.rt.usage.mu.Lock()
				d.rt.usage.agyRows = rows
				d.rt.usage.agyFetched = st.ModTime()
				d.rt.usage.mu.Unlock()
				return rows, st.ModTime()
			}
		}
	}
	d.rt.usage.mu.Lock()
	var rows []agy.UsageRow
	var fetchedAt time.Time
	if !d.rt.usage.agyFetched.IsZero() && now.Sub(d.rt.usage.agyFetched) <= agyUsageMaxAge {
		rows = append([]agy.UsageRow(nil), d.rt.usage.agyRows...)
		fetchedAt = d.rt.usage.agyFetched
	}
	d.rt.usage.mu.Unlock()
	return rows, fetchedAt
}

// agyUsageRows returns parsed `agy -p /usage` rows from <state>/agy-usage.txt or (when allowExec is true and
// cached rows are absent/stale) by running `agy -p /usage`. Never spawns `agy` when allowExec is false, and
// ignores readings older than agyUsageMaxAge.
func (d *Daemon) agyUsageRows(ctx context.Context, allowExec, force bool) ([]agy.UsageRow, time.Time) {
	now := d.now()
	usageFile := d.paths.AgyUsage()
	failedFile := d.paths.AgyUsageFailed()

	if !force {
		if b, err := os.ReadFile(usageFile); err == nil && len(bytes.TrimSpace(b)) > 0 {
			if st, serr := os.Stat(usageFile); serr == nil && now.Sub(st.ModTime()) <= agyUsageTTL {
				if rows := agy.ParseUsage(string(b)); len(rows) > 0 {
					d.rt.usage.mu.Lock()
					d.rt.usage.agyRows = rows
					d.rt.usage.agyFetched = st.ModTime()
					d.rt.usage.mu.Unlock()
					return rows, st.ModTime()
				}
			}
		}
	}

	// An agy that wanted a login is left alone for a while (until its token file changes): asking again would
	// only try to open the login again.
	_, loginHold := d.agyLoginState(now)
	if allowExec && !loginHold && (force || !failedWithin(failedFile, usageFailureTTL, now)) {
		var text string
		var err error
		if d.rt.cfg.Usage != nil && d.rt.cfg.Usage.ReadAgyUsage != nil {
			text, err = d.rt.cfg.Usage.ReadAgyUsage(ctx)
		} else if d.agyProbeBlocked() {
			err = probe.ErrLoginNeeded // no token file: agy is not run (agylogin.go)
		} else if bin, ok := d.agyBinary(); ok {
			// A probe: it can't open a login (internal/probe), and says so if it tries.
			out, cerr := probe.Run(ctx, probe.Spec{Bin: bin, Args: []string{"-p", "/usage", "--dangerously-skip-permissions"},
				Env: envSlice(d.cliBaseEnv()), Dir: d.paths.Probe(), Timeout: 45 * time.Second})
			text, err = string(out), cerr
		}
		if errors.Is(err, probe.ErrLoginNeeded) {
			d.noteAgyLogin()
			d.agyLoginAlertPass(ctx) // the roots of runs that use agy are told now, not at the next pass
		}
		if err != nil {
			noteUsageFailure(failedFile)
		} else if rows := agy.ParseUsage(text); len(rows) > 0 {
			_ = os.MkdirAll(filepath.Dir(usageFile), 0o700)
			_ = fsutil.WriteAtomic(usageFile, []byte(text), 0o600)
			_ = os.Remove(failedFile)
			d.clearAgyLogin()
			d.rt.usage.mu.Lock()
			d.rt.usage.agyRows = rows
			d.rt.usage.agyFetched = now
			d.rt.usage.mu.Unlock()
			return rows, now
		}
	}

	return d.cachedAgyUsageRows(now)
}

func envSlice(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func (d *Daemon) resolveAgyModelDisplay(ctx context.Context, model, effort string, allowExec bool) (string, string) {
	if model == "" {
		return "", ""
	}
	now := d.now()
	key := agyResolveKey{model: model, effort: effort}
	d.rt.usage.mu.Lock()
	cached, ok := d.rt.usage.agyResolved[key]
	d.rt.usage.mu.Unlock()
	if ok && now.Sub(cached.resolvedAt) <= agyResolveTTL {
		return cached.id, cached.display
	}
	if !allowExec {
		if ok {
			return cached.id, cached.display
		}
		return model, ""
	}
	var resolvedID, display string
	if d.rt.cfg.Usage != nil && d.rt.cfg.Usage.ReadAgyModels != nil {
		if listing, err := d.rt.cfg.Usage.ReadAgyModels(ctx); err == nil && listing != "" {
			if id, perr := agy.PickModel(listing, model, effort); perr == nil {
				resolvedID = id
				display = agy.ListingNames(listing)[id]
			}
		}
	} else if a, err := d.rt.adapters.Get("agy"); err == nil {
		if ag, ok := a.(*agy.Adapter); ok {
			if bin, have := d.agyBinary(); have || d.rt.cfg.Binaries["agy"] != "" {
				if id, disp, rerr := ag.ResolveForUsage(ctx, bin, model, effort, d.cliBaseEnv()); rerr == nil && id != "" {
					resolvedID, display = id, disp
				}
			}
		}
	}
	if resolvedID == "" {
		// Never cache a failed lookup, so a transient error doesn't leave the model unchecked.
		return model, ""
	}
	d.rt.usage.mu.Lock()
	if d.rt.usage.agyResolved == nil {
		d.rt.usage.agyResolved = map[agyResolveKey]agyResolvedName{}
	}
	d.rt.usage.agyResolved[key] = agyResolvedName{id: resolvedID, display: display, resolvedAt: now}
	d.rt.usage.mu.Unlock()
	return resolvedID, display
}

// warmUsage pre-fetches vendor usage and resolves model display names outside store.Write so pick()/reserveCheck()
// never shell out or hit the network under SQLite's writer lock. Skipped when no reserve is set.
func (d *Daemon) warmUsage(ctx context.Context, r store.Reader, kind string, extra ...agyResolveKey) {
	if d.reserveLimits(r, kind).off() {
		return
	}
	switch kind {
	case "claude":
		_ = d.claudeUsage(ctx, true, false)
	case "agy":
		rows, _ := d.agyUsageRows(ctx, true, false)
		if len(rows) > 0 || (d.rt.cfg.Usage != nil && d.rt.cfg.Usage.ReadAgyModels != nil) {
			for _, tier := range d.agyTierNames() {
				chain, effort, err := d.tierChain(r, "agy", tier)
				if err != nil {
					continue
				}
				// The efforts a model of the tier really runs at: its plan, its execution and its review (a reviewer
				// asks the cache at that effort, and a miss there reads as "not at the reserve").
				efforts := []string{effort}
				if e, err := d.execEffort(r, "agy", tier); err == nil && e != "" {
					efforts = append(efforts, e)
				}
				if e, err := d.reviewEffort(r, "agy", tier, false); err == nil && e != "" {
					efforts = append(efforts, e)
				}
				if e, err := d.reviewEffort(r, "agy", tier, true); err == nil && e != "" {
					efforts = append(efforts, e)
				}
				for _, m := range chain {
					seen := map[string]bool{}
					for _, e := range efforts {
						if !seen[e] {
							seen[e] = true
							d.resolveAgyModelDisplay(ctx, m, e, true)
						}
					}
				}
			}
			for _, k := range extra {
				if k.model != "" {
					d.resolveAgyModelDisplay(ctx, k.model, k.effort, true)
				}
			}
		}
	}
}

// agyRowHit is the row of a model's that is held for this sort of work: a row at its reserve outranks one only
// at the reviews-only line, and then the one with the least left.
func agyRowHit(rows []agy.UsageRow, lim reserveLimits, reviewer bool) (row agy.UsageRow, line float64, reviewsOnly, ok bool) {
	for _, r := range rows {
		ln, ro, held := lim.held(100-float64(r.Left), r.Weekly(), reviewer)
		if !held {
			continue
		}
		if !ok || (reviewsOnly && !ro) || (ro == reviewsOnly && r.Left < row.Left) {
			row, line, reviewsOnly, ok = r, ln, ro, true
		}
	}
	return row, line, reviewsOnly, ok
}

func (d *Daemon) agyModelAtReserveCached(model, effort string, lim reserveLimits, reviewer bool) *agyReserveHit {
	if lim.off() || model == "" {
		return nil
	}
	rows, fetchedAt := d.cachedAgyUsageRows(d.now())
	if len(rows) == 0 {
		return nil
	}
	id, disp := d.resolveAgyModelDisplay(context.Background(), model, effort, false)
	return agyHitOf(rows, fetchedAt, id, disp, lim, reviewer)
}

func (d *Daemon) agyModelAtReserve(ctx context.Context, model, effort string, lim reserveLimits, reviewer, allowExec bool) *agyReserveHit {
	if lim.off() || model == "" {
		return nil
	}
	rows, fetchedAt := d.agyUsageRows(ctx, allowExec, false)
	if len(rows) == 0 {
		return nil
	}
	id, disp := d.resolveAgyModelDisplay(ctx, model, effort, allowExec)
	return agyHitOf(rows, fetchedAt, id, disp, lim, reviewer)
}

func agyHitOf(rows []agy.UsageRow, fetchedAt time.Time, id, disp string, lim reserveLimits, reviewer bool) *agyReserveHit {
	q, line, ro, ok := agyRowHit(agy.ModelRows(id, disp, rows), lim, reviewer)
	if !ok {
		return nil
	}
	return &agyReserveHit{
		left:        q.Left,
		resets:      q.Resets,
		fetchedAt:   fetchedAt,
		model:       id,
		display:     disp,
		label:       q.Label,
		reviewsOnly: ro,
		weekly:      q.Weekly(),
		line:        line,
	}
}

func claudeWindowApplies(key, model string) bool {
	ml := strings.ToLower(model)
	switch key {
	case "five_hour", "seven_day":
		return true
	case "seven_day_opus":
		return strings.Contains(ml, "opus")
	case "seven_day_sonnet":
		return strings.Contains(ml, "sonnet")
	default:
		return false
	}
}

// claudeHit is a Claude window at its line for a sort of work.
type claudeHit struct {
	key, label  string
	used, line  float64
	resets      string
	reviewsOnly bool
}

// claudeAtReserve is the window of a Claude model's at its line for this sort of work: one at its reserve outranks
// one only at the reviews-only line.
func (d *Daemon) claudeAtReserve(ctx context.Context, model string, lim reserveLimits, reviewer, network bool) (claudeHit, bool) {
	if lim.off() {
		return claudeHit{}, false
	}
	snap := d.claudeUsage(ctx, network, false)
	if snap == nil {
		return claudeHit{}, false
	}
	now := d.now()
	var soft claudeHit
	haveSoft := false
	for _, meta := range claudeWindowOrder {
		if !claudeWindowApplies(meta.key, model) {
			continue
		}
		w, ok := snap.Windows[meta.key]
		if !ok || claudeWindowStale(w, now) {
			continue
		}
		line, ro, held := lim.held(w.Used, meta.key != "five_hour", reviewer)
		if !held {
			continue
		}
		h := claudeHit{key: meta.key, label: meta.label, used: w.Used, line: line, resets: w.Resets, reviewsOnly: ro}
		if !ro {
			return h, true
		}
		if !haveSoft {
			soft, haveSoft = h, true
		}
	}
	return soft, haveSoft
}

// ReserveGuard enforces the quota reserves: it returns ResourceExhausted if `kind`/`model` is at or past the
// reserve of its window (`quota_stop` short, `quota_stop_weekly` weekly), or, for work that is not a PR review
// (`reviewer` false), past `quota_reviews_only.<kind>`, and the run has not been placed `over_reserve` via
// `SetReserve` (`mesh reserve off`).
func (d *Daemon) ReserveGuard(ctx context.Context, r store.Reader, runID, kind, model, effort, what string, reviewer bool) error {
	d.warmUsage(ctx, r, kind)
	return d.reserveCheck(r, runID, kind, model, effort, what, reviewer)
}

// stopKey names the setting that holds a window's reserve.
func stopKey(weekly bool) string {
	if weekly {
		return "quota_stop_weekly"
	}
	return "quota_stop"
}

func (d *Daemon) reserveCheck(r store.Reader, runID, kind, model, effort, what string, reviewer bool) error {
	ctx := context.Background()
	allowLive := false
	if runID != "" {
		run, err := r.GetRun(runID)
		if err != nil {
			return err
		}
		if run.OverReserve {
			return nil
		}
	}
	lim := d.reserveLimits(r, kind)
	if lim.off() {
		return nil
	}
	switch kind {
	case "claude":
		if h, hit := d.claudeAtReserve(ctx, model, lim, reviewer, allowLive); hit {
			resetPart := ""
			if h.resets != "" {
				resetPart = ", resets " + h.resets
			}
			if h.reviewsOnly {
				return exhausted("%s: claude %s is at %.0f%% (quota_reviews_only.claude %.0f%%%s): claude takes only PR reviews now; use another CLI, wait for reset or run `mesh reserve off`",
					what, h.label, h.used, h.line, resetPart)
			}
			return exhausted("%s: claude %s is at %.0f%% (%s %.0f%%%s); wait for reset or run `mesh reserve off`",
				what, h.label, h.used, stopKey(h.key != "five_hour"), h.line, resetPart)
		}
	case "agy":
		if hit := d.agyModelAtReserve(ctx, model, effort, lim, reviewer, allowLive); hit != nil {
			name := hit.model
			if hit.display != "" {
				name = hit.display
			}
			resetPart := ""
			if hit.resets != "" {
				resetPart = ", resets " + hit.resets
			}
			if hit.reviewsOnly {
				return exhausted("%s: agy %s has %d%% left (%s, quota_reviews_only.agy %.0f%%%s): agy takes only PR reviews now; use another CLI, wait for reset or run `mesh reserve off`",
					what, name, hit.left, hit.label, hit.line, resetPart)
			}
			return exhausted("%s: agy %s has %d%% left (%s, %s %.0f%%%s); wait for reset or run `mesh reserve off`",
				what, name, hit.left, hit.label, stopKey(hit.weekly), hit.line, resetPart)
		}
	}
	return nil
}

func (d *Daemon) agyTierNames() []string {
	prefix := "tiers.agy."
	order := []string{"light", "mid", "strong"}
	seen := map[string]bool{}
	var out []string
	for _, t := range order {
		if _, ok := d.schema.Lookup(prefix + t + ".model"); ok {
			out = append(out, t)
			seen[t] = true
		}
	}
	for _, k := range d.schema.Keys(prefix) {
		if t, ok := strings.CutSuffix(strings.TrimPrefix(k, prefix), ".model"); ok && !seen[t] {
			out = append(out, t)
			seen[t] = true
		}
	}
	return out
}

// GetUsage reports Claude quota windows, agy quota rows, and each agy tier's current pick.
func (d *Daemon) GetUsage(ctx context.Context, req *connect.Request[v1.GetUsageRequest]) (*connect.Response[v1.GetUsageResponse], error) {
	r := d.store.Read(ctx)
	stop := d.quotaStop(r)
	refresh := req.Msg.Refresh
	climits, alimits := d.reserveLimits(r, "claude"), d.reserveLimits(r, "agy")

	out := &v1.GetUsageResponse{
		QuotaStop:         int32(math.Round(stop)),
		MinQuota:          int32(math.Round(100 - stop)),
		QuotaStopWeekly:   int32(math.Round(alimits.weekly)),
		ReviewsOnlyClaude: int32(math.Round(climits.reviews)),
		ReviewsOnlyAgy:    int32(math.Round(alimits.reviews)),
		SplitReserves:     true,
	}

	if snap := d.claudeUsage(ctx, true, refresh); snap != nil {
		cu := &v1.ClaudeUsage{
			Source: snap.Source,
		}
		if ft := snapshotTime(snap); !ft.IsZero() {
			cu.FetchedAt = timestamppb.New(ft)
		}
		now := d.now()
		for _, meta := range claudeWindowOrder {
			w, ok := snap.Windows[meta.key]
			if !ok {
				continue
			}
			line := climits.stopLine(meta.key != "five_hour")
			cw := &v1.ClaudeUsageWindow{
				Key:     meta.key,
				Label:   meta.label,
				Used:    w.Used,
				Reserve: int32(math.Round(line)),
			}
			if rt, ok := parseClaudeReset(w.Resets); ok {
				cw.ResetsAt = timestamppb.New(rt)
			}
			cu.Windows = append(cu.Windows, cw)
			if stale := claudeWindowStale(w, now); !stale {
				if w.Used >= line && line < 100 {
					cu.AtReserve = append(cu.AtReserve, meta.label)
				} else if climits.reviews < 100 && w.Used >= climits.reviews {
					cu.ReviewsOnly = append(cu.ReviewsOnly, meta.label)
				}
			}
		}
		out.Claude = cu
	}

	rows, _ := d.agyUsageRows(ctx, true, refresh)
	if since, _ := d.agyLoginState(d.now()); !since.IsZero() {
		out.Notes = append(out.Notes, agyLoginNote)
	}
	for _, row := range rows {
		line := alimits.stopLine(row.Weekly())
		atReserve := line < 100 && row.Left <= int(math.Round(100-line))
		out.AgyRows = append(out.AgyRows, &v1.AgyUsageRow{
			Group:       row.Group,
			Label:       row.Label,
			Left:        int32(row.Left),
			Resets:      row.Resets,
			AtReserve:   atReserve,
			ReviewsOnly: !atReserve && alimits.reviews < 100 && row.Left <= int(math.Round(100-alimits.reviews)),
		})
	}

	now := d.now()
	for _, tier := range d.agyTierNames() {
		chain, effort, err := d.tierChain(r, "agy", tier)
		if err != nil || len(chain) == 0 {
			continue
		}
		for _, m := range chain {
			d.resolveAgyModelDisplay(ctx, m, effort, true)
		}
		picked := d.firstFree(r, "agy", effort, chain, false)
		tp := &v1.AgyTierPick{Tier: tier, Model: picked}
		id, disp := d.resolveAgyModelDisplay(ctx, picked, effort, false)
		if q, ok := agy.ModelQuota(id, disp, rows); ok {
			tp.Left = int32(q.Left)
			tp.HasLeft = true
			tp.Group, tp.Label = q.Group, q.Label
		}
		for _, m := range chain {
			if until, _, err := r.ModelHold("agy", m, now); err == nil && !until.IsZero() {
				tp.Notes = append(tp.Notes, fmt.Sprintf("%s held until %s", m, until.UTC().Format(time.RFC3339)))
			} else if hit := d.agyModelAtReserveCached(m, effort, alimits, false); hit != nil {
				if hit.reviewsOnly {
					tp.Notes = append(tp.Notes, fmt.Sprintf("%s takes only PR reviews (%d%% left)", m, hit.left))
				} else {
					tp.Notes = append(tp.Notes, fmt.Sprintf("%s at reserve (%d%% left)", m, hit.left))
				}
			}
		}
		out.AgyTiers = append(out.AgyTiers, tp)
	}

	return connect.NewResponse(out), nil
}

// SetReserve toggles or inspects a run's `over_reserve` flag (`mesh reserve on|off`).
func (d *Daemon) SetReserve(ctx context.Context, req *connect.Request[v1.SetReserveRequest]) (*connect.Response[v1.SetReserveResponse], error) {
	m := req.Msg
	mode := strings.ToLower(strings.TrimSpace(m.Mode))
	if mode != "" && mode != "on" && mode != "off" {
		return nil, invalid("mode %q: must be \"on\", \"off\", or empty", m.Mode)
	}
	actor := auth.ActorFrom(ctx)
	runID := m.RunId
	if IsAgent(actor.Class) {
		if actor.RunId == "" {
			return nil, denied("caller has no run")
		}
		if runID != "" && runID != actor.RunId {
			return nil, denied("a root sets reserve only on its own run")
		}
		runID = actor.RunId
	} else if runID == "" {
		return nil, invalid("run_id is required")
	}

	formatSummary := func(over bool, r store.Reader) string {
		stop, weekly := d.quotaStop(r), d.percentSetting(r, "quota_stop_weekly", 90)
		if over {
			return fmt.Sprintf("reserve off: new tasks allowed past %.0f%% quota_stop (5h) and %.0f%% quota_stop_weekly, and past any reviews-only line", stop, weekly)
		}
		s := fmt.Sprintf("reserve on: new tasks stop at %.0f%% quota_stop (5h) and %.0f%% quota_stop_weekly (weekly)", stop, weekly)
		for _, kind := range []string{"claude", "agy"} {
			if at := d.reserveLimits(r, kind).reviews; at < 100 {
				s += fmt.Sprintf("; %s takes only PR reviews from %.0f%% (quota_reviews_only.%s)", kind, at, kind)
			}
		}
		return s
	}

	if mode == "" {
		r := d.store.Read(ctx)
		run, err := r.GetRun(runID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, notFound("no run %q", runID)
		}
		if err != nil {
			return nil, err
		}
		stop := d.quotaStop(r)
		return connect.NewResponse(&v1.SetReserveResponse{
			Run:         run,
			OverReserve: run.OverReserve,
			QuotaStop:   int32(math.Round(stop)),
			Summary:     formatSummary(run.OverReserve, r),
		}), nil
	}

	out := &v1.SetReserveResponse{}
	wantOver := mode == "off"
	_, err := d.write(ctx, m.Meta, agentmeshv1connect.MeshServiceSetReserveProcedure, out, func(tx *store.Tx) error {
		run, err := tx.GetRun(runID)
		if errors.Is(err, store.ErrNotFound) {
			return notFound("no run %q", runID)
		}
		if err != nil {
			return err
		}
		stop := d.quotaStop(tx.Reader)
		if run.OverReserve != wantOver {
			run.OverReserve = wantOver
			if err := tx.UpdateRun(run); err != nil {
				return err
			}
			if _, err := tx.AppendEvent(&v1.Event{
				RunId: run.Id,
				Payload: &v1.Event_ReserveChanged{
					ReserveChanged: &v1.ReserveChanged{OverReserve: wantOver},
				},
			}); err != nil {
				return err
			}
		}
		out.Run = run
		out.OverReserve = run.OverReserve
		out.QuotaStop = int32(math.Round(stop))
		out.Summary = formatSummary(run.OverReserve, tx.Reader)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

// reserveAlertPass notifies the lead of any open run (not over_reserve) when a vendor window or active agy
// model reaches its reserve (`quota_stop`, `quota_stop_weekly`) or its reviews-only line, deduped per run + alert
// key + reset window.
func (d *Daemon) reserveAlertPass(ctx context.Context) {
	r := d.store.Read(ctx)
	climits, alimits := d.reserveLimits(r, "claude"), d.reserveLimits(r, "agy")
	if climits.off() && alimits.off() {
		return
	}
	runs, err := r.ListRuns(true)
	if err != nil || len(runs) == 0 {
		return
	}
	agents, err := r.ListAgents("")
	if err != nil {
		return
	}
	leadsByRun := map[string]*v1.Agent{}
	usedKinds := map[string]bool{}
	// The models of a run's live agents; true when an agent that is not a reviewer is on it (a model only reviewers
	// use is never held back from them by the reviews-only line, so it is not told of it).
	activeClaudeModels := map[string]map[string]bool{}
	activeAgyModels := map[string]map[agyResolveKey]bool{}
	for _, a := range agents {
		if stopped(a) {
			continue
		}
		if a.Role == v1.AgentRole_AGENT_ROLE_ROOT {
			leadsByRun[a.RunId] = a
			continue
		}
		usedKinds[a.Kind] = true
		switch a.Kind {
		case "claude":
			if activeClaudeModels[a.RunId] == nil {
				activeClaudeModels[a.RunId] = map[string]bool{}
			}
			activeClaudeModels[a.RunId][a.Model] = activeClaudeModels[a.RunId][a.Model] || !isReviewer(a)
		case "agy":
			if a.Model != "" {
				if activeAgyModels[a.RunId] == nil {
					activeAgyModels[a.RunId] = map[agyResolveKey]bool{}
				}
				k := agyResolveKey{model: a.Model, effort: a.Effort}
				activeAgyModels[a.RunId][k] = activeAgyModels[a.RunId][k] || !isReviewer(a)
			}
		}
	}

	type alertItem struct {
		key       string
		expiresAt time.Time
		body      string
	}
	var claudeSnap *hook.ClaudeUsageSnapshot
	if usedKinds["claude"] {
		claudeSnap = d.claudeUsage(ctx, true, false)
	}

	if usedKinds["agy"] {
		_, _ = d.agyUsageRows(ctx, true, false)
	}

	now := d.now()
	for _, run := range runs {
		if run.OverReserve {
			continue
		}
		lead := leadsByRun[run.Id]
		if lead == nil {
			continue
		}
		var alerts []alertItem
		if models := activeClaudeModels[run.Id]; len(models) > 0 && claudeSnap != nil && !climits.off() {
			for _, meta := range claudeWindowOrder {
				w, ok := claudeSnap.Windows[meta.key]
				if !ok || claudeWindowStale(w, now) {
					continue
				}
				weekly := meta.key != "five_hour"
				stopLine := climits.stopLine(weekly)
				atStop := stopLine < 100 && w.Used >= stopLine
				reviewsOnly := !atStop && climits.reviews < 100 && w.Used >= climits.reviews
				if !atStop && !reviewsOnly {
					continue
				}
				applies := false
				for m, nonReviewer := range models {
					if claudeWindowApplies(meta.key, m) && (atStop || nonReviewer) {
						applies = true
						break
					}
				}
				if !applies {
					continue
				}
				dedupeKey, expiresAt := claudeAlertKeyAndExpiry(meta.key, w.Resets, now)
				resetPart := ""
				if w.Resets != "" {
					resetPart = ", resets " + w.Resets
				}
				body := fmt.Sprintf("[mesh alert reserve claude-%s] claude %s is at %.0f%% (%s %.0f%%%s). "+
					"New tasks on claude are held until reset or `mesh reserve off`.",
					meta.key, meta.label, w.Used, stopKey(weekly), stopLine, resetPart)
				if reviewsOnly {
					dedupeKey += ":reviews"
					body = fmt.Sprintf("[mesh alert reserve claude-%s] claude %s is at %.0f%% (quota_reviews_only.claude %.0f%%%s). "+
						"Claude now starts only PR reviews: send tasks to another CLI, wait for reset or `mesh reserve off`.",
						meta.key, meta.label, w.Used, climits.reviews, resetPart)
				}
				alerts = append(alerts, alertItem{key: dedupeKey, expiresAt: expiresAt, body: body})
			}
		}
		for k, nonReviewer := range activeAgyModels[run.Id] {
			if hit := d.agyModelAtReserve(ctx, k.model, k.effort, alimits, !nonReviewer, false); hit != nil {
				name := hit.model
				if hit.display != "" {
					name = hit.display
				}
				readingAt := hit.fetchedAt
				if readingAt.IsZero() {
					readingAt = now
				}
				_, expiresAt := agyAlertKeyAndExpiry(hit.label, hit.resets, readingAt, now)
				// Once per model and window: the reset is read as a span from each reading ("in 4h"), so the minute it
				// resolves to drifts from reading to reading and a key made of it was new at every pass. The alert is
				// held until the first estimate of the reset, and at least the cooldown.
				dedupeKey := "agy:" + hit.label + ":" + hit.model
				if min := now.Add(reserveAlertCooldown); expiresAt.Before(min) {
					expiresAt = min
				}
				resetPart := ""
				if hit.resets != "" {
					resetPart = ", resets " + hit.resets
				}
				body := fmt.Sprintf("[mesh alert reserve agy-%s] agy %s has %d%% left (%s, %s %.0f%%%s). "+
					"Consider switching models or `mesh reserve off`.",
					hit.model, name, hit.left, hit.label, stopKey(hit.weekly), hit.line, resetPart)
				if hit.reviewsOnly {
					dedupeKey += ":reviews"
					body = fmt.Sprintf("[mesh alert reserve agy-%s] agy %s has %d%% left (%s, quota_reviews_only.agy %.0f%%%s). "+
						"This model now starts only PR reviews: send tasks to another model or CLI, wait for reset or `mesh reserve off`.",
						hit.model, name, hit.left, hit.label, hit.line, resetPart)
				}
				alerts = append(alerts, alertItem{key: dedupeKey, expiresAt: expiresAt, body: body})
			}
		}
		for _, al := range alerts {
			if !d.shouldSendReserveAlert(run.Id, al.key, now) {
				continue
			}
			var delivered bool
			err := d.store.Write(ctx, meshdActor, "", func(tx *store.Tx) error {
				curLead, err := tx.GetAgent(lead.Id)
				if err != nil || stopped(curLead) {
					return nil
				}
				if _, err := storeMessage(tx, curLead, v1.MessageType_MESSAGE_TYPE_INFO, al.body); err != nil {
					return err
				}
				delivered = true
				return nil
			})
			if err == nil && delivered {
				d.markReserveAlertSent(run.Id, al.key, al.expiresAt)
				d.deliverSoon(lead.Id)
			}
		}
	}
}

var (
	wallClockResetRE = regexp.MustCompile(`(?i)^(?:(\d{1,2}):(\d{2})(?:\s*(am|pm))?|(\d{1,2})\s*(am|pm))(?:\s*\(([^)]+)\))?$`)
	dayDurPrefixRE   = regexp.MustCompile(`^(\d+)d(.*)$`)
)

func parseRelativeReset(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if len(s) >= 3 && strings.EqualFold(s[:3], "in ") {
		s = s[3:]
	}
	s = strings.ReplaceAll(strings.TrimSpace(s), " ", "")
	if s == "" || strings.HasPrefix(s, "-") {
		return 0, false
	}
	var extra time.Duration
	if m := dayDurPrefixRE.FindStringSubmatch(strings.ToLower(s)); m != nil {
		days, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, false
		}
		extra = time.Duration(days) * 24 * time.Hour
		s = m[2]
		if s == "" {
			return extra, extra > 0
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, false
	}
	total := extra + d
	return total, total > 0
}

func parseAgyReset(resets string, readingAt time.Time) (time.Time, bool) {
	s := strings.TrimSpace(resets)
	if s == "" {
		return time.Time{}, false
	}
	if t, ok := parseClaudeReset(s); ok {
		return t, true
	}
	if dur, ok := parseRelativeReset(s); ok {
		return readingAt.Add(dur), true
	}
	m := wallClockResetRE.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, false
	}
	hourStr, minStr, ampm, tzName := m[1], m[2], m[3], m[6]
	if hourStr == "" {
		hourStr, minStr, ampm = m[4], "0", m[5]
	}
	hour, herr := strconv.Atoi(hourStr)
	minute, merr := strconv.Atoi(minStr)
	if herr != nil || merr != nil || minute < 0 || minute > 59 {
		return time.Time{}, false
	}
	if ampm != "" {
		if hour < 1 || hour > 12 {
			return time.Time{}, false
		}
		hour %= 12
		if strings.EqualFold(ampm, "pm") {
			hour += 12
		}
	} else if hour < 0 || hour > 23 {
		return time.Time{}, false
	}
	loc := readingAt.Location()
	if tzName = strings.TrimSpace(tzName); tzName != "" {
		loaded, err := time.LoadLocation(tzName)
		if err != nil {
			return time.Time{}, false
		}
		loc = loaded
	}
	ref := readingAt.In(loc)
	at := time.Date(ref.Year(), ref.Month(), ref.Day(), hour, minute, 0, 0, loc)
	if !at.After(ref) {
		at = at.AddDate(0, 0, 1)
	}
	return at, true
}

func formatAlertResetKey(prefix, resets string, resetAt time.Time, hasReset bool, now time.Time) (string, time.Time) {
	if hasReset {
		return prefix + ":" + resetAt.UTC().Truncate(time.Minute).Format("2006-01-02T15:04Z"), resetAt
	}
	res := strings.TrimSpace(resets)
	if len(res) > 16 {
		res = res[:16]
	}
	return prefix + ":" + res, now.Add(reserveAlertCooldown)
}

func claudeAlertKeyAndExpiry(windowKey, resets string, now time.Time) (string, time.Time) {
	rt, ok := parseClaudeReset(resets)
	return formatAlertResetKey("claude:"+windowKey, resets, rt, ok, now)
}

func agyAlertKeyAndExpiry(label, resets string, readingAt, now time.Time) (string, time.Time) {
	rt, ok := parseAgyReset(resets, readingAt)
	return formatAlertResetKey("agy:"+label, resets, rt, ok, now)
}

func (d *Daemon) shouldSendReserveAlert(runID, key string, now time.Time) bool {
	d.rt.usage.mu.Lock()
	defer d.rt.usage.mu.Unlock()
	if m := d.rt.usage.alerted[runID]; m != nil {
		for k, exp := range m {
			if !now.Before(exp) {
				delete(m, k)
			}
		}
		if _, ok := m[key]; ok {
			return false
		}
	}
	return true
}

// claimReserveAlert is shouldSendReserveAlert and markReserveAlertSent in one lock section: it says whether the
// alert is to be sent, and marks it sent until expiresAt. A caller whose send fails gives the claim back
// (unclaimReserveAlert).
func (d *Daemon) claimReserveAlert(runID, key string, now, expiresAt time.Time) bool {
	d.rt.usage.mu.Lock()
	defer d.rt.usage.mu.Unlock()
	if m := d.rt.usage.alerted[runID]; m != nil {
		if exp, ok := m[key]; ok && now.Before(exp) {
			return false
		}
	}
	if d.rt.usage.alerted == nil {
		d.rt.usage.alerted = map[string]map[string]time.Time{}
	}
	if d.rt.usage.alerted[runID] == nil {
		d.rt.usage.alerted[runID] = map[string]time.Time{}
	}
	d.rt.usage.alerted[runID][key] = expiresAt
	return true
}

func (d *Daemon) unclaimReserveAlert(runID, key string) {
	d.rt.usage.mu.Lock()
	defer d.rt.usage.mu.Unlock()
	delete(d.rt.usage.alerted[runID], key)
}

// forgetReserveAlert drops an alert's mark in every run, so the next time its condition starts it is sent again.
func (d *Daemon) forgetReserveAlert(key string) {
	d.rt.usage.mu.Lock()
	defer d.rt.usage.mu.Unlock()
	for _, m := range d.rt.usage.alerted {
		delete(m, key)
	}
}

func (d *Daemon) markReserveAlertSent(runID, key string, expiresAt time.Time) {
	d.rt.usage.mu.Lock()
	defer d.rt.usage.mu.Unlock()
	if d.rt.usage.alerted == nil {
		d.rt.usage.alerted = map[string]map[string]time.Time{}
	}
	if d.rt.usage.alerted[runID] == nil {
		d.rt.usage.alerted[runID] = map[string]time.Time{}
	}
	d.rt.usage.alerted[runID][key] = expiresAt
}
