package copilot

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	agentmeshv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

func plan(t *testing.T, m *adapterv1.PlanLaunchRequest) *adapterv1.PlanLaunchResponse {
	t.Helper()
	a := &Adapter{NewSessionID: func() string { return "new-session" }}
	res, err := a.PlanLaunch(context.Background(), connect.NewRequest(m))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg
}

func TestPlanLaunchChoosesTheSessionAndReopensIt(t *testing.T) {
	first := plan(t, &adapterv1.PlanLaunchRequest{Binary: "/bin/copilot", Model: "auto", Effort: "medium"})
	if first.SessionId != "new-session" ||
		!slices.Equal(first.Argv, []string{"/bin/copilot", "--session-id", "new-session", "--model", "auto", "--auto-tier", "intelligence"}) {
		t.Fatalf("first launch: %+v", first)
	}
	again := plan(t, &adapterv1.PlanLaunchRequest{Model: "gpt-5", Effort: "high", ResumeSessionId: "old", SkipPermissions: true})
	if again.SessionId != "old" ||
		!slices.Equal(again.Argv, []string{"copilot", "--session-id", "old", "--model", "gpt-5", "--reasoning-effort", "high", "--yolo"}) {
		t.Fatalf("reopen: %+v", again)
	}
}

func TestPlanLaunchDeniesWriteAndPublishingForReadOnlyAndPlanningAgents(t *testing.T) {
	for name, m := range map[string]*adapterv1.PlanLaunchRequest{
		"asker":    {Role: agentmeshv1.AgentRole_AGENT_ROLE_ASKER, Model: "auto", SkipPermissions: true},
		"reviewer": {Role: agentmeshv1.AgentRole_AGENT_ROLE_REVIEWER, Model: "auto"},
		"planner":  {Role: agentmeshv1.AgentRole_AGENT_ROLE_WORKER, DenyEdits: true, Model: "auto"},
	} {
		argv := plan(t, m).Argv
		for _, rule := range []string{"--deny-tool=write", "--deny-tool=shell(git push:*)", "--deny-tool=shell(gh pr comment:*)"} {
			if !slices.Contains(argv, rule) {
				t.Errorf("%s: no %s in %v", name, rule, argv)
			}
		}
		// Each rule is its own `=` flag, so none can swallow the flag after it, and all precede `--yolo`.
		if i, j := slices.Index(argv, "--yolo"), slices.Index(argv, "--deny-tool=write"); i >= 0 && j > i {
			t.Errorf("%s: --yolo before the deny rules: %v", name, argv)
		}
	}
	if argv := plan(t, &adapterv1.PlanLaunchRequest{Role: agentmeshv1.AgentRole_AGENT_ROLE_WORKER, Model: "auto"}).Argv; strings.Contains(strings.Join(argv, " "), "deny-tool") {
		t.Errorf("an executing worker was denied: %v", argv)
	}
}

func ev(typ, ts, data string) Event {
	return Event{Type: typ, ID: typ + ts, Timestamp: ts, Data: []byte(data)}
}

func TestInspectReadsTurnsFromEventsAndAToolLoopIsStillWorking(t *testing.T) {
	user := ev("user.message", "2026-11-26T10:00:00.000Z", `{"content":"hi"}`)
	tools := ev("assistant.message", "2026-11-26T10:00:01.000Z", `{"content":"","toolRequests":[{"name":"view"}]}`)
	final := ev("assistant.message", "2026-11-26T10:00:03.000Z", `{"content":"done","toolRequests":[]}`)
	turnStart := ev("assistant.turn_start", "2026-11-26T10:00:00.500Z", `{}`)
	turnEnd := ev("assistant.turn_end", "2026-11-26T10:00:04.000Z", `{}`)

	if st := Inspect([]Event{user, turnStart}); st.State != "working" {
		t.Errorf("a prompt with no answer: %+v", st)
	}
	if st := Inspect([]Event{user, turnStart, tools, ev("assistant.turn_end", "2026-11-26T10:00:02.000Z", `{}`)}); st.State != "working" {
		t.Errorf("a tool call's turn end is not idle: %+v", st)
	}
	st := Inspect([]Event{user, turnStart, tools, ev("assistant.turn_end", "x", `{}`), turnStart, final, turnEnd})
	if st.State != "idle" || st.Key != turnEnd.ID || st.AnsweredAt.IsZero() {
		t.Errorf("an answered prompt: %+v", st)
	}
	if st := Inspect(nil); st.State != "" {
		t.Errorf("no events: %+v", st)
	}
}

func TestInspectStopsAtALimitUntilTheNextMessage(t *testing.T) {
	user := ev("user.message", "2026-11-26T10:00:00.000Z", `{"content":"hi"}`)
	quota := ev("session.error", "2026-11-26T10:00:01.000Z", `{"errorType":"quota","statusCode":402,"message":"402 no quota\nsecond line"}`)
	st := Inspect([]Event{user, quota})
	if st.Limit == nil || st.Limit.Hold != QuotaHold || st.Limit.Message != "402 no quota" || st.Limit.Policy {
		t.Fatalf("a 402: %+v", st.Limit)
	}
	next := ev("user.message", "2026-11-26T10:05:00.000Z", `{"content":"again"}`)
	if st := Inspect([]Event{user, quota, next}); st.Limit != nil {
		t.Errorf("a newer prompt left the limit: %+v", st.Limit)
	}
	answer := ev("assistant.message", "2026-11-26T10:05:02.000Z", `{"content":"ok","toolRequests":[]}`)
	if st := Inspect([]Event{user, quota, answer}); st.Limit != nil || st.AnsweredAt.IsZero() {
		t.Errorf("an answer after the limit: %+v", st)
	}
}

func TestClassifyErrorKnowsQuotaPolicyAndOthers(t *testing.T) {
	for name, tc := range map[string]struct {
		data   string
		ok     bool
		hold   time.Duration
		policy bool
	}{
		"errorType quota":    {`{"errorType":"quota","message":"m"}`, true, QuotaHold, false},
		"status 402":         {`{"statusCode":402,"message":"m"}`, true, QuotaHold, false},
		"errorCode":          {`{"errorCode":"quota_exceeded"}`, true, QuotaHold, false},
		"403 policy":         {`{"errorType":"query","message":"model resolution leaf failed: {\"status\":403,\"body\":\"unauthorized: not authorized to use this Copilot feature\"}"}`, true, PolicyHold, true},
		"access denied":      {`{"message":"Failed to load models: access denied by Copilot policy."}`, true, PolicyHold, true},
		"another error":      {`{"errorType":"query","message":"network down"}`, false, 0, false},
		"a 403 of something": {`{"statusCode":403,"message":"forbidden"}`, false, 0, false},
	} {
		l, ok := ClassifyError(Event{Type: "session.error", Data: []byte(tc.data)})
		if ok != tc.ok || l.Hold != tc.hold || l.Policy != tc.policy {
			t.Errorf("%s: %+v %v", name, l, ok)
		}
	}
	if _, ok := ClassifyError(Event{Type: "assistant.message", Data: []byte(`{"statusCode":402}`)}); ok {
		t.Error("a non-error event counted")
	}
}

func TestScreenLimitReadsRateLimitsAndPolicyButNotQuotedOnes(t *testing.T) {
	l, ok := ScreenLimit([]string{"old", "│ You've hit your session rate limit. Resets in 2h 30m.", ""})
	if !ok || l.Hold != 150*time.Minute || l.Policy {
		t.Errorf("a rate limit: %+v %v", l, ok)
	}
	if l, ok := ScreenLimit([]string{"You've reached your weekly rate limit."}); !ok || l.Hold != RateHold {
		t.Errorf("a weekly limit with no reset: %+v %v", l, ok)
	}
	if l, ok := ScreenLimit([]string{" ! Failed to load models: access denied by Copilot policy. (Request ID: X)"}); !ok || !l.Policy || l.Hold != PolicyHold {
		t.Errorf("a policy refusal: %+v %v", l, ok)
	}
	// The agent quoting the message in a sentence is not a limit.
	if _, ok := ScreenLimit([]string{"The CLI prints: You've hit your rate limit. when throttled", "I wrote \"You've hit your rate limit\" in the docs"}); ok {
		t.Error("a quoted limit counted")
	}
	if _, ok := ScreenLimit(nil); ok {
		t.Error("an empty screen counted")
	}
}

func TestTrustDialogNeedsBothTheTitleAndTheDefaultOption(t *testing.T) {
	dialog := []string{"│ Confirm folder trust", "│ Do you trust the files in this folder?", "│ ❯ 1. Yes", "│   2. Yes, and remember this folder for future sessions", "│   3. No (Esc)"}
	if !TrustDialog(dialog) {
		t.Error("the dialog was not seen")
	}
	if TrustDialog(dialog[:2]) || TrustDialog(dialog[2:]) || TrustDialog([]string{"Confirm folder trust", "1. Yes"}) {
		t.Error("a part of the dialog counted")
	}
}

func TestReadEventsSkipsGarbageAndACutFirstLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	if evs, err := ReadEvents(path); err != nil || evs != nil {
		t.Fatalf("a missing file: %v %v", evs, err)
	}
	good := `{"type":"user.message","id":"a","timestamp":"2026-11-26T10:00:00Z","data":{"content":"x"}}` + "\n"
	body := "not json\n" + good + `{"no":"type"}` + "\n" + good
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	evs, err := ReadEvents(path)
	if err != nil || len(evs) != 2 || evs[0].Time().IsZero() {
		t.Fatalf("events: %+v %v", evs, err)
	}
	// A file longer than the tail: the first line read is cut, so it is dropped.
	big := strings.Repeat("x", tailBytes) + "\n" + good
	os.WriteFile(path, []byte(big), 0o600)
	if evs, _ := ReadEvents(path); len(evs) != 1 {
		t.Fatalf("a tail: %+v", evs)
	}
}

func TestEventsPathFollowsCopilotHomeThenHome(t *testing.T) {
	if got := EventsPath(map[string]string{"COPILOT_HOME": "/c"}, "s1"); got != "/c/session-state/s1/events.jsonl" {
		t.Errorf("%s", got)
	}
	if got := EventsPath(map[string]string{"HOME": "/h"}, "s1"); got != "/h/.copilot/session-state/s1/events.jsonl" {
		t.Errorf("%s", got)
	}
}

// A turn whose events outgrow the tail read still ends: the prompt is out of the window, the final answer and
// its turn end are in it.
func TestInspectSeesATurnEndWhoseOpeningIsOutOfTheWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	line := func(typ, ts, data string) string {
		return `{"type":"` + typ + `","id":"` + typ + ts + `","timestamp":"` + ts + `","data":` + data + `}` + "\n"
	}
	body := line("user.message", "2026-11-26T10:00:00.000Z", `{"content":"read it all"}`) +
		line("assistant.turn_start", "2026-11-26T10:00:00.500Z", `{}`) +
		line("assistant.message", "2026-11-26T10:00:01.000Z", `{"content":"","toolRequests":[{"name":"view"}]}`)
	for i := 0; i < 4; i++ { // four 100 KiB tool outputs: past the tail
		body += line("tool.execution_complete", "2026-11-26T10:00:02.000Z", `{"result":"`+strings.Repeat("x", 100<<10)+`"}`)
	}
	working := body + line("assistant.turn_end", "2026-11-26T10:00:03.000Z", `{}`) +
		line("assistant.turn_start", "2026-11-26T10:00:04.000Z", `{}`)
	done := working + line("assistant.message", "2026-11-26T10:00:05.000Z", `{"content":"all read","toolRequests":[]}`) +
		line("assistant.turn_end", "2026-11-26T10:00:06.000Z", `{}`)
	for name, tc := range map[string]struct {
		body, state string
	}{"mid-turn": {working, "working"}, "ended": {done, "idle"}} {
		os.WriteFile(path, []byte(tc.body), 0o600)
		evs, err := ReadEvents(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			if e.Type == "user.message" {
				t.Fatalf("%s: the prompt is inside the window: the test does not test a cut", name)
			}
		}
		if st := Inspect(evs); st.State != tc.state {
			t.Errorf("%s: state %q, want %q", name, st.State, tc.state)
		}
	}
}

func TestInspectDatesTheAnsweredPromptAndScreenLimitsKnowTheirModel(t *testing.T) {
	user := ev("user.message", "2026-11-26T10:00:00.000Z", `{"content":"hi"}`)
	answer := ev("assistant.message", "2026-11-26T10:05:00.000Z", `{"content":"ok","toolRequests":[]}`)
	st := Inspect([]Event{user, answer})
	if want, _ := time.Parse(time.RFC3339Nano, "2026-11-26T10:00:00.000Z"); !st.PromptAt.Equal(want) || st.AnsweredAt.IsZero() {
		t.Errorf("prompt %v answered %v", st.PromptAt, st.AnsweredAt)
	}
	if st := Inspect([]Event{answer}); !st.PromptAt.IsZero() {
		t.Errorf("an answer whose prompt is out of the window has a prompt time: %v", st.PromptAt)
	}
	for line, perModel := range map[string]bool{
		"You've hit the rate limit for this model.": true,
		"You've hit your session rate limit.":       false,
		"You've reached your weekly rate limit.":    false,
		"You've hit your rate limit.":               false,
	} {
		l, ok := ScreenLimit([]string{line})
		if !ok || l.PerModel != perModel {
			t.Errorf("%q: %+v ok=%v, want per-model %v", line, l, ok, perModel)
		}
	}
}
