package copilot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	agentmeshv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// testdata/events-chat.jsonl has the shape of a real Copilot 1.0.103 events.jsonl (the field names and event types
// are copied from real sessions; the text, ids and paths are made up).
func chatHome(t *testing.T) (home, session string) {
	t.Helper()
	home, session = t.TempDir(), "00000000-0000-4000-8000-0000000000a1"
	raw, err := os.ReadFile("testdata/events-chat.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "session-state", session)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return home, session
}

func read(t *testing.T, home, session string, offset int64, backward bool, maxBytes int64) *adapterv1.ReadTranscriptResponse {
	t.Helper()
	res, err := New().ReadTranscript(context.Background(), connect.NewRequest(&adapterv1.ReadTranscriptRequest{
		SessionId: session, Env: map[string]string{"COPILOT_HOME": home}, Offset: offset, Backward: backward, MaxBytes: maxBytes}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg
}

func TestTranscriptShowsTheConversation(t *testing.T) {
	home, session := chatHome(t)
	res := read(t, home, session, 0, false, 0)
	type row struct {
		kind agentmeshv1.TranscriptKind
		text string
		tool string
		err  bool
	}
	var got []row
	for _, e := range res.Entries {
		got = append(got, row{e.Kind, e.Text, e.Tool, e.IsError})
	}
	K := agentmeshv1.TranscriptKind_value
	want := []row{
		{agentmeshv1.TranscriptKind(K["TRANSCRIPT_KIND_USER"]), "hello", "", false},
		{agentmeshv1.TranscriptKind(K["TRANSCRIPT_KIND_ASSISTANT"]), "I'll look at the repo.", "", false},
		{agentmeshv1.TranscriptKind(K["TRANSCRIPT_KIND_TOOL_CALL"]), "", "bash", false},
		{agentmeshv1.TranscriptKind(K["TRANSCRIPT_KIND_TOOL_RESULT"]), "total 0\nREADME.md", "", false},
		{agentmeshv1.TranscriptKind(K["TRANSCRIPT_KIND_TOOL_CALL"]), "", "view", false},
		{agentmeshv1.TranscriptKind(K["TRANSCRIPT_KIND_TOOL_CALL"]), "", "task", false},
		{agentmeshv1.TranscriptKind(K["TRANSCRIPT_KIND_TOOL_RESULT"]), "Path does not exist", "", true},
		{agentmeshv1.TranscriptKind(K["TRANSCRIPT_KIND_ASSISTANT"]), "All done.", "", false},
		{agentmeshv1.TranscriptKind(K["TRANSCRIPT_KIND_MESH"]), "📬 [mesh] task from parent (ref a1b2c3): run `mesh inbox`", "", false},
	}
	if len(got) != len(want) {
		t.Fatalf("entries = %+v, want %d", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if !res.AtStart || res.EndOffset == 0 {
		t.Errorf("at_start %v end %d", res.AtStart, res.EndOffset)
	}
	call := res.Entries[2]
	if call.ToolInput != "ls -la" || call.ToolUseId != "call_1" || res.Entries[3].ToolUseId != "call_1" {
		t.Errorf("the call %+v does not tie to its result %+v", call, res.Entries[3])
	}
	ids := map[string]bool{}
	for _, e := range res.Entries {
		if ids[e.Id] {
			t.Errorf("id %q repeats", e.Id)
		}
		ids[e.Id] = true
	}
}

func TestTranscriptPagesBackAndForward(t *testing.T) {
	home, session := chatHome(t)
	all := read(t, home, session, 0, false, 0)
	// A window of a few lines, read back from the end, then the rest back to the start: the same entries as one read.
	var back []string
	offset := int64(-1)
	for i := 0; i < 50; i++ {
		page := read(t, home, session, offset, true, 600)
		for j := len(page.Entries) - 1; j >= 0; j-- {
			back = append([]string{page.Entries[j].Id}, back...)
		}
		if page.AtStart {
			break
		}
		offset = page.StartOffset
	}
	var want []string
	for _, e := range all.Entries {
		want = append(want, e.Id)
	}
	if strings.Join(back, ",") != strings.Join(want, ",") {
		t.Errorf("backward pages = %v, want %v", back, want)
	}
	// Reading on from the end finds nothing, and a line still being written is left for the next read.
	if more := read(t, home, session, all.EndOffset, false, 0); len(more.Entries) != 0 || more.EndOffset != all.EndOffset {
		t.Errorf("past the end: %+v", more)
	}
	f, err := os.OpenFile(filepath.Join(home, "session-state", session, "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString(`{"type":"user.message","data":{"content":"later`)
	if more := read(t, home, session, all.EndOffset, false, 0); len(more.Entries) != 0 || more.EndOffset != all.EndOffset {
		t.Errorf("a half-written line was read: %+v", more)
	}
	f.WriteString("\"},\"id\":\"e99\",\"timestamp\":\"2026-10-25T07:00:00Z\"}\n")
	if more := read(t, home, session, all.EndOffset, false, 0); len(more.Entries) != 1 || more.Entries[0].Text != "later" {
		t.Errorf("the finished line: %+v", more)
	}
}

func TestTranscriptOfNoRecordIsEmpty(t *testing.T) {
	home := t.TempDir()
	if res := read(t, home, "00000000-0000-4000-8000-0000000000a2", 0, false, 0); len(res.Entries) != 0 || !res.AtStart {
		t.Errorf("a session with no file: %+v", res)
	}
	// A session id that names a path is never opened.
	if res := read(t, home, "../x", 0, false, 0); len(res.Entries) != 0 || !res.AtStart {
		t.Errorf("a path as a session id: %+v", res)
	}
}

func TestDescribeAdvertisesTranscript(t *testing.T) {
	res, err := New().Describe(context.Background(), connect.NewRequest(&adapterv1.DescribeRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Msg.Capabilities {
		if c == "transcript" {
			return
		}
	}
	t.Errorf("capabilities = %v", res.Msg.Capabilities)
}
