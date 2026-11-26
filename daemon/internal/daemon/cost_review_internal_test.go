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
