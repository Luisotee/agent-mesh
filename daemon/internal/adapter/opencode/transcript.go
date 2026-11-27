package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	agentmeshv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/transcript"
)

// The chat view reads OpenCode's own record, the SQLite database the status code reads too, and not its HTTP API: it
// needs no `--port`, and it answers for a stopped agent. A position in the record is a message's rowid (the table
// only grows at its end), so a page's `start_offset` is the first message it shows and `end_offset` the rowid after the
// last. An assistant message is shown once it is finished, because its parts change in place while the model works
// (a tool call goes from pending to completed): the last message of a session that has no `time.completed` yet is the
// one being written, and is left for a later read.

const (
	capTranscript = transcript.Cap
	// maxMessages is how many messages one forward read, or one batch of a backward read, covers.
	maxMessages = 100
	// maxBackMessages bounds a backward read that goes on looking for entries.
	maxBackMessages = 1000
)

type chatMessage struct {
	Role string `json:"role"`
	Time struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
}

type chatPart struct {
	Type      string        `json:"type"`
	Text      string        `json:"text"`
	Synthetic bool          `json:"synthetic"`
	Ignored   bool          `json:"ignored"`
	Tool      string        `json:"tool"`
	CallID    string        `json:"callID"`
	State     chatToolState `json:"state"`
}

type chatToolState struct {
	Status string          `json:"status"`
	Input  json.RawMessage `json:"input"`
	Output string          `json:"output"`
	Error  string          `json:"error"`
}

// ReadTranscript reads a page of the session's conversation for the chat view.
func (a *Adapter) ReadTranscript(_ context.Context, req *connect.Request[adapterv1.ReadTranscriptRequest]) (*connect.Response[adapterv1.ReadTranscriptResponse], error) {
	m := req.Msg
	db, err := openDB(a.DBPath(m.Env))
	if err != nil || m.SessionId == "" {
		// No database yet is a conversation that hasn't started.
		return connect.NewResponse(&adapterv1.ReadTranscriptResponse{AtStart: true}), nil
	}
	defer db.Close()
	entries, start, end, atStart, err := readTranscript(db, m.SessionId, m.Offset, m.Backward)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&adapterv1.ReadTranscriptResponse{Entries: entries, StartOffset: start, EndOffset: end, AtStart: atStart}), nil
}

type rowMessage struct {
	rowid int64
	id    string
	data  string
}

func readTranscript(db *sql.DB, session string, offset int64, backward bool) ([]*agentmeshv1.TranscriptEntry, int64, int64, bool, error) {
	var maxRow sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(rowid) FROM message WHERE session_id = ?`, session).Scan(&maxRow); err != nil {
		return nil, 0, 0, false, err
	}
	if !maxRow.Valid {
		return nil, 0, 0, true, nil
	}
	// limit is where a read stops: past the last message, or before it while it is being written.
	limit := maxRow.Int64 + 1
	var lastData string
	if err := db.QueryRow(`SELECT data FROM message WHERE rowid = ?`, maxRow.Int64).Scan(&lastData); err == nil {
		var last chatMessage
		if json.Unmarshal([]byte(lastData), &last) == nil && last.Role == "assistant" && last.Time.Completed == 0 {
			limit = maxRow.Int64
		}
	}
	if backward {
		return readBack(db, session, offset, limit)
	}
	if offset < 0 || offset >= limit {
		return nil, limit, limit, false, nil
	}
	msgs, err := messagesFrom(db, session, `rowid >= ? AND rowid < ? ORDER BY rowid ASC LIMIT ?`, offset, limit, maxMessages)
	if err != nil {
		return nil, 0, 0, false, err
	}
	var out []*agentmeshv1.TranscriptEntry
	end := offset
	for _, m := range msgs {
		es, err := messageEntries(db, m)
		if err != nil {
			return nil, 0, 0, false, err
		}
		out = append(out, es...)
		end = m.rowid + 1
	}
	if len(msgs) < maxMessages {
		// Everything up to the limit was read (the rowids of other sessions' messages count too).
		end = limit
	}
	return out, offset, end, offset <= firstRow(db, session), nil
}

func readBack(db *sql.DB, session string, offset, limit int64) ([]*agentmeshv1.TranscriptEntry, int64, int64, bool, error) {
	end := offset
	if end < 0 || end > limit {
		end = limit
	}
	var out []*agentmeshv1.TranscriptEntry
	first, scanned := end, 0
	for first > firstRow(db, session) && len(out) < transcript.WantEntries && scanned < maxBackMessages {
		msgs, err := messagesFrom(db, session, `rowid < ? ORDER BY rowid DESC LIMIT ?`, first, maxMessages)
		if err != nil {
			return nil, 0, 0, false, err
		}
		if len(msgs) == 0 {
			break
		}
		// msgs run newest first; the page keeps the record's order.
		for _, m := range msgs {
			es, err := messageEntries(db, m)
			if err != nil {
				return nil, 0, 0, false, err
			}
			out = append(es, out...)
			first = m.rowid
			scanned++
			if len(out) >= transcript.WantEntries {
				break
			}
		}
	}
	return out, first, end, first <= firstRow(db, session), nil
}

func firstRow(db *sql.DB, session string) int64 {
	var r sql.NullInt64
	_ = db.QueryRow(`SELECT MIN(rowid) FROM message WHERE session_id = ?`, session).Scan(&r)
	return r.Int64
}

func messagesFrom(db *sql.DB, session, where string, args ...any) ([]rowMessage, error) {
	// The bounds are the caller's; the session comes first.
	rows, err := db.Query(`SELECT rowid, id, data FROM message WHERE session_id = ? AND `+where, append([]any{session}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []rowMessage
	for rows.Next() {
		var m rowMessage
		if err := rows.Scan(&m.rowid, &m.id, &m.data); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// messageEntries is what one message shows: its text parts, and for an assistant its tool calls with their results.
func messageEntries(db *sql.DB, m rowMessage) ([]*agentmeshv1.TranscriptEntry, error) {
	var msg chatMessage
	if json.Unmarshal([]byte(m.data), &msg) != nil || (msg.Role != "user" && msg.Role != "assistant") {
		return nil, nil
	}
	rows, err := db.Query(`SELECT id, time_created, data FROM part WHERE message_id = ? ORDER BY rowid ASC`, m.id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*agentmeshv1.TranscriptEntry
	for rows.Next() {
		var id, raw string
		var created int64
		if err := rows.Scan(&id, &created, &raw); err != nil {
			return nil, err
		}
		var p chatPart
		if json.Unmarshal([]byte(raw), &p) != nil {
			continue
		}
		at := timestamppb.New(time.UnixMilli(created))
		add := func(i int, e *agentmeshv1.TranscriptEntry) {
			e.Id, e.Time, e.Offset = fmt.Sprintf("%s:%d", id, i), at, m.rowid
			e.Text, e.Truncated = transcript.Cut(e.Text, transcript.MaxText)
			out = append(out, e)
		}
		switch {
		case p.Type == "text" && !p.Synthetic && !p.Ignored && strings.TrimSpace(p.Text) != "":
			switch {
			case msg.Role == "assistant":
				add(0, &agentmeshv1.TranscriptEntry{Kind: agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_ASSISTANT, Text: p.Text})
			case strings.HasPrefix(strings.TrimSpace(p.Text), transcript.DoorbellMark):
				line, _, _ := strings.Cut(strings.TrimSpace(p.Text), "\n")
				add(0, &agentmeshv1.TranscriptEntry{Kind: agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_MESH, Text: line})
			default:
				add(0, &agentmeshv1.TranscriptEntry{Kind: agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_USER, Text: strings.TrimSpace(p.Text)})
			}
		case p.Type == "tool" && msg.Role == "assistant":
			add(0, &agentmeshv1.TranscriptEntry{Kind: agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_TOOL_CALL, Tool: p.Tool, ToolInput: transcript.Summarize(p.State.Input), ToolUseId: p.CallID})
			switch p.State.Status {
			case "completed":
				add(1, &agentmeshv1.TranscriptEntry{Kind: agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_TOOL_RESULT, Text: p.State.Output, ToolUseId: p.CallID})
			case "error":
				add(1, &agentmeshv1.TranscriptEntry{Kind: agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_TOOL_RESULT, Text: p.State.Error, ToolUseId: p.CallID, IsError: true})
			}
		}
	}
	return out, rows.Err()
}
