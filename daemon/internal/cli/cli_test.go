package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/registry"
)

func action(t *testing.T, name string) *registry.Action {
	t.Helper()
	reg, err := registry.Mesh()
	if err != nil {
		t.Fatal(err)
	}
	a := find(reg, name)
	if a == nil {
		t.Fatalf("no command %q", name)
	}
	return a
}

// as decodes a dynamic request into its generated type.
func as[T proto.Message](t *testing.T, m proto.Message, into T) T {
	t.Helper()
	b, err := protojson.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := protojson.Unmarshal(b, into); err != nil {
		t.Fatal(err)
	}
	return into
}

func TestEveryActionIsACommandWithItsFieldsAsFlags(t *testing.T) {
	reg, _ := registry.Mesh()
	var help bytes.Buffer
	usage(&help, reg, nil)
	for _, a := range reg.Actions() {
		if !strings.Contains(help.String(), "  "+a.CLI+" ") {
			t.Errorf("%s missing from help", a.CLI)
		}
		cmd := newCommand(a, Env{Stderr: &bytes.Buffer{}, Getenv: func(string) string { return "" }}, "", io.Discard)
		fields := a.Method.Input().Fields()
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			name := FlagName(f)
			if f.Name() == "meta" {
				name = "mutation-id"
			}
			if cmd.flags.Lookup(name) == nil {
				t.Errorf("%s has no --%s", a.CLI, name)
			}
		}
	}
}

func TestFlagsBuildTheRequest(t *testing.T) {
	file := filepath.Join(t.TempDir(), "task.md")
	os.WriteFile(file, []byte("the task\nin two lines"), 0o600)
	m, err := Request(action(t, "spawn"), []string{"--repo", "/r", "--slug", "fix", "--role", "reviewer",
		"--task", "@" + file, "--mutation-id", "m1"})
	if err != nil {
		t.Fatal(err)
	}
	req := as(t, m, &v1.SpawnAgentRequest{})
	if req.Repo != "/r" || req.Slug != "fix" || req.Role != v1.AgentRole_AGENT_ROLE_REVIEWER ||
		req.Task != "the task\nin two lines" || req.Meta.GetMutationId() != "m1" {
		t.Errorf("request %v", req)
	}

	// A write always gets a mutation id; --json gives the whole request, and flags refine it.
	m, err = Request(action(t, "send"), []string{"--type", "MESSAGE_TYPE_TASK", "--json", `{"to":"a1","body":"x"}`, "--body", "y"})
	if err != nil {
		t.Fatal(err)
	}
	send := as(t, m, &v1.SendMessageRequest{})
	if send.To != "a1" || send.Body != "y" || send.Type != v1.MessageType_MESSAGE_TYPE_TASK || send.Meta.GetMutationId() == "" {
		t.Errorf("send %v", send)
	}

	// A message field takes JSON.
	m, err = Request(action(t, "events"), []string{"--cursor", `{"generation":"g1","seq":"7"}`, "--run-id", "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if ev := as(t, m, &v1.SubscribeEventsRequest{}); ev.Cursor.GetSeq() != 7 || ev.RunId != "r1" {
		t.Errorf("events %v", ev)
	}
	// A bool field is a switch; @@ escapes a leading @; a flag after --json (one dash or two) still wins, and
	// an explicit --mutation-id beats the JSON's.
	m, err = Request(action(t, "inbox"), []string{"--peek", "--all"})
	if err != nil {
		t.Fatal(err)
	}
	if in := as(t, m, &v1.ReadInboxRequest{}); !in.Peek || !in.All {
		t.Errorf("switches %v", in)
	}
	m, err = Request(action(t, "send"), []string{"--body", "@@root done", "-json={\"to\":\"a1\",\"meta\":{\"mutationId\":\"old\"}}",
		"--mutation-id", "new"})
	if err != nil {
		t.Fatal(err)
	}
	if send := as(t, m, &v1.SendMessageRequest{}); send.Body != "@root done" || send.To != "a1" || send.Meta.GetMutationId() != "new" {
		t.Errorf("send %v", send)
	}
	// A value that looks like a flag is still just the value.
	m, err = Request(action(t, "send"), []string{"--body", "--json", "--to", "a2"})
	if err != nil {
		t.Fatal(err)
	}
	if send := as(t, m, &v1.SendMessageRequest{}); send.Body != "--json" || send.To != "a2" {
		t.Errorf("send %v", send)
	}
	if _, err := Request(action(t, "send"), []string{"--type", "nonsense"}); err == nil {
		t.Error("a bad enum was accepted")
	}
	if _, err := Request(action(t, "agent"), []string{"a1"}); err == nil {
		t.Error("a positional argument was accepted")
	}
}

func TestUnknownCommandAndMissingState(t *testing.T) {
	var out, errb bytes.Buffer
	env := Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, Getenv: func(string) string { return "" }}
	if code := Main([]string{"nope"}, env, nil); code != ExitUsage {
		t.Errorf("unknown command: %d", code)
	}
	if code := Main([]string{"agents"}, env, nil); code != ExitUsage || !strings.Contains(errb.String(), "--state") {
		t.Errorf("no state: %d %q", code, errb.String())
	}
	out.Reset()
	if code := Main([]string{"help", "spawn"}, env, nil); code != ExitOK || !strings.Contains(out.String(), "-slug") {
		t.Errorf("help spawn to stdout: %d %q", code, out.String())
	}
	if code := Main([]string{"help", "spwan"}, env, nil); code != ExitUsage {
		t.Errorf("help for an unknown command: %d", code)
	}
	ran := false
	if code := Main([]string{"--state", "/x", "hook", "a"}, env, map[string]func([]string) int{"hook": func(a []string) int {
		ran = len(a) == 1
		return 0
	}}); code != 0 || !ran {
		t.Errorf("hand-written command not dispatched: %d", code)
	}
	if code := Main([]string{"agents", "--state", "/nonexistent"}, env, nil); code != ExitError || !strings.Contains(errb.String(), "device token") {
		t.Errorf("--state after the command: %d %q", code, errb.String())
	}
	out.Reset()
	if code := Main([]string{"help"}, env, map[string]func([]string) int{"hook": nil}); code != ExitOK ||
		!strings.Contains(out.String(), "spawn") || !strings.Contains(out.String(), "hook") {
		t.Errorf("help: %d %q", code, out.String())
	}
}
