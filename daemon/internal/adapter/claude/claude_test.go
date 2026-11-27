package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	agentmeshv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

func TestReadOnlyRolesLaunchWithADenyListEvenUnderSkipPermissions(t *testing.T) {
	// The deny paths are made absolute and symlink-resolved (a temp dir may sit behind a symlink): so is the expectation.
	resolve := func(p string) string {
		abs, err := filepath.Abs(p)
		if err != nil {
			t.Fatal(err)
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			t.Fatal(err)
		}
		return real
	}
	cwd, repo := t.TempDir(), t.TempDir()
	plan := func(role agentmeshv1.AgentRole) []string {
		res, err := New().PlanLaunch(context.Background(), connect.NewRequest(&adapterv1.PlanLaunchRequest{
			Role: role, Cwd: cwd, Repo: repo, SkipPermissions: true, HookArgv: []string{"mesh", "hook"}}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.Argv
	}
	for _, role := range []agentmeshv1.AgentRole{agentmeshv1.AgentRole_AGENT_ROLE_ASKER, agentmeshv1.AgentRole_AGENT_ROLE_REVIEWER} {
		argv := strings.Join(plan(role), "\x00")
		for _, want := range []string{"--disallowedTools", "Edit(/" + resolve(cwd) + "/**)", "Edit(/" + resolve(repo) + "/**)", "Bash(git push:*)", "Bash(gh pr comment:*)"} {
			if !strings.Contains(argv, want) {
				t.Errorf("%v: no %q in %q", role, want, argv)
			}
		}
		// Variadic: another flag follows the list.
		for _, want := range []string{"Bash(gh pr create:*)", "Bash(gh issue close:*)", "Bash(gh release create:*)", "Bash(gh repo delete:*)"} {
			if !strings.Contains(argv, want) {
				t.Errorf("%v: no %q in %q", role, want, argv)
			}
		}
		// An untrusted PR head's project settings, hooks and .mcp.json are not read: only the user's own settings
		// (the mesh's hooks and plugin come by --settings and --plugin-dir, which are not sources).
		if last := "Bash(" + publishCommands[len(publishCommands)-1] + ":*)"; !strings.Contains(argv, last+"\x00--setting-sources\x00user\x00--settings") {
			t.Errorf("%v: the deny list is not followed by --setting-sources user and --settings: %q", role, argv)
		}
	}
	for _, role := range []agentmeshv1.AgentRole{agentmeshv1.AgentRole_AGENT_ROLE_WORKER, agentmeshv1.AgentRole_AGENT_ROLE_ROOT} {
		if argv := strings.Join(plan(role), " "); strings.Contains(argv, "--disallowedTools") {
			t.Errorf("%v got a deny list: %s", role, argv)
		} else if strings.Contains(argv, "--setting-sources") {
			t.Errorf("%v does not read an untrusted head: its own repo's settings are part of its job: %s", role, argv)
		}
	}
}

func TestPlanLaunch(t *testing.T) {
	a := New()
	a.NewSessionID = func() string { return "sid-1" }
	res, err := a.PlanLaunch(context.Background(), connect.NewRequest(&adapterv1.PlanLaunchRequest{
		Binary: "/opt/claude", Model: "opus", Effort: "high", SkipPermissions: true,
		HookArgv: []string{"/path with space/mesh", "hook", "claude", "--agent", "a1"},
		Env:      map[string]string{"PATH": "/bin", "CLAUDECODE": "1", "CLAUDE_CODE_SESSION_ID": "x"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	argv := res.Msg.Argv
	want := []string{"/opt/claude", "--session-id", "sid-1", "--model", "opus", "--effort", "high", "--settings"}
	if strings.Join(argv[:len(want)], " ") != strings.Join(want, " ") || argv[len(argv)-1] != "--dangerously-skip-permissions" {
		t.Fatalf("argv %q", argv)
	}
	if res.Msg.SessionId != "sid-1" {
		t.Errorf("session %q", res.Msg.SessionId)
	}
	if _, leaked := res.Msg.Env["CLAUDECODE"]; leaked || res.Msg.Env["CLAUDE_CODE_SESSION_ID"] != "" || res.Msg.Env["PATH"] != "/bin" {
		t.Errorf("env %v", res.Msg.Env)
	}
	var settings struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type, Command string
				Timeout       int
			}
		}
	}
	if err := json.Unmarshal([]byte(argv[len(want)]), &settings); err != nil {
		t.Fatal(err)
	}
	for _, ev := range hookEvents {
		groups := settings.Hooks[ev.name]
		wantTimeout := HookTimeout
		if ev.name == "PermissionRequest" { // held while the agent's parent decides
			wantTimeout = PermissionTimeout
		}
		if len(groups) != 1 || groups[0].Matcher != ev.matcher || groups[0].Hooks[0].Timeout != wantTimeout {
			t.Errorf("%s: %+v", ev.name, groups)
			continue
		}
		cmd := groups[0].Hooks[0].Command
		if cmd != "'/path with space/mesh' hook claude --agent a1 "+ev.name {
			t.Errorf("%s command %q", ev.name, cmd)
		}
	}
}

// A reopen resumes the same conversation (no new id), and a planner gets the deny list whatever its role.
func TestPlanLaunchResumesAndDeniesAPlanner(t *testing.T) {
	a := New()
	a.NewSessionID = func() string { t.Error("a resume picked a new session id"); return "new" }
	res, err := a.PlanLaunch(context.Background(), connect.NewRequest(&adapterv1.PlanLaunchRequest{
		Binary: "claude", Model: "sonnet", Effort: "medium", ResumeSessionId: "sid-7", DenyEdits: true,
		Role: agentmeshv1.AgentRole_AGENT_ROLE_WORKER, Cwd: t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Join(res.Msg.Argv, " ")
	if !strings.HasPrefix(argv, "claude --resume sid-7 --model sonnet --effort medium --disallowedTools Edit(/") ||
		strings.Contains(argv, "--session-id") || res.Msg.SessionId != "sid-7" {
		t.Errorf("argv %s, session %q", argv, res.Msg.SessionId)
	}
	res, _ = a.PlanLaunch(context.Background(), connect.NewRequest(&adapterv1.PlanLaunchRequest{
		ResumeSessionId: "sid-7", Role: agentmeshv1.AgentRole_AGENT_ROLE_WORKER}))
	if strings.Contains(strings.Join(res.Msg.Argv, " "), "--disallowedTools") {
		t.Errorf("an approved worker kept the deny list: %v", res.Msg.Argv)
	}
}

func decode(t *testing.T, session, event, payload string) []*adapterv1.AgentEvent {
	t.Helper()
	res, err := New().DecodeHook(context.Background(), connect.NewRequest(&adapterv1.DecodeHookRequest{
		SessionId: session, EventName: event, Payload: []byte(payload)}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.Events
}

// Payloads as Claude Code 2.1.332 sends them (fields trimmed; unknown ones are ignored).
func TestDecodeHook(t *testing.T) {
	cases := []struct {
		event, payload, want string
	}{
		{"SessionStart", `{"session_id":"s1","hook_event_name":"SessionStart","source":"startup","cwd":"/w"}`, "session_started"},
		{"UserPromptSubmit", `{"session_id":"s1","hook_event_name":"UserPromptSubmit","prompt":"hi"}`, "turn_started"},
		{"PreToolUse", `{"session_id":"s1","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{}}`, "activity"},
		{"PostToolUse", `{"session_id":"s1","hook_event_name":"PostToolUse","tool_name":"Edit"}`, "activity"},
		{"Notification", `{"session_id":"s1","hook_event_name":"Notification","notification_type":"permission_prompt","message":"Claude needs your permission to use Bash"}`, "needs_input"},
		{"Notification", `{"session_id":"s1","hook_event_name":"Notification","notification_type":"elicitation_dialog"}`, "needs_input"},
		{"Notification", `{"session_id":"s1","hook_event_name":"Notification","notification_type":"idle_prompt","message":"Claude is waiting for your input"}`, ""},
		{"Notification", `{"session_id":"s1","hook_event_name":"Notification","message":"Claude needs your permission to use Edit"}`, "needs_input"},
		{"Stop", `{"session_id":"s1","hook_event_name":"Stop","stop_hook_active":false}`, "turn_ended"},
		{"SubagentStop", `{"session_id":"s1","hook_event_name":"SubagentStop"}`, ""},
		{"PreCompact", `{"session_id":"s1","hook_event_name":"PreCompact","trigger":"auto"}`, "compacting"},
		{"SessionEnd", `{"session_id":"s1","hook_event_name":"SessionEnd","reason":"clear"}`, "session_ended"},
		// Another conversation's hook (a harness this agent runs) is dropped.
		{"Stop", `{"session_id":"other","hook_event_name":"Stop"}`, ""},
		// A /clear starts a new conversation: its SessionStart is taken.
		{"SessionStart", `{"session_id":"s2","hook_event_name":"SessionStart","source":"clear"}`, "session_started"},
		// A payload that doesn't parse is never a Stop.
		{"Stop", `{not json`, "decode_error"},
		// No event name in the payload: the hook's own name counts.
		{"Stop", `{"session_id":"s1"}`, "turn_ended"},
	}
	for _, c := range cases {
		evs := decode(t, "s1", c.event, c.payload)
		got := ""
		if len(evs) == 1 {
			got = string(evs[0].ProtoReflect().WhichOneof(evs[0].ProtoReflect().Descriptor().Oneofs().ByName("kind")).Name())
		} else if len(evs) > 1 {
			got = "many"
		}
		if got != c.want {
			t.Errorf("%s %s: %q, want %q", c.event, c.payload, got, c.want)
		}
	}
	if evs := decode(t, "s1", "SessionStart", `{"session_id":"s2","source":"clear"}`); evs[0].GetSessionStarted().SessionId != "s2" {
		t.Errorf("new session id not carried: %v", evs)
	}
}

func TestSettingsEmitsStatusLineWhenStateFlagPresent(t *testing.T) {
	b, err := Settings([]string{"/usr/bin/mesh", "hook", "--state", "/tmp/mesh state", "claude"}, false)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(b), &parsed); err != nil {
		t.Fatal(err)
	}
	sl, ok := parsed["statusLine"].(map[string]any)
	if !ok {
		t.Fatalf("missing statusLine in %s", b)
	}
	cmd, _ := sl["command"].(string)
	if sl["type"] != "command" || !strings.Contains(cmd, "statusline --state") || !strings.Contains(cmd, "'/tmp/mesh state'") {
		t.Errorf("unexpected statusLine command: %#v", sl)
	}

	bNoState, err := Settings([]string{"/usr/bin/mesh", "hook", "claude"}, false)
	if err != nil {
		t.Fatal(err)
	}
	var parsedNoState map[string]any
	if err := json.Unmarshal([]byte(bNoState), &parsedNoState); err != nil {
		t.Fatal(err)
	}
	if _, exists := parsedNoState["statusLine"]; exists {
		t.Errorf("statusLine emitted without --state: %s", bNoState)
	}
}

func TestPlanLaunchStandalone(t *testing.T) {
	a := New()
	res, err := a.PlanLaunch(context.Background(), connect.NewRequest(&adapterv1.PlanLaunchRequest{
		Role:            agentmeshv1.AgentRole_AGENT_ROLE_ASKER,
		DenyEdits:       true,
		Cwd:             t.TempDir(),
		Repo:            t.TempDir(),
		Binary:          "/opt/claude",
		Model:           "claude-sonnet-4-6",
		Effort:          "high",
		ResumeSessionId: "sess-42",
		SkipPermissions: true,
		HookArgv:        []string{"mesh", "hook", "--state", "/tmp/st"},
		Lean:            &adapterv1.LeanLaunch{DenyTools: []string{"Agent"}},
		Env: map[string]string{
			"HOME":        "/tmp/h",
			"CLAUDECODE":  "1",
			"ANTHROPIC_K": "v",
		},
		Standalone: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := "/opt/claude\x00--resume\x00sess-42\x00--model\x00claude-sonnet-4-6\x00--effort\x00high"
	if got := strings.Join(res.Msg.Argv, "\x00"); got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
	if _, ok := res.Msg.Env["CLAUDECODE"]; ok {
		t.Errorf("CLAUDECODE kept in standalone env: %v", res.Msg.Env)
	}
	if res.Msg.Env["HOME"] != "/tmp/h" {
		t.Errorf("HOME lost: %v", res.Msg.Env)
	}
	if _, err := a.PlanLaunch(context.Background(), connect.NewRequest(&adapterv1.PlanLaunchRequest{Standalone: true})); err == nil {
		t.Error("expected error when resume_session_id is empty")
	}
}

// The decision a PermissionRequest hook prints is the one Claude Code documents; any other prints nothing.
func TestPermissionJSON(t *testing.T) {
	for _, tc := range []struct{ decision, reason, want string }{
		{"allow", "", `{"hookSpecificOutput":{"decision":{"behavior":"allow"},"hookEventName":"PermissionRequest"}}` + "\n"},
		{"deny", "use a PR", `{"hookSpecificOutput":{"decision":{"behavior":"deny","message":"use a PR"},"hookEventName":"PermissionRequest"}}` + "\n"},
		{"", "", ""},
		{"maybe", "", ""},
	} {
		if got := string(PermissionJSON(tc.decision, tc.reason)); got != tc.want {
			t.Errorf("PermissionJSON(%q, %q) = %q, want %q", tc.decision, tc.reason, got, tc.want)
		}
	}
}

// What a parent is told of a prompt is a command or a path, never what an Edit or a Write would put in a file.
func TestPermissionSummaryIsACommandOrAPathNeverContents(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{`{"command":"rm -rf build","description":"x"}`, "rm -rf build"},
		{`{"file_path":"/repo/a.go","old_string":"SECRET","new_string":"SECRET2"}`, "/repo/a.go"},
		{`{"file_path":"/repo/b.go","content":"SECRET"}`, "/repo/b.go"},
		{`{"notebook_path":"/repo/n.ipynb","new_source":"SECRET"}`, "/repo/n.ipynb"},
		{`{"content":"SECRET","body":"SECRET"}`, ""},
		{`{"command":42}`, ""},
		{`not json`, ""},
	} {
		if got := PermissionSummary(json.RawMessage(tc.input)); got != tc.want {
			t.Errorf("PermissionSummary(%s) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// A root's Read tool is denied the whole of ORCHESTRATE.md, at the path it is given and at the one a symlink leads
// to; other roles read it as before.
func TestARootCannotReadTheWholeProcedureFile(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "install", "share")
	os.MkdirAll(filepath.Join(real, "protocol"), 0o755)
	os.WriteFile(filepath.Join(real, "protocol", "ORCHESTRATE.md"), []byte("x"), 0o644)
	link := filepath.Join(dir, "config-share")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("no symlinks")
	}
	plan := func(role agentmeshv1.AgentRole) string {
		res, err := New().PlanLaunch(context.Background(), connect.NewRequest(&adapterv1.PlanLaunchRequest{
			Role: role, Cwd: t.TempDir(), Env: map[string]string{"AGENT_MESH_SHARE": link}, HookArgv: []string{"mesh", "hook"}}))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(res.Msg.Argv, "\x00")
	}
	root := plan(agentmeshv1.AgentRole_AGENT_ROLE_ROOT)
	resolved, _ := filepath.EvalSymlinks(filepath.Join(link, "protocol", "ORCHESTRATE.md"))
	for _, want := range []string{"--disallowedTools\x00Read(/" + filepath.Join(link, "protocol", "ORCHESTRATE.md") + ")",
		"Read(/" + resolved + ")"} {
		if !strings.Contains(root, want) {
			t.Errorf("a root's launch lacks %q: %q", want, root)
		}
	}
	if w := plan(agentmeshv1.AgentRole_AGENT_ROLE_WORKER); strings.Contains(w, "ORCHESTRATE.md") {
		t.Errorf("a worker's launch denies the file: %q", w)
	}
}
