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
	"strings"
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
		if !text(v) {
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

var opencodeChain = []string{"opencode/deepseek-v4.1-flash-free", "opencode/flint-40b-free", "opencode/beam-free", "free"}

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
	{"opencode", "light", []string{"free"}, "low"},
	{"opencode", "mid", opencodeChain, "high"},
	{"opencode", "strong", opencodeChain, "high"},
}

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
		Leaf{Key: "review.reviewers.small", Type: Slots, Default: []any{chain("opencode", "agy")}, Choices: AgentKinds,
			Summary: "reviewer slots of a small PR: each takes its first available kind not picked yet"},
		Leaf{Key: "review.reviewers.large", Type: Slots, Default: []any{chain("claude", "agy", "copilot", "opencode")},
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
		Leaf{Key: "quota_stop", Type: Percent, Default: 90.0,
			Summary: "percent of a vendor's quota used at which nothing new starts on it"},
		Leaf{Key: "idle_retire", Type: Seconds, Default: 300.0,
			Summary: "seconds an agent idle after its done or failed stays open before it is retired; 0 never retires"},
		Leaf{Key: "accountability.nudge_after", Type: Seconds, Default: 30.0,
			Summary: "seconds an agent idle after a task, with no report to its parent, waits before it is nudged once (and, if it ends another turn the same way, its parent told); 0 turns nudges off"},
		Leaf{Key: "accountability.wait_max", Type: Seconds, Default: 14400.0,
			Summary: "seconds a declared wait (a `wait` message to the parent) holds off the nudge, at most"},
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
		Leaf{Key: "caps.agents", Type: Int, Default: 8.0,
			Summary: "live agent processes this host runs at once (agents still starting count); 0 means no cap"},
	)
	return append(ls, AppLeaves()...)
}

// NotifyEvents are the events the desktop app can notify about (app.notify.events).
var NotifyEvents = []string{"input", "plan", "question", "report", "failed", "exited"}

// DefaultNotifyEvents are the ones that notify until the user chooses (every event but a clean exit). The desktop
// app falls back to the same list before the host's settings arrive (desktop/src/prefs.ts).
var DefaultNotifyEvents = []string{"input", "plan", "question", "report", "failed"}

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
			Summary: "what notifies: an agent stopped at a prompt (input), a plan or a question to answer, a report (done), a failure, or any agent's exit"},
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
