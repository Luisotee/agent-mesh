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
	"sort"
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
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

const (
	claudeUsageTTL    = 5 * time.Minute
	agyUsageTTL       = 5 * time.Minute
	agyUsageMaxAge    = 3 * agyUsageTTL
	agyResolveTTL     = agyUsageTTL
	usageFailureTTL   = 2 * time.Minute
	claudeUsageURL    = "https://api.anthropic.com/api/oauth/usage"
	claudeUsageBeta   = "oauth-2025-04-20"
	reserveAlertEvery = 15 * time.Minute
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
	alerted     map[string]map[string]time.Time // runID -> alertKey -> last alerted
}

type agyReserveHit struct {
	left    int
	resets  string
	model   string
	display string
	label   string
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

// quotaStop returns the `quota_stop` percentage from settings (default 90).
func (d *Daemon) quotaStop(r store.Reader) float64 {
	v, err := d.Setting(r, "quota_stop")
	if err != nil {
		return 90
	}
	if f, ok := v.(float64); ok {
		return f
	}
	return 90
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
	windows := map[string]hook.ClaudeUsageWindow{}
	for _, meta := range claudeWindowOrder {
		v, ok := fields[meta.key]
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
	fetched := float64(d.now().Unix())
	if st, err := os.Stat(claudeJSON); err == nil {
		fetched = float64(st.ModTime().Unix())
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
	if disk != nil {
		return disk
	}
	return d.readClaudeCachedFallback(claudeJSON)
}

func (d *Daemon) agyBinary() (string, bool) {
	if bin := d.rt.cfg.Binaries["agy"]; bin != "" {
		_, err := os.Stat(bin)
		return bin, err == nil
	}
	if testing.Testing() {
		return "", false
	}
	bin, err := exec.LookPath("agy")
	return bin, err == nil
}

func (d *Daemon) cachedAgyUsageRows(now time.Time) []agy.UsageRow {
	usageFile := d.paths.AgyUsage()
	if b, err := os.ReadFile(usageFile); err == nil && len(bytes.TrimSpace(b)) > 0 {
		if st, serr := os.Stat(usageFile); serr == nil && now.Sub(st.ModTime()) <= agyUsageMaxAge {
			if rows := agy.ParseUsage(string(b)); len(rows) > 0 {
				d.rt.usage.mu.Lock()
				d.rt.usage.agyRows = rows
				d.rt.usage.agyFetched = st.ModTime()
				d.rt.usage.mu.Unlock()
				return rows
			}
		}
	}
	d.rt.usage.mu.Lock()
	var rows []agy.UsageRow
	if !d.rt.usage.agyFetched.IsZero() && now.Sub(d.rt.usage.agyFetched) <= agyUsageMaxAge {
		rows = append([]agy.UsageRow(nil), d.rt.usage.agyRows...)
	}
	d.rt.usage.mu.Unlock()
	return rows
}

// agyUsageRows returns parsed `agy -p /usage` rows from <state>/agy-usage.txt or (when allowExec is true and
// cached rows are absent/stale) by running `agy -p /usage`. Never spawns `agy` when allowExec is false, and
// ignores readings older than agyUsageMaxAge.
func (d *Daemon) agyUsageRows(ctx context.Context, allowExec, force bool) []agy.UsageRow {
	now := d.now()
	usageFile := d.paths.AgyUsage()
	failedFile := d.paths.AgyUsageFailed()

	if !force {
		if b, err := os.ReadFile(usageFile); err == nil && len(bytes.TrimSpace(b)) > 0 {
			if st, serr := os.Stat(usageFile); serr == nil && now.Sub(st.ModTime()) <= agyUsageTTL {
				if rows := agy.ParseUsage(string(b)); len(rows) > 0 {
					d.rt.usage.mu.Lock()
					d.rt.usage.agyRows = rows
					d.rt.usage.agyFetched = now
					d.rt.usage.mu.Unlock()
					return rows
				}
			}
		}
	}

	if allowExec && (force || !failedWithin(failedFile, usageFailureTTL, now)) {
		var text string
		var err error
		if d.rt.cfg.Usage != nil && d.rt.cfg.Usage.ReadAgyUsage != nil {
			text, err = d.rt.cfg.Usage.ReadAgyUsage(ctx)
		} else if bin, ok := d.agyBinary(); ok {
			runCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			cmd := exec.CommandContext(runCtx, bin, "-p", "/usage", "--dangerously-skip-permissions")
			cmd.Env = envSlice(d.cliBaseEnv())
			out, cerr := cmd.Output()
			text, err = string(out), cerr
		}
		if err != nil {
			noteUsageFailure(failedFile)
		} else if rows := agy.ParseUsage(text); len(rows) > 0 {
			_ = os.MkdirAll(filepath.Dir(usageFile), 0o700)
			_ = fsutil.WriteAtomic(usageFile, []byte(text), 0o600)
			_ = os.Remove(failedFile)
			d.rt.usage.mu.Lock()
			d.rt.usage.agyRows = rows
			d.rt.usage.agyFetched = now
			d.rt.usage.mu.Unlock()
			return rows
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
// never shell out or hit the network under SQLite's writer lock. Skipped when quota_stop >= 100.
func (d *Daemon) warmUsage(ctx context.Context, r store.Reader, kind string, extra ...agyResolveKey) {
	stop := d.quotaStop(r)
	if stop >= 100 {
		return
	}
	switch kind {
	case "claude":
		_ = d.claudeUsage(ctx, true, false)
	case "agy":
		rows := d.agyUsageRows(ctx, true, false)
		if len(rows) > 0 || (d.rt.cfg.Usage != nil && d.rt.cfg.Usage.ReadAgyModels != nil) {
			for _, tier := range d.agyTierNames() {
				chain, effort, err := d.tierChain(r, "agy", tier)
				if err != nil {
					continue
				}
				for _, m := range chain {
					d.resolveAgyModelDisplay(ctx, m, effort, true)
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

func (d *Daemon) agyModelAtReserveCached(model, effort string, stop float64) *agyReserveHit {
	if stop >= 100 || model == "" {
		return nil
	}
	minQuota := int(math.Round(100 - stop))
	rows := d.cachedAgyUsageRows(d.now())
	if len(rows) == 0 {
		return nil
	}
	id, disp := d.resolveAgyModelDisplay(context.Background(), model, effort, false)
	q, ok := agy.ModelQuota(id, disp, rows)
	if !ok || q.Left > minQuota {
		return nil
	}
	return &agyReserveHit{
		left:    q.Left,
		resets:  q.Resets,
		model:   id,
		display: disp,
		label:   q.Label,
	}
}

func (d *Daemon) agyModelAtReserve(ctx context.Context, model, effort string, stop float64, allowExec bool) *agyReserveHit {
	if stop >= 100 || model == "" {
		return nil
	}
	rows := d.agyUsageRows(ctx, allowExec, false)
	if len(rows) == 0 {
		return nil
	}
	id, disp := d.resolveAgyModelDisplay(ctx, model, effort, allowExec)
	q, ok := agy.ModelQuota(id, disp, rows)
	if !ok {
		return nil
	}
	minQuota := int(math.Round(100 - stop))
	if q.Left > minQuota {
		return nil
	}
	return &agyReserveHit{
		left:    q.Left,
		resets:  q.Resets,
		model:   id,
		display: disp,
		label:   q.Label,
	}
}

func claudeWindowApplies(key, model string) bool {
	ml := strings.ToLower(model)
	switch key {
	case "five_hour", "seven_day":
		return true
	case "seven_day_opus":
		return model == "" || strings.Contains(ml, "opus")
	case "seven_day_sonnet":
		return model == "" || strings.Contains(ml, "sonnet")
	default:
		return false
	}
}

func (d *Daemon) claudeAtReserve(ctx context.Context, model string, stop float64, network bool) (key, label string, used float64, resets string, hit bool) {
	if stop >= 100 {
		return "", "", 0, "", false
	}
	snap := d.claudeUsage(ctx, network, false)
	if snap == nil {
		return "", "", 0, "", false
	}
	now := d.now()
	for _, meta := range claudeWindowOrder {
		if !claudeWindowApplies(meta.key, model) {
			continue
		}
		w, ok := snap.Windows[meta.key]
		if !ok || claudeWindowStale(w, now) {
			continue
		}
		if w.Used >= stop {
			return meta.key, meta.label, w.Used, w.Resets, true
		}
	}
	return "", "", 0, "", false
}

// ReserveGuard enforces `quota_stop`: it returns ResourceExhausted if `kind`/`model` is at or past `quota_stop`
// and the run has not been placed `over_reserve` via `SetReserve` (`mesh reserve off`).
func (d *Daemon) ReserveGuard(ctx context.Context, r store.Reader, runID, kind, model, effort, what string) error {
	d.warmUsage(ctx, r, kind)
	return d.reserveCheck(r, runID, kind, model, effort, what)
}

func (d *Daemon) reserveCheck(r store.Reader, runID, kind, model, effort, what string) error {
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
	stop := d.quotaStop(r)
	if stop >= 100 {
		return nil
	}
	switch kind {
	case "claude":
		if _, label, used, resets, hit := d.claudeAtReserve(ctx, model, stop, allowLive); hit {
			resetPart := ""
			if resets != "" {
				resetPart = ", resets " + resets
			}
			return exhausted("%s: claude %s is at %.0f%% (quota_stop %.0f%%%s); wait for reset or run `mesh reserve off`",
				what, label, used, stop, resetPart)
		}
	case "agy":
		if hit := d.agyModelAtReserve(ctx, model, effort, stop, allowLive); hit != nil {
			name := hit.model
			if hit.display != "" {
				name = hit.display
			}
			resetPart := ""
			if hit.resets != "" {
				resetPart = ", resets " + hit.resets
			}
			return exhausted("%s: agy %s has %d%% left (quota_stop %.0f%%%s); wait for reset or run `mesh reserve off`",
				what, name, hit.left, stop, resetPart)
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

	out := &v1.GetUsageResponse{
		QuotaStop: int32(math.Round(stop)),
		MinQuota:  int32(math.Round(100 - stop)),
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
			cw := &v1.ClaudeUsageWindow{
				Key:   meta.key,
				Label: meta.label,
				Used:  w.Used,
			}
			if rt, ok := parseClaudeReset(w.Resets); ok {
				cw.ResetsAt = timestamppb.New(rt)
			}
			cu.Windows = append(cu.Windows, cw)
			if stop < 100 && !claudeWindowStale(w, now) && w.Used >= stop {
				cu.AtReserve = append(cu.AtReserve, meta.label)
			}
		}
		out.Claude = cu
	}

	rows := d.agyUsageRows(ctx, true, refresh)
	for _, row := range rows {
		out.AgyRows = append(out.AgyRows, &v1.AgyUsageRow{
			Group:  row.Group,
			Label:  row.Label,
			Left:   int32(row.Left),
			Resets: row.Resets,
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
		picked := d.firstFree(r, "agy", effort, chain)
		tp := &v1.AgyTierPick{Tier: tier, Model: picked}
		id, disp := d.resolveAgyModelDisplay(ctx, picked, effort, false)
		if q, ok := agy.ModelQuota(id, disp, rows); ok {
			tp.Left = int32(q.Left)
			tp.HasLeft = true
		}
		for _, m := range chain {
			if until, _, err := r.ModelHold("agy", m, now); err == nil && !until.IsZero() {
				tp.Notes = append(tp.Notes, fmt.Sprintf("%s held until %s", m, until.UTC().Format(time.RFC3339)))
			} else if hit := d.agyModelAtReserveCached(m, effort, stop); hit != nil {
				tp.Notes = append(tp.Notes, fmt.Sprintf("%s at reserve (%d%% left)", m, hit.left))
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

	formatSummary := func(over bool, stop float64) string {
		if over {
			return fmt.Sprintf("reserve off: new tasks allowed past %.0f%% quota_stop", stop)
		}
		return fmt.Sprintf("reserve on: new tasks stop at %.0f%% quota_stop", stop)
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
			Summary:     formatSummary(run.OverReserve, stop),
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
		out.Summary = formatSummary(run.OverReserve, stop)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

// reserveAlertPass notifies the lead of any open run (not over_reserve) when a vendor window or active agy
// model reaches `quota_stop`, deduped per run + alert key + reset window.
func (d *Daemon) reserveAlertPass(ctx context.Context) {
	r := d.store.Read(ctx)
	stop := d.quotaStop(r)
	if stop >= 100 {
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
	activeAgyModels := map[string]map[agyResolveKey]bool{}
	for _, a := range agents {
		if a.Role == v1.AgentRole_AGENT_ROLE_ROOT && !stopped(a) {
			leadsByRun[a.RunId] = a
		}
		if !stopped(a) {
			usedKinds[a.Kind] = true
			if a.Kind == "agy" && a.Model != "" {
				if activeAgyModels[a.RunId] == nil {
					activeAgyModels[a.RunId] = map[agyResolveKey]bool{}
				}
				activeAgyModels[a.RunId][agyResolveKey{model: a.Model, effort: a.Effort}] = true
			}
		}
	}

	type alertItem struct {
		key  string
		body string
	}
	var claudeAlerts []alertItem
	if usedKinds["claude"] {
		snap := d.claudeUsage(ctx, true, false)
		if snap != nil {
			now := d.now()
			for _, meta := range claudeWindowOrder {
				w, ok := snap.Windows[meta.key]
				if !ok || claudeWindowStale(w, now) || w.Used < stop {
					continue
				}
				dedupeKey := "claude:" + meta.key + ":" + w.Resets
				resetPart := ""
				if w.Resets != "" {
					resetPart = ", resets " + w.Resets
				}
				claudeAlerts = append(claudeAlerts, alertItem{
					key: dedupeKey,
					body: fmt.Sprintf("[mesh alert reserve claude-%s] claude %s is at %.0f%% (quota_stop %.0f%%%s). "+
						"New tasks on claude are held until reset or `mesh reserve off`.",
						meta.key, meta.label, w.Used, stop, resetPart),
				})
			}
		}
	}

	if usedKinds["agy"] {
		_ = d.agyUsageRows(ctx, true, false)
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
		alerts := append([]alertItem(nil), claudeAlerts...)
		for k := range activeAgyModels[run.Id] {
			if hit := d.agyModelAtReserve(ctx, k.model, k.effort, stop, false); hit != nil {
				name := hit.model
				if hit.display != "" {
					name = hit.display
				}
				dedupeKey := "agy:" + hit.label + ":" + hit.resets
				resetPart := ""
				if hit.resets != "" {
					resetPart = ", resets " + hit.resets
				}
				alerts = append(alerts, alertItem{
					key: dedupeKey,
					body: fmt.Sprintf("[mesh alert reserve agy-%s] agy %s has %d%% left (quota_stop %.0f%%%s). "+
						"Consider switching models or `mesh reserve off`.",
						hit.model, name, hit.left, stop, resetPart),
				})
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
				d.markReserveAlertSent(run.Id, al.key, now)
				d.deliverSoon(lead.Id)
			}
		}
	}
}

func (d *Daemon) shouldSendReserveAlert(runID, key string, _ time.Time) bool {
	d.rt.usage.mu.Lock()
	defer d.rt.usage.mu.Unlock()
	if m := d.rt.usage.alerted[runID]; m != nil {
		if _, ok := m[key]; ok {
			return false
		}
	}
	return true
}

func (d *Daemon) markReserveAlertSent(runID, key string, now time.Time) {
	d.rt.usage.mu.Lock()
	defer d.rt.usage.mu.Unlock()
	if d.rt.usage.alerted == nil {
		d.rt.usage.alerted = map[string]map[string]time.Time{}
	}
	if d.rt.usage.alerted[runID] == nil {
		d.rt.usage.alerted[runID] = map[string]time.Time{}
	}
	d.rt.usage.alerted[runID][key] = now
}
