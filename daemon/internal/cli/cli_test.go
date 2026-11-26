package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/registry"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
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
		cmd := newCommand(a, Env{Stderr: &bytes.Buffer{}, Getenv: func(string) string { return "" }}, "")
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
	if _, err := Request(action(t, "settings"), []string{"tiers."}); err == nil {
		t.Error("a positional argument was accepted by a command with no agent or to field")
	}
}

// The one bare argument is the request's `to` or `agent`, wherever it stands among the flags.
func TestAPositionalArgumentFillsToOrAgent(t *testing.T) {
	for _, args := range [][]string{
		{"parent", "--type", "done", "--body", "x"},
		{"--type", "done", "parent", "--body", "x"},
		{"--type", "done", "--body", "x", "parent"},
	} {
		m, err := Request(action(t, "send"), args)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if s := as(t, m, &v1.SendMessageRequest{}); s.To != "parent" || s.Body != "x" || s.Type != v1.MessageType_MESSAGE_TYPE_DONE {
			t.Errorf("%v: %v", args, s)
		}
	}
	m, err := Request(action(t, "approve"), []string{"--tier", "mid", "repo-fix", "--body", "x"})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if a := as(t, m, &v1.ApproveAgentRequest{}); a.Agent != "repo-fix" || a.Tier != "mid" {
		t.Errorf("approve %v", a)
	}
	for name, args := range map[string][]string{
		"given twice, argument and flag": {"parent", "--to", "other", "--type", "done"},
		"two arguments":                  {"parent", "other", "--type", "done"},
	} {
		if _, err := Request(action(t, "send"), args); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := Request(action(t, "agent"), []string{"a1"}); err != nil {
		t.Errorf("agent a1: %v", err)
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

// A request field named like one of the CLI's own flags would make the command unbuildable: say which.
func TestNoRequestFieldCollidesWithTheCLIsOwnFlags(t *testing.T) {
	reg, _ := registry.Mesh()
	own := map[string]bool{"state": true, "json": true, "mutation-id": true, "expires-in": true}
	for _, a := range reg.Actions() {
		fields := a.Method.Input().Fields()
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			if a.Write && f.Name() == "meta" {
				continue
			}
			if own[FlagName(f)] {
				t.Errorf("%s: request field %q is also the CLI's --%s", a.CLI, f.Name(), FlagName(f))
			}
		}
	}
}

func TestAtSignRules(t *testing.T) {
	read := func(stdin string, args ...string) (*v1.SendMessageRequest, error) {
		c := newCommand(action(t, "send"), Env{Stdin: strings.NewReader(stdin), Getenv: func(string) string { return "" }}, "")
		m, err := c.parse(append([]string{"--type", "info", "--to", "a1"}, args...))
		if err != nil {
			return nil, err
		}
		return as(t, m, &v1.SendMessageRequest{}), nil
	}
	// `@-` reads stdin once and the field gets it (it was empty when stdin was read twice).
	if got, err := read("fix the typo\n", "--body", "@-"); err != nil || got.Body != "fix the typo\n" {
		t.Errorf("@-: %q, %v", got.GetBody(), err)
	}
	if _, err := read("x", "--body", "@-", "--to", "@-"); err == nil || !strings.Contains(err.Error(), "read once") {
		t.Errorf("stdin for two fields: %v", err)
	}
	// A bare @ is itself, @@ escapes, and a path that doesn't open says how to write a literal @.
	if got, err := read("", "--body", "@"); err != nil || got.Body != "@" {
		t.Errorf("bare @: %q, %v", got.GetBody(), err)
	}
	if _, err := read("", "--body", "@lead done"); err == nil || !strings.Contains(err.Error(), "start it with @@") {
		t.Errorf("an @mention: %v", err)
	}
	// A file is read once, at parse.
	file := filepath.Join(t.TempDir(), "b.md")
	os.WriteFile(file, []byte("from a file"), 0o600)
	if got, err := read("", "--body", "@"+file); err != nil || got.Body != "from a file" {
		t.Errorf("@file: %q, %v", got.GetBody(), err)
	}
}

func TestAParseErrorIsPrintedOnce(t *testing.T) {
	var out, errb bytes.Buffer
	env := Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, Getenv: func(string) string { return "" }}
	if code := Main([]string{"--state", "/x", "send", "--type", "nonsense"}, env, nil); code != ExitUsage {
		t.Errorf("exit %d", code)
	}
	if n := strings.Count(strings.TrimSpace(errb.String()), "\n") + 1; n != 1 || strings.Contains(errb.String(), "-mutation-id") {
		t.Errorf("a bad value is one line, without the whole flag list (%d lines):\n%s", n, errb.String())
	}
	errb.Reset()
	if code := Main([]string{"--state", "/x", "send", "--nope"}, env, nil); code != ExitUsage || strings.Count(errb.String(), "flag provided but not defined") != 1 {
		t.Errorf("an unknown flag: %d %q", code, errb.String())
	}
	out.Reset()
	if code := Main([]string{"--state", "/x", "send", "-h"}, env, nil); code != ExitOK || !strings.Contains(out.String(), "-mutation-id") {
		t.Errorf("-h: %d %q", code, out.String())
	}
}

// A google.protobuf.Value flag takes any JSON, and text that isn't JSON is the string it spells.
func TestAValueFlagTakesJSONOrBareText(t *testing.T) {
	read := func(args ...string) *v1.SetSettingRequest {
		t.Helper()
		m, err := Request(action(t, "set"), append([]string{"--key", "k"}, args...))
		if err != nil {
			t.Fatal(err)
		}
		return as(t, m, &v1.SetSettingRequest{})
	}
	for _, c := range []struct {
		arg  string
		want any
	}{
		{"75", 75.0}, {"true", true}, {"null", nil}, {`"5"`, "5"}, {"sonnet", "sonnet"}, {"auto", "auto"},
		{`["haiku","sonnet"]`, []any{"haiku", "sonnet"}}, {"opencode/free", "opencode/free"},
	} {
		got := read("--value", c.arg).Value.AsInterface()
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("--value %s is %#v, want %#v", c.arg, got, c.want)
		}
	}
	// A typo in JSON is an error, not a string.
	for _, bad := range []string{`["sonnet"`, `{"a":`, `"open`} {
		if _, err := Request(action(t, "set"), []string{"--key", "k", "--value", bad}); err == nil {
			t.Errorf("--value %s was accepted", bad)
		}
	}
	// The whole request as JSON, before the command or after it.
	if got := read("--json", `{"value":["a"]}`).Value.AsInterface(); !reflect.DeepEqual(got, []any{"a"}) {
		t.Errorf("--json: %#v", got)
	}
	if help := describe(action(t, "set").Method.Input().Fields().ByName("value")); !strings.Contains(help, "bare text") {
		t.Errorf("help: %q", help)
	}
}

// A repeated field's flags add up; the first replaces what --json gave; the help says so only for those.
func TestRepeatedFlagsAddUpAndTheFirstReplacesJSON(t *testing.T) {
	desc := (&v1.HelloResponse{}).ProtoReflect().Descriptor().Fields().ByName("capabilities")
	if desc == nil || !desc.IsList() {
		t.Fatal("HelloResponse.capabilities is not a list any more: pick another repeated field")
	}
	c := &command{req: dynamicpb.NewMessage((&v1.HelloResponse{}).ProtoReflect().Descriptor()), listSeen: map[protoreflect.FieldNumber]bool{}}
	if err := protojson.Unmarshal([]byte(`{"capabilities":["from-json"]}`), c.req); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"a", "b", "c"} {
		val, err := c.value(desc, v)
		if err != nil {
			t.Fatal(err)
		}
		c.set(desc, val)
	}
	if got := as(t, c.req, &v1.HelloResponse{}).Capabilities; !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("capabilities %v", got)
	}
	if !strings.Contains(describe(desc), "(repeatable)") {
		t.Errorf("a list's help: %q", describe(desc))
	}
	if strings.Contains(describe(action(t, "send").Method.Input().Fields().ByName("body")), "repeatable") {
		t.Error("a scalar's help says repeatable")
	}
}

// `mesh --json <request> <command>`: the global flag is the command's own.
func TestGlobalJSONIsTheCommandsRequest(t *testing.T) {
	var out, errb bytes.Buffer
	env := Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, Getenv: func(string) string { return "" }}
	// No state: Main stops after parsing, so a bad request shows the flag reached the command.
	if code := Main([]string{"--state", "/x", "--json", `{"key":`, "set"}, env, nil); code != ExitUsage || !strings.Contains(errb.String(), "--json") {
		t.Errorf("a bad global --json: %d %q", code, errb.String())
	}
	errb.Reset()
	if code := Main([]string{"--json"}, env, nil); code != ExitUsage {
		t.Errorf("--json without a value: %d", code)
	}
	if !strings.Contains(func() string { var b bytes.Buffer; usage(&b, mustReg(t), nil); return b.String() }(), "--json") {
		t.Error("the usage line doesn't mention --json")
	}
}

func mustReg(t *testing.T) *registry.Registry {
	t.Helper()
	reg, err := registry.Mesh()
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// With no --state and no $AGENT_MESH_STATE the CLI uses the installed daemon's state dir, only when it is there.
func TestDefaultStateIsTheInstalledDaemonsWhenItsSocketExists(t *testing.T) {
	home := t.TempDir()
	envOf := func(m map[string]string) Env { return Env{Getenv: func(k string) string { return m[k] }} }
	if got := DefaultState(envOf(map[string]string{"HOME": home})); got != "" {
		t.Errorf("no daemon: %q", got)
	}
	dir := filepath.Join(home, ".local", "state", DefaultStateDir)
	os.MkdirAll(dir, 0o700)
	os.WriteFile(statedir.Paths{Dir: dir}.Socket(), nil, 0o600)
	if got := DefaultState(envOf(map[string]string{"HOME": home})); got != dir {
		t.Errorf("default %q, want %q", got, dir)
	}
	if got := DefaultState(envOf(map[string]string{"HOME": home, "AGENT_MESH_STATE": "/x"})); got != "/x" {
		t.Errorf("the environment wins: %q", got)
	}
	xdg := filepath.Join(home, "xdg")
	if got := DefaultState(envOf(map[string]string{"HOME": home, "XDG_STATE_HOME": xdg})); got != "" {
		t.Errorf("XDG_STATE_HOME is read first: %q", got)
	}
	if got := DefaultState(envOf(map[string]string{})); got != "" {
		t.Errorf("no HOME: %q", got)
	}
}

func TestDirTakesOnlyARunID(t *testing.T) {
	var out, errb bytes.Buffer
	env := Env{Stdout: &out, Stderr: &errb, Getenv: func(string) string { return "" }}
	if code := Dir(env, []string{"--state", "/s", "--run", "r1x"}); code != ExitOK || strings.TrimSpace(out.String()) != "/s/runs/r1x" {
		t.Errorf("dir: %d %q", code, out.String())
	}
	if code := Dir(env, []string{"--state", "/s", "--run", "../../x"}); code != ExitUsage {
		t.Errorf("a path as a run id: %d", code)
	}
}
