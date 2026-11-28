package daemon

import (
	"strings"
	"testing"
)

func TestTheTaskAndPlanSayWhatAnAgentIsFor(t *testing.T) {
	long := strings.Repeat("word ", 40)
	for _, c := range []struct{ name, body, want string }{
		{"heading", "# Design: who is doing what\n\nbody", "Design: who is doing what"},
		{"the opening line, not a later heading", "Fix the flaky relay reconnect test\n\n## Context\nx", "Fix the flaky relay reconnect test"},
		{"a marker-only heading, then the line", "# Direct task (no plan)\n\nFix the retry loop in relay.go", "Fix the retry loop in relay.go"},
		{"a word that starts like a marker", "Taskbar overflow fix", "Taskbar overflow fix"},
		{"a word that starts like a marker, plural", "Tasks queue ordering", "Tasks queue ordering"},
		{"a glob", "Fix glob handling of *.go files", "Fix glob handling of *.go files"},
		{"a leading underscore", "_internal package cleanup", "_internal package cleanup"},
		{"a flag", "-v flag ignored", "-v flag ignored"},
		{"a bullet", "- fix the thing", "fix the thing"},
		{"a task label", "Task: fix the thing", "fix the thing"},
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
	if got := planAbout("# PLAN: Upper case label"); got != "Upper case label" {
		t.Errorf("planAbout strips the label in any case: %q", got)
	}
	if got := planAbout("no heading here"); got != "" {
		t.Errorf("a plan with no heading has no title: %q", got)
	}
	if got := cutLines("a\n\n b \nc\nd", 3, 400); got != "a\nb\nc" {
		t.Errorf("cutLines keeps three lines: %q", got)
	}
}

// An ask whose body has no usable text gets no "Question:" stub: the derived line takes over.
func TestAnAskWithNoUsableTextHasNoStubAbout(t *testing.T) {
	if got := askAbout("---"); got != "" {
		t.Errorf("askAbout of a body of markdown noise = %q, want empty", got)
	}
	if got := askAbout("How does X work?"); got != "Question: How does X work?" {
		t.Errorf("askAbout = %q", got)
	}
}
