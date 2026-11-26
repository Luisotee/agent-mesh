package daemon

// A root's `mesh inbox` shows a long plan or `done` as a summary: the root's context is the scarce thing, a plan is
// read from its file when the root acts on it, and the root's own tool results hold every earlier body again on
// each later call (docs/cost-research/roles-and-reviews.md, "The root's weight"). The stored message is never
// changed: `mesh inbox --id <id>` (or --full) reads it whole.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

const (
	// summaryDoneChars is how much of a `done` report's start a summary keeps.
	summaryDoneChars = 1200
	// summaryRisksChars caps the plan section a summary prints whole ("Open questions / risks").
	summaryRisksChars = 1500
)

var prLinkRE = regexp.MustCompile(`https://github\.com/[\w.-]+/[\w.-]+/pull/\d+`)

// summarizeInbox returns msgs with every plan and done report longer than over characters (0: none) replaced by its
// summary, on a copy.
func (d *Daemon) summarizeInbox(msgs []*v1.Message, over int) []*v1.Message {
	if over <= 0 {
		return msgs
	}
	out := make([]*v1.Message, len(msgs))
	for i, m := range msgs {
		out[i] = m
		if (m.Type != v1.MessageType_MESSAGE_TYPE_PLAN && m.Type != v1.MessageType_MESSAGE_TYPE_DONE) ||
			utf8.RuneCountInString(m.Body) <= over {
			continue
		}
		c := proto.Clone(m).(*v1.Message)
		c.Body = d.summaryOf(m)
		out[i] = c
	}
	return out
}

// kept is the file a sender keeps a message's text in by convention (`plans/<name>.md`, `reports/<name>.md`), when
// it holds the same text; "" otherwise.
func (d *Daemon) kept(m *v1.Message, folder string) string {
	name := m.From.GetName()
	if name == "" || strings.ContainsAny(name, `/\`) {
		return ""
	}
	path := filepath.Join(d.paths.RunDir(m.RunId), folder, name+".md")
	b, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(b)) != strings.TrimSpace(m.Body) {
		return ""
	}
	return path
}

func (d *Daemon) summaryOf(m *v1.Message) string {
	chars := utf8.RuneCountInString(m.Body)
	var out []string
	var what, file string
	if m.Type == v1.MessageType_MESSAGE_TYPE_PLAN {
		what, file = "a plan", d.kept(m, "plans")
		out = planSummary(m.Body)
	} else {
		what, file = "a report", d.kept(m, "reports")
		out = doneSummary(m.Body)
	}
	if file != "" {
		out = append(out, "file: "+file)
	}
	out = append(out, fmt.Sprintf("[summary: %s of %d characters; `mesh inbox --id %s` prints it whole]", what, chars, m.Id))
	return strings.Join(out, "\n")
}

// section is a `## ` part of a markdown document: its heading, and its text with the heading line.
type section struct {
	name  string
	chars int
	text  string
}

// sections splits a document at its `## ` headings (outside code fences); head is what comes before the first.
func sections(body string) (head string, secs []section) {
	var cur *section
	var buf []string
	var headLines []string
	fence := false
	flush := func() {
		if cur != nil {
			cur.text = strings.TrimRight(strings.Join(buf, "\n"), "\n")
			cur.chars = utf8.RuneCountInString(cur.text)
			secs = append(secs, *cur)
		}
	}
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			fence = !fence
		}
		if !fence && strings.HasPrefix(ln, "## ") {
			flush()
			cur = &section{name: strings.TrimSpace(strings.TrimPrefix(ln, "## "))}
			buf = []string{ln}
			continue
		}
		if cur == nil {
			headLines = append(headLines, ln)
		} else {
			buf = append(buf, ln)
		}
	}
	flush()
	return strings.TrimSpace(strings.Join(headLines, "\n")), secs
}

// planSummary is a plan's title and header line, its sections with their sizes, and its open questions and risks
// whole (what a reply may need); the rest is read from the plan's file.
func planSummary(body string) []string {
	head, secs := sections(body)
	var out []string
	for i, ln := range strings.Split(head, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" && i < 4 {
			out = append(out, capRunes(ln, summaryDoneChars))
		}
	}
	var names []string
	for _, s := range secs {
		names = append(names, fmt.Sprintf("%s %d", s.name, s.chars))
	}
	if len(names) > 0 {
		out = append(out, "sections (characters): "+strings.Join(names, ", "))
	}
	for _, s := range secs {
		if strings.HasPrefix(strings.ToLower(s.name), "open questions") {
			out = append(out, "", capRunes(s.text, summaryRisksChars))
		}
	}
	return out
}

// doneSummary is a report's start, cut at a line, and the pull requests it names.
func doneSummary(body string) []string {
	var out []string
	n := 0
	for _, ln := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if n+utf8.RuneCountInString(ln) > summaryDoneChars && len(out) > 0 {
			out = append(out, "…")
			break
		}
		out = append(out, capRunes(ln, summaryDoneChars)) // a one-paragraph report is one line: cut it too
		n += utf8.RuneCountInString(ln) + 1
	}
	seen := map[string]bool{}
	var prs []string
	for _, u := range prLinkRE.FindAllString(body, -1) {
		if !seen[u] {
			seen[u] = true
			prs = append(prs, u)
		}
	}
	if len(prs) > 0 {
		out = append(out, "PRs: "+strings.Join(prs, ", "))
	}
	return out
}

func capRunes(s string, limit int) string {
	if r := []rune(s); len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return s
}
