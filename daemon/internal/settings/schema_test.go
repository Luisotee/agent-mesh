package settings

import (
	"strings"
	"testing"
)

func TestTheDefaultTableIsValid(t *testing.T) {
	s := Default() // panics on a default that fails its own rule, a duplicate key or a missing summary
	for _, key := range []string{"tiers.claude.mid.model", "tiers.claude.top.effort", "tiers.agy.strong.model",
		"tiers.opencode.mid.note", "review.rounds.large", "review.reviewers.small", "quota_stop", "idle_retire",
		"skip_permissions", "min_free_mb", "memory_pressure_max", "worker_cpus", "worker_memory_high", "caps.agents"} {
		if _, ok := s.Lookup(key); !ok {
			t.Errorf("no setting %s", key)
		}
	}
	// Only Claude has a top tier.
	if got := s.Keys("tiers.agy.top"); len(got) != 0 {
		t.Errorf("agy top tier: %v", got)
	}
	if got := s.Keys("tiers.claude.mid."); len(got) != 3 {
		t.Errorf("a tier has model, effort and note: %v", got)
	}
	if l, _ := s.Lookup("caps.agents"); l.Type != IntOrAuto || l.Default != "auto" {
		t.Errorf("the agent cap = %+v, want int_or_auto defaulting to auto", l)
	}
	if l, _ := s.Lookup("caps.working"); l.Type != IntOrAuto || l.Default != "auto" {
		t.Errorf("the working cap = %+v, want int_or_auto defaulting to auto", l)
	}
	if l, _ := s.Lookup("worker_cpus"); l.Type != Percent || l.Default != 25.0 {
		t.Errorf("worker_cpus = %+v", l)
	}
	if l, _ := s.Lookup("worker_memory_high"); l.Type != Int || l.Default != 0.0 {
		t.Errorf("worker_memory_high = %+v", l)
	}
}

func TestTheAppRowsAreInTheTable(t *testing.T) {
	s := Default()
	got := s.Keys("app.")
	want := []string{"app.chat_keys.interrupt", "app.chat_keys.recall", "app.dock.new_agents", "app.keys.chat_toggle", "app.keys.dock", "app.keys.palette", "app.keys.settings", "app.keys.tab_close", "app.keys.tab_next", "app.keys.tab_prev", "app.keys.waiting_next", "app.notify.enabled", "app.notify.events", "app.notify.sound", "app.theme", "app.tray.close_to_tray"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("app rows %v, want %v", got, want)
	}
	if l, _ := s.Lookup("app.tray.close_to_tray"); l.Default != false {
		t.Errorf("close to tray defaults to %v: a desktop without a tray would strand the window", l.Default)
	}
	if l, _ := s.Lookup("app.theme"); l.Default != "Dracula" {
		t.Errorf("the theme defaults to %v, want Dracula", l.Default)
	}
	l, _ := s.Lookup("app.notify.events")
	if _, err := Check(l, []any{"plan", "failed"}); err != nil {
		t.Error(err)
	}
	if _, err := Check(l, []any{"limited", "error"}); err != nil {
		t.Error(err)
	}
	// An agent in error notifies out of the box; one at its usage limit (the daemon usually moves it on) doesn't.
	def := strings.Join(DefaultNotifyEvents, ",")
	if !strings.Contains(","+def+",", ",error,") || strings.Contains(","+def+",", ",limited,") {
		t.Errorf("default notify events %v", DefaultNotifyEvents)
	}
	if _, err := Check(l, []any{"claude"}); err == nil {
		t.Error("an event outside the choices was accepted")
	}
}

func TestACheckNamesItsRule(t *testing.T) {
	leaf := func(ty Type) Leaf { return Leaf{Key: "k", Type: ty} }
	list := func(xs ...any) []any { return xs }
	cases := []struct {
		ty   Type
		ok   []any
		bad  []any
		rule string
	}{
		{String, []any{"x"}, []any{"", 1.0, nil, list("x")}, "a non-empty string"},
		{Strings, []any{list(), list("a", "b")}, []any{"a", list(""), list(1.0), nil}, "a list of non-empty strings"},
		{Int, []any{0.0, 7.0}, []any{-1.0, 1.5, "3", true, nil}, "a whole number, 0 or more"},
		{Percent, []any{0.0, 100.0}, []any{101.0, -1.0, 0.5}, "a whole number 0 to 100"},
		{Bool, []any{true, false}, []any{"true", 1.0, nil}, "true or false"},
		{Seconds, []any{0.0, 2.5, 300.0, 1e9}, []any{-1.0, "5", nil, 1e300, 1e9 + 1}, "a finite number of seconds"},
		{Effort, []any{"low", "max"}, []any{"ultra", "", 1.0}, "one of low, medium, high, xhigh, max"},
		{Chain, []any{list("sonnet"), list("a", "b")}, []any{list(), "sonnet", list("a", 2.0)}, "a non-empty list of model names"},
		{Kinds, []any{list(), list("claude", "agy")}, []any{list("gpt"), "claude"}, "a list of claude, agy, copilot, opencode"},
		{Slots, []any{list(list("claude")), list()}, []any{list(list()), list("claude"), list(list("nope"))}, "a list of slots"},
		{MBOrAuto, []any{"auto", 0.0, 2048.0}, []any{"fast", -5.0, 1.5, true}, `"auto" or a whole number of MB`},
		{IntOrAuto, []any{"auto", 0.0, 16.0}, []any{"fast", -5.0, 1.5, true}, `"auto" or a whole number, 0 or more`},
		{JSON, []any{nil, 1.0, "x", list(1.0), map[string]any{"a": 1.0}}, nil, ""},
	}
	for _, c := range cases {
		for _, v := range c.ok {
			if _, err := Check(leaf(c.ty), v); err != nil {
				t.Errorf("%s: %v refused: %v", c.ty, v, err)
			}
		}
		for _, v := range c.bad {
			_, err := Check(leaf(c.ty), v)
			if err == nil || !strings.Contains(err.Error(), "k must be "+c.rule) {
				t.Errorf("%s: %v gave %v, want an error naming %q", c.ty, v, err, c.rule)
			}
		}
	}
}

func TestAKindsLeafTakesItsOwnChoices(t *testing.T) {
	l := Leaf{Key: "k", Type: Kinds, Choices: []string{"a", "b"}}
	if _, err := Check(l, []any{"a"}); err != nil {
		t.Error(err)
	}
	if _, err := Check(l, []any{"claude"}); err == nil || !strings.Contains(err.Error(), "a list of a, b") {
		t.Errorf("a kind outside the choices: %v", err)
	}
}

func TestAStringLeafWithChoicesTakesOnlyThem(t *testing.T) {
	l, _ := Default().Lookup("app.dock.new_agents")
	for _, v := range []string{"dock", "badge", "tab", "none"} {
		if _, err := Check(l, v); err != nil {
			t.Errorf("%q refused: %v", v, err)
		}
	}
	if _, err := Check(l, "popup"); err == nil || !strings.Contains(err.Error(), "one of dock, badge, tab, none") {
		t.Errorf("a string outside the choices: %v", err)
	}
	// The other string with choices keeps working, and is checked now too.
	mcp, _ := Default().Lookup("claude.mcp")
	if _, err := Check(mcp, "inherit"); err != nil {
		t.Error(err)
	}
	if _, err := Check(mcp, "everything"); err == nil {
		t.Error("claude.mcp took a value outside its choices")
	}
}

func TestAChatKeyIsANamedKeyAloneOrAChord(t *testing.T) {
	for _, ok := range []string{"Escape", "ArrowUp", "Mod+Shift+P", "F5", "Alt+ArrowUp"} {
		if !ValidChatKey(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	// A bare letter would fire while typing in the composer.
	for _, bad := range []string{"P", "Shift+P", "k", "Nope", "Mod+"} {
		if ValidChatKey(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	l := Leaf{Key: "k", Type: ChatKey}
	for _, v := range []any{"", "Escape"} {
		if _, err := Check(l, v); err != nil {
			t.Errorf("%q: %v", v, err)
		}
	}
	if _, err := Check(l, "a"); err == nil {
		t.Error("a bare letter accepted")
	}
}

func TestAShortcutIsAKeyChord(t *testing.T) {
	for _, ok := range []string{"Mod+Shift+P", "Mod+,", "Ctrl+Tab", "Ctrl+Shift+Tab", "Alt+ArrowLeft", "F5", "Shift+F12", "Mod+Plus", "Mod+1", "Mod+/", "Meta+k", "Ctrl+Alt+Delete", "Ctrl+Space"} {
		if !ValidShortcut(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"P", "Shift+P", "Mod+", "+P", "Mod+Mod+P", "Cmd+P", "mod+P", "Mod+Shift", "Mod+PP", "Mod++", "Mod+ ", "F25", "F0", "F01", "Mod+Shift+Nope", "Mod+Shift+P+Q", "Tab"} {
		if ValidShortcut(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	l := Leaf{Key: "k", Type: Shortcut}
	if _, err := Check(l, ""); err != nil {
		t.Errorf("an unbound shortcut: %v", err)
	}
	for _, v := range []any{"Shift+P", 1.0, nil, true, []any{"Mod+P"}} {
		if _, err := Check(l, v); err == nil || !strings.Contains(err.Error(), "k must be a key chord such as Mod+Shift+P") {
			t.Errorf("%v gave %v", v, err)
		}
	}
	// The defaults don't clash with each other.
	seen := map[string]string{}
	for _, k := range Default().Keys("app.keys.") {
		l, _ := Default().Lookup(k)
		if other, dup := seen[l.Default.(string)]; dup {
			t.Errorf("%s and %s share %s", k, other, l.Default)
		}
		seen[l.Default.(string)] = k
	}
}

func TestASchemaRefusesABadTable(t *testing.T) {
	ok := Leaf{Key: "a", Type: Bool, Default: true, Summary: "s"}
	for name, ls := range map[string][]Leaf{
		"duplicate":        {ok, ok},
		"no summary":       {{Key: "a", Type: Bool, Default: true}},
		"default fails":    {{Key: "a", Type: Percent, Default: 200.0, Summary: "s"}},
		"unknown type":     {{Key: "a", Type: "nope", Default: true, Summary: "s"}},
		"key with a space": {{Key: "a b", Type: Bool, Default: true, Summary: "s"}},
		"empty key":        {{Key: "", Type: Bool, Default: true, Summary: "s"}},
	} {
		if _, err := NewSchema(ls); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
