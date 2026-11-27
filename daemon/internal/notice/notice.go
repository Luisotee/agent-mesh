// Package notice decides what an event means to the person, for the push notifications meshd sends to phones.
// The same rules as the apps' own local notifications: android/.../notify/Notice.kt (noticeFor, brief) and
// desktop/src/notify.ts. Change one, change all three.
package notice

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
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
)

// The screen a notification opens, as the apps name their tabs.
const (
	TabOverview = "overview"
	TabMessages = "messages"
	TabTerminal = "terminal"
)

// Notice is one notification.
type Notice struct {
	Kind    string
	Title   string
	Body    string
	AgentID string
	Tab     string
}

// Waiting reports whether the notice asks something of the user (a plan, a question, a prompt).
func (n *Notice) Waiting() bool { return n.Kind == Input || n.Kind == Plan || n.Kind == Question }

// For returns the notice for an event, or nil: an agent stopped at a prompt, a plan, a question, a report or a
// failure sent to a run's lead, an agent that died, one that hit its usage limit or is in error. Never what a device (the user) did. agent looks an agent up
// by id (nil when unknown). Unlike the apps, it doesn't judge whether the event is live: meshd calls it only for
// events as they commit.
func For(e *v1.Event, agent func(id string) *v1.Agent) *Notice {
	switch p := e.Payload.(type) {
	case *v1.Event_AgentStateChanged:
		a := agent(e.AgentId)
		ch := p.AgentStateChanged
		if a == nil || ch.To == ch.From {
			return nil
		}
		body := Brief(ch.Detail)
		switch ch.To {
		case v1.AgentState_AGENT_STATE_NEEDS_INPUT:
			if body == "" {
				body = "It is stopped at a prompt in its terminal."
			}
			return &Notice{Kind: Input, Title: a.Name + " needs you", Body: body, AgentID: a.Id, Tab: TabTerminal}
		case v1.AgentState_AGENT_STATE_LIMITED:
			if body == "" {
				body = "Its terminal says more."
			}
			return &Notice{Kind: Limited, Title: a.Name + " hit its usage limit", Body: body, AgentID: a.Id, Tab: TabTerminal}
		case v1.AgentState_AGENT_STATE_ERROR:
			if body == "" {
				body = "Its terminal says more."
			}
			return &Notice{Kind: Error, Title: a.Name + " is in error", Body: body, AgentID: a.Id, Tab: TabTerminal}
		}
		return nil
	case *v1.Event_MessageSent:
		m := p.MessageSent.GetMessage()
		if m == nil || m.GetFrom().GetClass() == v1.ActorClass_ACTOR_CLASS_DEVICE {
			return nil
		}
		to, from := agent(m.ToId), agent(m.GetFrom().GetId())
		// What reaches a run's lead is what the user is waiting for; talk among workers isn't.
		if to == nil || to.Role != v1.AgentRole_AGENT_ROLE_ROOT || from == nil {
			return nil
		}
		n := &Notice{Body: Brief(m.Body), AgentID: from.Id, Tab: TabMessages}
		switch m.Type {
		case v1.MessageType_MESSAGE_TYPE_PLAN:
			n.Kind, n.Title = Plan, from.Name+" sent a plan"
		case v1.MessageType_MESSAGE_TYPE_QUESTION:
			n.Kind, n.Title = Question, from.Name+" has a question"
		case v1.MessageType_MESSAGE_TYPE_DONE:
			n.Kind, n.Title = Report, from.Name+" reported done"
		case v1.MessageType_MESSAGE_TYPE_FAILED:
			n.Kind, n.Title = Failed, from.Name+" failed"
		default:
			return nil
		}
		return n
	case *v1.Event_AgentExited:
		a := agent(e.AgentId)
		x := p.AgentExited
		// The user stopping an agent isn't news.
		if a == nil || x.Reason == "killed" {
			return nil
		}
		n := &Notice{AgentID: a.Id, Tab: TabOverview}
		switch {
		case x.ExitCode == 0 && x.Reason != "lost":
			n.Kind, n.Title = Exited, a.Name+" finished"
		case x.Reason == "lost":
			n.Kind, n.Title = Failed, a.Name+" was lost"
		default:
			n.Kind, n.Title = Failed, a.Name+" exited with code "+strconv.Itoa(int(x.ExitCode))
		}
		return n
	}
	return nil
}

// Relevant reports whether For could return a notice for an event of this payload, without looking anything up.
func Relevant(e *v1.Event) bool {
	switch e.Payload.(type) {
	case *v1.Event_AgentStateChanged, *v1.Event_MessageSent, *v1.Event_AgentExited:
		return true
	}
	return false
}

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
