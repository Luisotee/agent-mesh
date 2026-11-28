package settings

import "strings"

// PlanKinds are the CLIs a host may describe the plan of (`plans.<kind>`), in the order the brief lists them.
var PlanKinds = []string{"claude", "agy", "opencode", "copilot"}

// planLeaves are the host facts the root reads every run in `mesh brief`: what each CLI's plan is (its size and
// limits, which only the user knows) and what each model is good and weak at. They are settings, not protocol, so
// the protocol stays process-agnostic and a host (later a user) states its own.
func planLeaves() []Leaf {
	var ls []Leaf
	for _, k := range PlanKinds {
		ls = append(ls, Leaf{Key: "plans." + k, Label: tierKindNames[k] + " plan", Group: GroupAgents, Type: String, Default: "",
			Summary: "what this host's " + k + " plan is, for the root to weigh when it routes work (for example \"Max 5x: the big pool\" or \"AI Pro: small, a second family, not volume\"); empty says nothing, and `mesh brief` prints it"})
	}
	return append(ls, Leaf{Key: "models.strengths", Label: "What each model is good at", Group: GroupAgents, Advanced: true, Type: JSON, Default: shippedStrengths(),
		Summary: "one line per model, by the name a tier chain uses (`sonnet`, `neon`, `opencode/deepseek-v4.1-flash-free`): what it is good and weak at, printed by `mesh brief` for the models this host's tiers name"})
}

// shippedStrengths are the report's lines (docs/model-routing-research.md, section 3) for the models the shipped tiers
// name. A host edits them as new models arrive: the lines are facts of the day, not protocol.
func shippedStrengths() map[string]any {
	m := map[string]any{
		"fable":                             "the hardest planning and must-be-right work; the most expensive per task: only when the user names it",
		"opus":                              "the best all-rounder: planning, review (best precision), frontend; overthinks at max",
		"sonnet":                            "within a point of Opus on agent tasks at half the price: the executor; good routine reviews, not the hardest; never medium",
		"haiku":                             "searches, summaries, mechanical edits; weak in agent loops and at codebase questions; low skips checks",
		"carbon":                            "agy's strongest and best at multi-step web apps; slow (about 36 minutes a task)",
		"neon":                              "best value per task and our best reviewer; a Claude model should review its work (it codes against retired APIs and rigs tests)",
		"hydrogen":                          "a fast, cheap executor between Haiku and Sonnet; weak at frontend; many tokens at xhigh",
		"argon":                             "overtaken by Hydrogen and Neon",
		"opencode/deepseek-v4.1-flash-free": "the strongest free model, 200K context, 32K output, never max effort; zero retention; good backend executor, reviewer and asker",
		"opencode/hy4-flash-free":           "fast, 1M context; weak in long tool loops; good in reviews",
		"opencode/seaglass-alpha-free":      "1M context, trains on prompts (acceptable here), no benchmarks; good in reviews",
		"opencode/mimo-v2.6-flash-free":     "a reasonable executor below DeepSeek",
		"opencode/beam-free":                "high vendor scores but 3.0 independent and weak in our reviews, so last resort",
		"opencode/flint-40b-free":           "last resort, little data",
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = strings.TrimSpace(v.(string))
	}
	return out
}
