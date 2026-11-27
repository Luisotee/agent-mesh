package opencode

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	agentmeshv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// The rows have the shape of a real OpenCode 1.19.6 opencode.db (message and part, a JSON `data` each); the text,
// ids and paths are made up.
func seedChat(t *testing.T) (dbPath string) {
	t.Helper()
	dbPath = filepath.Join(t.TempDir(), "opencode.db")
	seedTestDB(t, dbPath, false, func(db *sql.DB) {
		if _, err := db.Exec(`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`); err != nil {
			t.Fatal(err)
		}
		msg := func(id, session, data string) {
			if _, err := db.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, 1795085239858, 1795085240546, ?)`, id, session, data); err != nil {
				t.Fatal(err)
			}
		}
		part := func(id, message, data string) {
			if _, err := db.Exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, 'ses_a', 1795085239866, 1795085239867, ?)`, id, message, data); err != nil {
				t.Fatal(err)
			}
		}
		user := `{"role":"user","time":{"created":1795085239858},"agent":"build","model":{"providerID":"opencode","modelID":"big-pickle"}}`
		done := `{"role":"assistant","time":{"created":1795085240463,"completed":1795085243134},"finish":"stop"}`
		msg("msg_1", "ses_a", user)
		part("prt_1", "msg_1", `{"type":"text","text":"List the files"}`)
		msg("msg_2", "ses_a", done)
		part("prt_2a", "msg_2", `{"snapshot":"abc","type":"step-start"}`)
		part("prt_2b", "msg_2", `{"type":"reasoning","text":"hidden thought","time":{"start":1,"end":2}}`)
		part("prt_2c", "msg_2", `{"type":"text","text":"Listing them."}`)
		part("prt_2d", "msg_2", `{"type":"tool","tool":"bash","callID":"c1","state":{"status":"completed","input":{"command":"ls","description":"List"},"output":"a.txt\nb.txt","title":"ls","time":{"start":1,"end":2}}}`)
		part("prt_2e", "msg_2", `{"type":"tool","tool":"bash","callID":"c2","state":{"status":"error","input":{"command":"git push origin main"},"error":"denied by a rule","time":{"start":1,"end":2}}}`)
		part("prt_2f", "msg_2", `{"type":"step-finish","reason":"stop","tokens":{"total":1}}`)
		msg("msg_3", "ses_a", user)
		part("prt_3", "msg_3", "{\"type\":\"text\",\"text\":\"📬 [mesh] task from parent (ref x1): run `mesh inbox`\\n\"}")
		part("prt_3s", "msg_3", `{"type":"text","text":"synthetic context","synthetic":true}`)
		// Another session's messages sit in the same table.
		msg("msg_o", "ses_other", user)
		part("prt_o", "msg_o", `{"type":"text","text":"not ours"}`)
		// The reply being written: no time.completed on the session's last message.
		msg("msg_4", "ses_a", `{"role":"assistant","time":{"created":1795085250000}}`)
		if _, err := db.Exec(`UPDATE message SET time_updated = ? WHERE id = 'msg_4'`, time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
		part("prt_4", "msg_4", `{"type":"text","text":"half an ans"}`)
	})
	return dbPath
}

func readChat(t *testing.T, dbPath string, offset int64, backward bool) *adapterv1.ReadTranscriptResponse {
	t.Helper()
	res, err := New().ReadTranscript(context.Background(), connect.NewRequest(&adapterv1.ReadTranscriptRequest{
		SessionId: "ses_a", Env: map[string]string{"AGENT_MESH_OPENCODE_DB": dbPath}, Offset: offset, Backward: backward}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg
}

func kinds(es []*agentmeshv1.TranscriptEntry) string {
	var out []string
	for _, e := range es {
		out = append(out, strings.TrimPrefix(e.Kind.String(), "TRANSCRIPT_KIND_")+":"+e.Text+e.Tool)
	}
	return strings.Join(out, " | ")
}

func TestTranscriptShowsTheSessionsConversation(t *testing.T) {
	db := seedChat(t)
	res := readChat(t, db, 0, false)
	want := "USER:List the files | ASSISTANT:Listing them. | TOOL_CALL:bash | TOOL_RESULT:a.txt\nb.txt | TOOL_CALL:bash | TOOL_RESULT:denied by a rule | " +
		"MESH:📬 [mesh] task from parent (ref x1): run `mesh inbox`"
	if got := kinds(res.Entries); got != want {
		t.Fatalf("entries:\n got  %s\n want %s", got, want)
	}
	if !res.AtStart || res.StartOffset != 0 {
		t.Errorf("start %d at_start %v", res.StartOffset, res.AtStart)
	}
	// The message being written (rowid 5) is not read yet, and the end offset stops before it.
	if res.EndOffset != 5 {
		t.Errorf("end = %d, want 5 (the rowid of the message being written)", res.EndOffset)
	}
	call, result := res.Entries[2], res.Entries[3]
	if call.ToolInput != "ls" || call.ToolUseId != "c1" || result.ToolUseId != "c1" || result.IsError || !res.Entries[5].IsError {
		t.Errorf("tool call %+v result %+v", call, result)
	}
	if more := readChat(t, db, res.EndOffset, false); len(more.Entries) != 0 {
		t.Errorf("the half-written reply was read: %s", kinds(more.Entries))
	}
}

func TestTranscriptReadsOnWhenTheReplyFinishes(t *testing.T) {
	db := seedChat(t)
	first := readChat(t, db, 0, false)
	seed := openForTest(t, db)
	if _, err := seed.Exec(`UPDATE message SET data = ? WHERE id = 'msg_4'`, `{"role":"assistant","time":{"created":1795085250000,"completed":1795085251000}}`); err != nil {
		t.Fatal(err)
	}
	more := readChat(t, db, first.EndOffset, false)
	if got := kinds(more.Entries); got != "ASSISTANT:half an ans" {
		t.Errorf("after it finished: %s", got)
	}
}

func TestAnAbortedReplyIsShownOnceItIsLongQuiet(t *testing.T) {
	db := seedChat(t)
	if _, err := openForTest(t, db).Exec(`UPDATE message SET time_updated = ? WHERE id = 'msg_4'`, time.Now().Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	res := readChat(t, db, 0, false)
	if got := kinds(res.Entries); !strings.HasSuffix(got, "ASSISTANT:half an ans") || res.EndOffset != 6 {
		t.Errorf("an hour-old unfinished reply: %s (end %d)", got, res.EndOffset)
	}
}

func openForTest(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestTranscriptPagesBackward(t *testing.T) {
	db := seedChat(t)
	all := readChat(t, db, 0, false)
	// Backward from the end finds the same entries: the page is bounded by entries, so it reaches the start.
	back := readChat(t, db, -1, true)
	if kinds(back.Entries) != kinds(all.Entries) || !back.AtStart || back.EndOffset != all.EndOffset {
		t.Errorf("backward:\n got  %s (start %v end %d)\n want %s", kinds(back.Entries), back.AtStart, back.EndOffset, kinds(all.Entries))
	}
	// Backward from a message in the middle gives what is before it.
	mid := readChat(t, db, 3, true)
	if got := kinds(mid.Entries); !strings.HasPrefix(got, "USER:List the files") || strings.Contains(got, "MESH") || !mid.AtStart {
		t.Errorf("before message 3: %s", got)
	}
}

func TestTranscriptOfNoRecordIsEmpty(t *testing.T) {
	res, err := New().ReadTranscript(context.Background(), connect.NewRequest(&adapterv1.ReadTranscriptRequest{
		SessionId: "ses_a", Env: map[string]string{"AGENT_MESH_OPENCODE_DB": filepath.Join(t.TempDir(), "none.db")}}))
	if err != nil || len(res.Msg.Entries) != 0 || !res.Msg.AtStart {
		t.Errorf("no database: %+v %v", res.Msg, err)
	}
	db := seedChat(t)
	other, err := New().ReadTranscript(context.Background(), connect.NewRequest(&adapterv1.ReadTranscriptRequest{
		SessionId: "ses_none", Env: map[string]string{"AGENT_MESH_OPENCODE_DB": db}}))
	if err != nil || len(other.Msg.Entries) != 0 || !other.Msg.AtStart {
		t.Errorf("unknown session: %+v %v", other.Msg, err)
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
	t.Error(fmt.Sprint("capabilities = ", res.Msg.Capabilities))
}
