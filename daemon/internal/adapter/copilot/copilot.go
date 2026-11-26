// Package copilot is the adapter for the GitHub Copilot CLI (`copilot`), design §22 of docs/design.md. Copilot
// fires no hooks, so its turns are read from the events file it writes for every session
// (`$COPILOT_HOME/session-state/<id>/events.jsonl`); its session id is chosen at launch (`--session-id`, new or
// reopened), so it has no session lookup; a read-only or planning agent runs under `--deny-tool` rules.
package copilot

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1/adapterv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/opencode"
)

// Kind is the adapter's kind.
const Kind = "copilot"

// VerifiedVersions are the Copilot CLI versions the adapter was checked against.
var VerifiedVersions = []string{"1.0.115"}

// AutoModel is the model name that lets Copilot route each prompt; it refuses an effort and takes a routing
// profile instead.
const (
	AutoModel = "auto"
	AutoTier  = "intelligence"
)

// Holds: how long a Copilot limit keeps the kind out of review slots and tier chains when it names no reset.
const (
	// QuotaHold is for a 402 (its monthly quota): Copilot states no reset time.
	QuotaHold = 24 * time.Hour
	// PolicyHold is for a 403 policy refusal: an admin has to change a setting, so it is retried hourly.
	PolicyHold = time.Hour
	// RateHold is for a screen rate limit that names no reset: the longest window, as bin/mesh's
	// LIMIT_UNKNOWN_RESET counted any CLI whose reset it could not read.
	RateHold = 5 * time.Hour
)

// publishCommands are the shell commands a read-only or planning agent may not run (the other adapters' list).
var publishCommands = []string{
	"git push", "gh pr review", "gh pr comment", "gh pr merge", "gh pr edit", "gh pr close",
	"gh pr create", "gh pr ready", "gh pr reopen", "gh issue comment", "gh issue create", "gh issue close",
	"gh issue edit", "gh release create", "gh repo delete",
}

// DenyRules are the `--deny-tool` rules of a read-only or planning agent: no `write` tool (it writes its report
// or plan under $(mesh dir) with the shell, or sends it with `--body`) and no publishing. A shell rule needs
// `:*` to match the command with any arguments: Copilot ignores a bare `shell(gh pr comment)`.
func DenyRules() []string {
	rules := []string{"write"}
	for _, c := range publishCommands {
		rules = append(rules, "shell("+c+":*)")
	}
	return rules
}

// Adapter is the Copilot CLI adapter.
type Adapter struct {
	adapterv1connect.UnimplementedCliAdapterServiceHandler
	// NewSessionID picks a launch's conversation id; tests may fix it.
	NewSessionID func() string
}

// New returns the adapter.
func New() *Adapter { return &Adapter{NewSessionID: func() string { return uuid.NewString() }} }

var _ adapter.Adapter = (*Adapter)(nil)

// Describe says what the adapter is: no hooks and no startup hook (the daemon marks it idle once its first
// output settles), and no session lookup (the id is its own).
func (a *Adapter) Describe(context.Context, *connect.Request[adapterv1.DescribeRequest]) (*connect.Response[adapterv1.DescribeResponse], error) {
	return connect.NewResponse(&adapterv1.DescribeResponse{Kind: Kind, DisplayName: "GitHub Copilot",
		VerifiedVersions: VerifiedVersions, Capabilities: []string{"session-id"}}), nil
}

// DecodeHook returns empty: Copilot has no hooks.
func (a *Adapter) DecodeHook(context.Context, *connect.Request[adapterv1.DecodeHookRequest]) (*connect.Response[adapterv1.DecodeHookResponse], error) {
	return connect.NewResponse(&adapterv1.DecodeHookResponse{}), nil
}

// DescribeModel returns empty: Copilot's model ids do not encode an effort.
func (a *Adapter) DescribeModel(context.Context, *connect.Request[adapterv1.DescribeModelRequest]) (*connect.Response[adapterv1.DescribeModelResponse], error) {
	return connect.NewResponse(&adapterv1.DescribeModelResponse{}), nil
}

// LookupSession returns empty: the session id is chosen in PlanLaunch.
func (a *Adapter) LookupSession(context.Context, *connect.Request[adapterv1.LookupSessionRequest]) (*connect.Response[adapterv1.LookupSessionResponse], error) {
	return connect.NewResponse(&adapterv1.LookupSessionResponse{}), nil
}

// ModelArgs are the model flags: Auto picks the model per prompt and takes a routing profile; a named model
// takes its effort.
func ModelArgs(model, effort string) []string {
	if model == "" {
		return nil
	}
	if model == AutoModel {
		return []string{"--model", model, "--auto-tier", AutoTier}
	}
	args := []string{"--model", model}
	if effort != "" {
		args = append(args, "--reasoning-effort", effort)
	}
	return args
}

// PlanLaunch builds the command. The session id goes in `--session-id` for a first launch and a reopen alike (it
// starts a session with that id, and reopens it after), so a switch of model keeps the conversation. Each deny
// rule is its own `--deny-tool=<rule>`: the flag takes several values, so the `=` form can't swallow the flag
// after it, and a deny rule wins over `--yolo`.
func (a *Adapter) PlanLaunch(_ context.Context, req *connect.Request[adapterv1.PlanLaunchRequest]) (*connect.Response[adapterv1.PlanLaunchResponse], error) {
	m := req.Msg
	bin := m.Binary
	if bin == "" {
		bin = "copilot"
	}
	sid := m.ResumeSessionId
	if sid == "" {
		sid = a.NewSessionID()
	}
	argv := []string{bin}
	if adapter.ReadOnly(m.Role) || m.DenyEdits {
		for _, rule := range DenyRules() {
			argv = append(argv, "--deny-tool="+rule)
		}
	}
	argv = append(argv, "--session-id", sid)
	argv = append(argv, ModelArgs(m.Model, m.Effort)...)
	if m.SkipPermissions {
		argv = append(argv, "--yolo")
	}
	env := map[string]string{}
	for k, v := range m.Env {
		env[k] = v
	}
	return connect.NewResponse(&adapterv1.PlanLaunchResponse{Argv: argv, Env: env, SessionId: sid}), nil
}

// ── session events ──

// Home is Copilot's state folder: $COPILOT_HOME, else ~/.copilot.
func Home(env map[string]string) string {
	if h := env["COPILOT_HOME"]; h != "" {
		return h
	}
	home := env["HOME"]
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, ".copilot")
}

// EventsPath is the file Copilot writes every event of a session to.
func EventsPath(env map[string]string, sessionID string) string {
	return filepath.Join(Home(env), "session-state", sessionID, "events.jsonl")
}

// tailBytes is how much of the events file's end is read: the newest turn is what matters.
const tailBytes = 1 << 18

// Event is one line of events.jsonl, the fields the adapter reads.
type Event struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Timestamp string          `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

type eventData struct {
	Content      string            `json:"content"`
	ToolRequests []json.RawMessage `json:"toolRequests"`
	ErrorType    string            `json:"errorType"`
	ErrorCode    string            `json:"errorCode"`
	StatusCode   int               `json:"statusCode"`
	Message      string            `json:"message"`
}

func (e Event) data() eventData {
	var d eventData
	_ = json.Unmarshal(e.Data, &d)
	return d
}

// Time is when the event was written (zero when it carries no readable timestamp).
func (e Event) Time() time.Time {
	t, err := time.Parse(time.RFC3339Nano, e.Timestamp)
	if err != nil {
		return time.Time{}
	}
	return t
}

// ReadEvents reads the events in the last tailBytes of an events file, skipping a line that is not JSON (and a
// first line cut by the tail). A missing file is no events.
func ReadEvents(path string) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	cut := false
	if st.Size() > tailBytes {
		if _, err := f.Seek(st.Size()-tailBytes, io.SeekStart); err != nil {
			return nil, err
		}
		cut = true
	}
	var out []Event
	r := bufio.NewReaderSize(f, 1<<20)
	for first := true; ; first = false {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && !(first && cut) {
			var e Event
			if json.Unmarshal(line, &e) == nil && e.Type != "" {
				out = append(out, e)
			}
		}
		if err != nil {
			break
		}
	}
	return out, nil
}

// Limit is a limit Copilot answered a turn with.
type Limit struct {
	Message string
	// Hold is how long to count Copilot out: Copilot names no reset time.
	Hold time.Duration
	// Policy marks a 403 refusal by the account's Copilot policy rather than a quota.
	Policy bool
	// PerModel marks a rate limit on the model the agent runs ("the rate limit for this model"): the account's
	// other models are free. Every other limit is on the whole account.
	PerModel bool
	At       time.Time
}

var policyRE = regexp.MustCompile(`(?i)not authorized to use this Copilot feature|access denied by Copilot policy`)

// ClassifyError reads a `session.error` event as a limit: a 402 (`errorType` quota, `errorCode`
// quota_exceeded, or `statusCode` 402: the monthly quota) or a 403 policy refusal. Anything else is no limit.
func ClassifyError(e Event) (Limit, bool) {
	if e.Type != "session.error" {
		return Limit{}, false
	}
	d := e.data()
	switch {
	case d.ErrorType == "quota" || d.StatusCode == 402 || d.ErrorCode == "quota_exceeded":
		return Limit{Message: firstLine(d.Message, "quota exceeded"), Hold: QuotaHold, At: e.Time()}, true
	case policyRE.MatchString(d.Message):
		return Limit{Message: firstLine(d.Message, "access denied by Copilot policy"), Hold: PolicyHold, Policy: true, At: e.Time()}, true
	}
	return Limit{}, false
}

func firstLine(s, otherwise string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len(s) > 300 {
		s = s[:300]
	}
	if s == "" {
		return otherwise
	}
	return s
}

// Status is what a session's events say its agent is doing.
type Status struct {
	// State is "working" (a prompt got in, no final answer yet), "idle" (the newest prompt was answered) or ""
	// (no prompt yet).
	State string
	// Key identifies the newest event that decided State, so a poll acts once per change.
	Key string
	// Limit is the newest limit error with no user or assistant message after it: the session is stopped at it.
	Limit *Limit
	// AnsweredAt is when the newest assistant message with text was written, and PromptAt when the prompt it
	// answers was typed (zero when that prompt is out of the events read). A hold set after PromptAt says nothing
	// of the answer: the turn began before the limit did.
	AnsweredAt, PromptAt time.Time
}

// Inspect reads the state of a session from its events. A turn ends in `assistant.turn_end`, but a model call
// that asked for tools is followed by another turn: only a final answer (an assistant message with no tool
// requests) followed by its turn end means idle. The events are the tail of the file, so a turn can begin before
// them: its model calls still say it is working, and its end says it is over, whatever came before the window.
func Inspect(events []Event) Status {
	var st Status
	finalAnswer := false
	var promptAt time.Time
	for _, e := range events {
		switch e.Type {
		case "user.message":
			st.State, st.Key, st.Limit, finalAnswer, promptAt = "working", e.ID, nil, false, e.Time()
		case "assistant.turn_start":
			st.State, st.Key, finalAnswer = "working", e.ID, false
		case "assistant.message":
			d := e.data()
			st.Limit = nil
			if strings.TrimSpace(d.Content) != "" {
				st.AnsweredAt, st.PromptAt = e.Time(), promptAt
			}
			finalAnswer = len(d.ToolRequests) == 0
		case "assistant.turn_end":
			if finalAnswer {
				st.State, st.Key = "idle", e.ID
			}
		case "session.error":
			if l, ok := ClassifyError(e); ok {
				l := l
				st.Limit, st.Key = &l, e.ID
			}
		}
	}
	return st
}

// ── screen ──

// limitRE matches Copilot's rate-limit messages (`copilot help limits`): "You've hit the rate limit for this
// model.", "You've hit your session rate limit.", "You've reached your weekly rate limit.", "You've hit your
// rate limit." Anchored at the line start (past box and gutter glyphs) so the agent quoting one doesn't count.
var limitRE = regexp.MustCompile(`(?i)^[^A-Za-z\n]*You've (?:hit (?:your|the) .*rate limit|reached your weekly rate limit)`)

// perModelRE is the one rate-limit message about a single model; the session and weekly ones are the account's.
var perModelRE = regexp.MustCompile(`(?i)rate limit for this model`)

var resetRE = regexp.MustCompile(`(?i)(?:resets?|try again)\s+in\s+((?:\d+(?:\.\d+)?\s*[a-z]+\s*)+)`)

// ScreenLimit finds a rate limit or a policy refusal on the last lines of a screen.
func ScreenLimit(lines []string) (Limit, bool) {
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if limitRE.MatchString(line) {
			hold := RateHold
			if m := resetRE.FindStringSubmatch(line); m != nil {
				if d := opencode.ParseDuration(m[1]); d > 0 {
					hold = d
				}
			}
			return Limit{Message: firstLine(line, "rate limit"), Hold: hold, PerModel: perModelRE.MatchString(line)}, true
		}
		if strings.Contains(line, "Failed to load models: access denied by Copilot policy") {
			return Limit{Message: firstLine(line, "access denied by Copilot policy"), Hold: PolicyHold, Policy: true}, true
		}
	}
	return Limit{}, false
}

// TrustDialog reports whether the screen shows the folder-trust dialog, which neither `--yolo` nor `--add-dir`
// skips (1.0.115) and which holds the session until answered. The first option ("Yes", the default) trusts the
// folder for this session only: Enter answers it and writes nothing to Copilot's config.
func TrustDialog(lines []string) bool {
	trust, option := false, false
	for _, l := range lines {
		switch {
		case strings.Contains(l, "Confirm folder trust"):
			trust = true
		case strings.Contains(l, "1. Yes") && strings.Contains(l, "❯"):
			option = true
		}
	}
	return trust && option
}

// ScreenLines converts a terminal's repaint bytes to text lines.
func ScreenLines(raw []byte) []string { return opencode.ScreenLines(raw) }
