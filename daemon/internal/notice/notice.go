// Package notice holds what a notification is made of: its kinds, tabs and plain-text bodies. What deserves one is
// decided in internal/daemon (notify.go), once, as a UserNotice event that the apps and the phones' pushes follow.
// Android's own local rules (android/.../notify/Notice.kt) still follow the older per-event rules (TODO.md).
package notice

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The kinds, as the host's app.notify.events setting names them (settings.NotifyEvents).
const (
	Input    = "input"
	Plan     = "plan"
	Question = "question"
	Report   = "report"
	Failed   = "failed"
	Exited   = "exited"
	Limited  = "limited"
	Error    = "error"
	// Finished: an agent with no parent ended a turn, or a run finished (nothing of it is left to do or wait for).
	Finished = "finished"
	// Ready: a pull request is ready to merge where the user merges, or is one the user holds.
	Ready = "ready"
)

// The screen a notification opens, as the apps name their tabs.
const (
	TabOverview = "overview"
	TabMessages = "messages"
	TabTerminal = "terminal"
	// TabChat is the agent's chat, the tab of a UserNotice (a phone's payload says TabMessages for it).
	TabChat = "chat"
)

// Notice is one notification.
type Notice struct {
	Kind    string
	Title   string
	Body    string
	AgentID string
	Tab     string
	// ForUser: the user has something to do or to know. False: only the apps' Activity list shows it.
	ForUser bool
	// Tag: a later notice with the same tag replaces the earlier one on a screen.
	Tag string
}

// Waiting reports whether the notice asks something of the user (a plan, a question, a prompt).
func (n *Notice) Waiting() bool { return n.Kind == Input || n.Kind == Plan || n.Kind == Question }

const (
	maxBody = 200
	maxScan = 4000
)

var (
	lineMark = regexp.MustCompile(`^\s*(#{1,6}\s+|>\s*|[-*+]\s+|\d+\.\s+)`)
	fence    = regexp.MustCompile("^\\s*```\\w*\\s*$")
	code     = regexp.MustCompile("`([^`\n]+)`")
	spaces   = regexp.MustCompile(`\s+`)
)

// Brief makes a message body a notification's plain text: the Markdown's own marks dropped (headings, bullets,
// quotes, fences, paired emphasis and code ticks), cut to 200 characters. A lone `*` or backtick stays: it is
// the author's own character ("does 2*3 equal 6?").
func Brief(text string) string {
	if utf8.RuneCountInString(text) > maxScan {
		text = string([]rune(text)[:maxScan])
	}
	var parts []string
	for _, l := range strings.Split(text, "\n") {
		if fence.MatchString(l) {
			continue
		}
		parts = append(parts, lineMark.ReplaceAllString(l, ""))
	}
	flat := code.ReplaceAllString(strings.Join(parts, " "), "$1")
	flat = strings.TrimSpace(spaces.ReplaceAllString(stripEmphasis(flat), " "))
	if r := []rune(flat); len(r) > maxBody {
		return string(r[:maxBody-1]) + "…"
	}
	return flat
}

// stripEmphasis drops paired runs of one to three `*` around text, as Notice.kt's BOLD_ITALIC regex does (RE2
// has no lookaround or backreference): an opening run not after a word character or `*` and before a non-space,
// closed by the first run of the same length after a non-space and not before a word character or `*`.
func stripEmphasis(s string) string {
	r := []rune(s)
	var out []rune
	word := func(c rune) bool { return c == '_' || unicode.IsLetter(c) || unicode.IsDigit(c) }
	runAt := func(i int) int {
		n := 0
		for i+n < len(r) && r[i+n] == '*' {
			n++
		}
		return n
	}
	for i := 0; i < len(r); {
		n := runAt(i)
		if n == 0 {
			out = append(out, r[i])
			i++
			continue
		}
		open := n <= 3 && (i == 0 || !(word(r[i-1]) || r[i-1] == '*')) && i+n < len(r) && !unicode.IsSpace(r[i+n])
		if open {
			if j := closing(r, i+n, n, word); j >= 0 {
				out = append(out, r[i+n:j]...)
				i = j + n
				continue
			}
		}
		out = append(out, r[i:i+n]...)
		i += n
	}
	return string(out)
}

// closing finds the first run of exactly n `*` at or after from+1 that closes an emphasis, or -1.
func closing(r []rune, from, n int, word func(rune) bool) int {
	for j := from + 1; j < len(r); j++ {
		if r[j] != '*' {
			continue
		}
		k := j
		for k < len(r) && r[k] == '*' {
			k++
		}
		if k-j == n && !unicode.IsSpace(r[j-1]) && (k == len(r) || !(word(r[k]) || r[k] == '*')) {
			return j
		}
		j = k - 1
	}
	return -1
}
