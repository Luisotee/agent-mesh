package reducer

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

var t0 = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

func at(sec int, ev *adapterv1.AgentEvent) *adapterv1.AgentEvent {
	ev.Time = timestamppb.New(t0.Add(time.Duration(sec) * time.Second))
	return ev
}

func started(id string) *adapterv1.AgentEvent {
	return &adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_SessionStarted{SessionStarted: &adapterv1.SessionStarted{SessionId: id}}}
}
func startedBy(id, source string) *adapterv1.AgentEvent {
	return &adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_SessionStarted{SessionStarted: &adapterv1.SessionStarted{SessionId: id, Source: source}}}
}
func turn() *adapterv1.AgentEvent {
	return &adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_TurnStarted{TurnStarted: &adapterv1.TurnStarted{}}}
}
func tool() *adapterv1.AgentEvent {
	return &adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_Activity{Activity: &adapterv1.Activity{Detail: "Bash"}}}
}
func ask(r string) *adapterv1.AgentEvent {
	return &adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_NeedsInput{NeedsInput: &adapterv1.NeedsInput{Reason: r}}}
}
func stop() *adapterv1.AgentEvent {
	return &adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_TurnEnded{TurnEnded: &adapterv1.TurnEnded{}}}
}
func bad() *adapterv1.AgentEvent {
	return &adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_DecodeError{DecodeError: &adapterv1.DecodeError{Error: "x"}}}
}
func exit(code int32) *adapterv1.AgentEvent {
	return &adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_ProcessExited{ProcessExited: &adapterv1.ProcessExited{ExitCode: code}}}
}
func compacting() *adapterv1.AgentEvent {
	return &adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_Compacting{Compacting: &adapterv1.Compacting{}}}
}
func ended() *adapterv1.AgentEvent {
	return &adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_SessionEnded{SessionEnded: &adapterv1.SessionEnded{Reason: "clear"}}}
}
func limited(msg string) *adapterv1.AgentEvent {
	return &adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_RateLimited{RateLimited: &adapterv1.RateLimited{Message: msg}}}
}
func failed(msg string) *adapterv1.AgentEvent {
	return &adapterv1.AgentEvent{Kind: &adapterv1.AgentEvent_TurnFailed{TurnFailed: &adapterv1.TurnFailed{Error: msg}}}
}

func TestReduce(t *testing.T) {
	cases := []struct {
		name   string
		events []*adapterv1.AgentEvent
		want   v1.AgentState
		needs  int32
	}{
		{"launched", nil, v1.AgentState_AGENT_STATE_STARTING, 0},
		{"session up is idle", []*adapterv1.AgentEvent{started("s1")}, v1.AgentState_AGENT_STATE_IDLE, 0},
		{"a prompt starts a turn", []*adapterv1.AgentEvent{started("s1"), turn(), tool()}, v1.AgentState_AGENT_STATE_WORKING, 0},
		{"stop ends it", []*adapterv1.AgentEvent{started("s1"), turn(), stop()}, v1.AgentState_AGENT_STATE_IDLE, 0},
		{"prompts are counted", []*adapterv1.AgentEvent{turn(), ask("Bash"), ask("Edit")}, v1.AgentState_AGENT_STATE_NEEDS_INPUT, 2},
		{"a tool after the prompt clears it", []*adapterv1.AgentEvent{turn(), ask("Bash"), tool()}, v1.AgentState_AGENT_STATE_WORKING, 0},
		{"stop clears it", []*adapterv1.AgentEvent{turn(), ask("Bash"), stop()}, v1.AgentState_AGENT_STATE_IDLE, 0},
		{"a decode error is never a stop", []*adapterv1.AgentEvent{turn(), bad()}, v1.AgentState_AGENT_STATE_WORKING, 0},
		{"a session end alone doesn't end the agent", []*adapterv1.AgentEvent{turn(), stop(), ended(), started("s2")}, v1.AgentState_AGENT_STATE_IDLE, 0},
		{"a compaction's session start mid-turn is not idle", []*adapterv1.AgentEvent{started("s1"), turn(), compacting(), startedBy("s1", "compact")}, v1.AgentState_AGENT_STATE_WORKING, 0},
		{"a compaction from idle goes back to idle", []*adapterv1.AgentEvent{started("s1"), compacting(), startedBy("s1", "compact")}, v1.AgentState_AGENT_STATE_IDLE, 0},
		{"a prompt during a compaction doesn't outlive it", []*adapterv1.AgentEvent{started("s1"), compacting(), ask("Bash"), startedBy("s1", "compact")}, v1.AgentState_AGENT_STATE_IDLE, 0},
		{"a compaction from a prompt goes back to it", []*adapterv1.AgentEvent{started("s1"), turn(), ask("Bash"), compacting(), startedBy("s1", "compact")}, v1.AgentState_AGENT_STATE_NEEDS_INPUT, 1},
		{"a compaction that hasn't finished is working", []*adapterv1.AgentEvent{started("s1"), compacting()}, v1.AgentState_AGENT_STATE_WORKING, 0},
		{"the next turn forgets an unfinished compaction", []*adapterv1.AgentEvent{started("s1"), compacting(), turn(), stop(), compacting(), turn(), startedBy("s1", "compact")}, v1.AgentState_AGENT_STATE_WORKING, 0},
		{"a resume mid-turn is not idle", []*adapterv1.AgentEvent{started("s1"), turn(), startedBy("s1", "resume")}, v1.AgentState_AGENT_STATE_WORKING, 0},
		{"a launch that resumes a conversation is idle", []*adapterv1.AgentEvent{startedBy("s1", "resume")}, v1.AgentState_AGENT_STATE_IDLE, 0},
		{"a /clear is idle", []*adapterv1.AgentEvent{started("s1"), turn(), startedBy("s2", "clear")}, v1.AgentState_AGENT_STATE_IDLE, 0},
		{"a turn-time session start records from starting as idle", []*adapterv1.AgentEvent{startedBy("s1", "turn")}, v1.AgentState_AGENT_STATE_IDLE, 0},
		{"a turn-time session start keeps working mid-turn", []*adapterv1.AgentEvent{turn(), startedBy("s1", "turn")}, v1.AgentState_AGENT_STATE_WORKING, 0},
		{"rate limited moves to LIMITED", []*adapterv1.AgentEvent{started("s1"), turn(), limited("429")}, v1.AgentState_AGENT_STATE_LIMITED, 0},
		{"turn failed moves to ERROR", []*adapterv1.AgentEvent{started("s1"), turn(), failed("bad")}, v1.AgentState_AGENT_STATE_ERROR, 0},
		{"a new turn clears LIMITED", []*adapterv1.AgentEvent{started("s1"), turn(), limited("429"), turn()}, v1.AgentState_AGENT_STATE_WORKING, 0},
		{"a new turn clears ERROR", []*adapterv1.AgentEvent{started("s1"), turn(), failed("bad"), turn()}, v1.AgentState_AGENT_STATE_WORKING, 0},
		{"exit is final", []*adapterv1.AgentEvent{turn(), exit(1), started("s3")}, v1.AgentState_AGENT_STATE_EXITED, 0},
	}
	for _, c := range cases {
		s := Start(t0)
		for i, ev := range c.events {
			s = Reduce(s, at(i+1, ev))
		}
		if s.State != c.want || s.NeedsInput != c.needs {
			t.Errorf("%s: %v needs %d, want %v needs %d", c.name, s.State, s.NeedsInput, c.want, c.needs)
		}
	}
	s := Reduce(Reduce(Start(t0), at(1, started("s1"))), at(2, started("s2")))
	if s.SessionID != "s2" || !s.LastActivity.Equal(t0.Add(2*time.Second)) {
		t.Errorf("session %q activity %v", s.SessionID, s.LastActivity)
	}
	if s := Reduce(Start(t0), at(1, exit(7))); s.ExitCode != 7 {
		t.Errorf("exit code %d", s.ExitCode)
	}
}

func TestReplayGivesTheSameState(t *testing.T) {
	evs := []*adapterv1.AgentEvent{at(1, started("s")), at(2, turn()), at(3, ask("x")), at(4, tool()), at(5, stop())}
	a, b := Start(t0), Start(t0)
	for _, ev := range evs {
		a = Reduce(a, ev)
	}
	for _, ev := range evs {
		b = Reduce(b, ev)
	}
	if a != b {
		t.Errorf("%v != %v", a, b)
	}
}

func TestDisplayShowsSilentWorkAsUnknown(t *testing.T) {
	s := Reduce(Start(t0), at(0, turn()))
	if got := Display(s, t0.Add(5*time.Minute), time.Time{}, 10*time.Minute); got != v1.AgentState_AGENT_STATE_WORKING {
		t.Errorf("fresh: %v", got)
	}
	if got := Display(s, t0.Add(11*time.Minute), time.Time{}, 10*time.Minute); got != v1.AgentState_AGENT_STATE_UNKNOWN {
		t.Errorf("silent: %v", got)
	}
	if got := Display(s, t0.Add(11*time.Minute), t0.Add(9*time.Minute), 10*time.Minute); got != v1.AgentState_AGENT_STATE_WORKING {
		t.Errorf("output counts as life: %v", got)
	}
	idle := Reduce(s, at(1, stop()))
	if got := Display(idle, t0.Add(time.Hour), time.Time{}, 10*time.Minute); got != v1.AgentState_AGENT_STATE_IDLE {
		t.Errorf("idle never turns unknown: %v", got)
	}
	lim := Reduce(s, at(1, limited("429")))
	if got := Display(lim, t0.Add(time.Hour), time.Time{}, 10*time.Minute); got != v1.AgentState_AGENT_STATE_LIMITED {
		t.Errorf("limited never turns unknown: %v", got)
	}
	errS := Reduce(s, at(1, failed("err")))
	if got := Display(errS, t0.Add(time.Hour), time.Time{}, 10*time.Minute); got != v1.AgentState_AGENT_STATE_ERROR {
		t.Errorf("error never turns unknown: %v", got)
	}
}

func TestDisplayAgesOutAPromptNothingAnswered(t *testing.T) {
	s := Reduce(Reduce(Start(t0), at(0, turn())), at(1, ask("Bash")))
	if got := Display(s, t0.Add(5*time.Minute), time.Time{}, 10*time.Minute); got != v1.AgentState_AGENT_STATE_NEEDS_INPUT {
		t.Errorf("fresh prompt: %v", got)
	}
	if got := Display(s, t0.Add(11*time.Minute), time.Time{}, 10*time.Minute); got != v1.AgentState_AGENT_STATE_UNKNOWN {
		t.Errorf("stale prompt: %v", got)
	}
}
