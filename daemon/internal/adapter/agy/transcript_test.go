package agy

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	agentmeshv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// The step payloads have the layout found in real agy 1.5.17 conversation files (which fields hold the user's text,
// the reply, the tool call and its output); the texts, ids and paths are made up.
func str(num int, s string) []byte { return pbBytes(num, []byte(s)) }

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func head(typ int, sec uint64, rest ...[]byte) []byte {
	return cat(pbInt(1, uint64(typ)), pbBytes(5, cat(pbBytes(1, pbInt(1, sec)))), cat(rest...))
}

func userStep(sec uint64, text string) []byte {
	return head(14, sec, pbBytes(19, cat(str(2, text), str(3, text))))
}

func modelStep(sec uint64, reply, thinking string) []byte {
	return head(15, sec, pbBytes(20, cat(str(1, reply), str(3, thinking), str(8, reply))))
}

func toolStep(typ int, sec uint64, id, name, args, output string) []byte {
	call := cat(str(1, id), str(2, name), str(3, args))
	return cat(pbInt(1, uint64(typ)), pbBytes(5, cat(pbBytes(1, pbInt(1, sec)), pbBytes(4, call))),
		pbBytes(140, cat(str(1, "args"), pbBytes(2, str(1, output)))))
}

func chatSteps() []chatStepSeed {
	return []chatStepSeed{
		{14, 3, userStep(1792820600, "Plan the change")},
		{15, 3, modelStep(1792820601, "", "I should look at the repo first.")}, // thinking only: nothing to show
		{132, 3, toolStep(132, 1792820602, "toolu_1", "run_command", `{"CommandLine":"git status","WaitMsBeforeAsync":"5000","toolSummary":"Check the tree"}`, "Command exited with code 0.\n\nclean")},
		{101, 3, head(101, 1792820603, str(1, "[Message] system notice"))},
		{132, 6, toolStep(132, 1792820604, "toolu_2", "read_file", `{"Path":"/work/missing.go"}`, "no such file")},
		{15, 3, modelStep(1792820605, "Done: the tree is clean.", "thinking")},
		{14, 3, userStep(1792820606, "📬 [mesh] task from parent (ref a1): run `mesh inbox`\n")},
		{17, 3, head(17, 1792820607, str(1, "compaction summary"))},
		{132, 2, toolStep(132, 1792820608, "toolu_3", "run_command", `{"CommandLine":"sleep 100"}`, "")},
	}
}

type chatStepSeed struct {
	typ, status int
	payload     []byte
}

func writeChat(t *testing.T, dir, id string, seeds []chatStepSeed) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, id+".db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS steps (idx INTEGER PRIMARY KEY, step_type INTEGER NOT NULL DEFAULT 0, status INTEGER NOT NULL DEFAULT 0, metadata BLOB, step_payload BLOB)`); err != nil {
		t.Fatal(err)
	}
	for i, s := range seeds {
		if _, err := db.Exec(`INSERT OR REPLACE INTO steps (idx, step_type, status, step_payload) VALUES (?,?,?,?)`, i, s.typ, s.status, s.payload); err != nil {
			t.Fatal(err)
		}
	}
}

func readChat(t *testing.T, dir, id string, offset int64, backward bool) *adapterv1.ReadTranscriptResponse {
	t.Helper()
	res, err := New().ReadTranscript(context.Background(), connect.NewRequest(&adapterv1.ReadTranscriptRequest{
		SessionId: id, Env: map[string]string{"AGENT_MESH_AGY_CONVERSATIONS": dir}, Offset: offset, Backward: backward}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg
}

func shown(es []*agentmeshv1.TranscriptEntry) string {
	var out []string
	for _, e := range es {
		out = append(out, strings.TrimPrefix(e.Kind.String(), "TRANSCRIPT_KIND_")+":"+e.Tool+e.ToolInput+e.Text)
	}
	return strings.Join(out, " | ")
}

func TestTranscriptShowsTheConversation(t *testing.T) {
	dir := t.TempDir()
	writeChat(t, dir, "c1", chatSteps())
	res := readChat(t, dir, "c1", 0, false)
	want := "USER:Plan the change | TOOL_CALL:run_commandgit status | TOOL_RESULT:Command exited with code 0.\n\nclean | " +
		"TOOL_CALL:read_file/work/missing.go | TOOL_RESULT:no such file | ASSISTANT:Done: the tree is clean. | " +
		"MESH:📬 [mesh] task from parent (ref a1): run `mesh inbox`"
	if got := shown(res.Entries); got != want {
		t.Fatalf("entries:\n got  %s\n want %s", got, want)
	}
	// The last step still runs: the read stops before it.
	if res.EndOffset != 8 || !res.AtStart || res.StartOffset != 0 {
		t.Errorf("start %d end %d at_start %v", res.StartOffset, res.EndOffset, res.AtStart)
	}
	if !res.Entries[4].IsError || res.Entries[2].IsError || res.Entries[1].ToolUseId != "toolu_1" || res.Entries[2].ToolUseId != "toolu_1" {
		t.Errorf("tool ties or errors wrong: %+v / %+v", res.Entries[1], res.Entries[4])
	}
	if res.Entries[0].Time.AsTime().Unix() != 1792820600 {
		t.Errorf("time = %v", res.Entries[0].Time.AsTime())
	}
	// Once the step finishes, reading on from the end offset finds it.
	seeds := chatSteps()
	seeds[8].status = 3
	writeChat(t, dir, "c1", seeds)
	if more := readChat(t, dir, "c1", res.EndOffset, false); shown(more.Entries) != "TOOL_CALL:run_commandsleep 100 | TOOL_RESULT:" {
		t.Errorf("after the step finished: %s", shown(more.Entries))
	}
}

func TestAStepLeftRunningByAnInterruptedTurnIsShownOnceTheFileIsQuiet(t *testing.T) {
	dir := t.TempDir()
	writeChat(t, dir, "c1", chatSteps())
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "c1.db"), old, old); err != nil {
		t.Fatal(err)
	}
	res := readChat(t, dir, "c1", 0, false)
	if !strings.HasSuffix(shown(res.Entries), "TOOL_CALL:run_commandsleep 100") || res.EndOffset != 9 {
		t.Errorf("a quiet file's running step: %s (end %d)", shown(res.Entries), res.EndOffset)
	}
}

func TestTranscriptPagesBackward(t *testing.T) {
	dir := t.TempDir()
	writeChat(t, dir, "c1", chatSteps())
	all := readChat(t, dir, "c1", 0, false)
	back := readChat(t, dir, "c1", -1, true)
	if shown(back.Entries) != shown(all.Entries) || !back.AtStart || back.EndOffset != all.EndOffset {
		t.Errorf("backward:\n got  %s (start %d end %d)\n want %s", shown(back.Entries), back.StartOffset, back.EndOffset, shown(all.Entries))
	}
	if mid := readChat(t, dir, "c1", 5, true); !strings.HasPrefix(shown(mid.Entries), "USER:Plan the change") || strings.Contains(shown(mid.Entries), "ASSISTANT") {
		t.Errorf("before step 5: %s", shown(mid.Entries))
	}
}

func TestTranscriptOfNoRecordIsEmpty(t *testing.T) {
	dir := t.TempDir()
	if res := readChat(t, dir, "none", 0, false); len(res.Entries) != 0 || !res.AtStart {
		t.Errorf("no file: %+v", res)
	}
	if res := readChat(t, dir, "../x", 0, false); len(res.Entries) != 0 || !res.AtStart {
		t.Errorf("a path as a session id: %+v", res)
	}
	writeChat(t, dir, "c2", []chatStepSeed{{14, 3, []byte{0xff, 0xff}}}) // not protobuf: skipped
	if res := readChat(t, dir, "c2", 0, false); len(res.Entries) != 0 {
		t.Errorf("an undecodable step showed %s", shown(res.Entries))
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
