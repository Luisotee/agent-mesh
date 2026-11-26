// Package settings is the host's policy as data: every leaf's key, type, default, summary and choices in one
// table, and the check a value must pass. The settings screen, the CLI and the daemon's own reads all come from
// this table, so a leaf is added in one place. Only overrides are stored; a leaf nobody set is its default.
//
// The leaves port `bin/mesh`'s POLICY (design §21) for what the daemon uses or will soon. An app adds its own
// rows (the desktop's `app.*`) here too; it needs no action or event of its own.
package settings

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Type names a value's rule. They are `bin/mesh`'s POLICY_CHECKS names, plus json.
type Type string

const (
	String   Type = "string"
	Strings  Type = "strings"
	Int      Type = "int"
	Percent  Type = "percent"
	Bool     Type = "bool"
	Seconds  Type = "seconds"
	Effort   Type = "effort"
	Chain    Type = "chain"
	Slots    Type = "slots"
	Kinds    Type = "kinds"
	MBOrAuto Type = "mb_or_auto"
	JSON     Type = "json"
	// Shortcut is a key chord such as "Mod+Shift+P", or "" for none (ParseShortcut).
	Shortcut Type = "shortcut"
)

// Efforts are the levels an effort takes, lowest first.
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// AgentKinds are the CLI adapters a `kinds` or `slots` value may name (design §22).
var AgentKinds = []string{"claude", "agy", "copilot", "opencode"}

// Leaf is one setting's definition. Default is a JSON value: nil, bool, float64, string, []any or map[string]any.
type Leaf struct {
	Key     string
	Type    Type
	Default any
	Summary string
	// Choices fill a dropdown; for a kinds or slots leaf they are the kinds one may name.
	Choices []string
	// Restart marks a leaf the daemon reads only at start.
	Restart bool
}

// Schema is a set of leaves by key.
type Schema struct {
	leaves map[string]Leaf
	keys   []string // sorted
}

// NewSchema checks the leaves (unique keys, known types, defaults that pass their own rule) and indexes them.
func NewSchema(leaves []Leaf) (*Schema, error) {
	s := &Schema{leaves: map[string]Leaf{}}
	for _, l := range leaves {
		if l.Key == "" || strings.ContainsAny(l.Key, " \t\n") {
			return nil, fmt.Errorf("setting key %q is empty or has whitespace", l.Key)
		}
		if _, dup := s.leaves[l.Key]; dup {
			return nil, fmt.Errorf("setting %s is defined twice", l.Key)
		}
		if l.Summary == "" {
			return nil, fmt.Errorf("setting %s has no summary", l.Key)
		}
		if _, err := Check(l, l.Default); err != nil {
			return nil, fmt.Errorf("setting %s: its default %v: %w", l.Key, l.Default, err)
		}
		s.leaves[l.Key] = l
		s.keys = append(s.keys, l.Key)
	}
	sort.Strings(s.keys)
	return s, nil
}

// Lookup returns the leaf at a key.
func (s *Schema) Lookup(key string) (Leaf, bool) {
	l, ok := s.leaves[key]
	return l, ok
}

// Keys returns every key starting with prefix, sorted.
func (s *Schema) Keys(prefix string) []string {
	var out []string
	for _, k := range s.keys {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out
}

// Rule is the sentence a type's error ends with, such as "a whole number 0 to 100".
func (l Leaf) rule() string {
	switch l.Type {
	case String:
		if len(l.Choices) > 0 {
			return "one of " + strings.Join(l.Choices, ", ")
		}
		return "a non-empty string"
	case Strings:
		return "a list of non-empty strings"
	case Int:
		return "a whole number, 0 or more"
	case Percent:
		return "a whole number 0 to 100"
	case Bool:
		return "true or false"
	case Seconds:
		return "a finite number of seconds, 0 to 1000000000"
	case Effort:
		return "one of " + strings.Join(Efforts, ", ")
	case Chain:
		return "a non-empty list of model names"
	case Slots:
		return "a list of slots, each a non-empty list of " + strings.Join(l.kinds(), ", ")
	case Kinds:
		return "a list of " + strings.Join(l.kinds(), ", ")
	case MBOrAuto:
		return `"auto" or a whole number of MB, 0 or more`
	case JSON:
		return "any JSON value"
	case Shortcut:
		return `a key chord such as Mod+Shift+P (modifiers ` + strings.Join(shortcutModifiers, ", ") + `, then one key; at least one modifier besides Shift unless it is a function key), or "" for none`
	}
	return "a valid value"
}

func (l Leaf) kinds() []string {
	if len(l.Choices) > 0 {
		return l.Choices
	}
	return AgentKinds
}

// Check validates v (as decoded from JSON: float64, string, bool, []any, map[string]any, nil) against the
// leaf's type. The returned value is v in the type's canonical form (a whole number as a float64 that is
// whole). The error names the rule: "must be a whole number 0 to 100".
func Check(l Leaf, v any) (any, error) {
	bad := func() (any, error) { return nil, fmt.Errorf("%s must be %s", l.Key, l.rule()) }
	whole := func(x any, max float64) bool {
		f, ok := x.(float64)
		return ok && !math.IsNaN(f) && !math.IsInf(f, 0) && f == math.Trunc(f) && f >= 0 && f <= max
	}
	text := func(x any) bool { s, ok := x.(string); return ok && s != "" }
	list := func(x any, each func(any) bool) bool {
		xs, ok := x.([]any)
		if !ok {
			return false
		}
		for _, e := range xs {
			if !each(e) {
				return false
			}
		}
		return true
	}
	oneOf := func(x any, set []string) bool {
		s, ok := x.(string)
		if !ok {
			return false
		}
		for _, c := range set {
			if s == c {
				return true
			}
		}
		return false
	}
	switch l.Type {
	case String:
		if !text(v) || (len(l.Choices) > 0 && !oneOf(v, l.Choices)) {
			return bad()
		}
	case Strings:
		if !list(v, text) {
			return bad()
		}
	case Chain:
		if !list(v, text) || len(v.([]any)) == 0 {
			return bad()
		}
	case Int:
		if !whole(v, math.MaxInt32) {
			return bad()
		}
	case Percent:
		if !whole(v, 100) {
			return bad()
		}
	case Bool:
		if _, ok := v.(bool); !ok {
			return bad()
		}
	case Seconds:
		// Capped so a consumer's time.Duration(f * float64(time.Second)) can't overflow.
		if f, ok := v.(float64); !ok || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 1e9 {
			return bad()
		}
	case Effort:
		if !oneOf(v, Efforts) {
			return bad()
		}
	case Kinds:
		if !list(v, func(x any) bool { return oneOf(x, l.kinds()) }) {
			return bad()
		}
	case Slots:
		slot := func(x any) bool {
			return list(x, func(k any) bool { return oneOf(k, l.kinds()) }) && len(x.([]any)) > 0
		}
		if !list(v, slot) {
			return bad()
		}
	case MBOrAuto:
		if s, ok := v.(string); !ok || s != "auto" {
			if !whole(v, math.MaxInt32) {
				return bad()
			}
		}
	case JSON:
	case Shortcut:
		if s, ok := v.(string); !ok || (s != "" && !ValidShortcut(s)) {
			return bad()
		}
	default:
		return nil, fmt.Errorf("%s has unknown type %q", l.Key, l.Type)
	}
	return v, nil
}

// tierNotes say when to pick a tier; the tiers are what the root picks per agent and phase (design §9): a model chain and an effort per CLI, and a note
// on when to pick it. Only Claude has `top`, used when the user names it.
var tierNotes = map[string]string{
	"light":  "trivial tasks (copy, translations, a version bump, config) and short mechanical text",
	"mid":    "executing a clear plan, small tasks, routine questions and peer follow-ups, small-PR reviews",
	"strong": "planning, unclear or risky work (auth, money, data migrations, concurrency), large-PR reviews",
	"top":    "only when the user names it",
}

func chain(models ...string) []any {
	out := make([]any, len(models))
	for i, m := range models {
		out[i] = m
	}
	return out
}

// OpenCode runs on the one free model whose findings were worth checking (cost study,
// docs/cost-research/roles-and-reviews.md), with no `free` fallback: the others' were mostly rejected.
var opencodeChain = []string{"opencode/deepseek-v4.1-flash-free"}

// tierDefaults mirrors `bin/mesh`'s TIER_DEFAULTS.
var tierDefaults = []struct {
	kind, tier string
	models     []string
	effort     string
}{
	{"claude", "light", []string{"haiku"}, "medium"},
	{"claude", "mid", []string{"sonnet"}, "high"},
	{"claude", "strong", []string{"opus"}, "high"},
	{"claude", "top", []string{"fable"}, "high"},
	{"agy", "light", []string{"flash"}, "low"},
	{"agy", "mid", []string{"hydrogen"}, "high"},
	{"agy", "strong", []string{"neon", "opus", "carbon", "hydrogen"}, "high"},
	{"copilot", "light", []string{"auto"}, "low"},
	{"copilot", "mid", []string{"auto"}, "medium"},
	{"copilot", "strong", []string{"auto"}, "high"},
	{"opencode", "light", opencodeChain, "low"},
	{"opencode", "mid", opencodeChain, "high"},
	{"opencode", "strong", opencodeChain, "high"},
}

// The lean Claude defaults (bin/mesh's LEAN_*; design §11). Tools by name; bundled skills; plugins nothing in a
// mesh uses. A worker's skills are the share plugin's, bare and namespaced (`agent-mesh:`), because
// the same skill may be linked into the user's own skills folder.
var (
	leanDeny     = []string{"Artifact", "Workflow", "SendFeedback", "ScheduleWakeup", "ListAgents", "AskUserQuestion", "EnterWorktree"}
	rootLeanDeny = []string{"Artifact", "Workflow", "SendFeedback", "ScheduleWakeup", "ListAgents", "Agent", "ReportFindings"}
	leanSkills   = []string{"dataviz", "claude-api", "artifact-design", "artifact-diagramming", "artifact-capabilities",
		"schedule", "loop", "ship-local", "steward", "plugin-authoring", "workflow-authoring", "keybindings-help",
		"fewer-permission-prompts", "claude-approvals", "update-config"}
	workerLeanSkills = []string{"delegate", "delegate-review", "find-peers", "harvest-peers",
		"agent-mesh:delegate", "agent-mesh:delegate-review", "agent-mesh:find-peers",
		"agent-mesh:harvest-peers"}
	leanPlugins = []string{"pr-review-toolkit", "feature-dev", "code-simplifier", "claude-code-setup"}
)

var sizes = []string{"trivial", "small", "large"}

// DefaultLeaves is the host's table. An app's own rows (`app.*`) are appended here.
func DefaultLeaves() []Leaf {
	var ls []Leaf
	for _, t := range tierDefaults {
		p := "tiers." + t.kind + "." + t.tier + "."
		who := t.kind + " " + t.tier + " tier: "
		ls = append(ls,
			Leaf{Key: p + "model", Type: Chain, Default: chain(t.models...),
				Summary: who + "the models to try in order (the next when one is at its quota)"},
			Leaf{Key: p + "effort", Type: Effort, Default: t.effort, Choices: Efforts, Summary: who + "reasoning effort"},
			Leaf{Key: p + "note", Type: String, Default: tierNotes[t.tier], Summary: who + "when the root should pick it"})
	}
	reviewLevel := map[string]string{"trivial": "medium", "small": "medium", "large": "high"}
	selfLevel := map[string]string{"trivial": "low", "small": "low", "large": "medium"}
	ls = append(ls, Leaf{Key: "review.effort", Type: Effort, Default: "high", Choices: Efforts,
		Summary: "effort of a fresh reviewer, whatever its tier, unless the review names another"})
	for _, sz := range sizes {
		ls = append(ls,
			Leaf{Key: "review.level." + sz, Type: Effort, Default: reviewLevel[sz], Choices: Efforts,
				Summary: "the /code-review level of an outside reviewer on a " + sz + " PR"},
			Leaf{Key: "review.self_level." + sz, Type: Effort, Default: selfLevel[sz], Choices: Efforts,
				Summary: "the /code-review level of a worker's self-review of a " + sz + " PR, before its done"})
	}
	ls = append(ls,
		Leaf{Key: "review.reviewers.small", Type: Slots, Default: []any{chain("opencode", "agy", "claude")}, Choices: AgentKinds,
			Summary: "reviewer slots of a small PR: each takes its first available kind not picked yet"},
		Leaf{Key: "review.reviewers.large", Type: Slots, Default: []any{chain("agy", "claude", "opencode")},
			Choices: AgentKinds,
			Summary: "reviewer slots of a large PR: each takes its first available kind not picked yet"},
		Leaf{Key: "review.re_review.skip", Type: Kinds, Default: chain("copilot"), Choices: AgentKinds,
			Summary: "kinds that review a PR once and are not asked again after a fix"},
		Leaf{Key: "review.re_review.keep.small", Type: Kinds, Default: []any{}, Choices: AgentKinds,
			Summary: "kinds asked again after a fix on a small PR even when skipped"},
		Leaf{Key: "review.re_review.keep.large", Type: Kinds, Default: []any{}, Choices: AgentKinds,
			Summary: "kinds asked again after a fix on a large PR even when skipped"},
		Leaf{Key: "review.rounds.small", Type: Int, Default: 1.0,
			Summary: "review rounds a small PR gets at most (a ceiling, not a quota)"},
		Leaf{Key: "review.rounds.large", Type: Int, Default: 2.0,
			Summary: "review rounds a large PR gets at most (a ceiling, not a quota)"},
		Leaf{Key: "ask.timeout", Type: Seconds, Default: 1800.0,
			Summary: "seconds a round of reviewers (or another collected ask) waits for its members: the ones still silent are then settled as not answered, and you get what came in"},
		Leaf{Key: "peers.dir", Type: String, Default: "~/research",
			Summary: "where `mesh add-peer <url>` clones a project: <dir>/<group>/<repo> (a leading ~ is the daemon's home)"},
		Leaf{Key: "repos.dir", Type: Strings, Default: []any{},
			Summary: "folders a bare `--repo <name>` is looked up in, in order: the name must be a repo's own top folder there (a leading ~ is the daemon's home); a repo in the caller's working directory needs no setting"},
		Leaf{Key: "quota_stop", Type: Percent, Default: 90.0,
			Summary: "percent of a vendor's quota used at which nothing new starts on it"},
		Leaf{Key: "idle_retire", Type: Seconds, Default: 300.0,
			Summary: "seconds an agent idle after its done or failed stays open before it is retired; 0 never retires"},
		Leaf{Key: "accountability.nudge_after", Type: Seconds, Default: 30.0,
			Summary: "seconds an agent idle after a task, with no report to its parent, waits before it is nudged once (and, if it ends another turn the same way, its parent told); 0 turns nudges off"},
		Leaf{Key: "accountability.wait_max", Type: Seconds, Default: 14400.0,
			Summary: "seconds a declared wait (a `wait` message to the parent) holds off the nudge, at most"},
		Leaf{Key: "accountability.input_after", Type: Seconds, Default: 120.0,
			Summary: "seconds an agent waits at a permission prompt or question with no answer before its parent is told (once per prompt); 0 never tells"},
		Leaf{Key: "delivery.socket", Type: Bool, Default: true,
			Summary: "write a Claude agent's doorbell into its inbox socket (never into its input box) where it takes cross-session messages; false types every doorbell"},
		Leaf{Key: "delivery.confirm_after", Type: Seconds, Default: 10.0,
			Summary: "seconds a doorbell may go unconfirmed (no sign of life from the agent, its messages unread, its ref not above the input box) before it is rung again"},
		Leaf{Key: "delivery.ring_tries", Type: Int, Default: 4.0,
			Summary: "doorbells (written to an inbox socket or typed) rung for one set of messages before giving up with a DoorbellFailed event; nothing more is typed until the agent's activity moves"},
		Leaf{Key: "delivery.draft_hold_alert", Type: Seconds, Default: 600.0,
			Summary: "seconds a doorbell waits for a human's draft in the agent's input box before a DoorbellHeld event says so (it is never typed into the draft); 0 never says"},
		Leaf{Key: "compact.claude", Type: Int, Default: 300000.0,
			Summary: "tokens a stopped Claude conversation has at resume above which it is compacted first (a read-only task's threshold is double, except a peer's); 0 turns it off"},
		Leaf{Key: "compact.claude_cold", Type: Int, Default: 200000.0,
			Summary: "the same threshold once the conversation's prompt cache has expired (its next turn re-reads all of it at the cache-write price); the lower of the two counts; 0 turns it off"},
		Leaf{Key: "compact.agy", Type: Int, Default: 100000.0,
			Summary: "tokens an agy conversation has at resume above which it is compacted first; 0 turns it off"},
		Leaf{Key: "compact.approve", Type: Int, Default: 250000.0,
			Summary: "tokens a planner's conversation has at approve above which it is compacted, keeping the approved plan, before it carries the plan out on the same model; 0 turns it off"},
		Leaf{Key: "compact.approve_switch", Type: Int, Default: 200000.0,
			Summary: "the same threshold when approve moves it to another model (the new model's first call re-reads all of it, uncached); 0 turns it off"},
		Leaf{Key: "compact.followup", Type: Int, Default: 250000.0,
			Summary: "tokens a finished change worker has above which it is compacted before its next follow-up round (a warm cache); 0 turns it off"},
		Leaf{Key: "compact.followup_cold", Type: Int, Default: 200000.0,
			Summary: "the same threshold once its prompt cache has expired; the lower of the two counts; 0 turns it off"},
		Leaf{Key: "root_reset.at", Type: Int, Default: 150000.0,
			Summary: "tokens a root's conversation has above which its commands say to run `mesh reset` once it is cold, and the keeper resets it when idle"},
		Leaf{Key: "root_reset.auto", Type: Bool, Default: true,
			Summary: "reset an idle, big root by itself (quiet 5 minutes after a mesh turn, 30 after the user's); false only hints"},
		Leaf{Key: "root_reset.wait", Type: Seconds, Default: 1800.0,
			Summary: "seconds a reset waits for the root to sit at a quiet prompt, nobody typing, before it gives up"},
		Leaf{Key: "root_reset.relaunch", Type: Bool, Default: true,
			Summary: "reset a root by relaunching it with a fresh lean session; false types /clear into the running one (or /compact with --compact)"},
		Leaf{Key: "claude.lean", Type: Bool, Default: true,
			Summary: "launch Claude agents without the tools, skills, plugins and MCP servers a mesh doesn't use (about 39k tokens off every call); false loads the user's full set"},
		Leaf{Key: "claude.deny_tools", Type: Strings, Default: chain(leanDeny...),
			Summary: "tools a lean Claude agent starts without (a bare name drops the tool's schema from every request)"},
		Leaf{Key: "claude.root_deny_tools", Type: Strings, Default: chain(rootLeanDeny...),
			Summary: "tools a lean Claude root starts without (it keeps AskUserQuestion)"},
		Leaf{Key: "claude.skills_hidden", Type: Strings, Default: chain(leanSkills...),
			Summary: "bundled skills hidden from a lean Claude agent's listing (the user can still type them)"},
		Leaf{Key: "claude.worker_skills_hidden", Type: Strings, Default: chain(workerLeanSkills...),
			Summary: "skills also hidden from a lean Claude agent that isn't a root: only a root starts a mesh"},
		Leaf{Key: "claude.claudeai_skills", Type: Bool, Default: false,
			Summary: "keep the skills synced from claude.ai (`anthropic-skills:*`) in a lean Claude agent"},
		Leaf{Key: "claude.plugins_off", Type: Strings, Default: chain(leanPlugins...),
			Summary: "plugins (by name, any marketplace) a lean Claude agent turns off when the user's settings enable them"},
		Leaf{Key: "claude.mcp", Type: String, Default: "repo", Choices: []string{"repo", "none", "inherit"},
			Summary: "MCP servers a lean Claude agent loads besides claude.mcp_keep and the spawn's `mcp`: `repo` = the repo's own (its project entry in ~/.claude.json and its approved .mcp.json), `none`, or `inherit` = whatever the user's config loads"},
		Leaf{Key: "claude.mcp_keep", Type: Strings, Default: chain("context7"),
			Summary: "the user's MCP servers every lean Claude agent loads, with web search (never denied): docs for any library"},
		Leaf{Key: "claude.mcp_exclusive", Type: Strings, Default: chain("playwright"),
			Summary: "MCP servers at most one live agent at a time may load (for example Playwright's shared user browser)"},
		Leaf{Key: "claude.mcp_about", Type: JSON, Default: map[string]any{},
			Summary: "one-line descriptions shown by `mesh tools servers` for named MCP servers"},
		Leaf{Key: "skip_permissions", Type: Bool, Default: true,
			Summary: "launch agents with their CLI's permission prompts off"},
		Leaf{Key: "min_free_mb", Type: MBOrAuto, Default: "auto",
			Summary: `MB of memory plus swap under which no agent starts; "auto" is 10% of RAM plus 10% of swap plus 1 GB (at least 2 GB), 0 turns the guard off`},
		Leaf{Key: "memory_pressure_max", Type: Percent, Default: 10.0,
			Summary: "no agent starts while the memory pressure (full avg60) is over this percent; 0 turns the guard off"},
		Leaf{Key: "worker_cpus", Type: Percent, Default: 25.0,
			Summary: "percent of the machine's CPU cores each launched agent may use (at least one); 0 or 100 turns the cap off"},
		Leaf{Key: "worker_memory_high", Type: Int, Default: 0.0,
			Summary: "MB above which each launched agent is throttled in a transient systemd user scope (MemoryHigh); 0 turns the scope off"},
		Leaf{Key: "limits.auto", Type: String, Default: "switch", Choices: []string{"off", "continue", "switch"},
			Summary: "what the daemon does for an agent stopped at a usage limit: switch moves it to the next model of its chain " +
				"that has quota left when the limit is its model's, and (with continue) reopens it on the same model once the " +
				"limit is over; off leaves it to the root or the user"},
		Leaf{Key: "limits.hold", Type: Seconds, Default: 1800.0,
			Summary: "seconds a model counts as at its limit when the limit named no reset time"},
		Leaf{Key: "caps.agents", Type: Int, Default: 8.0,
			Summary: "live agent processes this host runs at once (agents still starting count); 0 means no cap"},
	)
	return append(ls, AppLeaves()...)
}

// NotifyEvents are the events the desktop app can notify about (app.notify.events).
var NotifyEvents = []string{"input", "plan", "question", "report", "failed", "exited", "limited", "error"}

// DefaultNotifyEvents are the ones that notify until the user chooses (every event but a clean exit and a usage
// limit, which the daemon usually handles itself by moving the agent to the next model). The desktop
// app falls back to the same list before the host's settings arrive (desktop/src/prefs.ts).
var DefaultNotifyEvents = []string{"input", "plan", "question", "report", "failed", "error"}

// shortcutModifiers are the modifiers a chord may hold; Mod is Ctrl on Linux and Windows and Cmd on macOS.
var shortcutModifiers = []string{"Mod", "Ctrl", "Alt", "Shift", "Meta"}

var shortcutNamedKeys = map[string]bool{
	"Enter": true, "Tab": true, "Space": true, "Escape": true, "Backspace": true, "Delete": true, "Insert": true,
	"Home": true, "End": true, "PageUp": true, "PageDown": true, "ArrowUp": true, "ArrowDown": true,
	"ArrowLeft": true, "ArrowRight": true, "Plus": true,
}

// ValidShortcut reports whether s is a key chord: modifiers (Mod, Ctrl, Alt, Shift, Meta, none twice) joined
// by "+" and then one key, a single printable character other than "+" (letters in any case), a named key
// (Enter, Tab, Space, Escape, Backspace, Delete, Insert, Home, End, PageUp, PageDown, an arrow, Plus) or F1 to
// F24. A chord needs a modifier other than Shift unless its key is a function key: a bare or shifted letter would fire
// while typing.
func ValidShortcut(s string) bool {
	parts := strings.Split(s, "+")
	key := parts[len(parts)-1]
	if key == "" {
		return false
	}
	seen := map[string]bool{}
	for _, m := range parts[:len(parts)-1] {
		ok := false
		for _, known := range shortcutModifiers {
			ok = ok || m == known
		}
		if !ok || seen[m] {
			return false
		}
		seen[m] = true
	}
	function := false
	if rest, ok := strings.CutPrefix(key, "F"); ok {
		n, err := strconv.Atoi(rest)
		function = err == nil && n >= 1 && n <= 24 && strconv.Itoa(n) == rest
	}
	switch {
	case function, shortcutNamedKeys[key]:
	case utf8.RuneCountInString(key) == 1 && key != "+" && unicode.IsPrint([]rune(key)[0]) && key != " ":
	default:
		return false
	}
	// Shift alone doesn't count: Shift+P is just a capital P.
	return len(seen) > 1 || (len(seen) == 1 && !seen["Shift"]) || function
}

// DockNewAgents are the choices of app.dock.new_agents.
var DockNewAgents = []string{"dock", "badge", "tab", "none"}

// AppLeaves are the desktop app's own preferences. They live on the host so every device and every agent sees
// and can change them (docs/apps.md, "Settings are data on the host"); the daemon itself reads none of them.
func AppLeaves() []Leaf {
	events := make([]any, len(DefaultNotifyEvents))
	for i, e := range DefaultNotifyEvents {
		events[i] = e
	}
	return []Leaf{
		{Key: "app.notify.enabled", Type: Bool, Default: true,
			Summary: "desktop notifications for what happens on this host"},
		{Key: "app.notify.sound", Type: Bool, Default: true,
			Summary: "play the system sound with a desktop notification"},
		{Key: "app.notify.events", Type: Kinds, Default: events, Choices: NotifyEvents,
			Summary: "what notifies: an agent stopped at a prompt (input), a plan or a question to answer, a report (done), a failure, an agent in error, an agent at its usage limit (limited), or any agent's exit"},
		{Key: "app.dock.new_agents", Type: String, Default: "dock", Choices: DockNewAgents,
			Summary: "what a worker that starts while the app is open does: pop into the live dock marked new (dock), only count on the dock's toggle (badge), open as a tab in the background (tab), or nothing (none). It never takes focus"},
		{Key: "app.keys.palette", Type: Shortcut, Default: "Mod+Shift+P", Summary: "shortcut: open the command palette"},
		{Key: "app.keys.settings", Type: Shortcut, Default: "Mod+,", Summary: "shortcut: open or close the host's settings"},
		{Key: "app.keys.chat_toggle", Type: Shortcut, Default: "Mod+Shift+J", Summary: "shortcut: switch the open agent between its chat and its terminal"},
		{Key: "app.keys.dock", Type: Shortcut, Default: "Mod+Shift+D", Summary: "shortcut: show or hide the live dock"},
		{Key: "app.keys.tab_next", Type: Shortcut, Default: "Ctrl+Tab", Summary: "shortcut: the next open agent tab"},
		{Key: "app.keys.tab_prev", Type: Shortcut, Default: "Ctrl+Shift+Tab", Summary: "shortcut: the previous open agent tab"},
		{Key: "app.keys.tab_close", Type: Shortcut, Default: "Mod+Shift+W", Summary: "shortcut: close the open agent tab"},
		{Key: "app.keys.waiting_next", Type: Shortcut, Default: "Mod+Shift+A", Summary: "shortcut: go to the next plan, question or agent that waits on you"},
		{Key: "app.tray.close_to_tray", Type: Bool, Default: false,
			Summary: "closing the desktop window keeps the app running in the tray (off by default: a desktop with no tray would leave no way back to the window)"},
	}
}

// Default returns the host's schema.
func Default() *Schema {
	s, err := NewSchema(DefaultLeaves())
	if err != nil {
		panic(err)
	}
	return s
}
