package agy

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	_ "modernc.org/sqlite"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	agentmeshv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

const sampleListing = `gemini-4-neon-high	Gemini 4 Neon (High)
gemini-4-neon-low	Gemini 4 Neon (Low)
gemini-4-hydrogen-high	Gemini 4 Hydrogen (High)
gemini-3.8-flash-low	Gemini 3.8 Flash (Low)
gemini-3.10-flash-low	Gemini 3.10 Flash (Low)
claude-opus-5-5-high	Claude Opus 5.5 (High)
gpt-oss-120b-medium	GPT-OSS 120B (Medium)
`

func TestPickModelResolvesNewestVersionAndExactIDs(t *testing.T) {
	cases := []struct {
		family, effort, want string
	}{
		{"flash", "low", "gemini-3.10-flash-low"},
		{"neon", "high", "gemini-4-neon-high"},
		{"opus", "high", "claude-opus-5-5-high"},
		{"gpt-oss-120b-medium", "high", "gpt-oss-120b-medium"},
	}
	for _, c := range cases {
		got, err := PickModel(sampleListing, c.family, c.effort)
		if err != nil || got != c.want {
			t.Errorf("PickModel(%q, %q) = %q, %v; want %q", c.family, c.effort, got, err, c.want)
		}
	}
	if _, err := PickModel(sampleListing, "missing", "high"); err == nil {
		t.Error("expected error for unknown family")
	}
}

func TestPlanLaunchFreshResumeDenyAndEnv(t *testing.T) {
	a := New()
	a.ListModels = func(context.Context, string, map[string]string) (string, error) { return sampleListing, nil }
	cwd, repo := t.TempDir(), t.TempDir()

	res, err := a.PlanLaunch(context.Background(), connect.NewRequest(&adapterv1.PlanLaunchRequest{
		AgentId: "a1", Role: agentmeshv1.AgentRole_AGENT_ROLE_WORKER, Cwd: cwd, Repo: repo,
		Binary: "/opt/agy", Model: "hydrogen", Effort: "high", SkipPermissions: true,
		HookArgv: []string{"/bin/mesh", "hook", "agy", "--state", "/tmp/st", "--agent", "a1"},
		Env:      map[string]string{"PATH": "/bin", "ANTIGRAVITY_AGENT": "1", "ANTIGRAVITY_CSRF_TOKEN": "sec"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Join(res.Msg.Argv, "\x00")
	for _, want := range []string{"/opt/agy", "--model\x00gemini-4-hydrogen-high", "--settings", "--dangerously-skip-permissions"} {
		if !strings.Contains(argv, want) {
			t.Errorf("missing %q in %q", want, argv)
		}
	}
	if strings.Contains(argv, "--effort") {
		t.Errorf("agy rejects --effort: %q", argv)
	}
	if _, leaked := res.Msg.Env["ANTIGRAVITY_AGENT"]; leaked || res.Msg.Env["ANTIGRAVITY_CSRF_TOKEN"] != "" || res.Msg.Env["PATH"] != "/bin" {
		t.Errorf("env leaked: %v", res.Msg.Env)
	}
	// Check --settings trustedWorkspaces JSON.
	for i, arg := range res.Msg.Argv {
		if arg == "--settings" {
			var cfg struct {
				TrustedWorkspaces []string `json:"trustedWorkspaces"`
			}
			if err := json.Unmarshal([]byte(res.Msg.Argv[i+1]), &cfg); err != nil || len(cfg.TrustedWorkspaces) == 0 {
				t.Fatalf("bad --settings: %s (%v)", res.Msg.Argv[i+1], err)
			}
		}
	}
	// Worker gets hooks.json without deny flags.
	if len(res.Msg.WorktreeFiles) != 1 || res.Msg.WorktreeFiles[0].Path != ".agents/hooks.json" || !res.Msg.WorktreeFiles[0].GitExclude {
		t.Fatalf("worktree files: %+v", res.Msg.WorktreeFiles)
	}
	if strings.Contains(string(res.Msg.WorktreeFiles[0].Content), "--deny-path") {
		t.Errorf("worker got deny flags: %s", res.Msg.WorktreeFiles[0].Content)
	}

	// Resume + DenyEdits (or read-only role) passes --conversation and adds --deny-path/--deny-publish to PreToolUse.
	res2, err := a.PlanLaunch(context.Background(), connect.NewRequest(&adapterv1.PlanLaunchRequest{
		AgentId: "a1", Role: agentmeshv1.AgentRole_AGENT_ROLE_REVIEWER, Cwd: cwd, Repo: repo,
		ResumeSessionId: "conv-123", Model: "gemini-4-neon-high", Effort: "low",
		HookArgv: []string{"mesh", "hook", "agy", "--state", "/tmp/st", "--agent", "a1"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	argv2 := strings.Join(res2.Msg.Argv, "\x00")
	if !strings.Contains(argv2, "--conversation\x00conv-123") || !strings.Contains(argv2, "--model\x00gemini-4-neon-low") {
		t.Errorf("resume argv: %q", argv2)
	}
	hooksText := string(res2.Msg.WorktreeFiles[0].Content)
	if !strings.Contains(hooksText, "--deny-path") || !strings.Contains(hooksText, "--deny-publish") || !strings.Contains(hooksText, `AGENT_MESH_AGENT`) {
		t.Errorf("reviewer hooks.json missing deny/guard: %s", hooksText)
	}
}

func TestEvaluatePreToolUse(t *testing.T) {
	wt := t.TempDir()
	repo := t.TempDir()
	state := t.TempDir()
	denyPaths := []string{wt, repo}

	// Symlink inside worktree pointing into repo or outside must still be resolved accurately.
	linkOut := filepath.Join(wt, "outlink")
	if err := os.Symlink(state, linkOut); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		payload  string
		wantDeny bool
	}{
		{"write_file inside worktree", `{"cwd":"` + wt + `","toolCall":{"name":"write_file","args":{"Path":"new/a.go"}}}`, true},
		{"edit_file inside repo", `{"cwd":"` + wt + `","toolCall":{"name":"edit_file","args":{"Path":"` + repo + `/main.go"}}}`, true},
		{"traversal out and back into repo", `{"cwd":"` + wt + `","toolCall":{"name":"write_file","args":{"Path":"` + state + `/../` + filepath.Base(repo) + `/x.go"}}}`, true},
		{"write_file in state dir (report/plan) allowed", `{"cwd":"` + wt + `","toolCall":{"name":"write_file","args":{"Path":"` + state + `/plans/p.md"}}}`, false},
		{"read_file in worktree allowed", `{"cwd":"` + wt + `","toolCall":{"name":"read_file","args":{"Path":"README.md"}}}`, false},
		{"apply_patch in worktree denied", `{"cwd":"` + wt + `","toolCall":{"name":"apply_patch","args":{"Patch":"*** Update File: foo.go\n"}}}`, true},
		{"run_command git push denied", `{"cwd":"` + wt + `","toolCall":{"name":"run_command","args":{"CommandLine":"git status && git -C . push origin HEAD"}}}`, true},
		{"run_command gh pr comment denied", `{"cwd":"` + wt + `","toolCall":{"name":"run_command","args":{"CommandLine":"gh pr comment 12 --body hi"}}}`, true},
		{"run_command echo git push allowed", `{"cwd":"` + wt + `","toolCall":{"name":"run_command","args":{"CommandLine":"echo \"do not git push\""}}}`, false},
		{"run_command go test allowed", `{"cwd":"` + wt + `","toolCall":{"name":"run_command","args":{"CommandLine":"go test ./..."}}}`, false},
		{"unparseable payload denied", `{"cwd":"` + wt + `","toolCall":{"name":"write_file","args":{"Path":"a.go","Content":"trunc`, true},
	}
	for _, c := range cases {
		deny, reason := EvaluatePreToolUse([]byte(c.payload), wt, denyPaths, true)
		if deny != c.wantDeny {
			t.Errorf("%s: deny=%v (%q), want %v", c.name, deny, reason, c.wantDeny)
		}
	}
}

func decodeOne(t *testing.T, session, event, body string) []*adapterv1.AgentEvent {
	t.Helper()
	res, err := New().DecodeHook(context.Background(), connect.NewRequest(&adapterv1.DecodeHookRequest{
		AgentId: "a1", SessionId: session, EventName: event, Payload: []byte(body),
	}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.Events
}

func TestDecodeHookEventsAnd429Limits(t *testing.T) {
	if evs := decodeOne(t, "", "SessionStart", `{"conversationId":"c1","hookEventName":"SessionStart"}`); len(evs) != 1 || evs[0].GetSessionStarted().GetSessionId() != "c1" || evs[0].GetSessionStarted().GetSource() != "turn" {
		t.Errorf("SessionStart: %v", evs)
	}
	// PreInvocation before SessionStart still captures conversationId.
	if evs := decodeOne(t, "", "PreInvocation", `{"conversationId":"c1","hookEventName":"PreInvocation","invocationNum":1}`); len(evs) != 2 || evs[0].GetSessionStarted().GetSessionId() != "c1" || evs[1].GetTurnStarted() == nil {
		t.Errorf("PreInvocation first: %v", evs)
	}
	if evs := decodeOne(t, "c1", "PreToolUse", `{"conversationId":"c1","hookEventName":"PreToolUse","toolCall":{"name":"read_file"}}`); len(evs) != 1 || evs[0].GetActivity().GetDetail() != "read_file" {
		t.Errorf("PreToolUse: %v", evs)
	}
	if evs := decodeOne(t, "c1", "Stop", `{"conversationId":"c1","hookEventName":"Stop","terminationReason":"NO_TOOL_CALL"}`); len(evs) != 1 || evs[0].GetTurnEnded() == nil {
		t.Errorf("Stop normal: %v", evs)
	}
	// A Stop that a continuation follows is not the turn's end.
	if evs := decodeOne(t, "c1", "Stop", `{"conversationId":"c1","hookEventName":"Stop","terminationReason":"NO_TOOL_CALL","continuationActive":true,"fullyIdle":false}`); len(evs) != 1 || evs[0].GetActivity() == nil {
		t.Errorf("Stop with a continuation: %v", evs)
	}
	// RESOURCE_EXHAUSTED (code 429) -> RateLimited, not TurnEnded.
	evs429 := decodeOne(t, "c1", "Stop", `{"conversationId":"c1","hookEventName":"Stop","terminationReason":"ERROR","error":"RESOURCE_EXHAUSTED (code 429): Individual quota reached. Resets in 2h15m."}`)
	if len(evs429) != 1 || evs429[0].GetRateLimited() == nil || evs429[0].GetRateLimited().ResetIn != "2h15m" {
		t.Errorf("Stop 429: %v", evs429)
	}
	// Non-limit ERROR -> TurnFailed.
	evsErr := decodeOne(t, "c1", "Stop", `{"conversationId":"c1","hookEventName":"Stop","terminationReason":"ERROR","error":"internal server failure"}`)
	if len(evsErr) != 1 || evsErr[0].GetTurnFailed() == nil {
		t.Errorf("Stop ERROR: %v", evsErr)
	}
	// Foreign conversation dropped.
	if evs := decodeOne(t, "c1", "Stop", `{"conversationId":"other","hookEventName":"Stop"}`); len(evs) != 0 {
		t.Errorf("foreign conversation not dropped: %v", evs)
	}
	// Malformed JSON -> DecodeError.
	if evs := decodeOne(t, "c1", "Stop", `{bad`); len(evs) != 1 || evs[0].GetDecodeError() == nil {
		t.Errorf("bad json: %v", evs)
	}
}

func TestLookupSessionFromSummariesAndHistory(t *testing.T) {
	home := t.TempDir()
	agyDir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(agyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(home, "wt-a1")
	repo := filepath.Join(home, "repo")
	dbPath := filepath.Join(agyDir, "conversation_summaries.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE conversation_summaries (
		conversation_id TEXT PRIMARY KEY,
		preview TEXT,
		workspace_uris TEXT,
		nesting_depth INTEGER,
		step_count INTEGER,
		last_modified_time INTEGER
	);
	INSERT INTO conversation_summaries VALUES ('sub-1', 'subagent', '["file://` + wt + `"]', 1, 5, 200);
	INSERT INTO conversation_summaries VALUES ('c-wt', 'hello', '["file://` + wt + `"]', 0, 2, 100);
	INSERT INTO conversation_summaries VALUES ('c-repo', 'human work', '["file://` + repo + `"]', 0, 10, 300);`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	a := New()
	// Worktree matches top-level conversation, ignoring subagent (nesting_depth=1).
	res, err := a.LookupSession(context.Background(), connect.NewRequest(&adapterv1.LookupSessionRequest{
		Cwd: wt, Repo: repo, Env: map[string]string{"HOME": home},
	}))
	if err != nil || res.Msg.SessionId != "c-wt" {
		t.Errorf("worktree lookup = %q, %v", res.Msg.SessionId, err)
	}
	// Shared main checkout never falls back to directory match.
	res2, err := a.LookupSession(context.Background(), connect.NewRequest(&adapterv1.LookupSessionRequest{
		Cwd: repo, Repo: repo, Env: map[string]string{"HOME": home},
	}))
	if err != nil || res2.Msg.SessionId != "" {
		t.Errorf("shared repo lookup should be empty, got %q, %v", res2.Msg.SessionId, err)
	}

	// History fallback when db is absent.
	os.Remove(dbPath)
	hist := `{"conversationId":"c-hist","display":"📬 [mesh] task from lead","workspace":"` + wt + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(agyDir, "history.jsonl"), []byte(hist), 0o600); err != nil {
		t.Fatal(err)
	}
	res3, err := a.LookupSession(context.Background(), connect.NewRequest(&adapterv1.LookupSessionRequest{
		Cwd: wt, Repo: repo, Env: map[string]string{"HOME": home},
	}))
	if err != nil || res3.Msg.SessionId != "c-hist" {
		t.Errorf("history lookup = %q, %v", res3.Msg.SessionId, err)
	}
}

func TestHooksGuardKeepsDenyRulesForAnotherSession(t *testing.T) {
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args")
	fake := filepath.Join(dir, "mesh")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"$*\" >> "+argsLog+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := HooksJSON("a1", []string{fake, "hook", "agy"}, true, dir)
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]hooksSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	run := func(cmd, agent string) string {
		t.Helper()
		os.Remove(argsLog)
		c := exec.Command("sh", "-c", cmd)
		c.Env = []string{"PATH=/usr/bin:/bin", "AGENT_MESH_AGENT=" + agent}
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", cmd, err, out)
		}
		got, _ := os.ReadFile(argsLog)
		return strings.TrimSpace(string(got))
	}
	pre := spec["agent-mesh"].PreToolUse[0].Hooks[0].Command
	stop := spec["agent-mesh"].Stop[0].Command
	if got := run(pre, "a1"); strings.Contains(got, "--check-only") || !strings.HasSuffix(got, "--deny-publish PreToolUse") {
		t.Errorf("the agent's PreToolUse ran %q", got)
	}
	if got := run(pre, ""); !strings.Contains(got, "--deny-publish --check-only PreToolUse") {
		t.Errorf("another session's PreToolUse ran %q: the deny rules must still be checked", got)
	}
	if got := run(stop, "someone-else"); got != "" {
		t.Errorf("another session's Stop reported: %q", got)
	}
}

func TestHooksJSONPreToolUseFailOpenForWritersAndFailClosedForDeny(t *testing.T) {
	// Without deny rules: PreToolUse matches "*", runs without GNU timeout, and exits 0 silently even when
	// mesh is missing, exits non-zero, or prints non-JSON.
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad-mesh")
	if err := os.WriteFile(bad, []byte("#!/bin/sh\necho 'not json!'\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, bin := range []string{bad, filepath.Join(dir, "missing-mesh")} {
		b, err := HooksJSON("a1", []string{bin, "hook", "agy"}, false)
		if err != nil {
			t.Fatal(err)
		}
		var spec map[string]hooksSpec
		if err := json.Unmarshal(b, &spec); err != nil {
			t.Fatal(err)
		}
		pre := spec["agent-mesh"].PreToolUse
		if len(pre) != 1 || pre[0].Matcher != "*" || pre[0].Hooks[0].Timeout != HookTimeout {
			t.Fatalf("unexpected writer PreToolUse: %+v", pre)
		}
		if strings.Contains(pre[0].Hooks[0].Command, "timeout ") {
			t.Errorf("writer PreToolUse should not depend on GNU timeout: %q", pre[0].Hooks[0].Command)
		}
		c := exec.Command("sh", "-c", pre[0].Hooks[0].Command)
		c.Env = []string{"PATH=/usr/bin:/bin", "AGENT_MESH_AGENT=a1"}
		out, err := c.CombinedOutput()
		if err != nil || len(out) != 0 {
			t.Errorf("%s: err=%v out=%q, want exit 0 with empty output", bin, err, out)
		}
	}

	// With deny rules: PreToolUse keeps Matcher "*" so every tool fails closed on hook failure.
	b, err := HooksJSON("a1", []string{bad, "hook", "agy"}, true, dir)
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]hooksSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	pre := spec["agent-mesh"].PreToolUse
	if len(pre) != 1 || pre[0].Matcher != "*" || pre[0].Hooks[0].Timeout != HookTimeout {
		t.Fatalf("unexpected deny PreToolUse: %+v", pre)
	}
}

func TestUsageParsingAndModelQuotaMatching(t *testing.T) {
	raw := strings.Join([]string{
		"header ignored",
		"Gemini 4 Neon\tNeon\t5%\t12:00",
		"Gemini 4 Hydrogen and Carbon models\tShared\t75%\t14:00",
		"Claude models\tClaude\t100%\tin 4h",
	}, "\n")
	rows := ParseUsage(raw)
	if len(rows) != 3 {
		t.Fatalf("got %d rows: %+v", len(rows), rows)
	}
	listing := "gemini-4-neon-high\tGemini 4 Neon (High)\n" +
		"gemini-4-hydrogen-high\tGemini 4 Hydrogen (High)\n" +
		"claude-opus-5-5-high\tClaude Opus 5.5 (High)\n"
	names := ListingNames(listing)
	if names["gemini-4-neon-high"] != "Gemini 4 Neon (High)" {
		t.Fatalf("ListingNames = %#v", names)
	}
	if q, ok := ModelQuota("gemini-4-neon-high", names["gemini-4-neon-high"], rows); !ok || q.Left != 5 || q.Resets != "12:00" {
		t.Errorf("neon quota = %+v, %v", q, ok)
	}
	if !QuotaApplies("Gemini and Claude models", "gemini-4-hydrogen-high", "") {
		t.Errorf("expected 'and ... models' group to match by model prefix")
	}
	if q, ok := ModelQuota("claude-opus-5-5-high", names["claude-opus-5-5-high"], rows); !ok || q.Left != 100 {
		t.Errorf("claude quota = %+v, %v", q, ok)
	}
}
