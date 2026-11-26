package daemon

import (
	"strings"
	"testing"
)

func TestAskSlugIsAskAndTheBodysWordsCutToTwentyCharacters(t *testing.T) {
	for body, want := range map[string]string{
		"Where is the retry logic, exactly?": "ask-where-is-the-ret",
		"short":                              "ask-short",
		"  ?!  ":                             "ask",
		"A  B":                               "ask-a-b",
	} {
		if got := askSlug(body); got != want {
			t.Errorf("askSlug(%q) = %q, want %q", body, got, want)
		}
	}
}

func TestReviewTaskReadsLikeBinMeshsWithTheDaemonsWaitLine(t *testing.T) {
	pr := &prInfo{Number: 7, URL: "https://github.com/o/r/pull/7", Head: "abcdef0123456789", Show: "git diff 1111111..abcdef012"}
	fresh := reviewTask(reviewSpec{pr: pr, kind: "claude", level: "high"})
	for _, want := range []string{"Review (read-only) of https://github.com/o/r/pull/7 at abcdef012:", "Its head is checked out in your worktree",
		"`git diff 1111111..abcdef012` shows the change", "`gh pr checks https://github.com/o/r/pull/7`", "/code-review high https://github.com/o/r/pull/7",
		"`mesh send parent --type wait --body \"/code-review running\"`"} {
		if !strings.Contains(fresh, want) {
			t.Errorf("fresh task lacks %q:\n%s", want, fresh)
		}
	}
	if !isReadOnlyTask(fresh) || strings.Contains(reviewTask(reviewSpec{pr: pr, kind: "agy"}), "/code-review") {
		t.Errorf("the review is read-only, and only Claude's runs /code-review")
	}
	// A re-review: only the fixes, by the reviewer's own memory or a fresh one's told target.
	last := "0123456789abcdef"
	for name, c := range map[string]struct {
		s    reviewSpec
		want []string
		not  []string
	}{
		"fresh since": {reviewSpec{pr: pr, kind: "claude", level: "high", last: last, narrow: true},
			[]string{"An earlier review covered this PR up to 012345678: review only what changed since (`git diff 012345678 abcdef012`)",
				"/code-review high 012345678..abcdef012", "Its target covers only the commits since your last review"}, nil},
		"asked again": {reviewSpec{pr: pr, kind: "agy", last: last, narrow: true, reused: true},
			[]string{"You last reviewed it at 012345678: say which of your earlier findings are fixed"}, []string{"An earlier review covered"}},
		"rebased": {reviewSpec{pr: pr, kind: "agy", last: last, reused: true},
			[]string{"the branch was rebased or merged its base since", "review the whole PR again"}, nil},
		"stays": {reviewSpec{pr: pr, kind: "claude", level: "high", last: last, narrow: true, reused: true, stayAt: last, stayWhy: "it is still working"},
			[]string{"Don't switch your branch", "Your worktree is still at 012345678, not the new head, because it is still working: read the new head with `git show abcdef012:<path>`",
				"check each finding with `git show abcdef012:<path>`"}, []string{"Its head is checked out"}},
	} {
		got := reviewTask(c.s)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s lacks %q:\n%s", name, w, got)
			}
		}
		for _, w := range c.not {
			if strings.Contains(got, w) {
				t.Errorf("%s has %q:\n%s", name, w, got)
			}
		}
	}
	// OpenCode reviewers receive the --body @path note; Claude and agy do not.
	oc := reviewTask(reviewSpec{pr: pr, kind: "opencode"})
	if !strings.Contains(oc, "--body @") {
		t.Errorf("opencode task lacks --body @ note: %s", oc)
	}
	if strings.Contains(fresh, "--body @") || strings.Contains(reviewTask(reviewSpec{pr: pr, kind: "agy"}), "--body @") {
		t.Errorf("claude/agy tasks should not contain --body @ note")
	}
	// The caller's notes ride along.
	if got := reviewTask(reviewSpec{pr: pr, kind: "agy", body: "mind the retry"}); !strings.HasSuffix(got, "Notes from your parent:\nmind the retry") {
		t.Errorf("notes: %s", got)
	}
}
