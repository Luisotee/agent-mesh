package notice

import (
	"strings"
	"testing"
	"time"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// The cases of android/.../NoticeTest.kt, so the three copies of the rules stay the same.

var agents = map[string]*v1.Agent{
	"lead": {Id: "lead", Name: "shop-lead", Role: v1.AgentRole_AGENT_ROLE_ROOT},
	"w1":   {Id: "w1", Name: "shop-fix", ParentId: "lead"},
}

func lookup(id string) *v1.Agent { return agents[id] }

func stateChanged(agent string, from, to v1.AgentState, detail string) *v1.Event {
	return &v1.Event{AgentId: agent, Payload: &v1.Event_AgentStateChanged{AgentStateChanged: &v1.AgentStateChanged{From: from, To: to, Detail: detail}}}
}

func sent(from, to string, t v1.MessageType, body string) *v1.Event {
	return &v1.Event{Payload: &v1.Event_MessageSent{MessageSent: &v1.MessageSent{Message: &v1.Message{
		From: &v1.Actor{Id: from, Class: v1.ActorClass_ACTOR_CLASS_WORKER}, ToId: to, Type: t, Body: body}}}}
}

func exited(agent string, code int32, reason string) *v1.Event {
	return &v1.Event{AgentId: agent, Payload: &v1.Event_AgentExited{AgentExited: &v1.AgentExited{ExitCode: code, Reason: reason}}}
}

func TestPromptNotifiesOnceAtTheTransition(t *testing.T) {
	n := For(stateChanged("w1", v1.AgentState_AGENT_STATE_WORKING, v1.AgentState_AGENT_STATE_NEEDS_INPUT, "Bash"), lookup)
	if n == nil || n.Kind != Input || n.Title != "shop-fix needs you" || n.Body != "Bash" || n.Tab != TabTerminal || !n.Waiting() {
		t.Fatalf("got %+v", n)
	}
	if n := For(stateChanged("w1", v1.AgentState_AGENT_STATE_NEEDS_INPUT, v1.AgentState_AGENT_STATE_NEEDS_INPUT, ""), lookup); n != nil {
		t.Fatalf("still waiting notified: %+v", n)
	}
	if n := For(stateChanged("w1", v1.AgentState_AGENT_STATE_IDLE, v1.AgentState_AGENT_STATE_WORKING, ""), lookup); n != nil {
		t.Fatalf("working notified: %+v", n)
	}
	if n := For(stateChanged("w1", v1.AgentState_AGENT_STATE_WORKING, v1.AgentState_AGENT_STATE_NEEDS_INPUT, ""), lookup); n.Body != "It is stopped at a prompt in its terminal." {
		t.Fatalf("empty detail: %q", n.Body)
	}
}

func TestAnAgentThatHitsItsLimitOrIsInErrorNotifiesOnceAtTheTransition(t *testing.T) {
	for _, c := range []struct {
		to          v1.AgentState
		kind, title string
	}{
		{v1.AgentState_AGENT_STATE_LIMITED, Limited, "shop-fix hit its usage limit"},
		{v1.AgentState_AGENT_STATE_ERROR, Error, "shop-fix is in error"},
	} {
		n := For(stateChanged("w1", v1.AgentState_AGENT_STATE_WORKING, c.to, "5-hour window"), lookup)
		if n == nil || n.Kind != c.kind || n.Title != c.title || n.Body != "5-hour window" || n.Tab != TabTerminal || n.AgentID != "w1" || n.Waiting() {
			t.Fatalf("%v: got %+v", c.to, n)
		}
		if n := For(stateChanged("w1", c.to, c.to, ""), lookup); n != nil {
			t.Fatalf("%v: staying there notified: %+v", c.to, n)
		}
		if n := For(stateChanged("w1", v1.AgentState_AGENT_STATE_WORKING, c.to, ""), lookup); n.Body != "Its terminal says more." {
			t.Fatalf("%v: empty detail: %q", c.to, n.Body)
		}
	}
	// Leaving them notifies nothing.
	if n := For(stateChanged("w1", v1.AgentState_AGENT_STATE_LIMITED, v1.AgentState_AGENT_STATE_WORKING, ""), lookup); n != nil {
		t.Fatalf("recovering notified: %+v", n)
	}
}

func TestWhatReachesTheLeadNotifies(t *testing.T) {
	kind := func(mt v1.MessageType) *Notice { return For(sent("w1", "lead", mt, "# Plan\n- **do** `x`"), lookup) }
	for mt, want := range map[v1.MessageType]string{
		v1.MessageType_MESSAGE_TYPE_PLAN: Plan, v1.MessageType_MESSAGE_TYPE_QUESTION: Question,
		v1.MessageType_MESSAGE_TYPE_DONE: Report, v1.MessageType_MESSAGE_TYPE_FAILED: Failed,
	} {
		if n := kind(mt); n == nil || n.Kind != want || n.Tab != TabMessages || n.AgentID != "w1" {
			t.Fatalf("%v: got %+v", mt, n)
		}
	}
	if n := kind(v1.MessageType_MESSAGE_TYPE_PLAN); n.Body != "Plan do x" || n.Title != "shop-fix sent a plan" {
		t.Fatalf("got %+v", n)
	}
	if n := kind(v1.MessageType_MESSAGE_TYPE_INFO); n != nil {
		t.Fatalf("info notified: %+v", n)
	}
}

func TestWorkerTalkAndTheUsersOwnActionsAreNotNews(t *testing.T) {
	if n := For(sent("lead", "w1", v1.MessageType_MESSAGE_TYPE_PLAN, ""), lookup); n != nil {
		t.Fatalf("to a worker: %+v", n)
	}
	byDevice := sent("w1", "lead", v1.MessageType_MESSAGE_TYPE_QUESTION, "")
	byDevice.GetMessageSent().Message.From.Class = v1.ActorClass_ACTOR_CLASS_DEVICE
	if n := For(byDevice, lookup); n != nil {
		t.Fatalf("by a device: %+v", n)
	}
	if n := For(sent("ghost", "lead", v1.MessageType_MESSAGE_TYPE_PLAN, ""), lookup); n != nil {
		t.Fatalf("unknown sender: %+v", n)
	}
}

func TestADeathIsNewsUnlessTheUserStoppedTheAgent(t *testing.T) {
	if n := For(exited("w1", 2, ""), lookup); n.Title != "shop-fix exited with code 2" || n.Kind != Failed {
		t.Fatalf("got %+v", n)
	}
	if n := For(exited("w1", -1, "crash"), lookup); n.Title != "shop-fix exited with code -1" {
		t.Fatalf("got %+v", n)
	}
	if n := For(exited("w1", 0, "lost"), lookup); n.Title != "shop-fix was lost" || n.Kind != Failed {
		t.Fatalf("got %+v", n)
	}
	if n := For(exited("w1", 0, ""), lookup); n.Kind != Exited || n.Title != "shop-fix finished" || n.Tab != TabOverview {
		t.Fatalf("got %+v", n)
	}
	if n := For(exited("w1", -1, "killed"), lookup); n != nil {
		t.Fatalf("killed notified: %+v", n)
	}
}

func TestBriefIsPlainTextCutToANotificationsSize(t *testing.T) {
	for in, want := range map[string]string{
		"## a\n\n> b\n1. c":                "a b c",
		"Change **the typo** in `main.go`": "Change the typo in main.go",
		"does 2*3 equal 6?":                "does 2*3 equal 6?",
		"2*3 and 4*5":                      "2*3 and 4*5",
		"a *star and a ` tick":             "a *star and a ` tick",
		"```sh\nrun it\n```":               "run it",
		"*one* and ***three***":            "one and three",
	} {
		if got := Brief(in); got != want {
			t.Errorf("Brief(%q) = %q, want %q", in, got, want)
		}
	}
	if got := []rune(Brief(strings.Repeat("x", 500))); len(got) != 200 {
		t.Fatalf("len %d", len(got))
	}
	t0 := time.Now()
	if got := []rune(Brief(strings.Repeat("a *b* ", 500_000))); len(got) != 200 {
		t.Fatalf("len %d", len(got))
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("took %v", d)
	}
}
