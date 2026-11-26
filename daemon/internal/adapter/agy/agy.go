// Package agy is the built-in adapter for Antigravity CLI (`agy`): launched in a PTY with `--settings`
// trustedWorkspaces and a git-excluded `.agents/hooks.json` in the worktree, local `PreToolUse` enforcement for
// read-only and planning agents, hook decoding (including RESOURCE_EXHAUSTED 429 limits), and session lookup.
package agy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protowire"
	_ "modernc.org/sqlite"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1/adapterv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/inputbox"
	"github.com/Luisotee/agent-mesh/daemon/internal/probe"
	"github.com/Luisotee/agent-mesh/daemon/internal/shellmatch"
)

// Kind is the adapter's kind.
const Kind = "agy"

// VerifiedVersions are the agy versions whose hook payloads this adapter was checked against: each has
// scrubbed payloads in testdata/<version>/ (daemon/scripts/capture-agy-hooks.sh), decoded and replayed through
// the reducer by fixtures_test.go.
var VerifiedVersions = []string{"1.5.17"}

// HookTimeout is the seconds agy gives each hook.
const HookTimeout = 10

// modelsCacheTTL is how long an `agy models` listing stays cached per binary.
const modelsCacheTTL = 6 * time.Hour

// namedEffort is the effort used when a model family is requested without one.
const namedEffort = "high"

// SessionVars are environment variables an agy session sets for itself that must never leak to spawned agents.
var SessionVars = []string{
	"ANTIGRAVITY_AGENT", "ANTIGRAVITY_AGENTAPI_EXE", "ANTIGRAVITY_APP_DATA_DIR",
	"ANTIGRAVITY_CONVERSATION_ID", "ANTIGRAVITY_CSRF_TOKEN", "ANTIGRAVITY_LS_ADDRESS",
	"ANTIGRAVITY_LS_VERSION", "ANTIGRAVITY_PROJECT_ID", "ANTIGRAVITY_SOURCE_METADATA",
	"ANTIGRAVITY_TRAJECTORY_ID",
}

// Adapter is the Antigravity CLI (`agy`) adapter.
type Adapter struct {
	adapterv1connect.UnimplementedCliAdapterServiceHandler
	// LoginNeeded is called when a probe of the CLI found it wants a login (the daemon records it).
	LoginNeeded func()
	// ListModels overrides `agy models` in tests.
	ListModels func(ctx context.Context, binary string, env map[string]string) (string, error)
}

// New returns the agy adapter.
func New() *Adapter { return &Adapter{} }

var _ adapter.Adapter = (*Adapter)(nil)

// Describe reports what the agy adapter supports. Note: agy fires no hooks at PTY startup, so it omits
// "startup-hook" and the daemon marks it idle once initial terminal output settles.
func (a *Adapter) Describe(context.Context, *connect.Request[adapterv1.DescribeRequest]) (*connect.Response[adapterv1.DescribeResponse], error) {
	return connect.NewResponse(&adapterv1.DescribeResponse{
		Kind:             Kind,
		DisplayName:      "Antigravity (agy)",
		VerifiedVersions: VerifiedVersions,
		Capabilities:     []string{"hooks", "session-lookup", "context"},
	}), nil
}

// ── models ──

var (
	geminiIDRE = regexp.MustCompile(`^gemini-([\d.]+)-([a-z]+)-(low|medium|high|xhigh|max)$`)
	claudeIDRE = regexp.MustCompile(`^claude-([a-z]+)-([\d-]+)-(low|medium|high|xhigh|max)$`)
)

type parsedModel struct {
	family  string
	version []int
	effort  string
}

func parseAgyID(id string) (parsedModel, bool) {
	var verStr, family, effort string
	if m := geminiIDRE.FindStringSubmatch(id); m != nil {
		verStr, family, effort = m[1], m[2], m[3]
	} else if m := claudeIDRE.FindStringSubmatch(id); m != nil {
		family, verStr, effort = m[1], m[2], m[3]
	} else {
		return parsedModel{}, false
	}
	var nums []int
	for _, s := range regexp.MustCompile(`\d+`).FindAllString(verStr, -1) {
		n, _ := strconv.Atoi(s)
		nums = append(nums, n)
	}
	return parsedModel{family: family, version: nums, effort: effort}, true
}

func compareVersions(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var va, vb int
		if i < len(a) {
			va = a[i]
		}
		if i < len(b) {
			vb = b[i]
		}
		if va != vb {
			if va < vb {
				return -1
			}
			return 1
		}
	}
	return 0
}

func listingIDs(listing string) []string {
	var ids []string
	for _, line := range strings.Split(listing, "\n") {
		id, _, _ := strings.Cut(line, "\t")
		id = strings.TrimSpace(id)
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// ListingNames maps model id -> display name from `agy models` output.
func ListingNames(listing string) map[string]string {
	names := map[string]string{}
	for _, line := range strings.Split(listing, "\n") {
		id, display, hasTab := strings.Cut(line, "\t")
		id = strings.TrimSpace(id)
		if id != "" && hasTab {
			names[id] = strings.TrimSpace(display)
		}
	}
	return names
}

// UsageRow is one quota row parsed from `agy -p /usage`.
type UsageRow struct {
	Group  string
	Label  string
	Left   int
	Resets string
}

var percentRE = regexp.MustCompile(`^(\d+)%$`)

// ParseUsage parses tab-separated rows of `agy -p /usage`: group, label, percent left, reset time.
func ParseUsage(text string) []UsageRow {
	var rows []UsageRow
	for _, line := range strings.Split(text, "\n") {
		parts := strings.Split(line, "\t")
		if len(parts) < 4 {
			continue
		}
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		m := percentRE.FindStringSubmatch(parts[2])
		if m == nil {
			continue
		}
		left, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		rows = append(rows, UsageRow{Group: parts[0], Label: parts[1], Left: left, Resets: parts[3]})
	}
	return rows
}

// QuotaApplies reports whether an `agy -p /usage` group covers `modelID` (display name `display`).
func QuotaApplies(group, modelID, display string) bool {
	gl := strings.ToLower(group)
	if strings.HasSuffix(gl, " models") {
		prefix := strings.TrimSuffix(gl, " models")
		for _, v := range strings.Split(prefix, " and ") {
			v = strings.TrimSpace(v)
			if v != "" && strings.HasPrefix(strings.ToLower(modelID), v+"-") {
				return true
			}
		}
		return false
	}
	return display != "" && strings.HasPrefix(strings.ToLower(display), gl)
}

// ModelQuota returns the tightest quota row covering `modelID` / `display`.
func ModelQuota(modelID, display string, rows []UsageRow) (UsageRow, bool) {
	var best UsageRow
	found := false
	for _, r := range rows {
		if !QuotaApplies(r.Group, modelID, display) {
			continue
		}
		if !found || r.Left < best.Left {
			best = r
			found = true
		}
	}
	return best, found
}

// ResolveForUsage resolves a model family or exact id at `effort` and returns the resolved model id and its
// display name from `agy models` (using the adapter's cached models listing).
func (a *Adapter) ResolveForUsage(ctx context.Context, bin, model, effort string, env map[string]string) (string, string, error) {
	if bin == "" {
		bin = "agy"
	}
	if effort == "" {
		effort = namedEffort
	}
	resolved := model
	if _, ok := parseAgyID(model); !ok {
		var err error
		resolved, err = a.resolveModel(ctx, bin, model, effort, env)
		if err != nil {
			return "", "", err
		}
	}
	listing, err := a.listModels(ctx, bin, env)
	if err != nil {
		return resolved, "", nil
	}
	return resolved, ListingNames(listing)[resolved], nil
}

// PickModel picks the newest model ID matching family and effort from `agy models` output, or returns family
// directly if it is already an exact listed ID.
func PickModel(listing, family, effort string) (string, error) {
	ids := listingIDs(listing)
	for _, id := range ids {
		if id == family {
			return family, nil
		}
	}
	var bestID string
	var bestVer []int
	for _, id := range ids {
		p, ok := parseAgyID(id)
		if !ok || p.family != family || p.effort != effort {
			continue
		}
		if bestID == "" || compareVersions(p.version, bestVer) > 0 || (compareVersions(p.version, bestVer) == 0 && id > bestID) {
			bestID, bestVer = id, p.version
		}
	}
	if bestID == "" {
		return "", fmt.Errorf("no agy model matches family=%s effort=%s; see `agy models`", family, effort)
	}
	return bestID, nil
}

// DescribeModel reports the effort an exact agy id names (`gemini-4-neon-low`: low); a family names none.
func (a *Adapter) DescribeModel(_ context.Context, req *connect.Request[adapterv1.DescribeModelRequest]) (*connect.Response[adapterv1.DescribeModelResponse], error) {
	out := &adapterv1.DescribeModelResponse{}
	if p, ok := parseAgyID(req.Msg.Model); ok {
		out.Effort = p.effort
	}
	return connect.NewResponse(out), nil
}

// resolveModel turns a model and effort into the id agy runs. An exact id at its own effort is kept as named (the
// daemon records an exact id named without an effort at that effort: DescribeModel); at another effort (a later
// SwitchAgent --effort) its family is resolved at the new one.
func (a *Adapter) resolveModel(ctx context.Context, bin, model, effort string, env map[string]string) (string, error) {
	if model == "" {
		return "", nil
	}
	if effort == "" {
		effort = namedEffort
	}
	if p, ok := parseAgyID(model); ok {
		if effort == p.effort {
			return model, nil
		}
		// An exact model id moved to another effort (SwitchAgent --effort): resolve its family at the new effort.
		model = p.family
	}
	listing, err := a.listModels(ctx, bin, env)
	if err != nil {
		return "", fmt.Errorf("listing agy models for %q (%s): %w", model, effort, err)
	}
	return PickModel(listing, model, effort)
}

// ModelFamily extracts the model family from an agy model ID (e.g. `gemini-4-neon-high` -> `neon`).
func ModelFamily(id string) (string, bool) {
	p, ok := parseAgyID(id)
	if !ok {
		return "", false
	}
	return p.family, true
}

// ListedFamilies returns the sorted unique model families listed by `agy models` (from the cached listing
// unless fresh is true). Returns nil, nil when !fresh and no cached listing exists yet.
func (a *Adapter) ListedFamilies(ctx context.Context, bin string, env map[string]string, fresh bool) ([]string, error) {
	if bin == "" {
		bin = "agy"
	}
	var listing string
	if !fresh {
		cacheFile := modelsCachePath(bin, env)
		if cacheFile == "" {
			return nil, nil
		}
		b, err := os.ReadFile(cacheFile)
		if err != nil || !hasParsedID(string(b)) {
			return nil, nil
		}
		listing = string(b)
	} else {
		var err error
		listing, err = a.listModelsDirect(ctx, bin, env, true)
		if err != nil {
			return nil, err
		}
	}
	seen := map[string]bool{}
	var families []string
	for _, id := range listingIDs(listing) {
		if p, ok := parseAgyID(id); ok && !seen[p.family] {
			seen[p.family] = true
			families = append(families, p.family)
		}
	}
	sort.Strings(families)
	return families, nil
}

func (a *Adapter) listModels(ctx context.Context, bin string, env map[string]string) (string, error) {
	return a.listModelsDirect(ctx, bin, env, false)
}

func (a *Adapter) listModelsDirect(ctx context.Context, bin string, env map[string]string, force bool) (string, error) {
	if a.ListModels != nil {
		return a.ListModels(ctx, bin, env)
	}
	cacheFile := modelsCachePath(bin, env)
	if !force && cacheFile != "" {
		if st, err := os.Stat(cacheFile); err == nil && time.Since(st.ModTime()) < modelsCacheTTL {
			if b, err := os.ReadFile(cacheFile); err == nil && hasParsedID(string(b)) {
				return string(b), nil
			}
		}
	}
	// A probe: it can't open a login (internal/probe), and a CLI that tries says so.
	out, err := probe.Run(ctx, probe.Spec{Bin: bin, Args: []string{"models"}, Env: mapToEnv(env), Timeout: 45 * time.Second})
	if errors.Is(err, probe.ErrLoginNeeded) && a.LoginNeeded != nil {
		a.LoginNeeded()
	}
	if err == nil && hasParsedID(string(out)) {
		if cacheFile != "" {
			_ = os.MkdirAll(filepath.Dir(cacheFile), 0o700)
			_ = fsutil.WriteAtomic(cacheFile, out, 0o600)
		}
		return string(out), nil
	}
	if !force && cacheFile != "" {
		if b, rerr := os.ReadFile(cacheFile); rerr == nil && hasParsedID(string(b)) {
			return string(b), nil
		}
	}
	if err != nil {
		return "", err
	}
	return "", errors.New("`agy models` returned no model ids")
}

func hasParsedID(listing string) bool {
	for _, id := range listingIDs(listing) {
		if _, ok := parseAgyID(id); ok {
			return true
		}
	}
	return false
}

func modelsCachePath(bin string, env map[string]string) string {
	base := env["XDG_CACHE_HOME"]
	if base == "" {
		if home := env["HOME"]; home != "" {
			base = filepath.Join(home, ".cache")
		}
	}
	if base == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(bin))
	return filepath.Join(base, "agent-mesh", "agy-models-"+hex.EncodeToString(sum[:6])+".txt")
}

func mapToEnv(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// ── launch & hooks.json ──

type hookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

type hookGroup struct {
	Matcher string        `json:"matcher"`
	Hooks   []hookCommand `json:"hooks"`
}

type hooksSpec struct {
	SessionStart   []hookCommand `json:"SessionStart"`
	PreInvocation  []hookCommand `json:"PreInvocation"`
	PostInvocation []hookCommand `json:"PostInvocation"`
	PreToolUse     []hookGroup   `json:"PreToolUse"`
	PostToolUse    []hookGroup   `json:"PostToolUse"`
	Stop           []hookCommand `json:"Stop"`
}

// HooksJSON builds the per-worktree `.agents/hooks.json` content. Every command checks `$AGENT_MESH_AGENT`, so a
// session that isn't the agent's (a person running agy in the same directory) never reports as it. The deny
// rules fail closed instead: that session's PreToolUse still checks them (`--check-only`, reporting nothing), so
// an agent whose environment lost the variable stays read-only. Every other event of such a session exits 0
// silently, Stop included: an agent that lost its `$AGENT_MESH_AGENT` reports no turn at all (its state comes
// back with its next reopen), which is the price of never reporting a person's session as the agent's. For
// agents with deny rules, PreToolUse matches `*` and fails closed with `HookTimeout`; for agents without deny
// rules, PreToolUse wraps the command to exit 0 silently so hook failures never block tool calls.
func HooksJSON(agentID string, hookArgv []string, deny bool, denyPaths ...string) ([]byte, error) {
	guarded := func(event string, extra ...string) string {
		argv := append(append([]string{}, hookArgv...), extra...)
		other := "exit 0"
		if len(extra) > 0 {
			other = "exec " + adapter.ShellQuote(append(append([]string{}, argv...), "--check-only", event)...)
		}
		argv = append(argv, event)
		return fmt.Sprintf(`[ "$AGENT_MESH_AGENT" = %s ] || %s; exec %s`,
			adapter.ShellQuote(agentID), other, adapter.ShellQuote(argv...))
	}
	var preGroup hookGroup
	if deny {
		var preExtra []string
		for _, p := range uniqueResolvedDirs(denyPaths...) {
			preExtra = append(preExtra, "--deny-path", p)
		}
		preExtra = append(preExtra, "--deny-publish")
		preGroup = hookGroup{
			Matcher: "*",
			Hooks:   []hookCommand{{Type: "command", Command: guarded("PreToolUse", preExtra...), Timeout: HookTimeout}},
		}
	} else {
		preArgv := append(append([]string{}, hookArgv...), "PreToolUse")
		preCmd := fmt.Sprintf(`[ "$AGENT_MESH_AGENT" = %s ] || exit 0; %s >/dev/null 2>&1; exit 0`,
			adapter.ShellQuote(agentID), adapter.ShellQuote(preArgv...))
		preGroup = hookGroup{
			Matcher: "*",
			Hooks:   []hookCommand{{Type: "command", Command: preCmd, Timeout: HookTimeout}},
		}
	}
	spec := map[string]hooksSpec{
		"agent-mesh": {
			SessionStart:   []hookCommand{{Type: "command", Command: guarded("SessionStart"), Timeout: HookTimeout}},
			PreInvocation:  []hookCommand{{Type: "command", Command: guarded("PreInvocation"), Timeout: HookTimeout}},
			PostInvocation: []hookCommand{{Type: "command", Command: guarded("PostInvocation"), Timeout: HookTimeout}},
			PreToolUse:     []hookGroup{preGroup},
			PostToolUse:    []hookGroup{{Matcher: "*", Hooks: []hookCommand{{Type: "command", Command: guarded("PostToolUse"), Timeout: HookTimeout}}}},
			Stop:           []hookCommand{{Type: "command", Command: guarded("Stop"), Timeout: HookTimeout}},
		},
	}
	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func uniqueResolvedDirs(paths ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		if p == "" {
			continue
		}
		candidates := []string{p}
		if abs, err := filepath.Abs(p); err == nil {
			candidates[0] = abs
			if real, err := filepath.EvalSymlinks(abs); err == nil && real != abs {
				candidates = append(candidates, real)
			}
		}
		for _, c := range candidates {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// PlanLaunch resolves the model/effort into an agy model ID, skips the folder-trust dialog with `--settings`,
// emits `.agents/hooks.json` as a git-excluded worktree file, and strips ANTIGRAVITY_* session env vars.
func (a *Adapter) PlanLaunch(ctx context.Context, req *connect.Request[adapterv1.PlanLaunchRequest]) (*connect.Response[adapterv1.PlanLaunchResponse], error) {
	m := req.Msg
	bin := m.Binary
	if bin == "" {
		bin = "agy"
	}
	env := map[string]string{}
	for k, v := range m.Env {
		env[k] = v
	}
	for _, k := range SessionVars {
		delete(env, k)
	}
	resolvedModel, err := a.resolveModel(ctx, bin, m.Model, m.Effort, env)
	if err != nil {
		return nil, err
	}
	if m.Standalone {
		if m.ResumeSessionId == "" {
			return nil, errors.New("standalone launch requires resume_session_id")
		}
		argv := []string{bin, "--conversation", m.ResumeSessionId}
		if resolvedModel != "" {
			argv = append(argv, "--model", resolvedModel)
		}
		return connect.NewResponse(&adapterv1.PlanLaunchResponse{Argv: argv, Env: env}), nil
	}
	argv := []string{bin}
	if m.ResumeSessionId != "" {
		argv = append(argv, "--conversation", m.ResumeSessionId)
	}
	if resolvedModel != "" {
		argv = append(argv, "--model", resolvedModel)
	}
	if m.Cwd != "" {
		trusted := uniqueResolvedDirs(m.Cwd)
		sb, err := json.Marshal(map[string]any{"trustedWorkspaces": trusted})
		if err != nil {
			return nil, err
		}
		argv = append(argv, "--settings", string(sb))
	}
	if m.SkipPermissions {
		argv = append(argv, "--dangerously-skip-permissions")
	}
	deny := adapter.ReadOnly(m.Role) || m.DenyEdits
	hooksContent, err := HooksJSON(m.AgentId, m.HookArgv, deny, m.Cwd, m.Repo)
	if err != nil {
		return nil, err
	}
	// A repo's own committed hooks stay in force for an agent that works on it (it runs inside the repo, with the
	// repo's hooks, as its CLAUDE.md applies); an asker or a reviewer, which may be reading an untrusted PR head,
	// gets only the mesh's.
	files := []*adapterv1.WorktreeFile{{
		Path:         ".agents/hooks.json",
		Content:      hooksContent,
		Mode:         0o600,
		GitExclude:   true,
		MergeTracked: !adapter.ReadOnly(m.Role),
	}}
	return connect.NewResponse(&adapterv1.PlanLaunchResponse{
		Argv:          argv,
		Env:           env,
		WorktreeFiles: files,
	}), nil
}

// ── PreToolUse deny evaluation ──

var editTools = map[string]bool{
	"write_file":                 true,
	"edit_file":                  true,
	"write_to_file":              true,
	"replace_file_content":       true,
	"multi_replace_file_content": true,
	"multi_edit":                 true,
	"notebook_edit":              true,
	"apply_patch":                true,
	"delete_file":                true,
}

type preToolPayload struct {
	Cwd      string `json:"cwd"`
	ToolCall struct {
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	} `json:"toolCall"`
}

// EvaluatePreToolUse checks an agy PreToolUse payload against denied workspace paths and publish commands.
// Returns (deny, reason). An empty reason with deny=false means allow (print nothing and exit 0). With any rule
// set, a payload it can't read is denied: the rules fail closed.
func EvaluatePreToolUse(raw []byte, fallbackCwd string, denyPaths []string, denyPublish bool) (bool, string) {
	if len(denyPaths) == 0 && !denyPublish {
		return false, ""
	}
	var p preToolPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return true, "read-only/planning agent: the tool call could not be checked (" + err.Error() + ")"
	}
	cwd := p.Cwd
	if cwd == "" {
		cwd = fallbackCwd
	}
	tool := strings.ToLower(p.ToolCall.Name)
	if editTools[tool] && len(denyPaths) > 0 {
		for _, target := range extractPaths(p.ToolCall.Args) {
			if pathUnderAny(target, cwd, denyPaths) {
				return true, fmt.Sprintf("read-only/planning agent may not modify %s", target)
			}
		}
	}
	if tool == "run_command" && denyPublish {
		if cmd := extractCommand(p.ToolCall.Args); cmd != "" {
			if pub := shellmatch.MatchPublishCommand(cmd); pub != "" {
				return true, fmt.Sprintf("read-only/planning agent may not run %q", pub)
			}
		}
	}
	return false, ""
}

func extractPaths(args map[string]any) []string {
	var out []string
	for _, k := range []string{"Path", "path", "TargetFile", "target_file", "FilePath", "file_path", "AbsolutePath", "absolute_path"} {
		if s, ok := args[k].(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	for _, k := range []string{"Patch", "patch"} {
		if s, ok := args[k].(string); ok && s != "" {
			for _, line := range strings.Split(s, "\n") {
				for _, prefix := range []string{"*** Add File: ", "*** Update File: ", "*** Delete File: ", "+++ b/", "--- a/"} {
					if rest, ok := strings.CutPrefix(line, prefix); ok && strings.TrimSpace(rest) != "" {
						out = append(out, strings.TrimSpace(rest))
					}
				}
			}
		}
	}
	return out
}

func extractCommand(args map[string]any) string {
	for _, k := range []string{"CommandLine", "commandLine", "command", "Command", "cmd"} {
		if s, ok := args[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func pathUnderAny(target, cwd string, roots []string) bool {
	if !filepath.IsAbs(target) {
		if cwd == "" {
			return false
		}
		target = filepath.Join(cwd, target)
	}
	resolvedTarget := resolvePathIncludingMissing(target)
	for _, r := range roots {
		if r == "" {
			continue
		}
		resolvedRoot := resolvePathIncludingMissing(r)
		if resolvedTarget == resolvedRoot || strings.HasPrefix(resolvedTarget, resolvedRoot+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// resolvePathIncludingMissing resolves symlinks on the longest existing ancestor and appends the remaining
// cleaned segments, so deny checks work for files that do not exist yet (e.g. write_file).
func resolvePathIncludingMissing(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	cur := abs
	var tail []string
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			parts := append([]string{real}, tail...)
			return filepath.Join(parts...)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
	}
}

// ── DecodeHook ──

type payload struct {
	ConversationID     string `json:"conversationId"`
	HookEventName      string `json:"hookEventName"`
	TerminationReason  string `json:"terminationReason"`
	Error              string `json:"error"`
	InvocationNum      int    `json:"invocationNum"`
	ContinuationActive bool   `json:"continuationActive"`
	ToolCall           struct {
		Name string `json:"name"`
	} `json:"toolCall"`
}

var (
	limitErrorRE = regexp.MustCompile(`(?i)(?:RESOURCE_EXHAUSTED|\b(?:code|HTTP)\s*429\b|quota\s+(?:reached|exceeded)|resource\s+has\s+been\s+exhausted)`)
	resetInRE    = regexp.MustCompile(`Resets in ([\dhms]+)`)
)

// DecodeHook converts an agy hook payload into neutral AgentEvents.
//
// In interactive PTY mode, agy fires no hooks at startup; SessionStart fires once on the first prompt of a
// fresh conversation (and never on `--conversation` resume). PreInvocation and Stop fire on both.
func (a *Adapter) DecodeHook(_ context.Context, req *connect.Request[adapterv1.DecodeHookRequest]) (*connect.Response[adapterv1.DecodeHookResponse], error) {
	m := req.Msg
	out := &adapterv1.DecodeHookResponse{}
	emit := func(ev *adapterv1.AgentEvent) {
		ev.Time = m.FiredAt
		out.Events = append(out.Events, ev)
	}
	var p payload
	if err := json.Unmarshal(m.Payload, &p); err != nil {
		emit(&adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_DecodeError{DecodeError: &adapterv1.DecodeError{Error: err.Error()}}})
		return connect.NewResponse(out), nil
	}
	name := p.HookEventName
	if name == "" {
		name = m.EventName
	}
	if m.SessionId != "" && p.ConversationID != "" && p.ConversationID != m.SessionId {
		return connect.NewResponse(out), nil
	}
	switch name {
	case "SessionStart":
		emit(&adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_SessionStarted{
			SessionStarted: &adapterv1.SessionStarted{SessionId: p.ConversationID, Source: "turn"},
		}})
	case "PreInvocation":
		if m.SessionId == "" && p.ConversationID != "" {
			emit(&adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_SessionStarted{
				SessionStarted: &adapterv1.SessionStarted{SessionId: p.ConversationID, Source: "turn"},
			}})
		}
		emit(&adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_TurnStarted{TurnStarted: &adapterv1.TurnStarted{}}})
	case "PreToolUse", "PostToolUse":
		detail := p.ToolCall.Name
		if detail == "" {
			detail = "tool"
		}
		emit(&adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_Activity{Activity: &adapterv1.Activity{Detail: detail}}})
	case "PostInvocation":
		emit(&adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_Activity{Activity: &adapterv1.Activity{Detail: "thinking"}}})
	case "Stop":
		if p.ContinuationActive {
			// A Stop agy goes on from (a continuation, which ends in a Stop of its own): the turn is not over, so
			// it is only activity, and no doorbell is typed into it. `fullyIdle: false` alone still ends the turn:
			// nothing says another Stop follows it, and an agent left WORKING would never get a doorbell. Every
			// captured Stop (1.5.17) has fullyIdle true and no continuation.
			emit(&adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_Activity{Activity: &adapterv1.Activity{Detail: "continuing"}}})
			break
		}
		if p.TerminationReason == "ERROR" || p.Error != "" {
			errText := p.Error
			if errText == "" {
				errText = "ERROR"
			}
			if limitErrorRE.MatchString(errText) {
				var reset string
				if rm := resetInRE.FindStringSubmatch(errText); rm != nil {
					reset = rm[1]
				}
				emit(&adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_RateLimited{
					RateLimited: &adapterv1.RateLimited{Message: firstLine(errText), ResetIn: reset},
				}})
				break
			}
			emit(&adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_TurnFailed{
				TurnFailed: &adapterv1.TurnFailed{Error: firstLine(errText)},
			}})
			break
		}
		emit(&adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_TurnEnded{TurnEnded: &adapterv1.TurnEnded{}}})
	}
	return connect.NewResponse(out), nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// ── LookupSession ──

// LookupSession finds an agy conversation ID from conversation_summaries.db and history.jsonl when no hook
// recorded one yet (e.g. a crash before the first turn): the newest top-level conversation in the agent's
// worktree, which is the agent's own. A shared main checkout (`cwd == repo`) is never matched, so it cannot pick
// up a person's own session there; the daemon's first prompt is a doorbell that names neither agent nor run. A
// conversation older than the agent (`not_before`) is someone else's: one with a history entry from before it, or
// a summary last modified before it (agy's index keeps no creation time).
func (a *Adapter) LookupSession(_ context.Context, req *connect.Request[adapterv1.LookupSessionRequest]) (*connect.Response[adapterv1.LookupSessionResponse], error) {
	m := req.Msg
	summariesPath, historyPath := sessionSources(m.Env)
	cwd := m.Cwd
	if cwd == "" || m.Repo != "" && resolvePathIncludingMissing(cwd) == resolvePathIncludingMissing(m.Repo) {
		return connect.NewResponse(&adapterv1.LookupSessionResponse{}), nil
	}
	var notBefore time.Time
	if m.NotBefore != nil {
		notBefore = m.NotBefore.AsTime()
	}
	fromHistory, older := lookupInHistory(historyPath, cwd, notBefore)
	if id := lookupInSummaries(summariesPath, cwd, notBefore, older); id != "" {
		return connect.NewResponse(&adapterv1.LookupSessionResponse{SessionId: id}), nil
	}
	return connect.NewResponse(&adapterv1.LookupSessionResponse{SessionId: fromHistory}), nil
}

func sessionSources(env map[string]string) (summaries, history string) {
	if p := env["AGENT_MESH_AGY_SUMMARIES"]; p != "" {
		summaries = p
	}
	if p := env["AGENT_MESH_AGY_HISTORY"]; p != "" {
		history = p
	}
	home := env["HOME"]
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home != "" {
		agyHome := filepath.Join(home, ".gemini", "antigravity-cli")
		if summaries == "" {
			summaries = filepath.Join(agyHome, "conversation_summaries.db")
		}
		if history == "" {
			history = filepath.Join(agyHome, "history.jsonl")
		}
	}
	return summaries, history
}

func lookupInSummaries(dbPath, cwd string, notBefore time.Time, older map[string]bool) string {
	if dbPath == "" {
		return ""
	}
	if _, err := os.Stat(dbPath); err != nil {
		return ""
	}
	uriPath := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(filepath.Clean(dbPath))
	db, err := sql.Open("sqlite", "file:"+uriPath+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return ""
	}
	defer db.Close()
	rows, err := db.Query(`SELECT conversation_id, workspace_uris, CAST(last_modified_time AS TEXT) FROM conversation_summaries
		WHERE nesting_depth = 0 AND step_count > 0 ORDER BY last_modified_time`)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var byCwd string
	for rows.Next() {
		var conv, urisJSON string
		var modified sql.NullString
		if err := rows.Scan(&conv, &urisJSON, &modified); err != nil || conv == "" || older[conv] {
			continue
		}
		if t, ok := parseAgyTime(modified.String); ok && !notBefore.IsZero() && t.Before(notBefore) {
			continue
		}
		if workspaceMatches(cwd, parseURIList(urisJSON)) {
			byCwd = conv
		}
	}
	return byCwd
}

// parseAgyTime reads a time as agy's index stores it (`2026-11-25 19:29:11.349387162+00:00`).
func parseAgyTime(s string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999Z07:00", time.RFC3339Nano} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// lookupInHistory returns the newest conversation in cwd that history.jsonl shows, and every conversation it
// shows a prompt of from before notBefore (millisecond timestamps), which is older than the agent.
func lookupInHistory(histPath, cwd string, notBefore time.Time) (string, map[string]bool) {
	older := map[string]bool{}
	if histPath == "" {
		return "", older
	}
	b, err := os.ReadFile(histPath)
	if err != nil {
		return "", older
	}
	var inCwd []string
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry struct {
			ConversationID string   `json:"conversationId"`
			Timestamp      int64    `json:"timestamp"`
			Workspace      string   `json:"workspace"`
			Workspaces     []string `json:"workspaces"`
		}
		if json.Unmarshal(line, &entry) != nil || entry.ConversationID == "" {
			continue
		}
		if !notBefore.IsZero() && entry.Timestamp > 0 && time.UnixMilli(entry.Timestamp).Before(notBefore) {
			older[entry.ConversationID] = true
		}
		ws := entry.Workspaces
		if len(ws) == 0 && entry.Workspace != "" {
			ws = []string{entry.Workspace}
		}
		if workspaceMatches(cwd, ws) {
			inCwd = append(inCwd, entry.ConversationID)
		}
	}
	for i := len(inCwd) - 1; i >= 0; i-- {
		if !older[inCwd[i]] {
			return inCwd[i], older
		}
	}
	return "", older
}

func parseURIList(raw string) []string {
	var uris []string
	if json.Unmarshal([]byte(raw), &uris) != nil {
		return nil
	}
	out := make([]string, 0, len(uris))
	for _, u := range uris {
		if parsed, err := url.Parse(u); err == nil && parsed.Path != "" {
			out = append(out, parsed.Path)
		} else if u != "" {
			out = append(out, u)
		}
	}
	return out
}

func workspaceMatches(cwd string, workspaces []string) bool {
	want := resolvePathIncludingMissing(cwd)
	for _, w := range workspaces {
		if w != "" && resolvePathIncludingMissing(w) == want {
			return true
		}
	}
	return false
}

// ReadInputBox reads the input box from a repaint: agy's rules box, its empty-box hint in colour 8 as well as dim.
func (a *Adapter) ReadInputBox(_ context.Context, req *connect.Request[adapterv1.ReadInputBoxRequest]) (*connect.Response[adapterv1.ReadInputBoxResponse], error) {
	return connect.NewResponse(adapter.ReadInputBox(req.Msg, inputbox.Agy)), nil
}

// ── cost / step usage ──

// TokenCall is one model step decoded from an agy conversation SQLite DB.
type TokenCall struct {
	Time   time.Time
	Input  int64
	Read   int64
	Output int64
}

// ConversationsDir returns the folder holding agy `<session>.db` files for env.
func ConversationsDir(env map[string]string) string {
	if p := env["AGENT_MESH_AGY_CONVERSATIONS"]; p != "" {
		return p
	}
	if p := env["MESH_AGY_CONVERSATIONS"]; p != "" {
		return p
	}
	home := env["HOME"]
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
}

// RecentConversations returns conversation ids whose `<session>.db` mtime is at or after `since` (all if zero).
func RecentConversations(convDir string, since time.Time) []string {
	if convDir == "" {
		return nil
	}
	entries, err := os.ReadDir(convDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		if !since.IsZero() {
			info, err := e.Info()
			if err != nil || info.ModTime().Before(since) {
				continue
			}
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".db"))
	}
	sort.Strings(out)
	return out
}

// ConversationCalls decodes step timestamps and token usage from `<convDir>/<session>.db`.
func ConversationCalls(convDir, session string) []TokenCall {
	if convDir == "" || session == "" {
		return nil
	}
	dbPath := filepath.Join(convDir, session+".db")
	if _, err := os.Stat(dbPath); err != nil {
		return nil
	}
	uriPath := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(filepath.Clean(dbPath))
	db, err := sql.Open("sqlite", "file:"+uriPath+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, err := db.Query(`SELECT metadata FROM steps ORDER BY idx`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var calls []TokenCall
	for rows.Next() {
		var meta []byte
		if err := rows.Scan(&meta); err != nil || len(meta) == 0 {
			continue
		}
		if c, ok := decodeStepMetadata(meta); ok {
			calls = append(calls, c)
		}
	}
	return calls
}

func decodeStepMetadata(meta []byte) (TokenCall, bool) {
	var stampBytes, usageBytes []byte
	for len(meta) > 0 {
		num, typ, n := protowire.ConsumeTag(meta)
		if n < 0 {
			return TokenCall{}, false
		}
		meta = meta[n:]
		if typ == protowire.BytesType {
			v, vn := protowire.ConsumeBytes(meta)
			if vn < 0 {
				return TokenCall{}, false
			}
			meta = meta[vn:]
			switch num {
			case 1:
				stampBytes = v
			case 9:
				usageBytes = v
			}
		} else {
			sn := protowire.ConsumeFieldValue(num, typ, meta)
			if sn < 0 {
				return TokenCall{}, false
			}
			meta = meta[sn:]
		}
	}
	if len(stampBytes) == 0 || len(usageBytes) == 0 {
		return TokenCall{}, false
	}
	var sec int64
	for len(stampBytes) > 0 {
		num, typ, n := protowire.ConsumeTag(stampBytes)
		if n < 0 {
			return TokenCall{}, false
		}
		stampBytes = stampBytes[n:]
		if typ == protowire.VarintType {
			v, vn := protowire.ConsumeVarint(stampBytes)
			if vn < 0 {
				return TokenCall{}, false
			}
			stampBytes = stampBytes[vn:]
			if num == 1 {
				sec = int64(v)
			}
		} else {
			sn := protowire.ConsumeFieldValue(num, typ, stampBytes)
			if sn < 0 {
				return TokenCall{}, false
			}
			stampBytes = stampBytes[sn:]
		}
	}
	if sec <= 0 {
		return TokenCall{}, false
	}
	var hasInput bool
	var input, read, output int64
	for len(usageBytes) > 0 {
		num, typ, n := protowire.ConsumeTag(usageBytes)
		if n < 0 {
			return TokenCall{}, false
		}
		usageBytes = usageBytes[n:]
		if typ == protowire.VarintType {
			v, vn := protowire.ConsumeVarint(usageBytes)
			if vn < 0 {
				return TokenCall{}, false
			}
			usageBytes = usageBytes[vn:]
			switch num {
			case 2:
				input = int64(v)
				hasInput = true
			case 5:
				read = int64(v)
			case 3:
				output = int64(v)
			}
		} else {
			sn := protowire.ConsumeFieldValue(num, typ, usageBytes)
			if sn < 0 {
				return TokenCall{}, false
			}
			usageBytes = usageBytes[sn:]
		}
	}
	if !hasInput {
		return TokenCall{}, false
	}
	return TokenCall{Time: time.Unix(sec, 0), Input: input, Read: read, Output: output}, true
}
