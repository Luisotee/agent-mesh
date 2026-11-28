package daemon

import (
	"strings"
	"testing"
)

func TestTheTaskAndPlanSayWhatAnAgentIsFor(t *testing.T) {
	long := strings.Repeat("word ", 40)
	for _, c := range []struct{ name, body, want string }{
		{"heading", "Some intro\n\n# Design: who is doing what\n\nbody", "Design: who is doing what"},
		{"direct marker", "Direct task (no plan): fix the retry loop\n\nmore", "fix the retry loop"},
		{"marker alone, then the line", "Direct task (no plan)\n\nFix the flaky test", "Fix the flaky test"},
		{"emphasis", "**Question (read-only)**: why does X fail?", "why does X fail?"},
		{"first line", "plain first line\nsecond", "plain first line"},
		{"empty", "  \n \n", ""},
	} {
		if got := taskAbout(c.body); got != c.want {
			t.Errorf("%s: taskAbout = %q, want %q", c.name, got, c.want)
		}
	}
	if got := taskAbout(long); len([]rune(got)) != maxAbout || !strings.HasSuffix(got, "…") {
		t.Errorf("a long line is cut to %d characters with an ellipsis: %q (%d)", maxAbout, got, len([]rune(got)))
	}
	if got := planAbout("# Plan: Retry with backoff\n\nRepo: x"); got != "Retry with backoff" {
		t.Errorf("planAbout = %q", got)
	}
	if got := planAbout("no heading here"); got != "" {
		t.Errorf("a plan with no heading has no title: %q", got)
	}
	if got := cutLines("a\n\n b \nc\nd", 3, 400); got != "a\nb\nc" {
		t.Errorf("cutLines keeps three lines: %q", got)
	}
}
