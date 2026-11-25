package claude

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"connectrpc.com/connect"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
)

func TestPlanLaunch(t *testing.T) {
	a := New()
	a.NewSessionID = func() string { return "sid-1" }
	res, err := a.PlanLaunch(context.Background(), connect.NewRequest(&adapterv1.PlanLaunchRequest{
		Binary: "/opt/claude", Model: "opus", Effort: "high", SkipPermissions: true,
		HookArgv: []string{"/path with space/meshctl", "hook", "claude", "--agent", "a1"},
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
		if len(groups) != 1 || groups[0].Matcher != ev.matcher || groups[0].Hooks[0].Timeout != HookTimeout {
			t.Errorf("%s: %+v", ev.name, groups)
			continue
		}
		cmd := groups[0].Hooks[0].Command
		if cmd != "'/path with space/meshctl' hook claude --agent a1 "+ev.name {
			t.Errorf("%s command %q", ev.name, cmd)
		}
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
