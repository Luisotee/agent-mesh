package claude

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentmeshv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/transcript"
)

// The chat view reads Claude Code's own record of a session: the same JSONL file as the context size (context.go),
// one JSON object per line. Only the lines a person would call the conversation are shown: what the user typed,
// what the model said, the tools it called and what they answered, and the mesh's doorbells. Thinking, subagents'
// own turns, injected context and the CLI's bookkeeping lines are left out, and a line that doesn't parse is skipped
// (the record is the CLI's, not ours, and is being written while we read it).

const (
	// CapTranscript is the capability of an adapter that can read a conversation for the chat view.
	CapTranscript = transcript.Cap
	// CapBracketedPaste is the capability of a CLI whose input box takes a bracketed paste, so a prompt of several
	// lines is one prompt.
	CapBracketedPaste = "bracketed-paste"
	// CapInterruptEsc is the capability of a CLI whose turn Esc interrupts, leaving it at its prompt
	// (InterruptAgent).
	CapInterruptEsc = "interrupt-esc"

	maxText     = transcript.MaxText
	wantEntries = transcript.WantEntries
)

// DoorbellMark begins a mesh doorbell (daemon.Doorbell), alone or after "Another Claude session sent a message:".
const doorbellMark = transcript.DoorbellMark

var (
	systemReminder = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
	// Claude Code wraps what a user pasted in tags that carry a random id, the closing tag too (`<pasted_content id="x">…
	// </pasted_content id="x">`): the user sees the pasted text, not the tags, so only the tags go.
	pastedTag = regexp.MustCompile(`</?pasted_content(?:\s+[^\s>"=]+(?:="[^"]*")?)*\s*>`)
	// A slash command the user typed is recorded as tags: its name, then its arguments (the CLI also writes its
	// output as <local-command-stdout>, which is shown as a command-output entry; its caveat is dropped).
	// Bash mode (`! cmd` in the box) is recorded as <bash-input>cmd</bash-input>, its output as <bash-stdout> and
	// <bash-stderr> in a user line of their own.
	bashInput   = regexp.MustCompile(`(?s)^<bash-input>\s*(.*?)\s*</bash-input>`)
	bashOutput  = regexp.MustCompile(`^<bash-(stdout|stderr)>`)
	localOutput = regexp.MustCompile(`^<local-command-(stdout|stderr|caveat)>`)
	commandOut  = regexp.MustCompile(`(?s)^<local-command-(stdout|stderr)>(.*?)(?:</local-command-(?:stdout|stderr)>)?\s*$`)
	ansi        = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b\([A-Za-z0-9]`)
	commandName = regexp.MustCompile(`(?s)<command-name>\s*(.*?)\s*</command-name>`)
	commandArgs = regexp.MustCompile(`(?s)<command-args>\s*(.*?)\s*</command-args>`)
)

type transcriptLine struct {
	Type        string `json:"type"`
	UUID        string `json:"uuid"`
	Timestamp   string `json:"timestamp"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	// IsCompactSummary and IsVisibleInTranscriptOnly mark what the CLI writes for itself (the summary a compaction
	// leaves, which is context for the model, not something the user said).
	IsCompactSummary          bool `json:"isCompactSummary"`
	IsVisibleInTranscriptOnly bool `json:"isVisibleInTranscriptOnly"`
	Message                   struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// TranscriptEntries turns one line of the record, which starts at offset, into the entries it shows (none for a line
// that isn't part of the conversation or doesn't parse).
func TranscriptEntries(line []byte, offset int64) []*agentmeshv1.TranscriptEntry {
	var l transcriptLine
	if json.Unmarshal(line, &l) != nil || l.IsSidechain || (l.Type != "user" && l.Type != "assistant") ||
		l.IsCompactSummary || l.IsVisibleInTranscriptOnly {
		return nil
	}
	var at *timestamppb.Timestamp
	if t, err := time.Parse(time.RFC3339Nano, l.Timestamp); err == nil {
		at = timestamppb.New(t)
	}
	var out []*agentmeshv1.TranscriptEntry
	add := func(i int, e *agentmeshv1.TranscriptEntry) {
		e.Id = fmt.Sprintf("%d", offset)
		if l.UUID != "" {
			e.Id = l.UUID
		}
		e.Id += fmt.Sprintf(":%d", i)
		e.Time, e.Offset = at, offset
		e.Text, e.Truncated = transcript.Cut(e.Text, maxText)
		out = append(out, e)
	}
	// A prompt typed by hand, or a doorbell, is a string; everything else is a list of blocks.
	var asText string
	if json.Unmarshal(l.Message.Content, &asText) == nil {
		if l.Type == "user" {
			if e := userEntry(asText, l.IsMeta); e != nil {
				add(0, e)
			}
		}
		return out
	}
	var blocks []contentBlock
	if json.Unmarshal(l.Message.Content, &blocks) != nil {
		return nil
	}
	for i, b := range blocks {
		switch {
		case l.Type == "user" && b.Type == "text":
			if e := userEntry(b.Text, l.IsMeta); e != nil {
				add(i, e)
			}
		case l.Type == "user" && b.Type == "tool_result":
			add(i, &agentmeshv1.TranscriptEntry{Kind: agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_TOOL_RESULT, Text: resultText(b.Content), ToolUseId: b.ToolUseID, IsError: b.IsError})
		case l.Type == "assistant" && b.Type == "text":
			if strings.TrimSpace(b.Text) != "" {
				add(i, &agentmeshv1.TranscriptEntry{Kind: agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_ASSISTANT, Text: b.Text})
			}
		case l.Type == "assistant" && b.Type == "tool_use":
			e := &agentmeshv1.TranscriptEntry{Kind: agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_TOOL_CALL, Tool: b.Name, ToolInput: transcript.Summarize(b.Input), ToolUseId: b.ID}
			if b.Name == ToolAskUserQuestion {
				e.Questions = ParseQuestions(b.Input)
			}
			add(i, e)
		}
	}
	return out
}

// doorbellPrefix is how Claude presents what its inbox socket received, before the doorbell's own line.
const doorbellPrefix = "Another Claude session sent a message:"

// userEntry is what a user line holds: a mesh doorbell (even when the CLI marks it as injected context), what the
// user typed, or nothing (injected context, an empty line). A doorbell is a line that begins with the mark, alone or
// after Claude's own prefix: injected context that only quotes it (the user's CLAUDE.md does) is not one.
func userEntry(text string, meta bool) *agentmeshv1.TranscriptEntry {
	text = strings.TrimSpace(systemReminder.ReplaceAllString(text, ""))
	if rest := strings.TrimSpace(strings.TrimPrefix(text, doorbellPrefix)); strings.HasPrefix(rest, doorbellMark) {
		line, _, _ := strings.Cut(rest, "\n")
		return &agentmeshv1.TranscriptEntry{Kind: agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_MESH, Text: line}
	}
	if m := commandOut.FindStringSubmatch(text); m != nil && !meta {
		// What a slash command printed (`/context`, `/usage`): the CLI's own words, kept so the chat answers it.
		out := strings.TrimSpace(ansi.ReplaceAllString(m[2], ""))
		if out == "" {
			return nil
		}
		return &agentmeshv1.TranscriptEntry{Kind: agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_COMMAND_OUTPUT, Text: out, IsError: m[1] == "stderr"}
	}
	// What the user pasted is theirs whatever it starts with: the marks below are looked for before the paste tags go, so a pasted
	// doorbell or `<bash-stdout>` is shown as typed text and not taken for the real one.
	if meta || pastedTag.ReplaceAllString(text, "") == "" || localOutput.MatchString(text) || bashOutput.MatchString(text) {
		return nil
	}
	if m := bashInput.FindStringSubmatch(text); m != nil {
		text = "! " + m[1]
	}
	if m := commandName.FindStringSubmatch(text); m != nil && strings.HasPrefix(text, "<command-") {
		// A slash command is what the user typed: the command and its arguments, not the tags around them.
		cmd := m[1]
		if a := commandArgs.FindStringSubmatch(text); a != nil && a[1] != "" {
			cmd += " " + a[1]
		}
		text = cmd
	}
	text = strings.TrimSpace(pastedTag.ReplaceAllString(text, ""))
	return &agentmeshv1.TranscriptEntry{Kind: agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_USER, Text: text}
}

// resultText is a tool result's text: a string, or the text blocks of a list (an image is named, not shown).
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "\n")
}

// ReadTranscript reads a page of the record at path (transcript.ReadJSONL with Claude's lines).
func ReadTranscript(path string, offset int64, backward bool, maxBytes int64) (entries []*agentmeshv1.TranscriptEntry, start, end int64, atStart bool, err error) {
	return transcript.ReadJSONL(path, TranscriptEntries, offset, backward, maxBytes)
}

// ParseQuestions reads the input of an AskUserQuestion call (the same JSON as the hook's `tool_input`) into its
// questions, or nil when it isn't one the user can answer: no questions, a question with no text or no options, an
// option with no label or the same label twice, or the same text twice (the answers are keyed by it).
func ParseQuestions(input []byte) []*agentmeshv1.PromptQuestion {
	var in struct {
		Questions []struct {
			Question    string `json:"question"`
			Header      string `json:"header"`
			MultiSelect bool   `json:"multiSelect"`
			Options     []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
				Preview     string `json:"preview"`
			} `json:"options"`
		} `json:"questions"`
	}
	if json.Unmarshal(input, &in) != nil || len(in.Questions) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []*agentmeshv1.PromptQuestion
	for _, q := range in.Questions {
		if strings.TrimSpace(q.Question) == "" || seen[q.Question] || len(q.Options) == 0 {
			return nil
		}
		seen[q.Question] = true
		labels := map[string]bool{}
		for _, o := range q.Options {
			if strings.TrimSpace(o.Label) == "" || labels[o.Label] {
				return nil // the answer is a label: an empty or repeated one is no choice
			}
			labels[o.Label] = true
		}
		pq := &agentmeshv1.PromptQuestion{Question: q.Question, Header: q.Header, MultiSelect: q.MultiSelect}
		for _, o := range q.Options {
			pq.Options = append(pq.Options, &agentmeshv1.PromptOption{Label: o.Label, Description: o.Description, Preview: o.Preview})
		}
		out = append(out, pq)
	}
	return out
}
