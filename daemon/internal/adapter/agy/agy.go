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
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	_ "modernc.org/sqlite"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1/adapterv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
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
		Capabilities:     []string{"hooks", "session-lookup"},
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

func (a *Adapter) listModels(ctx context.Context, bin string, env map[string]string) (string, error) {
	if a.ListModels != nil {
		return a.ListModels(ctx, bin, env)
	}
	cacheFile := modelsCachePath(bin, env)
	if cacheFile != "" {
		if st, err := os.Stat(cacheFile); err == nil && time.Since(st.ModTime()) < modelsCacheTTL {
			if b, err := os.ReadFile(cacheFile); err == nil && hasParsedID(string(b)) {
				return string(b), nil
			}
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, "models")
	if len(env) > 0 {
		cmd.Env = mapToEnv(env)
	}
	out, err := cmd.Output()
	if err == nil && hasParsedID(string(out)) {
		if cacheFile != "" {
			_ = os.MkdirAll(filepath.Dir(cacheFile), 0o700)
			_ = fsutil.WriteAtomic(cacheFile, out, 0o600)
		}
		return string(out), nil
	}
	if cacheFile != "" {
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
// back with its next reopen), which is the price of never reporting a person's session as the agent's.
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
	var preExtra []string
	if deny {
		for _, p := range uniqueResolvedDirs(denyPaths...) {
			preExtra = append(preExtra, "--deny-path", p)
		}
		preExtra = append(preExtra, "--deny-publish")
	}
	spec := map[string]hooksSpec{
		"agent-mesh": {
			SessionStart:   []hookCommand{{Type: "command", Command: guarded("SessionStart"), Timeout: HookTimeout}},
			PreInvocation:  []hookCommand{{Type: "command", Command: guarded("PreInvocation"), Timeout: HookTimeout}},
			PostInvocation: []hookCommand{{Type: "command", Command: guarded("PostInvocation"), Timeout: HookTimeout}},
			PreToolUse:     []hookGroup{{Matcher: "*", Hooks: []hookCommand{{Type: "command", Command: guarded("PreToolUse", preExtra...), Timeout: HookTimeout}}}},
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

// ghReads are the gh commands a read-only or planning agent may run, by group and verb: reads of the remote
// and local inspection. `*` is any verb of the group, and a group with only `*` and no verb is the command
// itself (`gh status`). Everything else is a publish (a create, comment, review, merge, close, edit, delete, a
// workflow run, a secret set, an extension, an alias), so a gh command added later is denied until listed here.
// `git push` and gh api writes are matched apart (matchPublishTokens, matchGh). The Claude adapter's PreToolUse
// check uses the same list (MatchPublishCommand).
var ghReads = map[string]map[string]bool{
	"pr":          {"list": true, "view": true, "diff": true, "checks": true, "status": true},
	"issue":       {"list": true, "view": true, "status": true},
	"release":     {"list": true, "view": true, "download": true},
	"repo":        {"view": true, "list": true, "clone": true, "gitignore": true, "license": true},
	"run":         {"list": true, "view": true, "watch": true, "download": true},
	"workflow":    {"list": true, "view": true},
	"gist":        {"list": true, "view": true, "clone": true},
	"label":       {"list": true},
	"cache":       {"list": true},
	"secret":      {"list": true},
	"variable":    {"list": true, "get": true},
	"ruleset":     {"list": true, "view": true, "check": true},
	"project":     {"list": true, "view": true, "field-list": true, "item-list": true},
	"org":         {"list": true},
	"codespace":   {"list": true, "view": true},
	"ssh-key":     {"list": true},
	"gpg-key":     {"list": true},
	"auth":        {"status": true},
	"config":      {"get": true, "list": true},
	"attestation": {"verify": true, "download": true, "inspect": true},
	"search":      {"*": true},
	"status":      {"*": true},
	"browse":      {"*": true},
	"help":        {"*": true},
	"version":     {"*": true},
	"completion":  {"*": true},
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
			if pub := matchPublishCommand(cmd); pub != "" {
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

var segmentSplitRE = regexp.MustCompile(`(?:\|\||&&|[;\n|&])+`)

// maxShellDepth bounds how deep `sh -c`, `eval` and `env -S` strings are looked into.
const maxShellDepth = 4

// matchPublishCommand names the publishing command a shell command line runs (`git push`, a gh write), or "".
// It reads the line the way a careful person would, not as a shell does: each segment between `;`, `&&`, `|`
// and `$(…)` is unwrapped (variable assignments, `env`, `sudo`, `nohup`, `timeout`, `xargs` and the like, `sh -c`
// and `eval` strings) and its command checked. It guards against an agent's honest mistake, not a determined one
// (a script it writes, a git alias): the role's prompt says what it may do.
func matchPublishCommand(cmdline string) string { return matchPublishDepth(cmdline, 0) }

// MatchPublishCommand is matchPublishCommand for the Claude adapter's PreToolUse check: one matcher, one list of
// gh reads, for both CLIs.
func MatchPublishCommand(cmdline string) string { return matchPublishCommand(cmdline) }

func matchPublishDepth(cmdline string, depth int) string {
	if depth > maxShellDepth {
		return ""
	}
	// Also inspect subshells $(...) and `...` (which run inside double quotes too) by turning them into segment
	// boundaries, and the (…) and { …; } groups outside quotes, so that `grep "(git push)"` stays a read.
	expanded := strings.NewReplacer("$(", ";", ")", ";", "`", ";").Replace(splitGroups(cmdline))
	for _, seg := range segmentSplitRE.Split(expanded, -1) {
		fields := shellWords(seg)
		if len(fields) == 0 {
			continue
		}
		if matched := matchPublishTokens(fields, depth); matched != "" {
			return matched
		}
	}
	return ""
}

// splitGroups turns the (, { and } that stand outside quotes into `;`.
func splitGroups(s string) string {
	b := []byte(s)
	var quote byte
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else if quote == '"' && c == '\\' {
				i++
			}
		case c == '\\':
			i++
		case c == '\'' || c == '"':
			quote = c
		case c == '(' || c == '{' || c == '}':
			b[i] = ';'
		}
	}
	return string(b)
}

var assignmentRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// wrapperValueFlags are, per command wrapper, the flags that take the next word as their value.
var wrapperValueFlags = map[string]map[string]bool{
	"command": {}, "builtin": {}, "nohup": {}, "time": {}, "stdbuf": {}, "setsid": {},
	"exec":    {"-a": true},
	"sudo":    {"-u": true, "-g": true, "-C": true, "-D": true, "-h": true, "-p": true, "-r": true, "-t": true, "-U": true},
	"doas":    {"-u": true, "-C": true},
	"nice":    {"-n": true},
	"ionice":  {"-c": true, "-n": true, "-p": true},
	"timeout": {"-s": true, "-k": true, "--signal": true, "--kill-after": true},
	"xargs": {"-I": true, "-n": true, "-L": true, "-P": true, "-s": true, "-d": true, "-E": true, "-a": true,
		"--max-args": true, "--max-procs": true, "--delimiter": true, "--arg-file": true},
	"env": {"-u": true, "-C": true, "--unset": true, "--chdir": true},
}

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "mksh": true, "ash": true, "busybox": true}

func matchPublishTokens(tokens []string, depth int) string {
	for len(tokens) > 0 {
		if assignmentRE.MatchString(tokens[0]) {
			tokens = tokens[1:]
			continue
		}
		name := filepath.Base(tokens[0])
		if name == "eval" {
			return matchPublishDepth(strings.Join(tokens[1:], " "), depth+1)
		}
		if shells[name] {
			args := tokens[1:]
			if name == "busybox" && len(args) > 0 && shells[args[0]] {
				args = args[1:]
			}
			for i := 0; i < len(args); i++ {
				t := args[i]
				switch {
				case t == "-o" || t == "+o" || t == "-O" || t == "+O" || t == "--rcfile" || t == "--init-file":
					i++
				case strings.HasPrefix(t, "-") && !strings.HasPrefix(t, "--") && strings.ContainsRune(t, 'c'):
					if i+1 < len(args) {
						return matchPublishDepth(args[i+1], depth+1)
					}
					return ""
				case strings.HasPrefix(t, "-") || strings.HasPrefix(t, "+"):
				default:
					return "" // a script file: not looked into
				}
			}
			return ""
		}
		values, wrapper := wrapperValueFlags[name]
		if !wrapper {
			break
		}
		tokens = tokens[1:]
		for len(tokens) > 0 {
			t := tokens[0]
			if name == "env" && (t == "-S" || t == "--split-string") && len(tokens) > 1 {
				return matchPublishDepth(strings.Join(tokens[1:], " "), depth+1)
			}
			if name == "env" && assignmentRE.MatchString(t) {
				tokens = tokens[1:]
				continue
			}
			if t == "--" {
				tokens = tokens[1:]
				break
			}
			if !strings.HasPrefix(t, "-") || t == "-" && name != "env" {
				break
			}
			tokens = tokens[1:]
			if values[t] && len(tokens) > 0 {
				tokens = tokens[1:]
			}
		}
		if name == "timeout" && len(tokens) > 0 {
			tokens = tokens[1:] // its duration
		}
	}
	if len(tokens) < 2 {
		return ""
	}
	cmd := filepath.Base(tokens[0])
	args := tokens[1:]
	switch cmd {
	case "git":
		// Skip global git flags such as -C <dir>, -c <k=v>, --git-dir <d>, --git-dir=<d>, --no-pager.
		for i := 0; i < len(args); i++ {
			t := args[i]
			if !strings.HasPrefix(t, "-") {
				if t == "push" {
					return "git push"
				}
				return ""
			}
			if t == "-C" || t == "-c" || t == "--git-dir" || t == "--work-tree" || t == "--namespace" ||
				t == "--exec-path" || t == "--super-prefix" || t == "--config-env" {
				i++
			}
		}
	case "gh":
		return matchGh(args)
	}
	return ""
}

// ghValueFlags are gh flags that take the next word as their value, wherever they stand: cobra reads a
// subcommand's flags before its name too (`gh pr -R o/r merge 5`).
var ghValueFlags = map[string]bool{
	"-R": true, "--repo": true, "-b": true, "--body": true, "-F": true, "--body-file": true, "-t": true, "--title": true,
	"-B": true, "--base": true, "-H": true, "--head": true, "-l": true, "--label": true, "-a": true, "--assignee": true,
	"-m": true, "--milestone": true, "-r": true, "--reviewer": true, "-p": true, "--project": true, "-S": true,
	"--search": true, "-s": true, "--state": true, "-L": true, "--limit": true, "-A": true, "--author": true,
	"-q": true, "--jq": true, "-T": true, "--template": true, "--json": true, "-X": true, "--method": true,
	"-f": true, "--field": true, "--raw-field": true, "--input": true, "--hostname": true,
	"--header": true, "--cache": true, "--preview": true,
}

func matchGh(args []string) string {
	var pos []string
	method, fields, mutation := "", false, false
	for i := 0; i < len(args); i++ {
		t := args[i]
		if !strings.HasPrefix(t, "-") || t == "-" {
			pos = append(pos, t)
			continue
		}
		var flag, value string
		if len(t) > 2 && !strings.HasPrefix(t, "--") && ghValueFlags[t[:2]] {
			flag, value = t[:2], t[2:] // an attached short flag: -XPOST, -fbody=x, -Fevent=APPROVE
		} else {
			var inline bool
			flag, value, inline = strings.Cut(t, "=")
			if !inline && ghValueFlags[flag] && i+1 < len(args) {
				i++
				value = args[i]
			}
		}
		switch flag {
		case "-X", "--method":
			method = strings.ToUpper(value)
		case "-f", "-F", "--field", "--raw-field", "--input":
			fields = true
		}
		if strings.Contains(value, "mutation") {
			mutation = true
		}
	}
	if len(pos) == 0 {
		return ""
	}
	if pos[0] == "api" {
		graphql := len(pos) > 1 && pos[1] == "graphql"
		switch {
		case method != "" && method != "GET" && method != "HEAD":
			return "gh api " + method
		case graphql && mutation:
			return "gh api graphql mutation"
		case !graphql && method == "" && fields:
			return "gh api POST" // fields make gh api a POST
		}
		return ""
	}
	reads, known := ghReads[pos[0]]
	switch {
	case !known:
		return "gh " + pos[0]
	case len(pos) == 1 || reads["*"] || reads[pos[1]]:
		return "" // a read, or the group's own help
	}
	return "gh " + pos[0] + " " + pos[1]
}

func shellWords(s string) []string {
	var words []string
	var cur strings.Builder
	var quote byte
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			} else if quote == '"' && c == '\\' && i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
			} else {
				cur.WriteByte(c)
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			inWord = true
		case '\\':
			if i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
				inWord = true
			}
		case ' ', '\t', '\r':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
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
