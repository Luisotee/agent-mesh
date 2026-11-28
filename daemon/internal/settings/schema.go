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
	"slices"
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
	// IntOrAuto is "auto" (the daemon works it out from the host) or a whole number, 0 or more.
	IntOrAuto Type = "int_or_auto"
	JSON      Type = "json"
	// Shortcut is a key chord such as "Mod+Shift+P", or "" for none (ParseShortcut).
	Shortcut Type = "shortcut"
	// ChatKey is a key the chat's composer acts on while it has focus: a named key alone ("Escape", "ArrowUp"), a
	// chord as for Shortcut, or "" for none (ValidChatKey).
	ChatKey Type = "chat_key"
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
	// Label is the name the settings screen gives it, in sentence case ("Retire idle agents after"); Group is the
	// screen section it is under (one of Groups). Advanced folds it away in its group until the user asks for it.
	Label    string
	Group    string
	Advanced bool
	// Choices fill a dropdown; for a kinds or slots leaf they are the kinds one may name.
	Choices []string
	// Restart marks a leaf the daemon reads only at start.
	Restart bool
}

// The groups of the settings screen, in the order it lists them.
const (
	GroupAppearance    = "Appearance"
	GroupNotifications = "Notifications"
	GroupShortcuts     = "Shortcuts"
	GroupAgents        = "Agents and models"
	GroupReviews       = "Reviews"
	GroupProjects      = "Projects and autonomy"
	GroupLimits        = "Limits and quota"
	GroupDelivery      = "Delivery"
)

// Groups lists every group a leaf may name.
var Groups = []string{GroupAppearance, GroupNotifications, GroupShortcuts, GroupAgents, GroupReviews, GroupProjects, GroupLimits, GroupDelivery}

// validLabel is a short name in sentence case: it starts with a capital letter (or a digit), has no trailing full
// stop and is not a sentence of its own.
func validLabel(label string) bool {
	if label == "" || len(label) > 60 || strings.HasSuffix(label, ".") || strings.TrimSpace(label) != label {
		return false
	}
	r, _ := utf8.DecodeRuneInString(label)
	return unicode.IsUpper(r) || unicode.IsDigit(r)
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
		if !validLabel(l.Label) {
			return nil, fmt.Errorf("setting %s: label %q must be a short name in sentence case with no full stop", l.Key, l.Label)
		}
		if !slices.Contains(Groups, l.Group) {
			return nil, fmt.Errorf("setting %s: group %q is not one of %s", l.Key, l.Group, strings.Join(Groups, ", "))
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
	case IntOrAuto:
		return `"auto" or a whole number, 0 or more`
	case JSON:
		return "any JSON value"
	case ChatKey:
		return `a named key alone (` + strings.Join(namedKeys(), ", ") + `), a key chord such as Mod+Shift+P, or "" for none`
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
		if !text(v) || len(l.Choices) > 0 && !oneOf(v, l.Choices) { // a leaf with choices takes one of them
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
	case IntOrAuto:
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
	case ChatKey:
		if s, ok := v.(string); !ok || (s != "" && !ValidChatKey(s)) {
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

// tierKindNames are the CLIs' names as a label spells them.
var tierKindNames = map[string]string{"claude": "Claude", "agy": "Agy", "copilot": "Copilot", "opencode": "OpenCode"}

// DefaultLeaves is the host's table. An app's own rows (`app.*`) are appended here.
func DefaultLeaves() []Leaf {
	var ls []Leaf
	for _, t := range tierDefaults {
		p := "tiers." + t.kind + "." + t.tier + "."
		who := t.kind + " " + t.tier + " tier: "
		name := tierKindNames[t.kind] + ", " + t.tier + " tier: "
		ls = append(ls,
			Leaf{Key: p + "model", Label: name + "models", Group: GroupAgents, Type: Chain, Default: chain(t.models...),
				Summary: who + "the models to try in order (the next when one is at its quota)"},
			Leaf{Key: p + "effort", Label: name + "effort", Group: GroupAgents, Type: Effort, Default: t.effort, Choices: Efforts, Summary: who + "reasoning effort"},
			Leaf{Key: p + "note", Label: name + "when to pick it", Group: GroupAgents, Advanced: true, Type: String, Default: tierNotes[t.tier], Summary: who + "when the root should pick it"})
	}
	reviewLevel := map[string]string{"trivial": "medium", "small": "medium", "large": "high"}
	selfLevel := map[string]string{"trivial": "low", "small": "low", "large": "medium"}
	ls = append(ls, Leaf{Key: "review.effort", Label: "Reviewer effort", Group: GroupReviews, Type: Effort, Default: "high", Choices: Efforts,
		Summary: "effort of a fresh reviewer, whatever its tier, unless the review names another"})
	for _, sz := range sizes {
		ls = append(ls,
			Leaf{Key: "review.level." + sz, Label: "Review level, " + sz + " PR", Group: GroupReviews, Advanced: true, Type: Effort, Default: reviewLevel[sz], Choices: Efforts,
				Summary: "the /code-review level of an outside reviewer on a " + sz + " PR"},
			Leaf{Key: "review.self_level." + sz, Label: "Self-review level, " + sz + " PR", Group: GroupReviews, Advanced: true, Type: Effort, Default: selfLevel[sz], Choices: Efforts,
				Summary: "the /code-review level of a worker's self-review of a " + sz + " PR, before its done"})
	}
	ls = append(ls,
		Leaf{Key: "review.reviewers.small", Label: "Reviewers of a small PR", Group: GroupReviews, Type: Slots, Default: []any{chain("opencode", "agy", "claude")}, Choices: AgentKinds,
			Summary: "reviewer slots of a small PR: each takes its first available kind not picked yet"},
		Leaf{Key: "review.reviewers.large", Label: "Reviewers of a large PR", Group: GroupReviews, Type: Slots, Default: []any{chain("agy", "claude", "opencode")},
			Choices: AgentKinds,
			Summary: "reviewer slots of a large PR: each takes its first available kind not picked yet"},
		Leaf{Key: "review.re_review.skip", Label: "Reviewers not asked again after a fix", Group: GroupReviews, Type: Kinds, Default: chain("copilot"), Choices: AgentKinds,
			Summary: "kinds that review a PR once and are not asked again after a fix"},
		Leaf{Key: "review.re_review.keep.small", Label: "Asked again after a fix, small PR", Group: GroupReviews, Advanced: true, Type: Kinds, Default: []any{}, Choices: AgentKinds,
			Summary: "kinds asked again after a fix on a small PR even when skipped"},
		Leaf{Key: "review.re_review.keep.large", Label: "Asked again after a fix, large PR", Group: GroupReviews, Advanced: true, Type: Kinds, Default: []any{}, Choices: AgentKinds,
			Summary: "kinds asked again after a fix on a large PR even when skipped"},
		Leaf{Key: "review.by", Label: "Who runs the review rounds", Group: GroupReviews, Type: String, Default: "worker", Choices: []string{"worker", "root"},
			Summary: "who runs the review rounds of a worker's PR: the worker itself (once its self-review is done), or its parent, the root (the user's override); a risky PR's worker runs a relay itself: `mesh spawn --reviews relay` and `mesh approve --reviews relay` (also `worker`, `root`) set it per worker"},
		Leaf{Key: "review.tier.small", Label: "Reviewer tier, small PR", Group: GroupReviews, Type: String, Default: "mid", Choices: []string{"light", "mid", "strong"},
			Summary: "the tier the reviewers of a small PR run on when the round names none (a worker's own round)"},
		Leaf{Key: "review.tier.large", Label: "Reviewer tier, large PR", Group: GroupReviews, Type: String, Default: "strong", Choices: []string{"light", "mid", "strong"},
			Summary: "the tier the reviewers of a large PR run on when the round names none (a worker's own round)"},
		Leaf{Key: "review.wait_room", Label: "Wait for room to review", Group: GroupReviews, Advanced: true, Type: Seconds, Default: 300.0,
			Summary: "seconds a review round waits, its `mesh ask` staying open, for room to start its first reviewer (the agent cap `caps.agents` or the memory floor) before it is refused as before; a quota is not waited for; 0 refuses at once"},
		Leaf{Key: "review.rounds.small", Label: "Review rounds, small PR", Group: GroupReviews, Type: Int, Default: 1.0,
			Summary: "review rounds a small PR gets at most (a ceiling, not a quota)"},
		Leaf{Key: "review.rounds.large", Label: "Review rounds, large PR", Group: GroupReviews, Type: Int, Default: 2.0,
			Summary: "review rounds a large PR gets at most (a ceiling, not a quota)"},
		Leaf{Key: "ask.timeout", Label: "Wait for review answers", Group: GroupReviews, Advanced: true, Type: Seconds, Default: 1800.0,
			Summary: "seconds a round of reviewers (or another collected ask) waits for its members: the ones still silent are then settled as not answered, and you get what came in"},
		Leaf{Key: "peers.dir", Label: "Peer projects folder", Group: GroupProjects, Advanced: true, Type: String, Default: "~/research",
			Summary: "where `mesh add-peer <url>` clones a project: <dir>/<group>/<repo> (a leading ~ is the daemon's home)"},
		Leaf{Key: "repos.dir", Label: "Repo folders", Group: GroupProjects, Type: Strings, Default: []any{},
			Summary: "folders a bare `--repo <name>` is looked up in, in order: the name must be a repo's own top folder there (a leading ~ is the daemon's home); a repo in the caller's working directory needs no setting"},
		Leaf{Key: "repos.user_merges", Label: "Repos where I merge", Group: GroupProjects, Type: Strings, Default: []any{},
			Summary: "repos whose pull requests the user merges (the \"I merge\" level): the root never merges one and tells the user when it is ready; a repo in no list is handled as before (a path, or a bare name of a repo's folder, as `repos.dir` takes)"},
		Leaf{Key: "repos.user_approves_plans", Label: "Repos where I approve plans", Group: GroupProjects, Type: Strings, Default: []any{},
			Summary: "repos whose plans the user approves, and whose pull requests the user merges (the \"I approve plans and merge\" level): the root approves none of their plans, asks the user, and merges none of their PRs; a repo in no list is handled as before"},
		Leaf{Key: "autonomy.ask_before", Label: "Ask me before", Group: GroupProjects, Type: Strings, Default: []any{},
			Summary: "things the root asks the user before doing, in every run and at every level (for example `migrations`, `deploys`, `pushes to the default branch`, `deletes outside a worktree`, `a run costing over $20`): each is a line of the root's brief"},
		Leaf{Key: "repos.no_free_models", Label: "Repos kept from free models", Group: GroupProjects, Type: Strings, Default: []any{},
			Summary: "top folders of repos whose code must not go to OpenCode's free models (some of their providers keep prompts or train on them): nothing there is started on, switched to or picked as a reviewer on a zero-cost model; a zero-cost model is one OpenCode itself lists at no cost, and one it lists no cost for is refused too; a paid OpenCode model is not held back (absolute folders; a leading ~ is the daemon's home: a relative entry matches nothing). Agents already running keep their model; one stopped is not resumed onto a free model, and a root names its model"},
		Leaf{Key: "tier_lean", Label: "How the root leans", Group: GroupAgents, Type: String, Default: "balanced", Choices: []string{"quality", "balanced", "thrifty"},
			Summary: "how the root leans when a task could go either way (guidance in `mesh brief` and ORCHESTRATE \"Tiers\", not enforced): quality takes the stronger tier or the higher effort, thrifty the cheaper one and raises it when the work shows it needs more, balanced has no lean: the root judges each task"},
		Leaf{Key: "quota_stop", Label: "Stop starting agents at 5h quota", Group: GroupLimits, Type: Percent, Default: 90.0,
			Summary: "percent of a vendor's short quota window (Claude's five-hour one, an agy five-hour row) used at which nothing new starts on it"},
		Leaf{Key: "quota_stop_weekly", Label: "Stop starting agents at weekly quota", Group: GroupLimits, Type: Percent, Default: 90.0,
			Summary: "percent of a vendor's weekly quota window (Claude's seven-day ones, an agy weekly row) used at which nothing new starts on it"},
		Leaf{Key: "quota_reviews_only.claude", Label: "Only PR reviews on Claude at", Group: GroupLimits, Type: Percent, Default: 100.0,
			Summary: "percent of a Claude window (any, the model's own) used at which Claude starts only reviewer-role agents (`mesh ask --review` members) and no task, resume of another role or other ask; between this and the stop reserve it is kept for PR reviews; 100 is off"},
		Leaf{Key: "quota_reviews_only.agy", Label: "Only PR reviews on agy at", Group: GroupLimits, Type: Percent, Default: 100.0,
			Summary: "percent of an agy model's quota (any row, the model's own) used at which it starts only reviewer-role agents (`mesh ask --review` members) and no task, resume of another role or other ask; between this and the stop reserve it is kept for PR reviews; 100 is off"},
		Leaf{Key: "idle_retire", Label: "Retire idle agents after", Group: GroupAgents, Type: Seconds, Default: 300.0,
			Summary: "seconds an agent idle after its done or failed stays open before it is retired; 0 never retires"},
		Leaf{Key: "idle_retire.review", Label: "Retire idle reviewers and askers after", Group: GroupAgents, Advanced: true, Type: Seconds, Default: 15.0,
			Summary: "seconds a reviewer or asker idle after its done or failed stays open before it is retired (the shorter of this and `idle_retire`; 0 uses `idle_retire`; `idle_retire` 0 turns every idle retire off, this too): nothing is waiting on it, and a re-review or a follow-up ask resumes the same conversation"},
		Leaf{Key: "accountability.nudge_after", Label: "Nudge a silent agent after", Group: GroupAgents, Advanced: true, Type: Seconds, Default: 30.0,
			Summary: "seconds an agent idle after a task, with no report to its parent, waits before it is nudged once (and, if it ends another turn the same way, its parent told); 0 turns nudges off"},
		Leaf{Key: "accountability.wait_max", Label: "Longest declared wait", Group: GroupAgents, Advanced: true, Type: Seconds, Default: 14400.0,
			Summary: "seconds a declared wait (a `wait` message to the parent) holds off the nudge, at most"},
		Leaf{Key: "prs.watch_every", Label: "Look at open PRs every", Group: GroupReviews, Advanced: true, Type: Seconds, Default: 300.0,
			Summary: "seconds between the daemon's looks at the open PRs of the change workers for CI failures and new review comments (`prs.feedback`); 0 stops the watch"},
		Leaf{Key: "prs.feedback", Label: "When a PR gets feedback", Group: GroupReviews, Type: String, Default: "notify", Choices: []string{"off", "notify", "resume"},
			Summary: "what a failing CI or a new review comment on a worker's PR does: off tells no one; notify tells the worker (an info; a retired worker's waits in its inbox) and, if it is retired, its parent in one line; resume also reopens a retired worker with the comments as a task (at most 3 times per PR, and only past the usual resume guards: it spends quota unattended)"},
		Leaf{Key: "accountability.wait_unseen", Label: "Wait nothing can wake", Group: GroupAgents, Advanced: true, Type: Seconds, Default: 900.0,
			Summary: "seconds a declared wait with no deadline of its own (`--until`) holds off the nudge when nothing the daemon can see would wake the agent (a Claude agent's `run_in_background` command or Monitor sends a notification when it ends, and is seen; a job started with `nohup` is not, nor anything of another CLI's agent, nor what a Claude agent waits on that is no such command: a background subagent, an asker it spawned with `mesh spawn --role asker`: give such a wait a deadline with `--until`): then the agent is nudged once, to report if its job ended or to declare a new wait with a deadline; at most `accountability.wait_max`; 0 holds every wait for `accountability.wait_max`"},
		Leaf{Key: "restart.resume", Label: "Resume agents lost in a restart", Group: GroupAgents, Type: Bool, Default: true,
			Summary: "after a machine restart (the terminal host dies with it) meshd resumes the agents that were live, each in its own conversation: the root first, then workers with no report since their last task, then the members of open asks, a few seconds apart and within the agent cap, the memory floor and the quota reserve (what is refused is tried again for 10 minutes, then its parent is told); each is told what died with the machine. A worker that had reported stays stopped; false resumes nothing by itself"},
		Leaf{Key: "opencode.stall_after", Label: "OpenCode stall alert after", Group: GroupAgents, Advanced: true, Type: Seconds, Default: 900.0,
			Summary: "seconds an OpenCode agent's open reply may go with no new part or token (a stalled stream, or one long tool call) before its parent is told once; 0 never tells"},
		Leaf{Key: "accountability.input_after", Label: "Tell the parent about a prompt after", Group: GroupAgents, Advanced: true, Type: Seconds, Default: 120.0,
			Summary: "seconds an agent waits at a permission prompt or question with no answer before its parent is told (once per prompt); 0 never tells"},
		Leaf{Key: "permission.wait", Label: "Hold permission prompts for the root", Group: GroupAgents, Advanced: true, Type: Seconds, Default: 300.0,
			Summary: "seconds a Claude agent's permission prompt is held for its parent to answer (`mesh permission`), at most 540; unanswered, the prompt stays on the agent's screen for the person there; 0 never holds it"},
		Leaf{Key: "delivery.socket", Label: "Use the inbox socket for doorbells", Group: GroupDelivery, Advanced: true, Type: Bool, Default: true,
			Summary: "write a Claude agent's doorbell into its inbox socket (never into its input box) where it takes cross-session messages; false types every doorbell"},
		Leaf{Key: "delivery.confirm_after", Label: "Ring again after", Group: GroupDelivery, Advanced: true, Type: Seconds, Default: 10.0,
			Summary: "seconds a doorbell may go unconfirmed (no sign of life from the agent, its messages unread, its ref not above the input box) before it is rung again"},
		Leaf{Key: "delivery.ring_tries", Label: "Doorbell tries", Group: GroupDelivery, Advanced: true, Type: Int, Default: 4.0,
			Summary: "doorbells (written to an inbox socket or typed) rung for one set of messages before giving up with a DoorbellFailed event; nothing more is typed until the agent's activity moves"},
		Leaf{Key: "delivery.draft_hold_alert", Label: "Alert on a held draft after", Group: GroupDelivery, Advanced: true, Type: Seconds, Default: 600.0,
			Summary: "seconds a doorbell waits for a human's draft in the agent's input box before a DoorbellHeld event says so (it is never typed into the draft); 0 never says"},
		Leaf{Key: "worktree.include_max_mb", Label: "Most copied into a new worktree", Group: GroupProjects, Advanced: true, Type: Int, Default: 50.0,
			Summary: "the most, in megabytes, a new worktree gets copied in from the repo's .worktreeinclude list (git-ignored files such as a .env); 0 copies nothing"},
		Leaf{Key: "editor.command", Label: "Editor command", Group: GroupProjects, Type: String, Default: "code",
			Summary: "the program that opens a path in an editor on this host (the app's open-in-editor button): run without a shell, with the path as its one argument, found on the login shell's PATH"},
		Leaf{Key: "compact.claude", Label: "Compact a Claude conversation at", Group: GroupLimits, Advanced: true, Type: Int, Default: 300000.0,
			Summary: "tokens a stopped Claude conversation has at resume above which it is compacted first (a read-only task's threshold is double, except a peer's); 0 turns it off"},
		Leaf{Key: "compact.claude_cold", Label: "Compact a cold Claude conversation at", Group: GroupLimits, Advanced: true, Type: Int, Default: 200000.0,
			Summary: "the same threshold once the conversation's prompt cache has expired (its next turn re-reads all of it at the cache-write price); the lower of the two counts; 0 turns it off"},
		Leaf{Key: "compact.agy", Label: "Compact an agy conversation at", Group: GroupLimits, Advanced: true, Type: Int, Default: 100000.0,
			Summary: "tokens an agy conversation has at resume above which it is compacted first; 0 turns it off"},
		Leaf{Key: "compact.approve", Label: "Compact a plan when approved", Group: GroupLimits, Advanced: true, Type: Int, Default: 250000.0,
			Summary: "tokens a planner's conversation has at approve above which it is compacted, keeping the approved plan, before it carries the plan out on the same model; 0 turns it off"},
		Leaf{Key: "compact.approve_switch", Label: "Compact a plan approved onto another model", Group: GroupLimits, Advanced: true, Type: Int, Default: 200000.0,
			Summary: "the same threshold when approve moves it to another model (the new model's first call re-reads all of it, uncached); 0 turns it off"},
		Leaf{Key: "compact.followup", Label: "Compact before a follow-up", Group: GroupLimits, Advanced: true, Type: Int, Default: 250000.0,
			Summary: "tokens a finished change worker has above which it is compacted before its next follow-up round (a warm cache); 0 turns it off"},
		Leaf{Key: "compact.followup_cold", Label: "Compact before a cold follow-up", Group: GroupLimits, Advanced: true, Type: Int, Default: 200000.0,
			Summary: "the same threshold once its prompt cache has expired; the lower of the two counts; 0 turns it off"},
		Leaf{Key: "root_reset.at", Label: "Reset a big root at", Group: GroupLimits, Advanced: true, Type: Int, Default: 150000.0,
			Summary: "tokens a root's conversation has above which its commands say to run `mesh reset` once it is cold, and the keeper resets it when idle"},
		Leaf{Key: "root_reset.auto", Label: "Reset an idle root by itself", Group: GroupLimits, Advanced: true, Type: Bool, Default: true,
			Summary: "reset an idle, big root by itself (quiet 5 minutes after a mesh turn, 30 after the user's); false only hints"},
		Leaf{Key: "root_reset.wait", Label: "Wait for a quiet prompt", Group: GroupLimits, Advanced: true, Type: Seconds, Default: 1800.0,
			Summary: "seconds a reset waits for the root to sit at a quiet prompt, nobody typing, before it gives up"},
		Leaf{Key: "root_reset.relaunch", Label: "Reset by relaunching", Group: GroupLimits, Advanced: true, Type: Bool, Default: true,
			Summary: "reset a root by relaunching it with a fresh lean session; false types /clear into the running one (or /compact with --compact)"},
		Leaf{Key: "claude.lean", Label: "Lean Claude agents", Group: GroupAgents, Advanced: true, Type: Bool, Default: true,
			Summary: "launch Claude agents without the tools, skills, plugins and MCP servers a mesh doesn't use (about 39k tokens off every call); false loads the user's full set"},
		Leaf{Key: "claude.deny_tools", Label: "Tools a Claude agent starts without", Group: GroupAgents, Advanced: true, Type: Strings, Default: chain(leanDeny...),
			Summary: "tools a lean Claude agent starts without (a bare name drops the tool's schema from every request)"},
		Leaf{Key: "claude.root_deny_tools", Label: "Tools a Claude root starts without", Group: GroupAgents, Advanced: true, Type: Strings, Default: chain(rootLeanDeny...),
			Summary: "tools a lean Claude root starts without (it keeps AskUserQuestion)"},
		Leaf{Key: "claude.skills_hidden", Label: "Skills hidden from Claude agents", Group: GroupAgents, Advanced: true, Type: Strings, Default: chain(leanSkills...),
			Summary: "bundled skills hidden from a lean Claude agent's listing (the user can still type them)"},
		Leaf{Key: "claude.worker_skills_hidden", Label: "Skills hidden from Claude workers", Group: GroupAgents, Advanced: true, Type: Strings, Default: chain(workerLeanSkills...),
			Summary: "skills also hidden from a lean Claude agent that isn't a root: only a root starts a mesh"},
		Leaf{Key: "claude.claudeai_skills", Label: "Keep claude.ai skills", Group: GroupAgents, Advanced: true, Type: Bool, Default: false,
			Summary: "keep the skills synced from claude.ai (`anthropic-skills:*`) in a lean Claude agent"},
		Leaf{Key: "claude.plugins_off", Label: "Plugins turned off", Group: GroupAgents, Advanced: true, Type: Strings, Default: chain(leanPlugins...),
			Summary: "plugins (by name, any marketplace) a lean Claude agent turns off when the user's settings enable them"},
		Leaf{Key: "claude.mcp", Label: "MCP servers a Claude agent loads", Group: GroupAgents, Advanced: true, Type: String, Default: "repo", Choices: []string{"repo", "none", "inherit"},
			Summary: "MCP servers a lean Claude agent loads besides claude.mcp_keep and the spawn's `mcp`: `repo` = the repo's own (its project entry in ~/.claude.json and its approved .mcp.json), `none`, or `inherit` = whatever the user's config loads"},
		Leaf{Key: "claude.mcp_keep", Label: "MCP servers always loaded", Group: GroupAgents, Advanced: true, Type: Strings, Default: chain("context7"),
			Summary: "the user's MCP servers every lean Claude agent loads, with web search (never denied): docs for any library"},
		Leaf{Key: "claude.mcp_exclusive", Label: "MCP servers one agent at a time may load", Group: GroupAgents, Advanced: true, Type: Strings, Default: chain("playwright"),
			Summary: "MCP servers at most one live agent at a time may load (for example Playwright's shared user browser)"},
		Leaf{Key: "claude.mcp_about", Label: "MCP server descriptions", Group: GroupAgents, Advanced: true, Type: JSON, Default: map[string]any{},
			Summary: "one-line descriptions shown by `mesh tools servers` for named MCP servers"},
		Leaf{Key: "skip_permissions", Label: "Skip permission prompts", Group: GroupAgents, Advanced: true, Type: Bool, Default: true,
			Summary: "launch agents with their CLI's permission prompts off"},
		Leaf{Key: "min_free_mb", Label: "Memory floor to start an agent", Group: GroupLimits, Type: MBOrAuto, Default: "auto",
			Summary: `MB of memory plus swap under which no agent starts; "auto" is 10% of RAM plus 10% of swap plus 1 GB (at least 2 GB), 0 turns the guard off`},
		Leaf{Key: "memory_pressure_max", Label: "Memory pressure limit", Group: GroupLimits, Advanced: true, Type: Percent, Default: 10.0,
			Summary: "no agent starts while the memory pressure (full avg60) is over this percent; 0 turns the guard off"},
		Leaf{Key: "worker_cpus", Label: "CPU share per agent", Group: GroupLimits, Advanced: true, Type: Percent, Default: 25.0,
			Summary: "percent of the machine's CPU cores each launched agent may use (at least one); 0 or 100 turns the cap off"},
		Leaf{Key: "worker_memory_high", Label: "Memory throttle per agent", Group: GroupLimits, Advanced: true, Type: Int, Default: 0.0,
			Summary: "MB above which each launched agent is throttled in a transient systemd user scope (MemoryHigh); 0 turns the scope off"},
		Leaf{Key: "limits.auto", Label: "At a usage limit", Group: GroupLimits, Type: String, Default: "switch", Choices: []string{"off", "continue", "switch"},
			Summary: "what the daemon does for an agent stopped at a usage limit: switch moves it to the next model of its chain " +
				"that has quota left when the limit is its model's, and (with continue) reopens it on the same model once the " +
				"limit is over; off leaves it to the root or the user"},
		Leaf{Key: "limits.hold", Label: "Hold a limited model for", Group: GroupLimits, Advanced: true, Type: Seconds, Default: 1800.0,
			Summary: "seconds a model counts as at its limit when the limit named no reset time"},
		Leaf{Key: "inbox.summary_over", Label: "Summarise long inbox items over", Group: GroupDelivery, Advanced: true, Type: Int, Default: 2000.0,
			Summary: "characters above which a root's `mesh inbox` shows a plan or a `done` as a summary (its key parts, its file, and " +
				"`mesh inbox --id <id>` for the whole text) instead of the whole body; 0 always prints whole bodies"},
		Leaf{Key: "caps.agents", Label: "Agents at once", Group: GroupLimits, Type: IntOrAuto, Default: "auto",
			Summary: `live agent processes this host runs at once (agents still starting count); "auto" sizes it from this host's memory and ` +
				"cores and what an agent measured holds (`mesh brief` prints the number); 0 means no cap"},
		Leaf{Key: "caps.working", Label: "Workers working at once", Group: GroupLimits, Type: IntOrAuto, Default: "auto",
			Summary: `workers the root keeps working at once; "auto" sizes it from this host's cores and what a working agent measured uses ` +
				"(advice to the root: the daemon refuses nothing over it); 0 means no limit"},
		Leaf{Key: "push.allow_private_endpoints", Label: "Allow private push endpoints", Group: GroupNotifications, Advanced: true, Type: Bool, Default: false,
			Summary: "let a device's push endpoint be on a loopback, private or link-local address (a ntfy on this host or the LAN); off, meshd pushes only to public addresses, so a paired device can't make it POST into this network; only the host's own device may change it"},
	)
	return append(ls, AppLeaves()...)
}

// NotifyEvents are the events the desktop app can notify about (app.notify.events).
var NotifyEvents = []string{"input", "plan", "question", "report", "failed", "exited", "limited", "error", "finished", "ready"}

// DefaultNotifyEvents are the ones that notify until the user chooses (every event but a clean exit and a usage
// limit, which the daemon usually handles itself by moving the agent to the next model). The desktop
// app falls back to the same list before the host's settings arrive (desktop/src/prefs.ts).
var DefaultNotifyEvents = []string{"input", "plan", "question", "report", "failed", "error", "finished", "ready"}

// shortcutModifiers are the modifiers a chord may hold; Mod is Ctrl on Linux and Windows and Cmd on macOS.
var shortcutModifiers = []string{"Mod", "Ctrl", "Alt", "Shift", "Meta"}

var shortcutNamedKeys = map[string]bool{
	"Enter": true, "Tab": true, "Space": true, "Escape": true, "Backspace": true, "Delete": true, "Insert": true,
	"Home": true, "End": true, "PageUp": true, "PageDown": true, "ArrowUp": true, "ArrowDown": true,
	"ArrowLeft": true, "ArrowRight": true, "Plus": true,
}

// ValidChatKey reports whether s is a key the chat's composer may act on: one of bareChatKeys alone (it fires only
// while the composer has focus, where a bare Escape or ArrowUp is what Claude Code uses), or a chord ValidShortcut
// accepts.
func ValidChatKey(s string) bool {
	return bareChatKeys[s] || ValidShortcut(s)
}

// bareChatKeys are the keys a chat key may be alone: none of them types or edits text, or sends (Enter).
var bareChatKeys = map[string]bool{"Escape": true, "ArrowUp": true, "ArrowDown": true, "PageUp": true, "PageDown": true}

func namedKeys() []string {
	out := make([]string, 0, len(bareChatKeys))
	for k := range bareChatKeys {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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

// AppLeaves are the apps' own preferences. They live on the host so every device and every agent sees and can
// change them (docs/apps.md, "Settings are data on the host"). The daemon reads only app.notify.enabled and
// app.notify.events, to choose what it pushes to phones (internal/daemon/push.go).
func AppLeaves() []Leaf {
	events := make([]any, len(DefaultNotifyEvents))
	for i, e := range DefaultNotifyEvents {
		events[i] = e
	}
	return []Leaf{
		{Key: "app.notify.enabled", Label: "Notifications", Group: GroupNotifications, Type: Bool, Default: true,
			Summary: "Tell me what happens on this host, on the desktop and on paired phones."},
		{Key: "app.notify.sound", Label: "Notification sound", Group: GroupNotifications, Type: Bool, Default: true,
			Summary: "play the system sound with a desktop notification"},
		{Key: "app.notify.events", Label: "Notify me about", Group: GroupNotifications, Type: Kinds, Default: events, Choices: NotifyEvents,
			Summary: "Which events notify: an agent stopped at a prompt, a plan or question that is yours to answer, a failure, an agent in error or at its usage limit, a finished run or agent, a pull request ready to merge (open, checks passing) where you merge, a report or a clean exit. A worker's plan, report or failure to a lead that decides it is only in Activity. A phone follows this unless it chose its own list."},
		{Key: "app.theme", Label: "Theme", Group: GroupAppearance, Type: String, Default: "Dracula",
			Summary: "The colours the apps draw with. An app without that theme uses Dracula, and the Android app's own choice overrides it on that phone."},
		{Key: "app.dock.new_agents", Label: "New agents in the live dock", Group: GroupAppearance, Type: String, Default: "dock", Choices: DockNewAgents,
			Summary: "What a worker that starts while the app is open does: show in the live dock marked new (dock), only count on the dock's toggle (badge), open as a background tab (tab), or nothing (none). It never takes focus."},
		{Key: "app.chat_keys.interrupt", Label: "Interrupt the agent", Group: GroupShortcuts, Type: ChatKey, Default: "Escape",
			Summary: "key in the chat's composer: interrupt the agent's turn while it works (Claude Code's Esc)"},
		{Key: "app.chat_keys.recall", Label: "Take back the waiting prompt", Group: GroupShortcuts, Type: ChatKey, Default: "ArrowUp",
			Summary: "key in the chat's composer, when it is empty: take back the newest prompt still waiting to be typed, to edit or drop it (Claude Code's Up arrow)"},
		{Key: "app.open_views", Label: "Let agents switch my view", Group: GroupAppearance, Type: Bool, Default: false,
			Summary: "Let an agent or the CLI switch the app to a view it asked for, never while you type. Off, the app shows a toast you open or dismiss. Only you can change this, an agent can't."},
		{Key: "app.keys.palette", Label: "Command palette", Group: GroupShortcuts, Type: Shortcut, Default: "Mod+Shift+P", Summary: "shortcut: open the command palette"},
		{Key: "app.keys.settings", Label: "Host settings", Group: GroupShortcuts, Type: Shortcut, Default: "Mod+,", Summary: "shortcut: open or close the host's settings"},
		{Key: "app.keys.chat_toggle", Label: "Switch between chat and terminal", Group: GroupShortcuts, Type: Shortcut, Default: "Mod+Shift+J", Summary: "shortcut: switch the open agent between its chat and its terminal"},
		{Key: "app.keys.dock", Label: "Live dock", Group: GroupShortcuts, Type: Shortcut, Default: "Mod+Shift+D", Summary: "shortcut: show or hide the live dock"},
		{Key: "app.keys.tab_next", Label: "Next tab", Group: GroupShortcuts, Type: Shortcut, Default: "Ctrl+Tab", Summary: "shortcut: the next open agent tab"},
		{Key: "app.keys.tab_prev", Label: "Previous tab", Group: GroupShortcuts, Type: Shortcut, Default: "Ctrl+Shift+Tab", Summary: "shortcut: the previous open agent tab"},
		{Key: "app.keys.tab_next_page", Label: "Next tab (second key)", Group: GroupShortcuts, Type: Shortcut, Default: "Ctrl+PageDown", Summary: "shortcut: the next open agent tab (a second key for it)"},
		{Key: "app.keys.tab_prev_page", Label: "Previous tab (second key)", Group: GroupShortcuts, Type: Shortcut, Default: "Ctrl+PageUp", Summary: "shortcut: the previous open agent tab (a second key for it)"},
		{Key: "app.keys.tab_close", Label: "Close the open tab", Group: GroupShortcuts, Type: Shortcut, Default: "Mod+W", Summary: "shortcut: close the open agent tab (in a focused terminal a Mod+letter key goes to the terminal, and the same key with Shift added runs the shortcut: Mod+Shift+W there)"},
		{Key: "app.keys.tab_new", Label: "New lead in this project", Group: GroupShortcuts, Type: Shortcut, Default: "Mod+T", Summary: "shortcut: start a new lead agent in the open agent's project"},
		{Key: "app.keys.tab_reopen", Label: "Open the last closed tab again", Group: GroupShortcuts, Type: Shortcut, Default: "Mod+Shift+T", Summary: "shortcut: open again the last agent tab closed in this session"},
		{Key: "app.keys.tab_1", Label: "Go to the first tab", Group: GroupShortcuts, Type: Shortcut, Default: "Mod+1", Summary: "shortcut: the first open agent tab"},
		{Key: "app.keys.tab_2", Label: "Go to the second tab", Group: GroupShortcuts, Type: Shortcut, Default: "Mod+2", Summary: "shortcut: the second open agent tab"},
		{Key: "app.keys.tab_3", Label: "Go to the third tab", Group: GroupShortcuts, Type: Shortcut, Default: "Mod+3", Summary: "shortcut: the third open agent tab"},
		{Key: "app.keys.tab_4", Label: "Go to the fourth tab", Group: GroupShortcuts, Type: Shortcut, Default: "Mod+4", Summary: "shortcut: the fourth open agent tab"},
		{Key: "app.keys.tab_5", Label: "Go to the fifth tab", Group: GroupShortcuts, Type: Shortcut, Default: "Mod+5", Summary: "shortcut: the fifth open agent tab"},
		{Key: "app.keys.tab_6", Label: "Go to the sixth tab", Group: GroupShortcuts, Type: Shortcut, Default: "Mod+6", Summary: "shortcut: the sixth open agent tab"},
		{Key: "app.keys.tab_7", Label: "Go to the seventh tab", Group: GroupShortcuts, Type: Shortcut, Default: "Mod+7", Summary: "shortcut: the seventh open agent tab"},
		{Key: "app.keys.tab_8", Label: "Go to the eighth tab", Group: GroupShortcuts, Type: Shortcut, Default: "Mod+8", Summary: "shortcut: the eighth open agent tab"},
		{Key: "app.keys.tab_last", Label: "Last tab", Group: GroupShortcuts, Type: Shortcut, Default: "Mod+9", Summary: "shortcut: the last open agent tab"},
		{Key: "app.keys.waiting_next", Label: "Next item waiting on me", Group: GroupShortcuts, Type: Shortcut, Default: "Mod+Shift+A", Summary: "shortcut: go to the next plan, question or agent that waits on you"},
		{Key: "app.keys.search", Label: "Search", Group: GroupShortcuts, Type: Shortcut, Default: "Mod+K", Summary: "shortcut: search the agents, runs and commands (the sidebar's search field; / outside a text field does it too)"},
		{Key: "app.keys.sidebar", Label: "Fold the sidebar", Group: GroupShortcuts, Type: Shortcut, Default: "Mod+B", Summary: "shortcut: fold the sidebar to icons, or unfold it"},
		{Key: "app.keys.sidebar_next", Label: "Next agent in the sidebar", Group: GroupShortcuts, Type: Shortcut, Default: "Alt+ArrowDown", Summary: "shortcut: open the next agent in the sidebar"},
		{Key: "app.keys.sidebar_prev", Label: "Previous agent in the sidebar", Group: GroupShortcuts, Type: Shortcut, Default: "Alt+ArrowUp", Summary: "shortcut: open the previous agent in the sidebar"},
		{Key: "app.keys.panel", Label: "Side panel", Group: GroupShortcuts, Type: Shortcut, Default: "Ctrl+Alt+B", Summary: "shortcut: show or hide the side panel beside the open agent"},
		{Key: "app.keys.find", Label: "Find in the window", Group: GroupShortcuts, Type: Shortcut, Default: "Mod+F", Summary: "shortcut: find text in the window (a terminal keeps its own search)"},
		{Key: "app.keys.composer", Label: "Focus the message box", Group: GroupShortcuts, Type: Shortcut, Default: "Mod+L", Summary: "shortcut: put the cursor in the open agent's message box (in a focused terminal on Windows and Linux Ctrl+L is the shell's clear-screen: Ctrl+Shift+L there)"},
		{Key: "app.keys.rename", Label: "Rename the open run", Group: GroupShortcuts, Type: Shortcut, Default: "F2", Summary: "shortcut: rename the open agent's run"},
		{Key: "app.tray.close_to_tray", Label: "Keep running in the tray", Group: GroupAppearance, Type: Bool, Default: false,
			Summary: "Closing the desktop window keeps the app running in the tray. Off by default: a desktop with no tray would leave no way back to the window."},
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
