package claude

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
)

func reply(in, read, create int, writes string) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"usage":{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"cache_creation":{%s}}}}`,
		in, read, create, writes)
}

func writeTranscript(t *testing.T, home, cwd, sid string, lines ...string) string {
	t.Helper()
	path := TranscriptPath(map[string]string{"HOME": home}, cwd, sid)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func measure(t *testing.T, home, cwd, sid string) *adapterv1.MeasureContextResponse {
	t.Helper()
	res, err := New().MeasureContext(context.Background(), connect.NewRequest(&adapterv1.MeasureContextRequest{
		SessionId: sid, Cwd: cwd, Env: map[string]string{"HOME": home}}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg
}

func TestTranscriptPathReplacesNonAlnum(t *testing.T) {
	got := TranscriptPath(map[string]string{"HOME": "/h"}, "/work/state/worktrees/ri4lh27/repo-capture", "abc")
	if want := "/h/.claude/projects/-work-state-worktrees-ri4lh27-repo-capture/abc.jsonl"; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if got := TranscriptPath(map[string]string{"CLAUDE_CONFIG_DIR": "/c"}, "/a.b", "x"); got != "/c/projects/-a-b/x.jsonl" {
		t.Fatalf("config dir: %s", got)
	}
}

func TestMeasureContextNewestReplySinceCompaction(t *testing.T) {
	home := t.TempDir()
	writeTranscript(t, home, "/w", "s1",
		reply(10, 1000, 500, `"ephemeral_1h_input_tokens":500`),
		`{"type":"user","message":{"content":"hi"}}`,
		reply(5, 2000, 100, `"ephemeral_1h_input_tokens":100`))
	u := measure(t, home, "/w", "s1").Usage
	if !u.Measured || u.Tokens != 2105 || u.TtlSeconds != 3600 || u.Cold {
		t.Fatalf("usage %+v", u)
	}
}

func TestMeasureContextSkipsSubagentAndEmptyReplies(t *testing.T) {
	home := t.TempDir()
	writeTranscript(t, home, "/w", "s1",
		reply(10, 150000, 0, ""),
		`{"type":"assistant","isSidechain":true,"message":{"usage":{"input_tokens":50,"cache_read_input_tokens":300,"cache_creation_input_tokens":0}}}`,
		`{"type":"assistant","message":{"model":"<synthetic>","usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`)
	u := measure(t, home, "/w", "s1").Usage
	if !u.Measured || u.Tokens != 150010 {
		t.Fatalf("a subagent's reply or an empty one set the size: %+v", u)
	}
}

func TestMeasureContextZeroAfterCompactBoundary(t *testing.T) {
	home := t.TempDir()
	writeTranscript(t, home, "/w", "s1",
		reply(10, 100000, 0, ""),
		`{"type":"system","subtype":"compact_boundary"}`)
	u := measure(t, home, "/w", "s1").Usage
	if !u.Measured || u.Tokens != 0 {
		t.Fatalf("usage %+v", u)
	}
}

func TestMeasureContextTTLFromEarlierReplyAndFiveMinutes(t *testing.T) {
	home := t.TempDir()
	writeTranscript(t, home, "/w", "s1",
		reply(1, 10, 20, `"ephemeral_5m_input_tokens":20`),
		reply(1, 30, 0, ""), reply(1, 40, 0, ""))
	u := measure(t, home, "/w", "s1").Usage
	if u.Tokens != 41+0+0 || u.TtlSeconds != 300 {
		t.Fatalf("usage %+v", u)
	}
	// A subagent's reply (sidechain) says nothing about the session's TTL.
	writeTranscript(t, home, "/w", "s2",
		`{"type":"assistant","isSidechain":true,"message":{"usage":{"input_tokens":1,"cache_creation":{"ephemeral_5m_input_tokens":9}}}}`,
		reply(1, 7, 0, ""))
	if u := measure(t, home, "/w", "s2").Usage; u.TtlSeconds != 3600 {
		t.Fatalf("sidechain TTL leaked: %+v", u)
	}
}

func TestMeasureContextColdWhenFileOlderThanTTL(t *testing.T) {
	home := t.TempDir()
	path := writeTranscript(t, home, "/w", "s1", reply(1, 10, 0, `"ephemeral_5m_input_tokens":3`))
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if u := measure(t, home, "/w", "s1").Usage; !u.Cold || u.TtlSeconds != 300 {
		t.Fatalf("5m cache after 10 min: %+v", u)
	}
	// An hour cache is still warm after the same ten minutes.
	path = writeTranscript(t, home, "/w", "s3", reply(1, 10, 0, `"ephemeral_1h_input_tokens":3`))
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if u := measure(t, home, "/w", "s3").Usage; u.Cold {
		t.Fatalf("1h cache after 10 min: %+v", u)
	}
}

func TestMeasureContextReadsAcrossChunks(t *testing.T) {
	home := t.TempDir()
	lines := []string{reply(1, 5, 0, `"ephemeral_1h_input_tokens":5`)}
	for i := 0; i < 40; i++ {
		lines = append(lines, `{"type":"user","message":{"content":"`+strings.Repeat("x", 200)+`"}}`)
	}
	path := writeTranscript(t, home, "/w", "s1", lines...)
	// A chunk much smaller than a line: every line is cut by it.
	tokens, ok, ttl, haveTTL, err := transcriptUsage(path, 64)
	if err != nil || !ok || tokens != 6 || !haveTTL || ttl != time.Hour {
		t.Fatalf("tokens %d ok %v ttl %v %v err %v", tokens, ok, ttl, haveTTL, err)
	}
}

func TestMeasureContextUnmeasured(t *testing.T) {
	home := t.TempDir()
	if u := measure(t, home, "/w", "missing").Usage; u.Measured {
		t.Fatalf("a missing file is unmeasured: %+v", u)
	}
	writeTranscript(t, home, "/w", "empty", `{"type":"user","message":{"content":"hi"}}`)
	if u := measure(t, home, "/w", "empty").Usage; u.Measured {
		t.Fatalf("no reply is unmeasured: %+v", u)
	}
}

func TestPlanCompactArgvIsLeanHeadlessAndIdentityFree(t *testing.T) {
	res, err := New().PlanCompact(context.Background(), connect.NewRequest(&adapterv1.PlanCompactRequest{
		Binary: "claude", SessionId: "sid", Model: "opus", Focus: "Keep the plan",
		DenyTools: []string{"Edit(/r/**)", "WebFetch"},
		Lean:      &adapterv1.LeanLaunch{McpConfigPath: "/m.json"},
		Env:       map[string]string{"HOME": "/h", "CLAUDECODE": "1"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Join(res.Msg.Argv, "\x00")
	for _, want := range []string{"claude\x00-p\x00--resume\x00sid\x00--model\x00opus\x00--dangerously-skip-permissions",
		"--disallowedTools\x00Edit(/r/**)\x00WebFetch", "--strict-mcp-config\x00--mcp-config\x00/m.json", `"promptCacheTtl":"5m"`} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv lacks %q: %q", want, res.Msg.Argv)
		}
	}
	if last := res.Msg.Argv[len(res.Msg.Argv)-1]; last != "/compact Keep the plan" {
		t.Errorf("prompt %q", last)
	}
	if _, ok := res.Msg.Env["CLAUDECODE"]; ok || res.Msg.Env["HOME"] != "/h" {
		t.Errorf("env %v", res.Msg.Env)
	}
}

func TestUserPromptByMesh(t *testing.T) {
	decode := func(prompt string) bool {
		res, _ := New().DecodeHook(context.Background(), connect.NewRequest(&adapterv1.DecodeHookRequest{
			EventName: "UserPromptSubmit", Payload: []byte(fmt.Sprintf(`{"session_id":"s","hook_event_name":"UserPromptSubmit","prompt":%q}`, prompt))}))
		return res.Msg.Events[0].GetTurnStarted().ByMesh
	}
	if !decode("📬 [mesh] You are x (worker) …") || decode("fix the bug") {
		t.Fatal("by_mesh must follow the doorbell prefix")
	}
}
