package daemon

import (
	"strings"
	"testing"
	"unicode/utf8"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

func TestTaskGistSkipsTheStandardPrefixes(t *testing.T) {
	cases := map[string]string{
		"# Add the retry\n\nmore": "Add the retry",
		"Direct task (no plan): research, make the change, verify it, open the PR and report `done`. If it turns out bigger or less clear than described here, send a `question` before changing anything.\n\nFix the flaky test\n\nbody": "Fix the flaky test",
		"Direct task: fix the flaky test":                               "fix the flaky test",
		"Question (read-only): answer it from the code.\n\nWhere is X?": "Where is X?",
		"": "",
	}
	for in, want := range cases {
		if got := taskGist(in); got != want {
			t.Errorf("taskGist(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWantsFollowsTheThresholdsAndTheCache(t *testing.T) {
	pc := &v1.PendingCompaction{WarmAt: 300, ColdAt: 200}
	cases := []struct {
		name string
		pc   *v1.PendingCompaction
		u    *v1.ContextUsage
		want bool
	}{
		{"warm under", pc, &v1.ContextUsage{Measured: true, Tokens: 250}, false},
		{"warm over", pc, &v1.ContextUsage{Measured: true, Tokens: 301}, true},
		{"warm at the threshold", pc, &v1.ContextUsage{Measured: true, Tokens: 300}, false},
		{"cold between", pc, &v1.ContextUsage{Measured: true, Tokens: 250, Cold: true}, true},
		{"cold under", pc, &v1.ContextUsage{Measured: true, Tokens: 150, Cold: true}, false},
		{"unmeasured, not asked", pc, &v1.ContextUsage{}, false},
		{"unmeasured, asked", &v1.PendingCompaction{Unmeasured: true}, &v1.ContextUsage{}, true},
		{"always", &v1.PendingCompaction{Always: true}, &v1.ContextUsage{Measured: true, Tokens: 1}, true},
		{"off", &v1.PendingCompaction{}, &v1.ContextUsage{Measured: true, Tokens: 1 << 30}, false},
		{"cold with no cold threshold uses the warm one", &v1.PendingCompaction{WarmAt: 300}, &v1.ContextUsage{Measured: true, Tokens: 250, Cold: true}, false},
	}
	for _, c := range cases {
		if got := wants(c.pc, c.u); got != c.want {
			t.Errorf("%s: wants = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestNonzeroMinTreatsZeroAsOff(t *testing.T) {
	if nonzeroMin(300, 200) != 200 || nonzeroMin(0, 200) != 200 || nonzeroMin(300, 0) != 300 || nonzeroMin(0, 0) != 0 {
		t.Fatal("nonzeroMin")
	}
}

func TestHookTextCutsFromTheMiddleKeepingTheLastLine(t *testing.T) {
	short := "agent-mesh reminder (resume): you are x.\nRun `mesh inbox` now."
	if got := hookText(short); got != short {
		t.Errorf("short text changed: %q", got)
	}
	long := "head line\n" + strings.Repeat("a task line with some words in it\n", 600) + "Run `mesh inbox` now."
	got := hookText(long)
	if len(got) > HookOutputMax-1 || !strings.HasSuffix(got, "Run `mesh inbox` now.") || !strings.Contains(got, "… (cut to fit a hook's output)") ||
		!strings.HasPrefix(got, "head line\n") {
		t.Errorf("cut text (%d bytes): head %q tail %q", len(got), got[:20], got[len(got)-30:])
	}
	// Never inside a character: a multibyte text still comes out valid.
	wide := strings.Repeat("é", 12000)
	if got := hookText(wide); !utf8.ValidString(got) || len(got) > HookOutputMax-1 {
		t.Errorf("invalid or over the cap: %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
}

func TestHeaderTextHasNoControlCharacters(t *testing.T) {
	got := headerText("the last reset failed: line one\nline two\r\x00end")
	if strings.ContainsAny(got, "\n\r\x00") || !strings.Contains(got, "line one line two") {
		t.Errorf("%q", got)
	}
	if n := len([]rune(headerText(strings.Repeat("é", 5000)))); n != 1001 {
		t.Errorf("length %d", n)
	}
}

func TestCapTextCountsCharactersAndNeverCutsOne(t *testing.T) {
	text := strings.Repeat("é", 5000) // 10000 bytes, no newline
	got := capText(text, 4000, "cat notes.md")
	if !utf8.ValidString(got) || !strings.Contains(got, "1000 more characters: cat notes.md") || !strings.HasPrefix(got, strings.Repeat("é", 4000)+"\n") {
		t.Errorf("%d bytes, valid %v: …%q", len(got), utf8.ValidString(got), got[len(got)-50:])
	}
	// Under the limit in characters (though over it in bytes): returned whole, not capped.
	if under := strings.Repeat("é", 3000); capText(under, 4000, "x") != under {
		t.Error("3000 characters were capped at a 4000 limit")
	}
}
