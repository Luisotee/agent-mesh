package daemon

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/proc"
)

func TestEvalLineIsTheCommandTheUserTyped(t *testing.T) {
	for in, want := range map[string]string{
		"source /h/.claude/shell-snapshots/s.sh 2>/dev/null || true && eval 'make test'": "make test",
		`source /h/.claude/shell-snapshots/s.sh && eval "go test ./... -run X"`:          "go test ./... -run X",
		"source /h/.claude/shell-snapshots/s.sh && eval 'echo it'\\''s' < /dev/null":     "echo it'\\''s",
		"source /h/.claude/shell-snapshots/s.sh":                                         "",
	} {
		if got := evalLine(in); got != want {
			t.Errorf("evalLine(%q) = %q, want %q", in, got, want)
		}
	}
	table := []proc.Info{{PID: 10, PPID: 1, Argv: []string{"claude"}}, {PID: 11, PPID: 10, Argv: []string{"bash", "-c", "source /h/.claude/shell-snapshots/s.sh && eval 'sleep 600'"}}}
	if cmd, ok := backgroundCommand(table, 10); !ok || cmd != "sleep 600" {
		t.Errorf("backgroundCommand = %q, %v", cmd, ok)
	}
}

func msg(id string, typ v1.MessageType, from, to *v1.Agent, body string) *v1.Message {
	return &v1.Message{Id: id, Type: typ, From: &v1.Actor{Id: from.Id}, ToId: to.Id, Body: body}
}

func TestWaitFromNamesWhatAnIdleAgentWaitsOn(t *testing.T) {
	lead := &v1.Agent{Id: "L", Name: "lead"}
	w := &v1.Agent{Id: "W", Name: "worker", ParentId: "L"}
	until := timestamppb.New(time.Date(2026, 11, 27, 14, 40, 0, 0, time.UTC))
	wait := msg("3", v1.MessageType_MESSAGE_TYPE_WAIT, w, lead, "CI on #12\nsecond line")
	wait.WaitUntil = until
	for _, tc := range []struct {
		name string
		msgs []*v1.Message
		kind v1.AgentWaitKind
		from string
		det  string
	}{
		{"nothing said", nil, v1.AgentWaitKind_AGENT_WAIT_KIND_MESSAGE, "lead", ""},
		{"a task and nothing since", []*v1.Message{msg("1", v1.MessageType_MESSAGE_TYPE_TASK, lead, w, "go")}, v1.AgentWaitKind_AGENT_WAIT_KIND_MESSAGE, "lead", ""},
		{"a declared wait", []*v1.Message{msg("1", v1.MessageType_MESSAGE_TYPE_TASK, lead, w, "go"), wait}, v1.AgentWaitKind_AGENT_WAIT_KIND_DECLARED, "lead", "CI on #12"},
		{"a question unanswered", []*v1.Message{msg("1", v1.MessageType_MESSAGE_TYPE_TASK, lead, w, "go"), msg("2", v1.MessageType_MESSAGE_TYPE_QUESTION, w, lead, "which one?")}, v1.AgentWaitKind_AGENT_WAIT_KIND_ANSWER, "lead", "which one?"},
		{"a plan unanswered", []*v1.Message{msg("2", v1.MessageType_MESSAGE_TYPE_PLAN, w, lead, "the plan")}, v1.AgentWaitKind_AGENT_WAIT_KIND_ANSWER, "lead", "the plan"},
		{"the answer came", []*v1.Message{msg("2", v1.MessageType_MESSAGE_TYPE_QUESTION, w, lead, "which?"), msg("3", v1.MessageType_MESSAGE_TYPE_ANSWER, lead, w, "this")}, v1.AgentWaitKind_AGENT_WAIT_KIND_MESSAGE, "lead", ""},
		{"a wait then a task", []*v1.Message{wait, msg("4", v1.MessageType_MESSAGE_TYPE_TASK, lead, w, "now this")}, v1.AgentWaitKind_AGENT_WAIT_KIND_MESSAGE, "lead", ""},
		{"a done reported", []*v1.Message{msg("1", v1.MessageType_MESSAGE_TYPE_TASK, lead, w, "go"), msg("5", v1.MessageType_MESSAGE_TYPE_DONE, w, lead, "done")}, v1.AgentWaitKind_AGENT_WAIT_KIND_REPORTED, "lead", ""},
		{"a wait after a done", []*v1.Message{msg("5", v1.MessageType_MESSAGE_TYPE_DONE, w, lead, "done"), wait}, v1.AgentWaitKind_AGENT_WAIT_KIND_DECLARED, "lead", "CI on #12"},
		{"an info to the parent says nothing", []*v1.Message{msg("1", v1.MessageType_MESSAGE_TYPE_TASK, lead, w, "go"), msg("6", v1.MessageType_MESSAGE_TYPE_INFO, w, lead, "fyi")}, v1.AgentWaitKind_AGENT_WAIT_KIND_MESSAGE, "lead", ""},
	} {
		got := waitFrom(w, "lead", tc.msgs)
		if got.Kind != tc.kind || got.From != tc.from || got.Detail != tc.det {
			t.Errorf("%s: %v from %q detail %q, want %v from %q detail %q", tc.name, got.Kind, got.From, got.Detail, tc.kind, tc.from, tc.det)
		}
	}
	if got := waitFrom(w, "lead", []*v1.Message{wait}); got.Until == nil || !got.Until.AsTime().Equal(until.AsTime()) {
		t.Errorf("a declared wait lost its deadline: %v", got.Until)
	}
}
