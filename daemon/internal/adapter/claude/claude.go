// Package claude is the built-in adapter for Claude Code: launched in a PTY with per-launch `--settings`
// hooks that report to the daemon, and its hook payloads turned into neutral events.
package claude

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1/adapterv1connect"
	agentmeshv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter"
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
	{"Notification", ""}, {"Stop", ""}, {"SubagentStop", ""}, {"PreCompact", ""}, {"SessionEnd", ""},
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
// name appended. Claude Code adds them to the user's and the repo's own hooks.
func Settings(hookArgv []string) (string, error) {
	hooks := map[string][]hookGroup{}
	for _, ev := range hookEvents {
		cmd := adapter.ShellQuote(append(append([]string{}, hookArgv...), ev.name)...)
		hooks[ev.name] = []hookGroup{{Matcher: ev.matcher, Hooks: []hookCommand{{Type: "command", Command: cmd, Timeout: HookTimeout}}}}
	}
	b, err := json.Marshal(map[string]any{"hooks": hooks})
	return string(b), err
}

// publishCommands are the shell commands a read-only agent may not run: what publishes to a remote or a PR.
// The same list as bin/mesh's PUBLISH_COMMANDS.
var publishCommands = []string{"git push", "gh pr review", "gh pr comment", "gh pr merge", "gh pr edit", "gh pr close",
	"gh issue comment"}

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
	settings, err := Settings(m.HookArgv)
	if err != nil {
		return nil, err
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
	if ReadOnly(m.Role) || m.DenyEdits {
		argv = append(argv, DenyArgs(m.Cwd, m.Repo)...)
	}
	argv = append(argv, "--settings", settings)
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
