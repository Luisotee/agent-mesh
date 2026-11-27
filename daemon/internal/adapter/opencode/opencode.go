// Package opencode is the built-in adapter for OpenCode (`opencode`): launched in a PTY with per-launch
// `OPENCODE_CONFIG_CONTENT` (one deterministic agent name per model+effort so `-s` resume honours `-m`),
// `OPENCODE_PERMISSION` deny rules for read-only and planning agents, `opencode models --verbose` listing and
// `free` chain expansion, read-only SQLite inspection of `opencode.db`, and limit/unavailability detection.
package opencode

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
	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1/adapterv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
	"github.com/Luisotee/agent-mesh/daemon/internal/inputbox"
	"github.com/Luisotee/agent-mesh/daemon/internal/probe"
)

// Kind is the adapter's kind.
const Kind = "opencode"

// FreeFamily is OpenCode's built-in family of zero-cost tool-calling `opencode/*` models, newest first.
const FreeFamily = "free"

// VerifiedVersions are the OpenCode versions this adapter was verified against.
var VerifiedVersions = []string{"1.19.6"}

const (
	modelsCacheTTL = 6 * time.Hour
	namedEffort    = "high"
	limitLines     = 20

	// DefaultLimitHold is how long an OpenCode model is held when a usage/availability limit names no reset.
	DefaultLimitHold = 5 * time.Hour
	// DefaultRateHold is how long an OpenCode model is held after a 429 rate limit.
	DefaultRateHold = 10 * time.Minute
)

// publishCommands matches the Claude adapter's publish deny list (`git push` and `gh` write subcommands).
var publishCommands = []string{
	"git push", "gh pr review", "gh pr comment", "gh pr merge", "gh pr edit", "gh pr close",
	"gh pr create", "gh pr ready", "gh pr reopen", "gh issue comment", "gh issue create", "gh issue close",
	"gh issue edit", "gh release create", "gh repo delete",
}

// Adapter is the OpenCode CLI (`opencode`) adapter.
type Adapter struct {
	adapterv1connect.UnimplementedCliAdapterServiceHandler
	// ProbeDir is where the probe's stub openers are made (internal/probe); empty means the temp dir.
	ProbeDir string
	// ListModels overrides `opencode models --verbose` in tests.
	ListModels func(ctx context.Context, binary string, env map[string]string) (string, error)
	// UserHome returns the home directory used when env sets neither HOME nor XDG_*_HOME; nil defaults to
	// os.UserHomeDir. Tests set it (or pass HOME in env) to stay off the host's real home directory.
	UserHome func() (string, error)
}

func (a *Adapter) userHome() string {
	if a != nil && a.UserHome != nil {
		home, _ := a.UserHome()
		return home
	}
	home, _ := os.UserHomeDir()
	return home
}

// New returns the OpenCode adapter.
func New() *Adapter { return &Adapter{} }

var _ adapter.Adapter = (*Adapter)(nil)

// Describe reports what the OpenCode adapter supports. OpenCode fires no hooks, so it omits "hooks" and
// "startup-hook" (the daemon marks it idle once initial terminal output settles) and supports "session-lookup".
func (a *Adapter) Describe(context.Context, *connect.Request[adapterv1.DescribeRequest]) (*connect.Response[adapterv1.DescribeResponse], error) {
	return connect.NewResponse(&adapterv1.DescribeResponse{
		Kind:             Kind,
		DisplayName:      "OpenCode",
		VerifiedVersions: VerifiedVersions,
		Capabilities:     []string{"session-lookup", capTranscript},
	}), nil
}

// DecodeHook returns empty: OpenCode has no hooks.
func (a *Adapter) DecodeHook(context.Context, *connect.Request[adapterv1.DecodeHookRequest]) (*connect.Response[adapterv1.DecodeHookResponse], error) {
	return connect.NewResponse(&adapterv1.DecodeHookResponse{}), nil
}

// DescribeModel returns empty: OpenCode model IDs do not encode effort.
func (a *Adapter) DescribeModel(context.Context, *connect.Request[adapterv1.DescribeModelRequest]) (*connect.Response[adapterv1.DescribeModelResponse], error) {
	return connect.NewResponse(&adapterv1.DescribeModelResponse{}), nil
}

// ── models ──

// ModelCost is a model's token pricing from `opencode models --verbose`.
type ModelCost struct {
	Input  *float64 `json:"input"`
	Output *float64 `json:"output"`
}

// ModelCaps is a model's capabilities from `opencode models --verbose`.
type ModelCaps struct {
	ToolCall *bool `json:"toolcall"`
}

// ModelInfo is one entry parsed from `opencode models --verbose`.
type ModelInfo struct {
	ID           string         `json:"id"`
	ProviderID   string         `json:"providerID"`
	Status       string         `json:"status"`
	ReleaseDate  string         `json:"release_date"`
	Cost         ModelCost      `json:"cost"`
	Capabilities ModelCaps      `json:"capabilities"`
	Variants     map[string]any `json:"variants"`
}

var modelHeaderRE = regexp.MustCompile(`(?m)^([A-Za-z0-9_.-]+/[A-Za-z0-9_.:@/-]+)[ \t]*\r?$`)

// ParseModels parses `opencode models --verbose` output: each `provider/model` line followed by its JSON object.
func ParseModels(text string) map[string]ModelInfo {
	models := map[string]ModelInfo{}
	pos := 0
	for _, idx := range modelHeaderRE.FindAllStringSubmatchIndex(text, -1) {
		if idx[0] < pos {
			continue
		}
		name := text[idx[2]:idx[3]]
		tail := text[idx[1]:]
		trimmed := strings.TrimLeft(tail, " \t\r\n")
		ws := len(tail) - len(trimmed)
		dec := json.NewDecoder(strings.NewReader(trimmed))
		var info ModelInfo
		if err := dec.Decode(&info); err != nil {
			models[name] = ModelInfo{}
			continue
		}
		pos = idx[1] + ws + int(dec.InputOffset())
		models[name] = info
	}
	return models
}

// FreeModels returns OpenCode's own active, zero-cost, tool-calling `opencode/*` models, newest first.
func FreeModels(listing map[string]ModelInfo) []string {
	type cand struct {
		date  string
		model string
	}
	var found []cand
	for model, info := range listing {
		if !strings.HasPrefix(model, "opencode/") {
			continue
		}
		if info.Cost.Input == nil || *info.Cost.Input != 0 || info.Cost.Output == nil || *info.Cost.Output != 0 {
			continue
		}
		if info.Status != "" && info.Status != "active" {
			continue
		}
		if info.Capabilities.ToolCall != nil && !*info.Capabilities.ToolCall {
			continue
		}
		found = append(found, cand{date: info.ReleaseDate, model: model})
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].date != found[j].date {
			return found[i].date > found[j].date
		}
		return found[i].model > found[j].model
	})
	out := make([]string, len(found))
	for i, c := range found {
		out[i] = c.model
	}
	return out
}

// Cost classes of a model in OpenCode's own listing (`opencode models --verbose`, the cost fields).
const (
	// CostPaid: both token prices are listed and one is above zero.
	CostPaid = "paid"
	// CostZero: both token prices are listed and are zero, whatever the provider, status or tool support.
	CostZero = "zero"
)

// CostClasses classes every model of the listing by the cost OpenCode itself reports: CostZero or CostPaid. A model
// with a missing or unreadable cost (or one that is not listed) is left out: its cost is unknown.
func CostClasses(listing map[string]ModelInfo) map[string]string {
	out := make(map[string]string, len(listing))
	for model, info := range listing {
		in, o := info.Cost.Input, info.Cost.Output
		switch {
		case in == nil || o == nil:
		case *in == 0 && *o == 0:
			out[model] = CostZero
		default:
			out[model] = CostPaid
		}
	}
	return out
}

// Listing returns OpenCode's model listing (the cached one while it is fresh).
func (a *Adapter) Listing(ctx context.Context, bin string, env map[string]string) (map[string]ModelInfo, error) {
	return a.listModels(ctx, bin, env)
}

// ExpandChain expands any `free` family entry in `chain` to OpenCode's free models (newest first), deduplicating.
func (a *Adapter) ExpandChain(ctx context.Context, bin string, env map[string]string, chain []string) ([]string, error) {
	needListing := false
	for _, item := range chain {
		if item == FreeFamily {
			needListing = true
			break
		}
	}
	var free []string
	var listErr error
	if needListing {
		if listing, err := a.listModels(ctx, bin, env); err == nil {
			free = FreeModels(listing)
		} else {
			listErr = err
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, item := range chain {
		var cands []string
		if item == FreeFamily {
			cands = free
		} else {
			cands = []string{item}
		}
		for _, m := range cands {
			if m == "" || seen[m] {
				continue
			}
			seen[m] = true
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		if listErr != nil {
			return nil, fmt.Errorf("listing opencode models: %w", listErr)
		}
		return nil, errors.New("OpenCode lists no free model (`opencode models opencode`)")
	}
	return out, nil
}

// ListedFreeModels returns OpenCode's active zero-cost tool-calling `opencode/*` models, newest first (from the
// cached listing unless fresh is true). Returns nil, nil when !fresh and no cached listing exists yet.
func (a *Adapter) ListedFreeModels(ctx context.Context, bin string, env map[string]string, fresh bool) ([]string, error) {
	if bin == "" {
		bin = "opencode"
	}
	if !fresh {
		cacheFile := a.modelsCachePath(bin, env)
		if cacheFile == "" {
			return nil, nil
		}
		b, err := os.ReadFile(cacheFile)
		if err != nil {
			return nil, nil
		}
		var cached map[string]ModelInfo
		if json.Unmarshal(b, &cached) != nil || len(cached) == 0 {
			return nil, nil
		}
		return FreeModels(cached), nil
	}
	models, err := a.listModelsDirect(ctx, bin, env, true)
	if err != nil {
		return nil, err
	}
	return FreeModels(models), nil
}

func (a *Adapter) listModels(ctx context.Context, bin string, env map[string]string) (map[string]ModelInfo, error) {
	return a.listModelsDirect(ctx, bin, env, false)
}

func (a *Adapter) listModelsDirect(ctx context.Context, bin string, env map[string]string, force bool) (map[string]ModelInfo, error) {
	if bin == "" {
		bin = "opencode"
	}
	if a.ListModels != nil {
		raw, err := a.ListModels(ctx, bin, env)
		if err != nil {
			return nil, err
		}
		return ParseModels(raw), nil
	}
	cacheFile := a.modelsCachePath(bin, env)
	if !force && cacheFile != "" {
		if st, err := os.Stat(cacheFile); err == nil && time.Since(st.ModTime()) < modelsCacheTTL {
			if b, err := os.ReadFile(cacheFile); err == nil {
				var cached map[string]ModelInfo
				if json.Unmarshal(b, &cached) == nil && len(cached) > 0 {
					return cached, nil
				}
			}
		}
	}
	// A probe: it can't open a login (internal/probe).
	out, err := probe.Run(ctx, probe.Spec{Bin: bin, Args: []string{"models", "--verbose"}, Env: mapToEnv(env), Dir: a.ProbeDir, Timeout: 60 * time.Second})
	if err == nil {
		models := ParseModels(string(out))
		if len(models) > 0 {
			if cacheFile != "" {
				if b, merr := json.Marshal(models); merr == nil {
					_ = os.MkdirAll(filepath.Dir(cacheFile), 0o700)
					_ = fsutil.WriteAtomic(cacheFile, b, 0o600)
				}
			}
			return models, nil
		}
	}
	if !force && cacheFile != "" {
		if b, rerr := os.ReadFile(cacheFile); rerr == nil {
			var cached map[string]ModelInfo
			if json.Unmarshal(b, &cached) == nil && len(cached) > 0 {
				return cached, nil
			}
		}
	}
	if err != nil {
		return nil, err
	}
	return nil, errors.New("`opencode models --verbose` returned no models")
}

func (a *Adapter) modelsCachePath(bin string, env map[string]string) string {
	base := env["XDG_CACHE_HOME"]
	if base == "" {
		home := env["HOME"]
		if home == "" {
			home = a.userHome()
		}
		if home != "" {
			base = filepath.Join(home, ".cache")
		}
	}
	if base == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(bin))
	return filepath.Join(base, "agent-mesh", "opencode-models-"+hex.EncodeToString(sum[:6])+".json")
}

func mapToEnv(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

// ── launch config ──

// AgentName returns the deterministic OpenCode agent name for (model, effort), matching `bin/mesh`'s
// `opencode_agent` so reopening `-s <session>` with another model or effort uses a distinct agent and `-m` holds.
func AgentName(model, effort string) string {
	u := uuid.NewSHA1(uuid.NameSpaceURL, []byte(model+"|"+effort))
	return "mesh-" + hex.EncodeToString(u[:])[:6]
}

type orderedKV struct {
	key string
	val string
}

// marshalOrderedStringMap encodes key-value pairs in exact slice order, because OpenCode evaluates `edit`
// permission patterns last-match-wins and a standard Go map marshals keys alphabetically.
func marshalOrderedStringMap(pairs []orderedKV) ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := json.Marshal(p.key)
		if err != nil {
			return nil, err
		}
		vb, err := json.Marshal(p.val)
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// toolchainDirs are what an agent that runs a repo's tests reads outside its worktree: the Go and build tools the
// daemon's own install uses (`daemon/CLAUDE.md`, "Toolchain"), the Go caches, and the scratch dirs tests write to.
// Relative ones are under the agent's HOME.
var toolchainDirs = []string{".local/go", "go", ".cache/go-build", ".local/opt", "/usr/local/go", "/usr/lib/go", "/tmp", "/var/tmp"}

// externalPermission is the config's `permission` for what is outside an agent's worktree. OpenCode's dialog for
// "Access external directory" (the default is ask) is never answered in a mesh: the agent hangs there until its
// ask times out. So everything outside is denied, which gives the agent a refusal it carries on from, except what
// it needs: the installed share folder (the protocol and skills its first doorbell names; read only, its edits are
// denied), the run's folder (`$AGENT_MESH_RUN_DIR`: plans and reports) and the toolchain dirs above (edits denied
// there too, except in the scratch dirs: an agent that may edit is not let overwrite a tool). Not the repo's main
// checkout: its untracked files are the user's. OpenCode evaluates the patterns last match wins, so `*` comes
// first and the allows after it, in the order written here. Always set: the deny holds when the daemon has no
// share path.
func (a *Adapter) externalPermission(env map[string]string, cwd string) (map[string]any, error) {
	share := ""
	if s := env["AGENT_MESH_SHARE"]; filepath.IsAbs(s) {
		share = filepath.ToSlash(filepath.Clean(s))
	}
	home := env["HOME"]
	if home == "" {
		home = a.userHome()
	}
	var dirs, tools []string // tools: the dirs whose files are not the agent's to edit
	add := func(d string) bool {
		d = filepath.ToSlash(filepath.Clean(d))
		if filepath.IsAbs(d) {
			dirs = append(dirs, d)
		}
		return filepath.IsAbs(d)
	}
	if share != "" {
		add(share)
	}
	if rd := env["AGENT_MESH_RUN_DIR"]; rd != "" {
		add(rd)
	}
	// A TMPDIR other than /tmp is where the agent's tools write too.
	if t := env["TMPDIR"]; filepath.IsAbs(t) && filepath.Clean(t) != "/" && filepath.Clean(t) != filepath.Clean(home) {
		add(t)
	}
	for _, d := range toolchainDirs {
		if !filepath.IsAbs(d) {
			if home == "" {
				continue
			}
			d = filepath.Join(home, d)
		}
		if add(d) && d != "/tmp" && d != "/var/tmp" {
			tools = append(tools, filepath.ToSlash(filepath.Clean(d)))
		}
	}
	pairs := []orderedKV{{key: "*", val: "deny"}}
	for _, d := range dirs {
		// The bare directory too: a pattern `d/*` does not match `d` itself.
		pairs = append(pairs, orderedKV{key: d, val: "allow"}, orderedKV{key: d + "/*", val: "allow"}, orderedKV{key: d + "/**", val: "allow"})
	}
	raw, err := marshalOrderedStringMap(pairs)
	if err != nil {
		return nil, err
	}
	perm := map[string]any{"external_directory": json.RawMessage(raw)}
	noEdit := map[string]string{}
	if share != "" {
		tools = append(tools, share)
	}
	for _, d := range tools {
		noEdit[d+"/*"], noEdit[d+"/**"] = "deny", "deny"
		// OpenCode matches an edit's pattern as the path relative to the worktree (`../../x` for a file outside
		// it), never absolute: the absolute rule above is for a tool that is given an absolute path.
		if rel, ok := relFrom(cwd, d); ok {
			noEdit[rel+"/*"], noEdit[rel+"/**"] = "deny", "deny"
		}
	}
	if len(noEdit) > 0 {
		perm["edit"] = noEdit
	}
	return perm, nil
}

// relFrom is d as OpenCode writes a path in an edit's permission pattern: relative to the worktree (cwd).
func relFrom(cwd, d string) (string, bool) {
	if cwd == "" || d == "" {
		return "", false
	}
	rel, err := filepath.Rel(resolvePathIncludingMissing(cwd), resolvePathIncludingMissing(d))
	if err != nil {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// readOnlyBash is what a read-only or planning agent may run in bash, as OpenCode patterns (last match wins, so
// `*` denies first, the allows follow and the denies of the dangerous forms of those come last). OpenCode 1.19.6
// splits a command line into its simple commands (`&&`, `;`, `|`, `$( )`, backticks, subshells, `<( )`) and asks
// for each one alone, with the command's own text (the whole text, redirect included, when it is redirected): a
// command that matches no allow is denied, so `ls; rm x` is refused at `rm x`. A trailing ` *` also matches the
// command with no arguments. What it can't see: a bare redirect (`> file`, no command) is no command, so it
// is not asked for at all: `share/opencode/readonly-guard.js`, a plugin the launch loads (PlanLaunch), refuses
// a bash command with a `>` before it runs.
//
// Not listed on purpose: `sed` (`w` and `e` write and run inside the script), `xargs`, `env`, `sh`, `rm`, `mv`,
// `cp`, `tee`, `git -C`, `git branch` that is not a `--list`, `git fetch`, and a command with a leading
// `VAR=value` (its text starts with the variable, which no allow matches).
var readOnlyBash = func() []orderedKV {
	var p []orderedKV
	add := func(act string, pats ...string) {
		for _, x := range pats {
			p = append(p, orderedKV{key: x, val: act})
		}
	}
	add("deny", "*")
	var git []string
	for _, sub := range []string{"status", "diff", "show", "log", "rev-parse", "ls-files", "ls-tree", "cat-file", "merge-base",
		"blame", "grep", "show-ref", "shortlog", "describe", "branch --list", "branch --show-current", "branch -a",
		"branch -r", "branch -vv", "worktree list"} {
		git = append(git, "git "+sub+" *", "git --no-pager "+sub+" *")
	}
	add("allow", git...)
	add("allow", "gh pr view *", "gh pr diff *", "gh pr checks *", "gh pr list *")
	add("allow", "ls *", "cat *", "head *", "tail *", "wc *", "grep *", "rg *", "find *", "jq *", "awk *", "cut *", "tr *",
		"sort *", "diff *", "echo *", "printf *", "pwd", "stat *", "file *", "which *", "basename *", "dirname *",
		"realpath *", "test *", "[ *")
	add("allow", "go test *", "go vet *", "go build -o /tmp/*", "go env *", "gofmt -l *", "gofmt -d *", "buf lint *",
		"buf breaking *", "pnpm typecheck *", "pnpm test *", "pnpm run typecheck *", "pnpm run test *", "uvx pytest *",
		"mesh *")
	// The dangerous forms of what is allowed. A `>` is a redirect (or an awk print to a file): `2>&1` goes too.
	add("deny", "*>*", "*-exec*", "*toolexec*", "*--output*",
		"find *-delete*", "find *-ok*", "find *-fprint*", "find *-fls*",
		"awk *system*", "awk *getline*", "awk *|*", "awk *-i *", "awk *-l *",
		"sort *-o*", "sort *--compress-program*",
		"rg *--pre=*", "rg *--pre *", "rg *--hostname-bin*",
		"git grep*-O*", "git grep*--open-files-in-pager*",
		"git branch*-d*", "git branch*-D*", "git branch*-m*", "git branch*-M*", "git branch*-c*", "git branch*-C*",
		"git branch*-f*", "git branch*-u*", "git branch*-t*", "git branch*--edit*",
		"go test*-o *", "go test*-o=*", "go test*-c", "go test*-c *", "go test*profile*", "go test*-trace*",
		"go test*-outputdir*", "go test*-fuzz*", "go vet*-vettool*", "go env*-w*", "go env*-u*", "go build*/../*",
		"gofmt*-w*")
	return p
}()

// PermissionJSON builds `OPENCODE_PERMISSION` for a read-only or planning agent. Edits: denied everywhere (`*`)
// but the run folder's `reports/` (and `plans/` for a planner), which OpenCode matches as paths relative to the
// worktree; and the main checkout, denied after. Bash: only the read-only commands of readOnlyBash, then the
// publish commands denied.
func PermissionJSON(cwd, repo, runDir string, planner bool) (string, error) {
	editPairs := []orderedKV{{key: "*", val: "deny"}}
	if rel, ok := relFrom(cwd, runDir); ok && filepath.IsAbs(runDir) {
		editPairs = append(editPairs, orderedKV{key: rel + "/reports/*", val: "allow"})
		if planner {
			editPairs = append(editPairs, orderedKV{key: rel + "/plans/*", val: "allow"})
		}
	}
	if rel, ok := relFrom(cwd, repo); ok && rel != "." {
		editPairs = append(editPairs, orderedKV{key: rel + "/*", val: "deny"})
	}
	editRaw, err := marshalOrderedStringMap(editPairs)
	if err != nil {
		return "", err
	}
	bashPairs := append([]orderedKV(nil), readOnlyBash...)
	for _, cmd := range publishCommands {
		bashPairs = append(bashPairs, orderedKV{key: cmd + "*", val: "deny"})
	}
	bashRaw, err := marshalOrderedStringMap(bashPairs)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(struct {
		Edit json.RawMessage `json:"edit"`
		Bash json.RawMessage `json:"bash"`
	}{editRaw, bashRaw})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// PlanLaunch builds the OpenCode launch command and per-launch environment (`OPENCODE_CONFIG_CONTENT`,
// `OPENCODE_DISABLE_AUTOUPDATE`, and `OPENCODE_PERMISSION` when read-only or planning).
func (a *Adapter) PlanLaunch(ctx context.Context, req *connect.Request[adapterv1.PlanLaunchRequest]) (*connect.Response[adapterv1.PlanLaunchResponse], error) {
	m := req.Msg
	bin := m.Binary
	if bin == "" {
		bin = "opencode"
	}
	env := map[string]string{}
	for k, v := range m.Env {
		env[k] = v
	}
	model := m.Model
	if model == FreeFamily {
		expanded, err := a.ExpandChain(ctx, bin, env, []string{FreeFamily})
		if err != nil {
			return nil, err
		}
		model = expanded[0]
	}
	effort := m.Effort
	if effort == "" {
		effort = namedEffort
	}
	agentName := AgentName(model, effort)
	agentCfg := map[string]any{
		"mode": "primary",
	}
	if model != "" {
		agentCfg["model"] = model
		if effort != "" {
			if listing, err := a.listModels(ctx, bin, env); err == nil {
				if info, ok := listing[model]; ok {
					if _, has := info.Variants[effort]; has {
						agentCfg["variant"] = effort
					}
				}
			}
		}
	}
	cfg := map[string]any{"agent": map[string]any{agentName: agentCfg}}
	perm, err := a.externalPermission(env, m.Cwd)
	if err != nil {
		return nil, err
	}
	cfg["permission"] = perm
	// The guards are files of the installed share; a meshd with no share has no protocol for its agents either, so it is
	// no real launch. Every agent's bash refuses a kill by pattern (kill-guard.js); a read-only or planning agent's also
	// closes what its bash patterns can't see, a bare redirect (readonly-guard.js, `readOnlyBash`).
	if share := env["AGENT_MESH_SHARE"]; !m.Standalone && filepath.IsAbs(share) {
		guard := func(name string) string {
			return (&url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(share, "opencode", name))}).String()
		}
		plugins := []string{guard("kill-guard.js")}
		if adapter.ReadOnly(m.Role) || m.DenyEdits {
			plugins = append(plugins, guard("readonly-guard.js"))
		}
		cfg["plugin"] = plugins
	}
	cfgBytes, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	env["OPENCODE_CONFIG_CONTENT"] = string(cfgBytes)
	env["OPENCODE_DISABLE_AUTOUPDATE"] = "1"
	if m.Standalone {
		if m.ResumeSessionId == "" {
			return nil, errors.New("standalone launch requires resume_session_id")
		}
		argv := []string{bin, "--agent", agentName}
		if model != "" {
			argv = append(argv, "-m", model)
		}
		argv = append(argv, "-s", m.ResumeSessionId)
		return connect.NewResponse(&adapterv1.PlanLaunchResponse{Argv: argv, Env: env}), nil
	}
	if adapter.ReadOnly(m.Role) {
		// An asker or a reviewer can sit on a PR head nobody trusted: its `opencode.json` and `.opencode/` can name
		// a plugin, an MCP server, a formatter or a command that runs code, and set permissions. 1.19.6 drops all
		// of the project's config under this variable (`opencode debug config`: plugin, mcp, command, formatter and
		// permission come back empty); the mesh's own config comes by OPENCODE_CONFIG_CONTENT, which stays.
		env["OPENCODE_DISABLE_PROJECT_CONFIG"] = "1"
	}
	if adapter.ReadOnly(m.Role) || m.DenyEdits {
		perm, err := PermissionJSON(m.Cwd, m.Repo, env["AGENT_MESH_RUN_DIR"], !adapter.ReadOnly(m.Role))
		if err != nil {
			return nil, err
		}
		env["OPENCODE_PERMISSION"] = perm
	}

	argv := []string{bin, "--agent", agentName}
	if model != "" {
		argv = append(argv, "-m", model)
	}
	if m.ResumeSessionId != "" {
		argv = append(argv, "-s", m.ResumeSessionId)
	}
	if m.SkipPermissions {
		argv = append(argv, "--auto")
	}
	return connect.NewResponse(&adapterv1.PlanLaunchResponse{
		Argv: argv,
		Env:  env,
	}), nil
}

// ── session lookup and db inspection ──

// DBPath returns the OpenCode SQLite database path from `env` (or XDG_DATA_HOME / HOME).
func DBPath(env map[string]string) string {
	return (*Adapter)(nil).DBPath(env)
}

// DBPath returns the OpenCode SQLite database path from `env` (or XDG_DATA_HOME / HOME / UserHome).
func (a *Adapter) DBPath(env map[string]string) string {
	if p := env["AGENT_MESH_OPENCODE_DB"]; p != "" {
		return p
	}
	if p := env["MESH_OPENCODE_DB"]; p != "" {
		return p
	}
	base := env["XDG_DATA_HOME"]
	if base == "" {
		home := env["HOME"]
		if home == "" {
			home = a.userHome()
		}
		if home != "" {
			base = filepath.Join(home, ".local", "share")
		}
	}
	if base == "" {
		return ""
	}
	return filepath.Join(base, "opencode", "opencode.db")
}

func openDB(dbPath string) (*sql.DB, error) {
	if dbPath == "" {
		return nil, os.ErrNotExist
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil, err
	}
	uriPath := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(filepath.Clean(dbPath))
	return sql.Open("sqlite", "file:"+uriPath+"?mode=ro&_pragma=busy_timeout(2000)")
}

// LookupSession finds an OpenCode agent's top-level session in its worktree (never in a shared main checkout)
// that is not older than `not_before`.
func (a *Adapter) LookupSession(_ context.Context, req *connect.Request[adapterv1.LookupSessionRequest]) (*connect.Response[adapterv1.LookupSessionResponse], error) {
	m := req.Msg
	var notBefore time.Time
	if m.NotBefore != nil {
		notBefore = m.NotBefore.AsTime()
	}
	db, err := openDB(a.DBPath(m.Env))
	if err != nil {
		return connect.NewResponse(&adapterv1.LookupSessionResponse{}), nil
	}
	defer db.Close()
	sid := lookupSessionDB(db, m.Cwd, m.Repo, notBefore)
	return connect.NewResponse(&adapterv1.LookupSessionResponse{SessionId: sid}), nil
}

func lookupSessionDB(db *sql.DB, cwd, repo string, notBefore time.Time) string {
	if cwd == "" || (repo != "" && resolvePathIncludingMissing(cwd) == resolvePathIncludingMissing(repo)) {
		return ""
	}
	rows, err := db.Query(`SELECT id, directory, time_created FROM session WHERE parent_id IS NULL ORDER BY time_created DESC`)
	if err != nil {
		return ""
	}
	defer rows.Close()
	wantCwd := resolvePathIncludingMissing(cwd)
	minMs := int64(0)
	if !notBefore.IsZero() {
		minMs = notBefore.UnixMilli()
	}
	for rows.Next() {
		var id, dir string
		var created int64
		if err := rows.Scan(&id, &dir, &created); err != nil || id == "" {
			continue
		}
		if minMs > 0 && created < minMs {
			continue
		}
		if resolvePathIncludingMissing(dir) == wantCwd {
			return id
		}
	}
	return ""
}

// SessionInfo summarizes the latest state of an OpenCode session since a turn boundary.
type SessionInfo struct {
	SessionID string
	Status    string // "", "working", "idle", "limited", "error"
	MessageID string
	// LastRole is the newest message's role ("user" while the reply to it has no row yet).
	LastRole      string
	CreatedAt     time.Time
	CompletedAt   time.Time
	HasModelReply bool
	LimitMessage  string
	ResetIn       string
	IsRate        bool
	Evidence      string
	ErrorText     string
	// UpdatedAt is when the open reply (Status "working") last changed: the later of its own row and its newest
	// part's. A reply that stays open with this unchanged for minutes is stalled. Zero when the DB doesn't say.
	UpdatedAt time.Time
}

type opencodeMessage struct {
	Role       string `json:"role"`
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
	Finish     string `json:"finish"`
	Time       struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Error *struct {
		Name string `json:"name"`
		Data struct {
			Message      string `json:"message"`
			StatusCode   int    `json:"statusCode"`
			ResponseBody string `json:"responseBody"`
		} `json:"data"`
	} `json:"error"`
}

// InspectSession reads OpenCode's SQLite DB read-only and reports the agent's session id and turn status since
// `since`. Returns os.ErrNotExist or a sqlite error when the DB cannot be read (so callers can distinguish an
// unreadable DB from one that has no replies yet).
func InspectSession(dbPath, sessionID, cwd, repo, model string, since, notBefore time.Time) (SessionInfo, error) {
	db, err := openDB(dbPath)
	if err != nil {
		return SessionInfo{}, err
	}
	defer db.Close()

	if sessionID == "" {
		sessionID = lookupSessionDB(db, cwd, repo, notBefore)
	}
	out := SessionInfo{SessionID: sessionID}
	if sessionID == "" {
		return out, nil
	}
	sinceMs := int64(0)
	if !since.IsZero() {
		sinceMs = since.UnixMilli()
	}
	rows, err := db.Query(`SELECT id, data, time_created FROM message WHERE session_id = ? AND time_created >= ? ORDER BY time_created ASC, rowid ASC`,
		sessionID, sinceMs)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	var (
		lastID  string
		lastMsg opencodeMessage
		lastAt  int64
		hasLast bool
	)
	for rows.Next() {
		var id, raw string
		var createdMs int64
		if err := rows.Scan(&id, &raw, &createdMs); err != nil {
			continue
		}
		var m opencodeMessage
		if json.Unmarshal([]byte(raw), &m) != nil {
			continue
		}
		if id == "" {
			id = strconv.FormatInt(createdMs, 10)
		}
		if m.Role == "assistant" {
			msgModel := m.ProviderID + "/" + m.ModelID
			if model == "" || msgModel == model {
				out.HasModelReply = true
			}
		}
		lastID, lastMsg, lastAt, hasLast = id, m, createdMs, true
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if !hasLast {
		return out, nil
	}
	out.MessageID = lastID
	out.LastRole = lastMsg.Role
	created := lastMsg.Time.Created
	if created == 0 {
		created = lastAt
	}
	if created > 0 {
		out.CreatedAt = time.UnixMilli(created)
	}
	if lastMsg.Time.Completed > 0 {
		out.CompletedAt = time.UnixMilli(lastMsg.Time.Completed)
	}

	if lastMsg.Role == "user" || (lastMsg.Role == "assistant" && (lastMsg.Time.Completed == 0 || lastMsg.Finish == "tool-calls") && lastMsg.Error == nil) {
		out.Status = "working"
		out.UpdatedAt = replyUpdatedAt(db, lastID)
		return out, nil
	}
	if lastMsg.Role == "assistant" && lastMsg.Error != nil {
		// Only treat a reply error as belonging to the agent's current model (after a switch, an old reply may
		// still be on the previous model).
		msgModel := lastMsg.ProviderID + "/" + lastMsg.ModelID
		if model != "" && msgModel != "/" && msgModel != model {
			return out, nil
		}
		if lastMsg.Error.Name == "MessageAbortedError" {
			out.Status = "idle"
			return out, nil
		}
		if hit, ok := ClassifyReplyError(lastID, lastMsg.Error.Name, lastMsg.Error.Data.Message, lastMsg.Error.Data.ResponseBody, lastMsg.Error.Data.StatusCode); ok {
			out.Status = "limited"
			out.LimitMessage = hit.Message
			out.ResetIn = hit.ResetIn
			out.IsRate = hit.IsRate
			out.Evidence = hit.Evidence
			return out, nil
		}
		out.Status = "error"
		out.ErrorText = firstLine(lastMsg.Error.Data.Message)
		if out.ErrorText == "" {
			out.ErrorText = firstLine(lastMsg.Error.Name)
		}
		if out.ErrorText == "" {
			out.ErrorText = "error"
		}
		return out, nil
	}
	if lastMsg.Role == "assistant" {
		out.Status = "idle"
	}
	return out, nil
}

// replyUpdatedAt is when a message, or any of its parts, last changed, from the DB's time_updated columns; zero when
// they can't be read (an older schema), so no stall is ever read from a DB that doesn't say.
func replyUpdatedAt(db *sql.DB, messageID string) time.Time {
	var ms sql.NullInt64
	if err := db.QueryRow(`SELECT time_updated FROM message WHERE id = ?`, messageID).Scan(&ms); err != nil || !ms.Valid {
		return time.Time{}
	}
	latest := ms.Int64
	var pms sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(time_updated) FROM part WHERE message_id = ?`, messageID).Scan(&pms); err == nil && pms.Valid && pms.Int64 > latest {
		latest = pms.Int64
	}
	if latest <= 0 {
		return time.Time{} // a row that says 0 says nothing: never a stall of fifty years
	}
	return time.UnixMilli(latest)
}

// ── limits & screen parsing ──

// LimitHit describes a usage limit, rate limit, or withdrawn model detected from OpenCode's DB or terminal pane.
type LimitHit struct {
	Message  string
	ResetIn  string
	IsRate   bool
	Evidence string
}

// OpenCode renders status, retry and error banners inside a heavy vertical-bar callout (`  ┃  ...`, U+2503).
// Anchoring on that gutter keeps fixture, comment, diff and thin-box lines (`│ …`, `// …`, `+ …`) from matching.
//
// OpenCode 1.19.6 draws the free-usage and 429 retry lines in the footer under the input box instead: after a
// spinner of `■` and `⬝` cells (`   ■■⬝⬝⬝⬝⬝⬝ Free usage exceeded, subscribe to Go [retrying in 5h 27m attempt #1]`),
// so a line may start with either. Both are anchored at the line start.
const bannerStart = "(?im)^[ \\t]*(?:\u2503|[\u25a0\u2b1d]{2,})[ \\t]+"

var (
	// FreeUsage/GoUsage/Model unavailable/429 retry lines on the pane (no named RE2 capture needed beyond simple groups).
	paneFreeLimitRE   = regexp.MustCompile(bannerStart + `(?:Free usage exceeded[^\n]*|Free limit reached[^\n]*|(?:(?:[\w./-]+(?::[\w./-]+)?|\(\w[\w ]*\))\s+){0,4}usage limit reached\. It will reset in [^\n]*|Upstream request failed: Model is unavailable[^\n]*)`)
	paneRateRetryRE   = regexp.MustCompile(bannerStart + `(?:(?:\w+ ?)?Error:?\s+)?(?:429(?:\s+Too Many Requests)?\s*|(?:Too Many Requests|Rate limit (?:reached|exceeded))[^\n\[]{0,160}?)\[retrying[^\]]*attempt #(?:[3-9]|[1-9]\d+)\]`)
	opencodeResetRE   = regexp.MustCompile(`(?i)(?:reset in|Free (?:usage exceeded|limit reached)[^\n]*retrying in) ((?:\d+(?:\.\d+)?\s*[a-z]+\s*)+)`)
	durationPartRE    = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*(d|h|m|s)[a-z]*`)
	modelUnavailRE    = regexp.MustCompile(`(?i)model is unavailable`)
	squeezeSpaceRE    = regexp.MustCompile(`\s+`)
	cursorPosEscRE    = regexp.MustCompile(`\x1b\[\d+(?:;\d+)?[Hf]`)
	oscEscRE          = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)
	csiEscRE          = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	otherTwoByteEscRE = regexp.MustCompile(`\x1b[@-Z\\-_]`)
)

// ParseDuration parses durations like "4h 12m", "3 hours", "30s", "2 days". Returns 0 when nothing parses.
func ParseDuration(text string) time.Duration {
	var total float64
	for _, m := range durationPartRE.FindAllStringSubmatch(strings.ToLower(text), -1) {
		val, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		switch m[2] {
		case "d":
			total += val * 86400
		case "h":
			total += val * 3600
		case "m":
			total += val * 60
		case "s":
			total += val
		}
	}
	if total <= 0 {
		return 0
	}
	return time.Duration(total * float64(time.Second))
}

// ClassifyReplyError checks whether an OpenCode stored assistant error is a usage limit, 429 rate limit, or
// withdrawn model ("Upstream request failed: Model is unavailable").
func ClassifyReplyError(msgID, errName, errMsg, respBody string, statusCode int) (LimitHit, bool) {
	freeOrGo := strings.Contains(respBody, "FreeUsageLimitError") || strings.Contains(respBody, "GoUsageLimitError")
	unavailable := modelUnavailRE.MatchString(errMsg) || modelUnavailRE.MatchString(respBody)
	if !freeOrGo && !unavailable && statusCode != 429 {
		return LimitHit{}, false
	}
	text := firstLine(errMsg)
	if text == "" {
		text = firstLine(errName)
	}
	if text == "" {
		if statusCode == 429 {
			text = "rate limited (429)"
		} else {
			text = "limit reached"
		}
	}
	isRate := statusCode == 429 && !freeOrGo && !unavailable
	hold := DefaultLimitHold
	if isRate {
		hold = DefaultRateHold
	} else if rm := opencodeResetRE.FindStringSubmatch(text); rm != nil {
		if d := ParseDuration(rm[1]); d > 0 {
			hold = d
		}
	}
	return LimitHit{
		Message:  text,
		ResetIn:  hold.String(),
		IsRate:   isRate,
		Evidence: "reply:" + msgID,
	}, true
}

// MatchPaneLimit inspects the last `limitLines` non-blank lines of a terminal screen for an OpenCode limit or
// retry banner (newest first).
func MatchPaneLimit(lines []string) (LimitHit, bool) {
	start := 0
	if len(lines) > limitLines {
		start = len(lines) - limitLines
	}
	tail := lines[start:]
	for i := len(tail) - 1; i >= 0; i-- {
		line := tail[i]
		if m := paneFreeLimitRE.FindString(line); m != "" {
			hold := DefaultLimitHold
			if rm := opencodeResetRE.FindStringSubmatch(m); rm != nil {
				if d := ParseDuration(rm[1]); d > 0 {
					hold = d
				}
			}
			return LimitHit{
				Message:  strings.TrimSpace(m),
				ResetIn:  hold.String(),
				IsRate:   false,
				Evidence: "pane:" + squeezeWS(m),
			}, true
		}
		if m := paneRateRetryRE.FindString(line); m != "" {
			return LimitHit{
				Message:  strings.TrimSpace(m),
				ResetIn:  DefaultRateHold.String(),
				IsRate:   true,
				Evidence: "pane:" + squeezeWS(m),
			}, true
		}
	}
	return LimitHit{}, false
}

// ScreenLines converts raw ANSI/repaint terminal bytes into plain text lines, trimming trailing blank lines.
func ScreenLines(raw []byte) []string {
	s := string(raw)
	s = cursorPosEscRE.ReplaceAllString(s, "\n")
	s = oscEscRE.ReplaceAllString(s, "")
	s = csiEscRE.ReplaceAllString(s, "")
	s = otherTwoByteEscRE.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func squeezeWS(s string) string {
	return strings.TrimSpace(squeezeSpaceRE.ReplaceAllString(s, " "))
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

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

// ReadInputBox reads the input box from a repaint: OpenCode's `┃` gutter box, whose foot is the agent and model.
func (a *Adapter) ReadInputBox(_ context.Context, req *connect.Request[adapterv1.ReadInputBoxRequest]) (*connect.Response[adapterv1.ReadInputBoxResponse], error) {
	return connect.NewResponse(adapter.ReadInputBox(req.Msg, inputbox.OpenCode)), nil
}

// ── cost / message usage ──

// SessionCall is one assistant reply's token and cost usage from `opencode.db`.
type SessionCall struct {
	ID     string
	Time   time.Time
	Model  string
	Input  int64
	Write  int64 // cache writes
	Read   int64
	Output int64
	USD    float64
}

type opencodeUsageMessage struct {
	ID         string  `json:"id"`
	Role       string  `json:"role"`
	ProviderID string  `json:"providerID"`
	ModelID    string  `json:"modelID"`
	Cost       float64 `json:"cost"`
	Time       struct {
		Created int64 `json:"created"`
	} `json:"time"`
	Tokens *struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
}

// RecentSessions returns OpenCode session ids updated at or after `since` (all if zero).
func RecentSessions(dbPath string, since time.Time) []string {
	db, err := openDB(dbPath)
	if err != nil {
		return nil
	}
	defer db.Close()
	sinceMs := int64(0)
	if !since.IsZero() {
		sinceMs = since.UnixMilli()
	}
	rows, err := db.Query(`SELECT id FROM session WHERE time_updated >= ? ORDER BY time_updated, id`, sinceMs)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil && id != "" {
			out = append(out, id)
		}
	}
	return out
}

// SessionCalls reads assistant token and cost records for `session` from `opencode.db`, oldest first.
func SessionCalls(dbPath, session string) []SessionCall {
	if session == "" {
		return nil
	}
	db, err := openDB(dbPath)
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, data FROM message WHERE session_id = ? ORDER BY time_created ASC, rowid ASC`, session)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var calls []SessionCall
	for rows.Next() {
		var rowID, raw string
		if err := rows.Scan(&rowID, &raw); err != nil {
			continue
		}
		var m opencodeUsageMessage
		if json.Unmarshal([]byte(raw), &m) != nil || m.Role != "assistant" || m.Time.Created <= 0 || m.Tokens == nil {
			continue
		}
		id := m.ID
		if id == "" {
			id = rowID
		}
		model := m.ModelID
		if m.ProviderID != "" && m.ModelID != "" {
			model = m.ProviderID + "/" + m.ModelID
		}
		calls = append(calls, SessionCall{
			ID:     id,
			Time:   time.UnixMilli(m.Time.Created),
			Model:  model,
			Input:  m.Tokens.Input,
			Write:  m.Tokens.Cache.Write,
			Read:   m.Tokens.Cache.Read,
			Output: m.Tokens.Output + m.Tokens.Reasoning,
			USD:    m.Cost,
		})
	}
	return calls
}
