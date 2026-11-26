package claude

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	agentmeshv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

func decodeAt(t *testing.T, event, payload string, fired time.Time) []*adapterv1.AgentEvent {
	t.Helper()
	res, err := New().DecodeHook(context.Background(), connect.NewRequest(&adapterv1.DecodeHookRequest{
		SessionId: "s1", EventName: event, Payload: []byte(payload), FiredAt: timestamppb.New(fired)}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.Events
}

func stopFailure(errKind, message, details string) string {
	b, _ := json.Marshal(map[string]any{"session_id": "s1", "hook_event_name": "StopFailure", "error": errKind,
		"last_assistant_message": message, "error_details": details})
	return string(b)
}

// A usage limit is `StopFailure` with error rate_limit and Claude's own words; a capacity 429 is not one.
func TestStopFailureUsageLimits(t *testing.T) {
	fired := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	sp, _ := time.LoadLocation("America/Sao_Paulo")
	cases := []struct {
		name, kind, message string
		limited, all        bool
		reset               time.Time
	}{
		{"session", "rate_limit", "You've hit your session limit · resets 3pm (America/Sao_Paulo)", true, true,
			time.Date(2026, 11, 25, 15, 0, 0, 0, sp)},
		{"weekly with a date", "rate_limit", "You've hit your weekly limit · resets Nov 27, 3:30pm (America/Sao_Paulo)", true, true,
			time.Date(2026, 11, 27, 15, 30, 0, 0, sp)},
		{"a year away", "rate_limit", "You've hit your weekly limit · resets Jan 2, 2027, 9am (UTC)", true, true,
			time.Date(2027, 1, 2, 9, 0, 0, 0, time.UTC)},
		{"opus only", "rate_limit", "You've hit your Opus limit · resets 8pm (UTC)", true, false,
			time.Date(2026, 11, 25, 20, 0, 0, 0, time.UTC)},
		{"sonnet only", "rate_limit", "You've hit your Sonnet limit · resets 8pm (UTC)", true, false,
			time.Date(2026, 11, 25, 20, 0, 0, 0, time.UTC)},
		{"no reset named", "rate_limit", "You've hit your limit", true, true, time.Time{}},
		{"out of extra usage", "rate_limit", "You're out of extra usage · resets 11am (UTC)", true, true,
			time.Date(2026, 11, 26, 11, 0, 0, 0, time.UTC)},
		{"billing", "billing_error", "You've hit your monthly spend limit", true, true, time.Time{}},
		{"a capacity 429", "rate_limit", "API Error: Request rejected (429) · this may be a temporary capacity issue", false, false, time.Time{}},
		{"overloaded", "server_error", "API Error: 529 overloaded", false, false, time.Time{}},
		{"a login", "authentication_failed", "Please run /login", false, false, time.Time{}},
		// The agent's own text quoting a limit is no StopFailure of a limit.
		{"quoted", "unknown", "You've hit your session limit", false, false, time.Time{}},
	}
	for _, c := range cases {
		evs := decodeAt(t, "StopFailure", stopFailure(c.kind, c.message, ""), fired)
		if len(evs) != 1 {
			t.Fatalf("%s: %v", c.name, evs)
		}
		rl := evs[0].GetRateLimited()
		if (rl != nil) != c.limited {
			t.Errorf("%s: limited %v, want %v (%v)", c.name, rl != nil, c.limited, evs[0])
			continue
		}
		if !c.limited {
			if tf := evs[0].GetTurnFailed(); tf == nil || tf.Error == "" {
				t.Errorf("%s: %v, want a TurnFailed", c.name, evs[0])
			}
			continue
		}
		if rl.AllModels != c.all {
			t.Errorf("%s: all_models %v, want %v", c.name, rl.AllModels, c.all)
		}
		if got := rl.ResetAt.AsTime(); c.reset.IsZero() != (rl.ResetAt == nil) || (rl.ResetAt != nil && !got.Equal(c.reset)) {
			t.Errorf("%s: reset %v, want %v", c.name, got, c.reset)
		}
		if !strings.HasPrefix(rl.Message, "You") {
			t.Errorf("%s: message %q", c.name, rl.Message)
		}
	}
	// The limit text may come in the details only.
	evs := decodeAt(t, "StopFailure", stopFailure("rate_limit", "", "You've hit your session limit · resets 3pm (UTC)"), fired)
	if len(evs) != 1 || evs[0].GetRateLimited() == nil {
		t.Errorf("details only: %v", evs)
	}
	// Another conversation's StopFailure is dropped, like any hook.
	if evs := decodeAt(t, "StopFailure", strings.Replace(stopFailure("rate_limit", "You've hit your limit", ""), `"s1"`, `"other"`, 1), fired); len(evs) != 0 {
		t.Errorf("another conversation's: %v", evs)
	}
}

func TestParseResetBareTimeIsTheNextOne(t *testing.T) {
	fired := time.Date(2026, 11, 25, 16, 0, 0, 0, time.UTC)
	at, ok := parseReset("limit · resets 3pm (UTC)", fired)
	if !ok || !at.Equal(time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)) {
		t.Errorf("3pm after 4pm: %v %v", at, ok)
	}
	at, ok = parseReset("resets 12am (UTC)", fired)
	if !ok || !at.Equal(time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("12am: %v %v", at, ok)
	}
	at, ok = parseReset("resets 12pm (UTC)", time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC))
	if !ok || !at.Equal(time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("12pm: %v %v", at, ok)
	}
	// A date that has passed this year is next year's; an unknown zone reads in the host's.
	at, ok = parseReset("resets Jan 2, 9am (UTC)", fired)
	if !ok || !at.Equal(time.Date(2027, 1, 2, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("Jan 2: %v %v", at, ok)
	}
	if at, ok = parseReset("resets 3pm (Not/AZone)", fired); !ok || at.Location() != time.Local && at.Hour() != 15 {
		t.Errorf("unknown zone: %v %v", at, ok)
	}
	for _, text := range []string{"no reset here", "resets soon", "resets 13pm (UTC)"} {
		if _, ok := parseReset(text, fired); ok {
			t.Errorf("%q parsed", text)
		}
	}
}

func TestPreToolUsePublishCheck(t *testing.T) {
	bash := func(cmd string) []byte {
		b, _ := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash",
			"tool_input": map[string]any{"command": cmd}})
		return b
	}
	for _, cmd := range []string{"git push", "git -C . push origin HEAD", `bash -c "git push"`, "gh pr --repo o/r merge 5",
		"gh pr create -t x", "gh issue close 3", "gh release create v1", "gh repo delete o/r --yes",
		"gh api repos/o/r/issues/1/comments -f body=x", "cd x && git push"} {
		if deny, reason := EvaluatePreToolUse(bash(cmd), true); !deny || !strings.Contains(reason, "may not run") {
			t.Errorf("%q: deny %v %q", cmd, deny, reason)
		}
	}
	for _, cmd := range []string{"git status", "git log --oneline", "gh pr view 5 --comments", "gh pr diff 5", "gh pr list",
		"gh api repos/o/r/pulls/5/comments", "go test ./...", `echo "git push"`} {
		if deny, reason := EvaluatePreToolUse(bash(cmd), true); deny {
			t.Errorf("%q denied: %s", cmd, reason)
		}
	}
	// Only Bash runs commands; another tool is not read.
	other, _ := json.Marshal(map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"command": "git push"}})
	if deny, _ := EvaluatePreToolUse(other, true); deny {
		t.Error("an Edit call was read as a command")
	}
	// Fail closed on a payload that can't be read, and off without the rule.
	if deny, _ := EvaluatePreToolUse([]byte(`{not json`), true); !deny {
		t.Error("an unreadable payload was let through")
	}
	if deny, _ := EvaluatePreToolUse(bash("git push"), false); deny {
		t.Error("denied without the rule")
	}
	var out struct {
		Hook struct {
			Event, Decision, Reason string
		} `json:"hookSpecificOutput"`
	}
	raw := DenyJSON("no")
	if err := json.Unmarshal(raw, &out); err != nil || !strings.HasSuffix(string(raw), "\n") ||
		!strings.Contains(string(raw), `"permissionDecision":"deny"`) || !strings.Contains(string(raw), `"hookEventName":"PreToolUse"`) {
		t.Errorf("deny JSON %q: %v", raw, err)
	}
}

// Only a read-only or planning launch carries the PreToolUse check, and every launch hooks StopFailure.
func TestSettingsDenyPublishAndStopFailure(t *testing.T) {
	plan := func(role agentmeshv1.AgentRole, denyEdits bool) string {
		res, err := New().PlanLaunch(context.Background(), connect.NewRequest(&adapterv1.PlanLaunchRequest{
			Role: role, DenyEdits: denyEdits, Cwd: t.TempDir(), HookArgv: []string{"meshctl", "hook", "claude"}}))
		if err != nil {
			t.Fatal(err)
		}
		for i, a := range res.Msg.Argv {
			if a == "--settings" {
				return res.Msg.Argv[i+1]
			}
		}
		t.Fatal("no --settings")
		return ""
	}
	for _, c := range []struct {
		role agentmeshv1.AgentRole
		deny bool
		want bool
	}{
		{agentmeshv1.AgentRole_AGENT_ROLE_REVIEWER, false, true},
		{agentmeshv1.AgentRole_AGENT_ROLE_ASKER, false, true},
		{agentmeshv1.AgentRole_AGENT_ROLE_WORKER, true, true},
		{agentmeshv1.AgentRole_AGENT_ROLE_WORKER, false, false},
		{agentmeshv1.AgentRole_AGENT_ROLE_ROOT, false, false},
	} {
		settings := plan(c.role, c.deny)
		if got := strings.Contains(settings, "--deny-publish PreToolUse"); got != c.want {
			t.Errorf("%v deny=%v: deny-publish %v, want %v: %s", c.role, c.deny, got, c.want, settings)
		}
		if strings.Count(settings, "--deny-publish") > 1 {
			t.Errorf("--deny-publish on more than PreToolUse: %s", settings)
		}
		if !strings.Contains(settings, `"StopFailure"`) {
			t.Errorf("no StopFailure hook: %s", settings)
		}
	}
}
