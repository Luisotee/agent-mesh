package settings

// AskerKinds are the CLIs a fresh asker may run on, in the order `roles.asker.kinds` tries them by default.
var AskerKinds = []string{"opencode", "claude"}

// askerDefaults are the shipped models of the asker role (docs/model-routing-research.md, section 6.4): the free
// models first, at high effort, so a question costs no Claude quota and DeepSeek's cap stays for reviews; Claude's
// Sonnet is the fallback. A CLI the default kind list leaves out (agy, Copilot) still has a list, for a host that adds
// it to `roles.asker.kinds`.
var askerDefaults = []struct {
	kind   string
	models []string
	effort string
}{
	{"opencode", []string{"opencode/hy4-flash-free", "opencode/seaglass-alpha-free", "opencode/deepseek-v4.1-flash-free"}, "high"},
	{"claude", []string{"sonnet"}, "medium"},
	{"agy", []string{"hydrogen"}, "high"},
	{"copilot", []string{"auto"}, "medium"},
}

// roleLeaves are the settings of the asker role: which CLIs a fresh asker (a plain ask, or a peer's) runs on when the
// ask names no tier, kind or model, and the models and effort on each.
func roleLeaves() []Leaf {
	ls := []Leaf{{Key: "roles.asker.kinds", Label: "CLIs of a fresh asker", Group: GroupAgents, Type: Kinds, Default: chain(AskerKinds...), Choices: AgentKinds,
		Summary: "the CLIs a fresh asker (an `ask` or an `ask --peers` that names no tier, kind or model) tries in order: the first that is installed, allowed for the repo (`repos.no_free_models`) and has a model with quota"}}
	for _, r := range askerDefaults {
		p := "roles.asker." + r.kind + "."
		name := tierKindNames[r.kind] + ", asker: "
		ls = append(ls,
			Leaf{Key: p + "model", Label: name + "models", Group: GroupAgents, Type: Chain, Default: chain(r.models...),
				Summary: r.kind + " asker: the models to try in order (the next when one is at its quota)"},
			Leaf{Key: p + "effort", Label: name + "effort", Group: GroupAgents, Type: Effort, Default: r.effort, Choices: Efforts,
				Summary: r.kind + " asker: reasoning effort"})
	}
	return ls
}
