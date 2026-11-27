package agy

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/timestamppb"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	agentmeshv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/transcript"
)

// The chat view reads agy's conversation file, the `<session>.db` SQLite the cost tally reads too. Its `steps` table
// holds one protobuf blob per step (`step_payload`); the schema is agy's, so only the fields found in real files are
// read, and a step that doesn't decode is skipped. A position in the record is a step's `idx`. Shown are: step type 14,
// what the user typed (field 19, text in 2); type 15, a model turn (field 20: the reply in 1, its thinking in 3, which
// is left out); and type 132, a tool run (field 5.4 names the call and holds its JSON arguments, field 140.2.1 the
// output). Types 17, 23 and 101 (compaction bookkeeping, system notices) are left out. A step is finished at status 3
// (done), 4 (refused), 6 or 7 (failed); any other status (1, 2, 8: pending, running, the model generating) is a step
// whose content changes in place, and a read stops before the earliest of those among the last few steps (parallel
// tool calls finish out of order), so it is read whole later. A turn that was interrupted leaves its step so: once
// the file (`.db` and its `-wal`, where a live conversation writes) has been quiet for `quiet`, it is shown as it is; a step started longer than `quiet` ago that never finished (the CLI dropped it) holds nothing back.

const (
	capTranscript = transcript.Cap

	stepUser, stepModel, stepTool = 14, 15, 132
	// quiet is how long a file must be unwritten before its running steps are taken as ended (a tool may run for
	// minutes without a write).
	quiet = 10 * time.Minute
	// tailSteps is how far back from the newest step a running one holds a read back.
	tailSteps    = 30
	maxSteps     = 200
	maxBackSteps = 2000
)

// statusDone is a step that ended with its result; 4, 6 and 7 are ones that ended without it (refused by a hook, failed).
const statusDone = 3

// finished reports whether a step's content is settled: anything but done and the failures is still changing.
func finished(status int) bool {
	return status == statusDone || status == 4 || status == 6 || status == 7
}

// toolSummaryKeys are agy's tools' arguments that say what a call is about, in the order they are tried.
var toolSummaryKeys = []string{"CommandLine", "Path", "TargetFile", "AbsolutePath", "query", "Url", "ToolName", "toolSummary", "Action"}

// ReadTranscript reads a page of the session's conversation for the chat view.
func (a *Adapter) ReadTranscript(_ context.Context, req *connect.Request[adapterv1.ReadTranscriptRequest]) (*connect.Response[adapterv1.ReadTranscriptResponse], error) {
	m := req.Msg
	dir := ConversationsDir(m.Env)
	if dir == "" || m.SessionId == "" || strings.ContainsAny(m.SessionId, `/\`) || m.SessionId == ".." {
		return connect.NewResponse(&adapterv1.ReadTranscriptResponse{AtStart: true}), nil
	}
	db, err := openConversation(filepath.Join(dir, m.SessionId+".db"))
	if err != nil {
		// No file yet is a conversation that hasn't started.
		return connect.NewResponse(&adapterv1.ReadTranscriptResponse{AtStart: true}), nil
	}
	defer db.Close()
	live := false
	for _, name := range []string{m.SessionId + ".db", m.SessionId + ".db-wal"} {
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil && time.Since(st.ModTime()) < quiet {
			live = true
		}
	}
	entries, start, end, atStart, err := readSteps(db, m.Offset, m.Backward, live)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&adapterv1.ReadTranscriptResponse{Entries: entries, StartOffset: start, EndOffset: end, AtStart: atStart}), nil
}

func openConversation(dbPath string) (*sql.DB, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, err
	}
	uriPath := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(filepath.Clean(dbPath))
	return sql.Open("sqlite", "file:"+uriPath+"?mode=ro&_pragma=busy_timeout(2000)")
}

type chatStep struct {
	idx     int64
	typ     int
	status  int
	payload []byte
}

func readSteps(db *sql.DB, offset int64, backward, live bool) ([]*agentmeshv1.TranscriptEntry, int64, int64, bool, error) {
	var last sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(idx) FROM steps`).Scan(&last); err != nil {
		return nil, 0, 0, false, err
	}
	if !last.Valid {
		return nil, 0, 0, true, nil
	}
	// limit is where a read stops: past the last step, or before the earliest recent step still changing.
	limit := last.Int64 + 1
	if live {
		if running, err := earliestRunning(db, last.Int64); err != nil {
			return nil, 0, 0, false, err
		} else if running >= 0 {
			limit = running
		}
	}
	var first int64
	_ = db.QueryRow(`SELECT MIN(idx) FROM steps`).Scan(&first)
	if backward {
		end := offset
		if end < 0 || end > limit {
			end = limit
		}
		var out []*agentmeshv1.TranscriptEntry
		from, scanned := end, 0
		for from > first && len(out) < transcript.WantEntries && scanned < maxBackSteps {
			steps, err := stepsWhere(db, `idx < ? ORDER BY idx DESC LIMIT ?`, from, maxSteps)
			if err != nil {
				return nil, 0, 0, false, err
			}
			if len(steps) == 0 {
				break
			}
			for _, s := range steps { // newest first; the page keeps the record's order
				out = append(stepEntries(s), out...)
				from = s.idx
				scanned++
				if len(out) >= transcript.WantEntries {
					break
				}
			}
		}
		return out, from, end, from <= first, nil
	}
	if offset < 0 || offset >= limit {
		return nil, limit, limit, false, nil
	}
	steps, err := stepsWhere(db, `idx >= ? AND idx < ? ORDER BY idx ASC LIMIT ?`, offset, limit, maxSteps)
	if err != nil {
		return nil, 0, 0, false, err
	}
	var out []*agentmeshv1.TranscriptEntry
	end := offset
	for _, s := range steps {
		out = append(out, stepEntries(s)...)
		end = s.idx + 1
	}
	if len(steps) < maxSteps {
		end = limit
	}
	return out, offset, end, offset <= first, nil
}

// earliestRunning is the idx of the earliest step among the last tailSteps that is still changing and was started
// less than `quiet` ago, or -1. An older one is a step the CLI never finished (before a compaction, or a background
// command it dropped): it would hold the read back for as long as the conversation goes on.
func earliestRunning(db *sql.DB, last int64) (int64, error) {
	rows, err := db.Query(`SELECT idx, step_payload FROM steps WHERE status NOT IN (3, 4, 6, 7) AND idx > ? ORDER BY idx`, last-tailSteps)
	if err != nil {
		return -1, err
	}
	defer rows.Close()
	for rows.Next() {
		var idx int64
		var payload []byte
		if err := rows.Scan(&idx, &payload); err != nil {
			return -1, err
		}
		sec := varintAt(bytesAt(payload, 5, 1), 1)
		if sec == 0 || time.Since(time.Unix(int64(sec), 0)) < quiet {
			return idx, nil
		}
	}
	return -1, rows.Err()
}

func stepsWhere(db *sql.DB, where string, args ...any) ([]chatStep, error) {
	rows, err := db.Query(`SELECT idx, step_type, status, step_payload FROM steps WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []chatStep
	for rows.Next() {
		var s chatStep
		if err := rows.Scan(&s.idx, &s.typ, &s.status, &s.payload); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// stepEntries is what one step shows (none for a step that isn't part of the conversation or doesn't decode).
func stepEntries(s chatStep) []*agentmeshv1.TranscriptEntry {
	var at *timestamppb.Timestamp
	if sec := varintAt(bytesAt(s.payload, 5, 1), 1); sec > 0 {
		at = timestamppb.New(time.Unix(int64(sec), 0))
	}
	var out []*agentmeshv1.TranscriptEntry
	add := func(i int, e *agentmeshv1.TranscriptEntry) {
		e.Id, e.Time, e.Offset = fmt.Sprintf("%d:%d", s.idx, i), at, s.idx
		e.Text, e.Truncated = transcript.Cut(e.Text, transcript.MaxText)
		out = append(out, e)
	}
	switch s.typ {
	case stepUser:
		text := strings.TrimSpace(string(bytesAt(s.payload, 19, 2)))
		switch {
		case text == "":
		case strings.HasPrefix(text, transcript.DoorbellMark):
			line, _, _ := strings.Cut(text, "\n")
			add(0, &agentmeshv1.TranscriptEntry{Kind: agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_MESH, Text: line})
		default:
			add(0, &agentmeshv1.TranscriptEntry{Kind: agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_USER, Text: text})
		}
	case stepModel:
		text := bytesAt(s.payload, 20, 1)
		if len(text) == 0 {
			text = bytesAt(s.payload, 20, 8)
		}
		if t := strings.TrimSpace(string(text)); t != "" {
			add(0, &agentmeshv1.TranscriptEntry{Kind: agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_ASSISTANT, Text: t})
		}
	case stepTool:
		call := bytesAt(s.payload, 5, 4)
		name, id, args := string(bytesAt(call, 2)), string(bytesAt(call, 1)), bytesAt(call, 3)
		if name == "" {
			name = "tool"
		}
		if id == "" {
			id = fmt.Sprintf("step-%d", s.idx)
		}
		add(0, &agentmeshv1.TranscriptEntry{Kind: agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_TOOL_CALL, Tool: name, ToolInput: transcript.SummarizeKeys(args, toolSummaryKeys...), ToolUseId: id})
		if finished(s.status) {
			text := string(bytesAt(s.payload, 140, 2, 1))
			failed := s.status != statusDone
			if text == "" && failed {
				// A refused or failed call has no output: its error is in field 31 (3, or 2 when that is the text).
				if text = string(bytesAt(s.payload, 31, 3)); text == "" {
					if t := bytesAt(s.payload, 31, 2); utf8.Valid(t) && !strings.ContainsAny(string(t), "\x00\x01\x02\x03\x04\x05\x06\x07\x08") {
						text = string(t)
					}
				}
			}
			add(1, &agentmeshv1.TranscriptEntry{Kind: agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_TOOL_RESULT, Text: text, ToolUseId: id, IsError: failed})
		}
	}
	return out
}

// bytesAt follows a path of field numbers through nested messages and returns the first value found at the end (nil
// when a field is missing or the bytes aren't a message).
func bytesAt(b []byte, path ...protowire.Number) []byte {
	for _, num := range path {
		var next []byte
		for len(b) > 0 {
			n, typ, tl := protowire.ConsumeTag(b)
			if tl < 0 {
				return nil
			}
			b = b[tl:]
			vl := protowire.ConsumeFieldValue(n, typ, b)
			if vl < 0 {
				return nil
			}
			if n == num && typ == protowire.BytesType {
				v, _ := protowire.ConsumeBytes(b)
				next = v
				break
			}
			b = b[vl:]
		}
		if next == nil {
			return nil
		}
		b = next
	}
	return b
}

// varintAt is the first varint field num of the message b (0 when there is none).
func varintAt(b []byte, num protowire.Number) uint64 {
	for len(b) > 0 {
		n, typ, tl := protowire.ConsumeTag(b)
		if tl < 0 {
			return 0
		}
		b = b[tl:]
		vl := protowire.ConsumeFieldValue(n, typ, b)
		if vl < 0 {
			return 0
		}
		if n == num && typ == protowire.VarintType {
			v, _ := protowire.ConsumeVarint(b)
			return v
		}
		b = b[vl:]
	}
	return 0
}
