package daemon

import "testing"

func TestReviewTaskPRFromBodyReadsTheDaemonsOwnPrefix(t *testing.T) {
	for name, tc := range map[string]struct{ repo, body, want string }{
		"full url with a head": {"/src/repo", "Review (read-only) of https://github.com/o/repo/pull/7 at abcdef012: review this pull request. Change no files.", "repo#7"},
		"full url no head":     {"/src/repo", "Review (read-only) of https://github.com/o/repo/pull/7: review this pull request.", "repo#7"},
		"url, repo unknown":    {"", "Review (read-only) of https://github.com/o/repo/pull/9 at abcdef012: review this pull request.", "repo#9"},
		"bare number":          {"/src/repo", "Review (read-only) of 12: review this pull request.", "repo#12"},
		"leading space":        {"/src/repo", "  Review (read-only) of https://github.com/o/repo/pull/7: review this pull request.", "repo#7"},
		"not a review":         {"/src/repo", "Review (read-only): the PR.", ""},
	} {
		if got := reviewTaskPRFromBody(tc.repo, tc.body); got != tc.want {
			t.Errorf("%s: reviewTaskPRFromBody = %q, want %q", name, got, tc.want)
		}
	}
}

// The regex is coupled to the review task's wording: feed it what reviewTask really builds.
func TestReviewTaskPRFromBodyReadsWhatReviewTaskBuilds(t *testing.T) {
	pr := &prInfo{Number: 7, URL: "https://github.com/o/repo/pull/7", Head: "abcdef0123456789abcdef0123456789abcdef01", Show: "git diff a..b"}
	for name, s := range map[string]reviewSpec{
		"fresh":   {pr: pr},
		"own at":  {pr: pr, ownBranch: true},
		"re-ask":  {pr: pr, last: "0123456789abcdef0123456789abcdef01234567", narrow: true, reused: true, stayAt: "x", stayWhy: "y"},
		"narrowd": {pr: pr, last: "0123456789abcdef0123456789abcdef01234567", narrow: true},
	} {
		if got := reviewTaskPRFromBody("/src/repo", reviewTask(s)); got != "repo#7" {
			t.Errorf("%s: reviewTaskPRFromBody(reviewTask) = %q, want repo#7", name, got)
		}
	}
}
