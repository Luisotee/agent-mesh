// Package claude is the built-in adapter for Claude Code: launched in a PTY with per-launch `--settings`
// hooks that report to the daemon, and its hook payloads turned into neutral events.
package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1/adapterv1connect"
	agentmeshv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/agy"
)

// Kind is the adapter's kind.
const Kind = "claude"

// VerifiedVersions are the Claude Code versions whose hook payloads this adapter was checked against: each has the
// scrubbed payloads of a real session in testdata/<version>/ (daemon/scripts/capture-claude-hooks.sh), decoded and
// replayed through the reducer by fixtures_test.go, which also fails when this list and testdata disagree. The
// decoding is tolerant (unknown fields ignored, missing ones defaulted), but a version outside the list gets a
// warning in the apps.
var VerifiedVersions = []string{"2.1.332"}

// HookTimeout is the seconds Claude Code gives each hook. The hook command never waits on a down daemon: it
// spools within about 3 seconds.
const HookTimeout = 10

// SessionVars are what Claude Code sets for the one session a process runs under; an agent never inherits
// them (bin/mesh's CLAUDE_SESSION_VARS).
var SessionVars = []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_CHILD_SESSION",
	"CLAUDE_CODE_SESSION_KIND", "CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_CODE_MESSAGING_SOCKET",
	"CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_PID", "CLAUDE_EFFORT", "CLAUDE_JOB_DIR", "CLAUDE_BG_BACKEND",
	"CLAUDE_BG_SOCKET_TOKENS_PATH"}

// hookEvents are the hooks every launch registers, with their matcher ("*" for tool events).
var hookEvents = []struct{ name, matcher string }{
	{"SessionStart", ""}, {"UserPromptSubmit", ""}, {"PreToolUse", "*"}, {"PostToolUse", "*"},
	{"Notification", ""}, {"Stop", ""}, {"StopFailure", ""}, {"SubagentStop", ""}, {"PreCompact", ""}, {"SessionEnd", ""},
}

// Adapter is the Claude Code adapter.
type Adapter struct {
	adapterv1connect.UnimplementedCliAdapterServiceHandler
	// NewSessionID picks a launch's conversation id; tests may fix it.
	NewSessionID func() string
}

// New returns the adapter.
func New() *Adapter { return &Adapter{NewSessionID: func() string { return uuid.NewString() }} }

var _ adapter.Adapter = (*Adapter)(nil)

// Describe says what the adapter is.
func (a *Adapter) Describe(context.Context, *connect.Request[adapterv1.DescribeRequest]) (*connect.Response[adapterv1.DescribeResponse], error) {
	return connect.NewResponse(&adapterv1.DescribeResponse{Kind: Kind, DisplayName: "Claude Code",
		VerifiedVersions: VerifiedVersions, Capabilities: []string{"hooks", "session-id", "startup-hook"}}), nil
}

// LookupSession returns empty: Claude picks its session id up front in PlanLaunch.
func (a *Adapter) LookupSession(context.Context, *connect.Request[adapterv1.LookupSessionRequest]) (*connect.Response[adapterv1.LookupSessionResponse], error) {
	return connect.NewResponse(&adapterv1.LookupSessionResponse{}), nil
}

type hookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

type hookGroup struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []hookCommand `json:"hooks"`
}

// Settings is the `--settings` JSON a launch gets: its hooks, each running the hook command with the event's
// name appended. Claude Code adds them to the user's and the repo's own hooks. With denyPublish its PreToolUse
// hook also checks every shell command against the publish rules (EvaluatePreToolUse), and a denial from a hook
// holds in every permission mode.
func Settings(hookArgv []string, denyPublish bool) (string, error) {
	hooks := map[string][]hookGroup{}
	for _, ev := range hookEvents {
		argv := append([]string{}, hookArgv...)
		if denyPublish && ev.name == "PreToolUse" {
			argv = append(argv, "--deny-publish")
		}
		cmd := adapter.ShellQuote(append(argv, ev.name)...)
		hooks[ev.name] = []hookGroup{{Matcher: ev.matcher, Hooks: []hookCommand{{Type: "command", Command: cmd, Timeout: HookTimeout}}}}
	}
	b, err := json.Marshal(map[string]any{"hooks": hooks})
	return string(b), err
}

// publishCommands are the shell commands a read-only agent may not run, as `--disallowedTools` prefix rules: the
// backstop to the PreToolUse check (EvaluatePreToolUse), which reads a command line the way a careful person
// would (`git -C . push`, `gh pr --repo o/r merge`, `bash -c "git push"`, gh api writes), where a prefix rule
// misses all of them, and which a hook that fails to run lets through. The same writes as bin/mesh's
// PUBLISH_COMMANDS, and more.
var publishCommands = []string{"git push", "gh pr review", "gh pr comment", "gh pr merge", "gh pr edit", "gh pr close",
	"gh pr create", "gh pr ready", "gh pr reopen", "gh issue comment", "gh issue create", "gh issue close",
	"gh issue edit", "gh release create", "gh repo delete"}

// ReadOnly reports whether a role may not change code: an asker or a reviewer.
func ReadOnly(role agentmeshv1.AgentRole) bool { return adapter.ReadOnly(role) }

// DenyArgs is the `--disallowedTools` list of a read-only agent: no edits under its worktree or the main
// checkout, no publishing commands. Deny rules win over `--dangerously-skip-permissions`, so a read-only agent
// stays read-only under it. The flag takes several values: put another flag after it. An edit rule stops Write
// too; an agent can still write its report under the mesh's state dir.
func DenyArgs(paths ...string) []string {
	args := []string{"--disallowedTools"}
	seen := map[string]bool{}
	for _, p := range paths {
		if p == "" {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		if !seen[p] {
			seen[p] = true
			args = append(args, "Edit(/"+p+"/**)")
		}
	}
	for _, c := range publishCommands {
		args = append(args, "Bash("+c+":*)")
	}
	return args
}

// PlanLaunch builds the command line: a conversation id chosen now (so a resume finds it) or the one to resume,
// the model and effort, the deny list (a read-only role, or a planner), and the hooks.
func (a *Adapter) PlanLaunch(_ context.Context, req *connect.Request[adapterv1.PlanLaunchRequest]) (*connect.Response[adapterv1.PlanLaunchResponse], error) {
	m := req.Msg
	bin := m.Binary
	if bin == "" {
		bin = "claude"
	}
	deny := ReadOnly(m.Role) || m.DenyEdits
	settings, err := Settings(m.HookArgv, deny)
	if err != nil {
		return nil, err
	}
	// A folder Claude hasn't seen shows a trust dialog before any hook fires: trust the worktree first.
	if err := Pretrust(m.Env, m.Cwd); err != nil {
		return nil, fmt.Errorf("trusting %s for Claude Code: %w", m.Cwd, err)
	}
	// A reopen resumes the agent's own conversation (same id, its context kept); a first launch picks the id now.
	sid := m.ResumeSessionId
	argv := []string{bin, "--resume", sid}
	if sid == "" {
		sid = a.NewSessionID()
		argv = []string{bin, "--session-id", sid}
	}
	if m.Model != "" {
		argv = append(argv, "--model", m.Model)
	}
	if m.Effort != "" {
		argv = append(argv, "--effort", m.Effort)
	}
	if deny {
		argv = append(argv, DenyArgs(m.Cwd, m.Repo)...)
	}
	argv = append(argv, "--settings", settings)
	// The daemon's skills, loaded for this session only (never installed into the user's own): the share folder
	// is a Claude plugin (`.claude-plugin/plugin.json`, `skills/`).
	if share := m.Env["AGENT_MESH_SHARE"]; share != "" {
		if _, err := os.Stat(filepath.Join(share, ".claude-plugin", "plugin.json")); err == nil {
			argv = append(argv, "--plugin-dir", share)
		}
	}
	if m.SkipPermissions {
		argv = append(argv, "--dangerously-skip-permissions")
	}
	env := map[string]string{}
	for k, v := range m.Env {
		env[k] = v
	}
	for _, k := range SessionVars {
		delete(env, k)
	}
	return connect.NewResponse(&adapterv1.PlanLaunchResponse{Argv: argv, Env: env, SessionId: sid}), nil
}

// payload is the part of a hook's stdin the adapter reads; anything else is ignored.
type payload struct {
	SessionID        string `json:"session_id"`
	HookEventName    string `json:"hook_event_name"`
	Source           string `json:"source"`
	Reason           string `json:"reason"`
	NotificationType string `json:"notification_type"`
	Message          string `json:"message"`
	ToolName         string `json:"tool_name"`
	// ToolInput is read only for a Bash call: another tool's (an MCP tool's) may hold anything, a `command` that
	// isn't a string included, and must not fail the decoding of the whole payload.
	ToolInput json.RawMessage `json:"tool_input"`
	// StopFailure: the turn ended on an API error (`rate_limit`, `billing_error`, `authentication_failed`, …).
	Error       string `json:"error"`
	ErrorDetail string `json:"error_details"`
	LastMessage string `json:"last_assistant_message"`
}

// DecodeHook turns a hook's payload into neutral events. A hook from another conversation is dropped, except
// a SessionStart (a /clear starts a new conversation in the same process). An unparseable payload is a
// DecodeError, never a Stop.
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
	if m.SessionId != "" && p.SessionID != "" && p.SessionID != m.SessionId && name != "SessionStart" {
		return connect.NewResponse(out), nil
	}
	switch name {
	case "SessionStart":
		emit(&adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_SessionStarted{SessionStarted: &adapterv1.SessionStarted{
			SessionId: p.SessionID, Source: p.Source}}})
	case "UserPromptSubmit":
		emit(&adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_TurnStarted{TurnStarted: &adapterv1.TurnStarted{}}})
	case "PreToolUse", "PostToolUse":
		emit(&adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_Activity{Activity: &adapterv1.Activity{Detail: p.ToolName}}})
	case "Notification":
		if reason, ok := needsInput(p); ok {
			emit(&adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_NeedsInput{NeedsInput: &adapterv1.NeedsInput{Reason: reason}}})
		}
	case "Stop":
		emit(&adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_TurnEnded{TurnEnded: &adapterv1.TurnEnded{}}})
	case "StopFailure":
		fired := time.Now()
		if m.FiredAt != nil {
			fired = m.FiredAt.AsTime()
		}
		if limit, ok := limitOf(p, fired); ok {
			emit(&adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_RateLimited{RateLimited: limit}})
		} else {
			emit(&adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_TurnFailed{TurnFailed: &adapterv1.TurnFailed{
				Error: failureText(p)}}})
		}
	case "PreCompact":
		emit(&adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_Compacting{Compacting: &adapterv1.Compacting{}}})
	case "SessionEnd":
		emit(&adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_SessionEnded{SessionEnded: &adapterv1.SessionEnded{Reason: p.Reason}}})
	case "SubagentStop":
		// A subagent's turn ended; the agent's own turn goes on. Subagents get their own roster later.
	}
	return connect.NewResponse(out), nil
}

// needsInput reads a Notification: a permission prompt or a question waits on the user; the idle reminder
// and login notices don't.
func needsInput(p payload) (string, bool) {
	switch p.NotificationType {
	case "permission_prompt":
		return orDefault(p.Message, "permission"), true
	case "elicitation_dialog":
		return orDefault(p.Message, "question"), true
	case "":
		// Versions without notification_type: only the permission message waits on the user.
		if strings.Contains(strings.ToLower(p.Message), "permission") {
			return p.Message, true
		}
	}
	return "", false
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// ── limits ──

// limitRE reads the message of a usage limit ("You've hit your session limit · resets 3pm (America/Sao_Paulo)",
// "You're out of extra usage · resets …", "You've hit your monthly spend limit"): its prefixes are Claude Code's
// RATE_LIMIT_ERROR_PREFIXES for the rejected case, and the words after "your" name the limit. A capacity 429
// ("API Error: Request rejected (429) …") is a `rate_limit` error too, but is no usage limit: it names none of these.
var limitRE = regexp.MustCompile(`(?i)(?:you've hit your ([^·\n]*?limit)|(you're out of extra usage))`)

// modelLimitRE are the limits of one model family ("Opus limit", "Sonnet limit"); the others (session, weekly,
// usage, monthly spend, extra usage) are the account's, which every model of a chain shares.
var modelLimitRE = regexp.MustCompile(`(?i)\b(?:opus|sonnet|haiku)\b`)

// limitOf reads a StopFailure as a usage limit.
func limitOf(p payload, fired time.Time) (*adapterv1.RateLimited, bool) {
	if p.Error != "" && p.Error != "rate_limit" && p.Error != "billing_error" {
		return nil, false
	}
	for _, text := range []string{p.LastMessage, p.ErrorDetail} {
		m := limitRE.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		out := &adapterv1.RateLimited{Message: firstLine(text), AllModels: !modelLimitRE.MatchString(m[1])}
		if at, ok := parseReset(text, fired); ok {
			out.ResetAt = timestamppb.New(at)
		}
		return out, true
	}
	return nil, false
}

// failureText is what a StopFailure that isn't a usage limit says: its message, else its details, else its error.
func failureText(p payload) string {
	for _, s := range []string{p.LastMessage, p.ErrorDetail, p.Error} {
		if t := firstLine(s); t != "" {
			return t
		}
	}
	return "error"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// resetRE reads the time of a "resets …" tail as Claude Code writes it (utils/format.ts formatResetTime): `3pm`
// or `3:30pm` within a day, `Nov 27, 3pm` past one (the year too when it isn't this one: `Nov 27, 2027, 3pm`),
// then the zone in parentheses.
var resetRE = regexp.MustCompile(`(?i)\bresets\s+(?:([a-z]{3})\s+(\d{1,2}),\s+(?:(\d{4}),\s+)?)?(\d{1,2})(?::(\d{2}))?\s*([ap])m\b(?:\s*\(([^)]+)\))?`)

// parseReset is when the limit a message names is over. A time of day with no date is the next one after fired
// (Claude says a bare time only within 24 hours). With no zone, or one this host doesn't know, the host's local
// zone reads it.
func parseReset(text string, fired time.Time) (time.Time, bool) {
	m := resetRE.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, false
	}
	loc := time.Local
	if m[7] != "" {
		if l, err := time.LoadLocation(strings.TrimSpace(m[7])); err == nil {
			loc = l
		}
	}
	var hour, min, day, year int
	fmt.Sscanf(m[4], "%d", &hour)
	fmt.Sscanf(m[5], "%d", &min)
	if hour < 1 || hour > 12 || min > 59 {
		return time.Time{}, false
	}
	hour %= 12
	if strings.EqualFold(m[6], "p") {
		hour += 12
	}
	now := fired.In(loc)
	if m[1] == "" {
		at := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, loc)
		if !at.After(now) {
			at = at.AddDate(0, 0, 1)
		}
		return at, true
	}
	month, ok := months[strings.ToLower(m[1])]
	if !ok {
		return time.Time{}, false
	}
	fmt.Sscanf(m[2], "%d", &day)
	if m[3] != "" {
		fmt.Sscanf(m[3], "%d", &year)
	} else {
		year = now.Year()
	}
	at := time.Date(year, month, day, hour, min, 0, 0, loc)
	if m[3] == "" && at.Before(now) {
		at = at.AddDate(1, 0, 0) // a date with no year that has passed this year is next year's
	}
	return at, true
}

var months = map[string]time.Month{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8,
	"sep": 9, "oct": 10, "nov": 11, "dec": 12}

// ── PreToolUse publish check ──

// EvaluatePreToolUse checks a Claude PreToolUse payload against the publish rules, and says whether the tool
// call is denied and why. Only the Bash tool's command is read, by agy's matcher (the same list of gh reads, the
// same unwrapping of `env`, `sh -c` and the like). With denyPublish set, a payload that can't be read is denied:
// the rule fails closed. It guards against an agent's mistake, not a determined one (a script it writes, a git
// alias): the role's prompt says what it may do.
func EvaluatePreToolUse(raw []byte, denyPublish bool) (bool, string) {
	if !denyPublish {
		return false, ""
	}
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return true, "read-only/planning agent: the tool call could not be checked (" + err.Error() + ")"
	}
	if p.ToolName != "Bash" {
		return false, ""
	}
	var in struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(p.ToolInput, &in); err != nil {
		return true, "read-only/planning agent: the shell command could not be read (" + err.Error() + ")"
	}
	if pub := agy.MatchPublishCommand(in.Command); pub != "" {
		return true, fmt.Sprintf("read-only/planning agent may not run %q", pub)
	}
	return false, ""
}

// DenyJSON is what a Claude PreToolUse hook prints to deny a tool call.
func DenyJSON(reason string) []byte {
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse",
		"permissionDecision": "deny", "permissionDecisionReason": reason}})
	return append(b, '\n')
}
