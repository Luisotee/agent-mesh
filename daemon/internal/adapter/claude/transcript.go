package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentmeshv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// The chat view reads Claude Code's own record of a session: the same JSONL file as the context size (context.go),
// one JSON object per line. Only the lines a person would call the conversation are shown: what the user typed,
// what the model said, the tools it called and what they answered, and the mesh's doorbells. Thinking, subagents'
// own turns, injected context and the CLI's bookkeeping lines are left out, and a line that doesn't parse is skipped
// (the record is the CLI's, not ours, and is being written while we read it).

const (
	// CapTranscript is the capability of an adapter that can read a conversation for the chat view.
	CapTranscript = "transcript"
	// CapBracketedPaste is the capability of a CLI whose input box takes a bracketed paste, so a prompt of several
	// lines is one prompt.
	CapBracketedPaste = "bracketed-paste"

	// DefaultTranscriptBytes is how much of the record a read covers when the caller doesn't say.
	DefaultTranscriptBytes = 256 << 10
	// MaxTranscriptBytes bounds what one read covers however much is asked.
	MaxTranscriptBytes = 2 << 20
	// maxLine is the longest line read (a tool result holding a big file or an image); a longer one is skipped.
	maxLine = 16 << 20
	// maxText is the longest text of an entry; more is cut, and the entry says so.
	maxText = 8 << 10
	// maxSummary is the longest one-line summary of a tool's input.
	maxSummary = 300
	// wantEntries is how many entries a backward read goes on to find when its first window held few (a stretch of
	// bookkeeping lines), up to maxBackBytes of the record.
	wantEntries  = 20
	maxBackBytes = 8 << 20
)

// DoorbellMark begins a mesh doorbell (daemon.Doorbell), alone or after "Another Claude session sent a message:".
const doorbellMark = "📬 [mesh]"

var (
	systemReminder = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
	// A slash command the user typed is recorded as tags: its name, then its arguments (the CLI also writes its
	// output as <local-command-stdout>, which is dropped).
	// Bash mode (`! cmd` in the box) is recorded as <bash-input>cmd</bash-input>, its output as <bash-stdout> and
	// <bash-stderr> in a user line of their own.
	bashInput   = regexp.MustCompile(`(?s)^<bash-input>\s*(.*?)\s*</bash-input>`)
	bashOutput  = regexp.MustCompile(`^<bash-(stdout|stderr)>`)
	localOutput = regexp.MustCompile(`^<local-command-(stdout|stderr|caveat)>`)
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
		e.Text, e.Truncated = cut(e.Text, maxText)
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
			add(i, &agentmeshv1.TranscriptEntry{Kind: agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_TOOL_CALL, Tool: b.Name, ToolInput: summarize(b.Input), ToolUseId: b.ID})
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
	if meta || text == "" || localOutput.MatchString(text) || bashOutput.MatchString(text) {
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

// summaryKeys are the arguments that say what a tool call is about, in the order they are tried.
var summaryKeys = []string{"command", "file_path", "path", "pattern", "query", "url", "description", "prompt"}

// summarize is a tool input as one short line: the command, the path, the pattern.
func summarize(raw json.RawMessage) string {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	for _, k := range summaryKeys {
		if s, ok := in[k].(string); ok && s != "" {
			return oneLine(s)
		}
	}
	return oneLine(string(raw))
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s, _ = cut(s, maxSummary)
	return s
}

// cut shortens s to at most n bytes, at a character boundary, and says whether it did.
func cut(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// ReadTranscript reads a page of the record at path. Forward (backward false) it reads from offset on; backward it
// reads what comes before offset (-1: the end), going on until it has a few entries. Offsets are those of the lines'
// starts, as an earlier answer gave them. A record that doesn't exist yet is an empty conversation. The last line of
// the file is left for a later read when it isn't complete (the CLI is writing it).
func ReadTranscript(path string, offset int64, backward bool, maxBytes int64) (entries []*agentmeshv1.TranscriptEntry, start, end int64, atStart bool, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, 0, true, nil
	}
	if err != nil {
		return nil, 0, 0, false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, 0, false, err
	}
	size := st.Size()
	if maxBytes <= 0 {
		maxBytes = DefaultTranscriptBytes
	}
	maxBytes = min(maxBytes, MaxTranscriptBytes)
	if backward {
		return readBackward(f, size, offset, maxBytes)
	}
	return readForward(f, size, offset, maxBytes)
}

func readForward(f *os.File, size, offset, maxBytes int64) ([]*agentmeshv1.TranscriptEntry, int64, int64, bool, error) {
	// An offset past the end is a record that was rewritten: say where it ends, and the reader starts over.
	if offset < 0 || offset >= size {
		return nil, size, size, false, nil
	}
	r := bufio.NewReaderSize(io.NewSectionReader(f, offset, size-offset), 64<<10)
	var entries []*agentmeshv1.TranscriptEntry
	pos := offset
	for pos-offset < maxBytes {
		line, n, complete, err := readLine(r)
		if !complete {
			// A line still being written (or a read that failed): leave it for the next read.
			if err != nil && !errors.Is(err, io.EOF) {
				return entries, offset, pos, offset == 0, err
			}
			break
		}
		entries = append(entries, TranscriptEntries(line, pos)...)
		pos += int64(n)
	}
	return entries, offset, pos, offset == 0, nil
}

func readBackward(f *os.File, size, offset, maxBytes int64) ([]*agentmeshv1.TranscriptEntry, int64, int64, bool, error) {
	end := offset
	if end < 0 || end > size {
		end = size
	}
	// The end of the file may be a line being written: it is not part of this page, and a later read finds it whole.
	if end == size && size > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], size-1); err != nil {
			return nil, end, end, false, err
		}
		if last[0] != '\n' {
			var err error
			if end, err = lastLineStart(f, size); err != nil {
				return nil, end, end, false, err
			}
		}
	}
	var entries []*agentmeshv1.TranscriptEntry
	first, spent, window := end, int64(0), maxBytes
	for first > 0 && len(entries) < wantEntries && spent < maxBackBytes {
		from := max(0, first-window)
		buf := make([]byte, first-from)
		if _, err := f.ReadAt(buf, from); err != nil && !errors.Is(err, io.EOF) {
			return entries, first, end, false, err
		}
		lineStart := from
		if from > 0 {
			i := bytes.IndexByte(buf, '\n')
			// A newline only at the very end is the end of the line before the page: no line of this window starts in it.
			if i < 0 || i == len(buf)-1 {
				// One line is longer than the window: look further back for its start. What this reads doesn't count
				// against the budget, or a line of a few MB would use it up before the window could reach the line's
				// start, and every page would end where it began.
				if window >= maxLine {
					first = from
					break
				}
				window = min(window*2, maxLine)
				continue
			}
			lineStart = from + int64(i) + 1
			buf = buf[i+1:]
		}
		spent += int64(len(buf))
		entries = append(entriesOf(buf, lineStart), entries...)
		first, window = lineStart, maxBytes
	}
	return entries, first, end, first == 0, nil
}

// entriesOf parses the complete lines of buf, which starts at a line's start at offset.
func entriesOf(buf []byte, offset int64) []*agentmeshv1.TranscriptEntry {
	var out []*agentmeshv1.TranscriptEntry
	for pos := offset; len(buf) > 0; {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		if i <= maxLine {
			out = append(out, TranscriptEntries(bytes.TrimRight(buf[:i], "\r"), pos)...)
		}
		buf, pos = buf[i+1:], pos+int64(i)+1
	}
	return out
}

// lastLineStart is where the last line of the file, which has no newline yet, begins.
func lastLineStart(f *os.File, size int64) (int64, error) {
	const chunk = 64 << 10
	for to := size; to > 0; {
		from := max(0, to-chunk)
		buf := make([]byte, to-from)
		if _, err := f.ReadAt(buf, from); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			return from + int64(i) + 1, nil
		}
		to = from
	}
	return 0, nil
}

// readLine reads one line: its bytes without the newline, how many bytes it took, and whether it ended with a
// newline (an incomplete last line doesn't). A line longer than maxLine is consumed and returned empty.
func readLine(r *bufio.Reader) (line []byte, n int, complete bool, err error) {
	tooLong := false
	for {
		chunk, e := r.ReadSlice('\n')
		n += len(chunk)
		if !tooLong {
			if len(line)+len(chunk) > maxLine {
				tooLong, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		switch {
		case e == nil:
			return bytes.TrimRight(line, "\r\n"), n, true, nil
		case errors.Is(e, bufio.ErrBufferFull):
			continue
		default:
			return line, n, false, e
		}
	}
}
