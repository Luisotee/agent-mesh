package daemon

import (
	"strings"
	"testing"
)

// A draft is told to the app whole or not at all: the user confirms to discard what they were shown, and a draft cut for
// the dialog would let text past the cut be cleared unseen (SendPromptAnyway compares the whole squeezed text).
func TestADraftIsToldWholeOrNotAtAll(t *testing.T) {
	if got := shownDraft("  half   a\n thou "); got != "half a thou" {
		t.Errorf("a short draft: %q", got)
	}
	whole := strings.TrimSpace(strings.Repeat("word ", maxDraftShown/5))
	if got := shownDraft(whole); got != whole {
		t.Errorf("a draft of the most: %d characters, told as %d", len(whole), len(got))
	}
	if got := shownDraft(whole + " more"); got != "" {
		t.Errorf("a draft over the most is not told: %q", got)
	}
	// What is compared is the squeezed whole, so two drafts that differ only past what could be shown differ.
	a, b := squeezed(whole+" one"), squeezed(whole+" two")
	if a == b {
		t.Error("drafts differing at the end compare equal")
	}
}
