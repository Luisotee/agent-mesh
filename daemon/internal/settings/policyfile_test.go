package settings

import (
	"reflect"
	"strings"
	"testing"
)

// userFile is the shape of the owner's real ~/.config/agent-mesh.json.
const userFile = `{
  "repos_dir": ["~/projects"],
  "compact": {"claude": 250000},
  "root_reset": {"relaunch": false},
  "review": {"reviewers": {"small": [["opencode","agy","claude"]], "large": [["agy","claude"]]}},
  "tiers": {"opencode": {"light": {"model": ["opencode/deepseek-v4.1-flash-free"]},
                         "mid": {"model": ["opencode/deepseek-v4.1-flash-free"]},
                         "strong": {"model": ["opencode/deepseek-v4.1-flash-free"]}}},
  "claude": {"mcp_keep": ["context7"]}
}`

func got(t *testing.T, text string) (map[string]any, map[string]string) {
	t.Helper()
	vals, skips, err := ParsePolicy(Default(), []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	v := map[string]any{}
	for _, pv := range vals {
		v[pv.Key] = pv.Value
	}
	k := map[string]string{}
	for _, s := range skips {
		k[s.Key] = s.Why
	}
	return v, k
}

func TestThePolicyFileOfTheOwnerImportsWholly(t *testing.T) {
	vals, skips := got(t, userFile)
	if len(skips) != 0 {
		t.Fatalf("skipped %v", skips)
	}
	want := map[string]any{
		"repos.dir":                   []any{"~/projects"},
		"compact.claude":              250000.0,
		"root_reset.relaunch":         false,
		"review.reviewers.small":      []any{[]any{"opencode", "agy", "claude"}},
		"review.reviewers.large":      []any{[]any{"agy", "claude"}},
		"claude.mcp_keep":             []any{"context7"},
		"tiers.opencode.light.model":  []any{"opencode/deepseek-v4.1-flash-free"},
		"tiers.opencode.mid.model":    []any{"opencode/deepseek-v4.1-flash-free"},
		"tiers.opencode.strong.model": []any{"opencode/deepseek-v4.1-flash-free"},
	}
	if !reflect.DeepEqual(vals, want) {
		t.Errorf("values %v\nwant %v", vals, want)
	}
}

func TestCommentsRenamesAndUnusedLeaves(t *testing.T) {
	vals, skips := got(t, `{"_note": "hi", "peers_dir": "~/x", "hang_after": 5, "held_wait": 1, "side_panes": 2,
		"watch": {"timeout": 3}, "models": {"claude": {"small": {"view": "pane"}}}, "quota_stop": 80}`)
	if vals["peers.dir"] != "~/x" || vals["quota_stop"] != 80.0 || len(vals) != 2 {
		t.Errorf("values %v", vals)
	}
	for _, k := range []string{"hang_after", "held_wait", "side_panes", "watch.timeout", "models"} {
		if !strings.Contains(skips[k], "not used by the daemon") {
			t.Errorf("%s: %q", k, skips[k])
		}
	}
	if _, ok := skips["_note"]; ok {
		t.Error("a comment is not a skip")
	}
}

func TestBadKeysAndValuesAreSkippedAndTheRestImports(t *testing.T) {
	vals, skips := got(t, `{"nope": 1, "compact": {"claude": "lots", "agy": 5}, "tiers": {"claude": {"mid": {"effort": "turbo"}}},
		"review": {"rounds": {"huge": 3}}}`)
	if len(vals) != 1 || vals["compact.agy"] != 5.0 {
		t.Errorf("values %v", vals)
	}
	for _, k := range []string{"nope", "compact.claude", "tiers.claude.mid.effort", "review.rounds.huge"} {
		if skips[k] == "" {
			t.Errorf("%s was not skipped: %v", k, skips)
		}
	}
	if !strings.Contains(skips["compact.claude"], "whole number") {
		t.Errorf("the rule is named: %q", skips["compact.claude"])
	}
}

func TestCopilotModelStandsInForTheTiersTheFileLeavesOut(t *testing.T) {
	vals, skips := got(t, `{"copilot_model": "gpt-5", "tiers": {"copilot": {"strong": {"model": ["claude-opus"]}}}}`)
	if len(skips) != 0 {
		t.Fatal(skips)
	}
	if !reflect.DeepEqual(vals["tiers.copilot.light.model"], []any{"gpt-5"}) ||
		!reflect.DeepEqual(vals["tiers.copilot.mid.model"], []any{"gpt-5"}) ||
		!reflect.DeepEqual(vals["tiers.copilot.strong.model"], []any{"claude-opus"}) {
		t.Errorf("values %v", vals)
	}
	if _, skips := got(t, `{"copilot_model": 3}`); skips["copilot_model"] == "" {
		t.Error("a non-string copilot_model is skipped")
	}
}

func TestAJSONLeafKeepsItsObjectAndADuplicateIsSkipped(t *testing.T) {
	vals, skips := got(t, `{"claude": {"mcp_about": {"x": "an x"}}, "repos_dir": ["/a"], "repos": {"dir": ["/b"]}}`)
	if !reflect.DeepEqual(vals["claude.mcp_about"], map[string]any{"x": "an x"}) {
		t.Errorf("values %v", vals)
	}
	if len(vals["repos.dir"].([]any)) != 1 || skips["repos_dir"] == "" && skips["repos.dir"] == "" {
		t.Errorf("the same leaf twice: %v %v", vals, skips)
	}
}

func TestNotAnObject(t *testing.T) {
	for _, text := range []string{``, `[]`, `{`, `{} {}`, `"x"`} {
		if _, _, err := ParsePolicy(Default(), []byte(text)); err == nil {
			t.Errorf("%q parsed", text)
		}
	}
}
