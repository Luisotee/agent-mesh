package copilot

import (
	"context"
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

// The chat view reads Copilot's own record of a session, the `events.jsonl` the status code reads too: one event per
// line. Shown are what the user typed (`user.message`), what the model said and the tools it called
// (`assistant.message`) and their answers (`tool.execution_complete`). A subagent's own lines (they carry a
// `parentToolCallId`, or a `source` naming the agent that sent the prompt), reasoning and the session's bookkeeping
// events are left out.

const capTranscript = transcript.Cap

type chatData struct {
	Content          string `json:"content"`
	ParentToolCallID string `json:"parentToolCallId"`
	Source           string `json:"source"`
	ToolCallID       string `json:"toolCallId"`
	ToolRequests     []struct {
		ToolCallID string          `json:"toolCallId"`
		Name       string          `json:"name"`
		Arguments  json.RawMessage `json:"arguments"`
	} `json:"toolRequests"`
	Success *bool `json:"success"`
	Error   struct {
		Message string `json:"message"`
	} `json:"error"`
	Result struct {
		Content string `json:"content"`
	} `json:"result"`
}

// TranscriptEntries turns one line of events.jsonl, which starts at offset, into the entries it shows.
func TranscriptEntries(line []byte, offset int64) []*agentmeshv1.TranscriptEntry {
	var ev Event
	if json.Unmarshal(line, &ev) != nil {
		return nil
	}
	switch ev.Type {
	case "user.message", "assistant.message", "tool.execution_complete":
	default:
		return nil
	}
	var d chatData
	if json.Unmarshal(ev.Data, &d) != nil || d.ParentToolCallID != "" || strings.HasPrefix(d.Source, "agent-") {
		return nil
	}
	var at *timestamppb.Timestamp
	if t, err := time.Parse(time.RFC3339Nano, ev.Timestamp); err == nil {
		at = timestamppb.New(t)
	}
	var out []*agentmeshv1.TranscriptEntry
	add := func(i int, e *agentmeshv1.TranscriptEntry) {
		e.Id = ev.ID
		if e.Id == "" {
			e.Id = fmt.Sprintf("%d", offset)
		}
		e.Id += fmt.Sprintf(":%d", i)
		e.Time, e.Offset = at, offset
		e.Text, e.Truncated = transcript.Cut(e.Text, transcript.MaxText)
		out = append(out, e)
	}
	switch ev.Type {
	case "user.message":
		text := strings.TrimSpace(d.Content)
		switch {
		case text == "":
		case strings.HasPrefix(text, transcript.DoorbellMark):
			line, _, _ := strings.Cut(text, "\n")
			add(0, &agentmeshv1.TranscriptEntry{Kind: agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_MESH, Text: line})
		default:
			add(0, &agentmeshv1.TranscriptEntry{Kind: agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_USER, Text: text})
		}
	case "assistant.message":
		i := 0
		if strings.TrimSpace(d.Content) != "" {
			add(i, &agentmeshv1.TranscriptEntry{Kind: agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_ASSISTANT, Text: d.Content})
			i++
		}
		for _, r := range d.ToolRequests {
			add(i, &agentmeshv1.TranscriptEntry{Kind: agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_TOOL_CALL, Tool: r.Name, ToolInput: transcript.Summarize(r.Arguments), ToolUseId: r.ToolCallID})
			i++
		}
	case "tool.execution_complete":
		failed := d.Success != nil && !*d.Success
		text := d.Result.Content
		if failed && text == "" {
			text = d.Error.Message
		}
		add(0, &agentmeshv1.TranscriptEntry{Kind: agentmeshv1.TranscriptKind_TRANSCRIPT_KIND_TOOL_RESULT, Text: text, ToolUseId: d.ToolCallID, IsError: failed})
	}
	return out
}

// ReadTranscript reads a page of the session's events.jsonl for the chat view.
func (a *Adapter) ReadTranscript(_ context.Context, req *connect.Request[adapterv1.ReadTranscriptRequest]) (*connect.Response[adapterv1.ReadTranscriptResponse], error) {
	m := req.Msg
	if m.SessionId == "" || strings.ContainsAny(m.SessionId, `/\`) || m.SessionId == ".." {
		return connect.NewResponse(&adapterv1.ReadTranscriptResponse{AtStart: true}), nil
	}
	entries, start, end, atStart, err := transcript.ReadJSONL(EventsPath(m.Env, m.SessionId), TranscriptEntries, m.Offset, m.Backward, m.MaxBytes)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&adapterv1.ReadTranscriptResponse{Entries: entries, StartOffset: start, EndOffset: end, AtStart: atStart}), nil
}
