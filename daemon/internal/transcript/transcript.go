// Package transcript is what the adapters share to give the chat view a conversation: reading a JSONL record in
// pages by byte offset (the Claude and Copilot records), and the cuts every entry gets (text length, a tool's
// one-line summary).
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	agentmeshv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

const (
	// Cap is the capability of an adapter that can read a conversation for the chat view.
	Cap = "transcript"

	// DefaultBytes is how much of the record a read covers when the caller doesn't say.
	DefaultBytes = 256 << 10
	// MaxBytes bounds what one read covers however much is asked.
	MaxBytes = 2 << 20
	// maxLine is the longest line read (a tool result holding a big file or an image); a longer one is skipped.
	maxLine = 16 << 20
	// MaxText is the longest text of an entry; more is cut, and the entry says so.
	MaxText = 8 << 10
	// maxSummary is the longest one-line summary of a tool's input.
	maxSummary = 300
	// WantEntries is how many entries a backward read goes on to find when its first window held few (a stretch of
	// bookkeeping lines), up to maxBackBytes of the record.
	WantEntries  = 20
	maxBackBytes = 8 << 20
)

// DoorbellMark begins a mesh doorbell (daemon.Doorbell): a user line that starts with it is shown as the mesh's.
const DoorbellMark = "📬 [mesh]"

// Parse turns one line of a record, which starts at offset, into the entries it shows (none for a line that isn't
// part of the conversation or doesn't parse).
type Parse func(line []byte, offset int64) []*agentmeshv1.TranscriptEntry

// summaryKeys are the arguments that say what a tool call is about, in the order they are tried.
var summaryKeys = []string{"command", "file_path", "path", "pattern", "query", "url", "description", "prompt"}

// Summarize is a tool input as one short line: the command, the path, the pattern.
func Summarize(raw json.RawMessage) string { return SummarizeKeys(raw, summaryKeys...) }

// SummarizeKeys is Summarize for a CLI whose tools name their arguments otherwise: the first of keys the input has.
func SummarizeKeys(raw json.RawMessage, keys ...string) string {
	var in map[string]any
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	for _, k := range keys {
		if s, ok := in[k].(string); ok && s != "" {
			return OneLine(s)
		}
	}
	return OneLine(string(raw))
}

// OneLine is s on one line, cut to a summary's length.
func OneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s, _ = Cut(s, maxSummary)
	return s
}

// Cut shortens s to at most n bytes, at a character boundary, and says whether it did.
func Cut(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// ReadJSONL reads a page of the record at path. Forward (backward false) it reads from offset on; backward it
// reads what comes before offset (-1: the end), going on until it has a few entries. Offsets are those of the lines'
// starts, as an earlier answer gave them. A record that doesn't exist yet is an empty conversation. The last line of
// the file is left for a later read when it isn't complete (the CLI is writing it).
func ReadJSONL(path string, parse Parse, offset int64, backward bool, maxBytes int64) (entries []*agentmeshv1.TranscriptEntry, start, end int64, atStart bool, err error) {
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
		maxBytes = DefaultBytes
	}
	maxBytes = min(maxBytes, MaxBytes)
	if backward {
		return readBackward(f, parse, size, offset, maxBytes)
	}
	return readForward(f, parse, size, offset, maxBytes)
}

func readForward(f *os.File, parse Parse, size, offset, maxBytes int64) ([]*agentmeshv1.TranscriptEntry, int64, int64, bool, error) {
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
		entries = append(entries, parse(line, pos)...)
		pos += int64(n)
	}
	return entries, offset, pos, offset == 0, nil
}

func readBackward(f *os.File, parse Parse, size, offset, maxBytes int64) ([]*agentmeshv1.TranscriptEntry, int64, int64, bool, error) {
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
	for first > 0 && len(entries) < WantEntries && spent < maxBackBytes {
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
		entries = append(entriesOf(buf, parse, lineStart), entries...)
		first, window = lineStart, maxBytes
	}
	return entries, first, end, first == 0, nil
}

// entriesOf parses the complete lines of buf, which starts at a line's start at offset.
func entriesOf(buf []byte, parse Parse, offset int64) []*agentmeshv1.TranscriptEntry {
	var out []*agentmeshv1.TranscriptEntry
	for pos := offset; len(buf) > 0; {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		if i <= maxLine {
			out = append(out, parse(bytes.TrimRight(buf[:i], "\r"), pos)...)
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
