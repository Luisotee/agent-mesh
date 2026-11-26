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
	"os"
	"os/exec"
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
		Capabilities:     []string{"session-lookup"},
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
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, "models", "--verbose")
	if len(env) > 0 {
		cmd.Env = mapToEnv(env)
	}
	out, err := cmd.Output()
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

// PermissionJSON builds `OPENCODE_PERMISSION` for a read-only or planning agent: edits denied in the worktree
// (`*`) and main checkout (`<rel>/*`) with `../*` (`$(mesh dir)`) allowed, and publish commands denied in bash.
func PermissionJSON(cwd, repo string) (string, error) {
	editPairs := []orderedKV{
		{key: "*", val: "deny"},
		{key: "../*", val: "allow"},
	}
	if cwd != "" && repo != "" {
		resCwd := resolvePathIncludingMissing(cwd)
		resRepo := resolvePathIncludingMissing(repo)
		if resCwd != resRepo {
			if rel, err := filepath.Rel(resCwd, resRepo); err == nil {
				editPairs = append(editPairs, orderedKV{key: filepath.ToSlash(rel) + "/*", val: "deny"})
			}
		}
	}
	editRaw, err := marshalOrderedStringMap(editPairs)
	if err != nil {
		return "", err
	}
	bash := make(map[string]string, len(publishCommands))
	for _, cmd := range publishCommands {
		bash[cmd+"*"] = "deny"
	}
	doc := struct {
		Edit json.RawMessage   `json:"edit"`
		Bash map[string]string `json:"bash"`
	}{
		Edit: editRaw,
		Bash: bash,
	}
	b, err := json.Marshal(doc)
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
	cfgBytes, err := json.Marshal(map[string]any{
		"agent": map[string]any{agentName: agentCfg},
	})
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
	if adapter.ReadOnly(m.Role) || m.DenyEdits {
		perm, err := PermissionJSON(m.Cwd, m.Repo)
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
	SessionID     string
	Status        string // "", "working", "idle", "limited", "error"
	MessageID     string
	CreatedAt     time.Time
	CompletedAt   time.Time
	HasModelReply bool
	LimitMessage  string
	ResetIn       string
	IsRate        bool
	Evidence      string
	ErrorText     string
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
const bannerStart = "(?im)^[ \\t]*\u2503[ \\t]+"

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
			Read int64 `json:"read"`
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
			Read:   m.Tokens.Cache.Read,
			Output: m.Tokens.Output + m.Tokens.Reasoning,
			USD:    m.Cost,
		})
	}
	return calls
}
