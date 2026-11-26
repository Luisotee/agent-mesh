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
	if l, _ := s.Lookup("caps.agents"); l.Default != 16.0 {
		t.Errorf("the agent cap defaults to %v, want 16", l.Default)
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
	want := []string{"app.notify.enabled", "app.notify.events", "app.notify.sound", "app.theme", "app.tray.close_to_tray"}
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
