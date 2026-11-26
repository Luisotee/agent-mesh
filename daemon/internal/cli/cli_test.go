package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

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
		if !strings.Contains(help.String(), "  "+spoken(a.CLI)+" ") {
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
	if code := Main([]string{"--state", "/x", "hook", "a"}, env, map[string]func(Env, []string) int{"hook": func(_ Env, a []string) int {
		ran = len(a) == 1
		return 0
	}}); code != 0 || !ran {
		t.Errorf("hand-written command not dispatched: %d", code)
	}
	if code := Main([]string{"agents", "--state", "/nonexistent"}, env, nil); code != ExitError || !strings.Contains(errb.String(), "device token") {
		t.Errorf("--state after the command: %d %q", code, errb.String())
	}
	out.Reset()
	if code := Main([]string{"help"}, env, map[string]func(Env, []string) int{"hook": nil}); code != ExitOK ||
		!strings.Contains(out.String(), "spawn") || !strings.Contains(out.String(), "hook") {
		t.Errorf("help: %d %q", code, out.String())
	}
}

// A request field named like one of the CLI's own flags would make the command unbuildable: say which.
func TestNoRequestFieldCollidesWithTheCLIsOwnFlags(t *testing.T) {
	reg, _ := registry.Mesh()
	own := map[string]bool{"state": true, "json": true, "mutation-id": true, "expires-in": true, "format": true}
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

// The global --state reaches the hand-written commands, before or after the command's name.
func TestGlobalStateReachesWhoamiAndDir(t *testing.T) {
	for _, args := range [][]string{{"--state", "/s", "dir", "--run", "r1x"}, {"dir", "--state", "/s", "--run", "r1x"}} {
		var out, errb bytes.Buffer
		env := Env{Stdout: &out, Stderr: &errb, Getenv: func(string) string { return "" }}
		hand := map[string]func(Env, []string) int{"dir": Dir}
		if code := Main(args, env, hand); code != ExitOK || strings.TrimSpace(out.String()) != "/s/runs/r1x" {
			t.Errorf("%v: %d %q %q", args, code, out.String(), errb.String())
		}
	}
	var out, errb bytes.Buffer
	env := Env{Stdout: &out, Stderr: &errb, Getenv: func(string) string { return "" }}
	if code := Dir(env, []string{"--help"}); code != ExitOK || !strings.Contains(out.String(), "-run") || errb.Len() != 0 {
		t.Errorf("dir --help: %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}
	out.Reset()
	if code := Whoami(env, []string{"-h"}); code != ExitOK || !strings.Contains(out.String(), "-state") || errb.Len() != 0 {
		t.Errorf("whoami -h: %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}
	// A bad flag says so on stderr, with the usage, and prints nothing to stdout.
	out.Reset()
	if code := Dir(env, []string{"--nope"}); code != ExitUsage || out.Len() != 0 || !strings.Contains(errb.String(), "nope") ||
		!strings.Contains(errb.String(), "-run") {
		t.Errorf("dir --nope: %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}
}

// `mesh --help` is `mesh help`: the usage on stdout, exit 0; a bad global flag is a usage error on stderr only.
func TestGlobalHelpIsTheHelpCommand(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"--state", "/s", "--help"}} {
		var out, errb bytes.Buffer
		env := Env{Stdout: &out, Stderr: &errb, Getenv: func(string) string { return "" }}
		if code := Main(args, env, nil); code != ExitOK || !strings.Contains(out.String(), "usage: mesh") || errb.Len() != 0 {
			t.Errorf("%v: %d stdout %q stderr %q", args, code, out.String(), errb.String())
		}
	}
	var out, errb bytes.Buffer
	env := Env{Stdout: &out, Stderr: &errb, Getenv: func(string) string { return "" }}
	if code := Main([]string{"--nope"}, env, nil); code != ExitUsage || out.Len() != 0 || !strings.Contains(errb.String(), "nope") ||
		!strings.Contains(errb.String(), "usage: mesh") {
		t.Errorf("--nope: %d stdout %q stderr %q", code, out.String(), errb.String())
	}
}

// An empty bare argument still counts as the one: a second is an error, not a replacement.
func TestAnEmptyPositionalIsStillThePositional(t *testing.T) {
	if _, err := Request(action(t, "send"), []string{"", "extra", "--type", "done"}); err == nil {
		t.Error(`send "" extra was accepted`)
	}
}

// Every flag says what it is for, in the schema's own words: spawn's --base is not just "text".
func TestHelpDescribesEachFlagFromTheSchemasComments(t *testing.T) {
	var out, errb bytes.Buffer
	env := Env{Stdout: &out, Stderr: &errb, Getenv: func(string) string { return "" }}
	if code := Main([]string{"help", "spawn"}, env, nil); code != ExitOK {
		t.Fatalf("%d %s", code, errb.String())
	}
	for _, want := range []string{"The branch or commit to start the worktree from", "The git repository: a path, or a bare name of a folder of the repos.dir setting.",
		"The task has a plan phase", "A text flag's value starting with @"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help spawn lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "text; @file reads a file") {
		t.Errorf("the @ rule is said once, not under every flag:\n%s", out.String())
	}
}

func TestInboxAndSettingsHaveATextForm(t *testing.T) {
	ts := timestamppb.New(time.Date(2026, 11, 26, 4, 12, 19, 0, time.UTC))
	var out bytes.Buffer
	renderInbox(&v1.ReadInboxResponse{Messages: []*v1.Message{
		{Id: "m1", Type: v1.MessageType_MESSAGE_TYPE_TASK, From: &v1.Actor{Name: "lead"}, CreatedAt: ts, Body: "do it\n"},
		{Id: "m2", Type: v1.MessageType_MESSAGE_TYPE_INFO, From: &v1.Actor{Name: "lead"}, CreatedAt: ts, Body: "fyi"},
	}}, &out)
	want := "── task from lead at 2026-11-26T04:12:19Z (m1) ──\ndo it\n\n── info from lead at 2026-11-26T04:12:19Z (m2) ──\nfyi\n"
	if out.String() != want {
		t.Errorf("inbox:\n%s\nwant\n%s", out.String(), want)
	}
	out.Reset()
	renderInbox(&v1.ReadInboxResponse{}, &out)
	if out.String() != "no messages\n" {
		t.Errorf("empty inbox: %q", out.String())
	}
	out.Reset()
	renderSettings(&v1.ListSettingsResponse{Settings: []*v1.Setting{
		{Key: "caps.agents", Value: structpb.NewNumberValue(8), Default: structpb.NewNumberValue(8)},
		{Key: "tiers.claude.mid.model", Value: structpb.NewStringValue("sonnet"), Default: structpb.NewStringValue("opus"), IsSet: true},
	}}, &out)
	if want := "caps.agents = 8\ntiers.claude.mid.model = \"sonnet\"   (set; default \"opus\")\n"; out.String() != want {
		t.Errorf("settings: %q", out.String())
	}
	// The same through the generic path: a dynamic response and the command's name.
	res := dynamicpb.NewMessage(action(t, "inbox").Method.Output())
	if err := renderText("inbox", res, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out.String(), "no messages\n") || !hasText("inbox") || hasText("spawn") {
		t.Errorf("renderText: %q", out.String())
	}
}

// --format is checked before the daemon is dialled: a bad or empty one is a usage error, with no daemon needed.
func TestAFormatThatIsNeitherTextNorJSONIsAUsageError(t *testing.T) {
	for _, format := range []string{"yaml", ""} {
		var out, errb bytes.Buffer
		env := Env{Stdout: &out, Stderr: &errb, Getenv: func(string) string { return "" }}
		code := Main([]string{"--state", t.TempDir(), "inbox", "--format", format}, env, nil)
		if code != ExitUsage || out.Len() != 0 || !strings.Contains(errb.String(), "--format is text or json") ||
			strings.Contains(errb.String(), "device token") {
			t.Errorf("--format %q: %d stdout %q stderr %q", format, code, out.String(), errb.String())
		}
	}
	// A command without a text form has no --format at all.
	var errb bytes.Buffer
	env := Env{Stdout: io.Discard, Stderr: &errb, Getenv: func(string) string { return "" }}
	if code := Main([]string{"--state", t.TempDir(), "spawn", "--format", "json"}, env, nil); code != ExitUsage {
		t.Errorf("spawn --format: %d %s", code, errb.String())
	}
}

func TestARepeatedTextFlagIsDescribedAsRepeatable(t *testing.T) {
	f := (&v1.HelloResponse{}).ProtoReflect().Descriptor().Fields().ByName("capabilities")
	if f == nil || !f.IsList() || f.Kind() != protoreflect.StringKind {
		t.Fatalf("the test needs a repeated string field: %v", f)
	}
	key := string(f.FullName())
	defer func(old string, had bool) {
		if had {
			v1.FieldDocs[key] = old
		} else {
			delete(v1.FieldDocs, key)
		}
	}(v1.FieldDocs[key], v1.FieldDocs[key] != "")
	v1.FieldDocs[key] = "What it can do."
	if got := flagHelp(f); got != "What it can do. (repeatable)" {
		t.Errorf("repeated text flag: %q", got)
	}
}

func TestReservePositionalAndUsageRenderer(t *testing.T) {
	m, err := Request(action(t, "reserve"), []string{"off", "--run-id", "r1"})
	if err != nil {
		t.Fatal(err)
	}
	req := as(t, m, &v1.SetReserveRequest{})
	if req.Mode != "off" || req.RunId != "r1" {
		t.Errorf("reserve positional: %+v", req)
	}

	var out bytes.Buffer
	renderUsage(&v1.GetUsageResponse{
		QuotaStop: 90,
		MinQuota:  10,
		Claude: &v1.ClaudeUsage{
			Source: "statusline",
			Windows: []*v1.ClaudeUsageWindow{
				{Key: "five_hour", Label: "five-hour window", Used: 92, ResetsAt: timestamppb.New(time.Date(2026, 11, 26, 18, 0, 0, 0, time.UTC))},
			},
			AtReserve: []string{"five-hour window"},
		},
		AgyRows: []*v1.AgyUsageRow{
			{Group: "Gemini 4 Neon", Label: "Neon", Left: 5, Resets: "1h"},
		},
		AgyTiers: []*v1.AgyTierPick{
			{Tier: "strong", Model: "hydrogen", Left: 80, HasLeft: true, Notes: []string{"neon at reserve (5% left)"}},
		},
	}, &out)
	s := out.String()
	for _, want := range []string{"Claude: five-hour window 92%", "at the reserve: five-hour window", "Gemini 4 Neon", "5% left", "agy strong: hydrogen (80% left)", "quota_stop"} {
		if !strings.Contains(s, want) {
			t.Errorf("renderUsage missing %q in:\n%s", want, s)
		}
	}
}

// The CLI rounds token counts as the desktop does (half up), so the two screens never differ by one.
func TestTokensTextRoundsHalfUpLikeTheDesktop(t *testing.T) {
	for n, want := range map[int64]string{300: "300", 999: "999", 1000: "1k", 2500: "3k", 2499: "2k", 340_000: "340k", 999_400: "999k", 999_600: "1.0M", 1_250_000: "1.3M", 1_240_000: "1.2M", 12_345_678: "12.3M"} {
		if got := tokensText(n); got != want {
			t.Errorf("tokensText(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestCostModelsAndConflictsRenderers(t *testing.T) {
	var out bytes.Buffer
	renderCost(&v1.GetCostResponse{
		Scope:       "run r1",
		PricesAsOf:  "2026-10-28",
		Usd:         0.18,
		TotalTokens: 1_246_000,
		TotalRead:   9_000_000,
		Clis: []*v1.CliCost{
			{Cli: "claude", Calls: 2, Input: 1_000_000, W1: 200_000, Output: 40_000, Total: 1_240_000, Read: 8_000_000, Basis: "exact: the API usage"},
			{Cli: "agy", Calls: 1, Input: 5000, Output: 1000, Total: 6000, Read: 1_000_000, Basis: "an estimate"},
		},
		Agents: []*v1.AgentCost{
			{Run: "r1", Name: "repo-fix", Kind: "worker", Calls: 2, Input: 800_000, W1: 100_000, Output: 4000, Total: 904_000, Read: 7_000_000, Usd: 0.05, PlanUsd: 0.02, ExecUsd: 0.03, SubUsd: 0.05, Cold: 1, ColdUsd: 0.02},
		},
		Kinds: []*v1.KindCost{
			{Kind: "worker", Sessions: 1, Calls: 2, Usd: 0.05, Share: 100, UsdPerCall: 0.025, OutPerCall: 2000, FirstCtxMedian: 30000, HasFirstCtxMedian: true},
		},
		ByModel: []*v1.ModelPhaseCost{
			{Model: "opus", Phase: "plan", Calls: 1, Usd: 0.02, OutPerCall: 1000},
		},
		Verdicts: []*v1.VerdictCostSummary{
			{Kind: "opencode", Model: "opencode/big-pickle", Tier: "mid", Reviews: 2, ByAuthor: 1, Findings: 8, AgreedPct: 38, HasAgreedPct: true, Blocking: 1},
		},
		Reviews: &v1.ReviewCostSummary{
			ReviewedPrs: 1, ReviewersPerPr: 1.0, HasReviewersPerPr: true,
			ByCli: map[string]float64{"opencode": 1.0},
		},
	}, &out)
	for _, want := range []string{"run r1: 1.2M tokens", "cache reads 9.0M apart", "≈ $0.18 at API prices", "total = input + cache write + output", "agy: an estimate", "cache write", "904k", "repo-fix", "by model and phase:", "review findings, by the judge's verdict", "38%", "reviews: 1 PRs, 1.0 reviewers per PR (opencode 1.0)", "root resets: 0"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("renderCost missing %q in:\n%s", want, out.String())
		}
	}

	out.Reset()
	renderModels(&v1.CheckModelsResponse{
		Kinds: []*v1.KindModels{
			{Kind: "agy", Listed: []*v1.ModelFamilyEntry{{Family: "neon", InTiers: true}, {Family: "xenon"}}},
		},
		Notice: "mesh: new agy models no tier names: xenon.",
	}, &out)
	if !strings.Contains(out.String(), "agy: neon (a tier), xenon") || !strings.Contains(out.String(), "xenon.") {
		t.Errorf("renderModels:\n%s", out.String())
	}

	out.Reset()
	renderConflicts(&v1.CheckConflictsResponse{
		CheckedBranches: 2,
		Conflicts:       []*v1.BranchConflict{{Repo: "repo", Branch: "mesh/r1/b1", Against: "mesh/r1/b2", Files: []string{"a.txt"}}},
		Overlaps:        []*v1.BranchConflict{{Repo: "repo", Branch: "mesh/r1/b1", Against: "mesh/r1/b3", Files: []string{"b.txt"}}},
	}, &out)
	for _, want := range []string{"mesh/r1/b1", "a.txt", "Note: these workers change the same files", "b.txt"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("renderConflicts missing %q in:\n%s", want, out.String())
		}
	}
	if !hasText("cost") || !hasText("models") || !hasText("conflicts") {
		t.Error("expected cost, models, conflicts to have text renderers")
	}
}

func TestPathOptionResolvesRelativeAndHomePathsAgainstCallerCwd(t *testing.T) {
	home := t.TempDir()
	wd := filepath.Join(home, "projects", "demo")
	env := Env{
		Stdin:  strings.NewReader(""),
		Stdout: io.Discard,
		Stderr: io.Discard,
		Getenv: func(k string) string {
			if k == "HOME" {
				return home
			}
			return ""
		},
		Getwd: func() (string, error) { return wd, nil },
	}
	parse := func(cmd string, args ...string) proto.Message {
		c := newCommand(action(t, cmd), env, "")
		m, err := c.parse(args)
		if err != nil {
			t.Fatalf("%s %v: %v", cmd, args, err)
		}
		return m
	}

	if req := as(t, parse("ask", "--repo", ".", "--tier", "mid", "--body", "hi"), &v1.AskRequest{}); req.Repo != wd {
		t.Errorf("ask --repo . = %q, want %q", req.Repo, wd)
	}
	if req := as(t, parse("spawn", "--repo", "../other", "--slug", "x", "--tier", "mid"), &v1.SpawnAgentRequest{}); req.Repo != filepath.Join(home, "projects", "other") {
		t.Errorf("spawn --repo ../other = %q", req.Repo)
	}
	if req := as(t, parse("add-peer", "--source", "./clone"), &v1.AddPeerRequest{}); req.Source != filepath.Join(wd, "clone") {
		t.Errorf("add-peer local = %q", req.Source)
	}
	if req := as(t, parse("add-peer", "--source", "https://github.com/org/repo.git"), &v1.AddPeerRequest{}); req.Source != "https://github.com/org/repo.git" {
		t.Errorf("add-peer https URL = %q", req.Source)
	}
	if req := as(t, parse("add-peer", "--source", "git@github.com:org/repo.git"), &v1.AddPeerRequest{}); req.Source != "git@github.com:org/repo.git" {
		t.Errorf("add-peer ssh URL = %q", req.Source)
	}
	if req := as(t, parse("import-peers"), &v1.ImportPeersRequest{}); req.From != "" {
		t.Errorf("import-peers default from = %q, want empty", req.From)
	}
	if req := as(t, parse("import-peers", "--from", "~/peers.json"), &v1.ImportPeersRequest{}); req.From != filepath.Join(home, "peers.json") {
		t.Errorf("import-peers ~/peers.json = %q", req.From)
	}
}

func TestRepoNameStaysBareUnlessAFolderOfThatNameIsHere(t *testing.T) {
	wd := t.TempDir()
	os.MkdirAll(filepath.Join(wd, "here", ".git"), 0o755)
	os.MkdirAll(filepath.Join(wd, "plain"), 0o755)
	env := Env{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard,
		Getenv: func(string) string { return "" }, Getwd: func() (string, error) { return wd, nil }}
	repo := func(cmd string, args ...string) string {
		t.Helper()
		m, err := newCommand(action(t, cmd), env, "").parse(args)
		if err != nil {
			t.Fatal(err)
		}
		if cmd == "ask" {
			return as(t, m, &v1.AskRequest{}).Repo
		}
		return as(t, m, &v1.SpawnAgentRequest{}).Repo
	}
	if got := repo("spawn", "--repo", "agent-mesh", "--slug", "x", "--tier", "mid"); got != "agent-mesh" {
		t.Errorf("a name with no folder here = %q: it goes to the daemon as it is", got)
	}
	if got := repo("ask", "--repo", "agent-mesh", "--tier", "mid", "--body", "x"); got != "agent-mesh" {
		t.Errorf("ask: %q", got)
	}
	// A plain subfolder of the repo the caller is in is no repo of that name (a monorepo's api/): the name goes to
	// the daemon, which finds the other repo in repos.dir.
	if got := repo("spawn", "--repo", "plain", "--slug", "x", "--tier", "mid"); got != "plain" {
		t.Errorf("a plain subfolder = %q: it is no repo, so the daemon looks the name up", got)
	}
	if got := repo("spawn", "--repo", "here", "--slug", "x", "--tier", "mid"); got != filepath.Join(wd, "here") {
		t.Errorf("a folder here wins: %q", got)
	}
	if got := repo("spawn", "--repo", "./agent-mesh", "--slug", "x", "--tier", "mid"); got != filepath.Join(wd, "agent-mesh") {
		t.Errorf("a path is a path: %q", got)
	}
}

func TestModelAndImportSettingsHaveTextForms(t *testing.T) {
	var out bytes.Buffer
	renderModel(&v1.ResolveModelResponse{Model: "hydrogen", Effort: "high", Chain: []*v1.ChainModel{
		{Model: "neon", Standing: "at the reserve (6% left)"}, {Model: "hydrogen"}}}, &out)
	if got := out.String(); got != "hydrogen high\n  neon   (at the reserve (6% left))\n  hydrogen\n" {
		t.Errorf("model: %q", got)
	}
	out.Reset()
	renderImportSettings(&v1.ImportSettingsResponse{File: "/f.json", DryRun: true, Imported: []*v1.ImportedSetting{
		{Key: "repos.dir", Value: func() *structpb.Value { v, _ := structpb.NewValue([]any{"~/p"}); return v }(), From: "repos_dir"}}, Skipped: []string{"hang_after: not used by the daemon"}}, &out)
	for _, want := range []string{"/f.json: would import 1 setting(s)", `repos.dir = ["~/p"]   (from repos_dir)`, "left alone: hang_after"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("import-settings lacks %q: %s", want, out.String())
		}
	}
	if !hasText("model") || !hasText("import-settings") {
		t.Error("no text renderer")
	}
}

func TestTwoWordToolsDispatchAndRenderers(t *testing.T) {
	// Dispatch two-word subcommands: tools add <names...> and tools servers.
	var helpOut, helpErr bytes.Buffer
	env := Env{Stdin: strings.NewReader(""), Stdout: &helpOut, Stderr: &helpErr, Getenv: func(string) string { return "" }}
	if code := Main([]string{"help", "tools", "add"}, env, nil); code != ExitOK || !strings.Contains(helpOut.String(), "usage: mesh tools add ") {
		t.Errorf("help tools add: %d out=%q err=%q", code, helpOut.String(), helpErr.String())
	}
	// The listing speaks the group form too.
	helpOut.Reset()
	if code := Main([]string{"help"}, env, nil); code != ExitOK || !strings.Contains(helpOut.String(), "tools servers") || strings.Contains(helpOut.String(), "tools-servers") {
		t.Errorf("help: %d out=%q", code, helpOut.String())
	}
	// A short plain word after a group that is no subcommand is a typo, not an agent.
	helpErr.Reset()
	if code := Main([]string{"tools", "ad", "x"}, env, nil); code != ExitUsage || !strings.Contains(helpErr.String(), `unknown subcommand "ad"`) || !strings.Contains(helpErr.String(), "add, servers") {
		t.Errorf("tools ad: %d err=%q", code, helpErr.String())
	}

	// Only a group joins its words: `mesh stop run` keeps `run` as an argument of `stop`, not `stop-run`.
	helpOut.Reset()
	if code := Main([]string{"help", "stop", "run"}, env, nil); code != ExitOK || strings.Contains(helpOut.String(), "stop-run") {
		t.Errorf("help stop run: %d out=%q", code, helpOut.String())
	}

	m, err := Request(action(t, "tools-add"), []string{"Edit", "--next", "finish", "playwright"})
	if err != nil {
		t.Fatal(err)
	}
	addReq := as(t, m, &v1.AddToolsRequest{})
	if !reflect.DeepEqual(addReq.Names, []string{"Edit", "playwright"}) || addReq.Next != "finish" {
		t.Errorf("tools-add parsed = %+v", addReq)
	}

	var out bytes.Buffer
	renderTools(&v1.ListToolsResponse{
		Lean:               true,
		Tools:              []string{"Edit"},
		Skills:             []string{"delegate-agy"},
		McpMode:            "repo",
		McpLoaded:          []string{"context7"},
		McpCut:             []string{"playwright"},
		ClaudeaiConnectors: "claude.ai",
		Added:              []string{"sentry"},
	}, &out)
	for _, want := range []string{"tools cut: Edit", "skills cut: delegate-agy", "mcp (repo): loaded [context7]; cut [playwright]", "connectors cut: claude.ai", "added back: sentry"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("renderTools missing %q in:\n%s", want, out.String())
		}
	}

	out.Reset()
	renderToolsServers(&v1.ListMCPServersResponse{Servers: []*v1.MCPServerInfo{
		{Name: "context7", About: "stdio ctx", Always: true},
		{Name: "playwright", About: "stdio pw", Exclusive: true, HeldBy: "repo-ui (run r1)"},
	}}, &out)
	for _, want := range []string{"context7    stdio ctx  [always loaded]", "playwright  stdio pw  [exclusive]  held by repo-ui (run r1)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("renderToolsServers missing %q in:\n%s", want, out.String())
		}
	}
}

func TestRenderLogAndRing(t *testing.T) {
	t1 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.Local)
	t2 := time.Date(2026, 5, 1, 10, 5, 0, 0, time.Local)
	t3 := time.Date(2026, 5, 2, 9, 0, 0, 0, time.Local)

	var out bytes.Buffer
	var st streamState
	for _, entry := range []*v1.LogEntry{
		{Time: timestamppb.New(t1), Kind: "run_start"},
		{Time: timestamppb.New(t2), Kind: "message/plan", Who: "w1 → lead", Detail: "ready"},
		{Time: timestamppb.New(t3), Kind: "run_stop"},
	} {
		renderLog(&v1.StreamLogResponse{Entry: entry}, &out, &st)
	}
	gotLog := out.String()
	for _, want := range []string{
		"── 2026-05-01 ──\n",
		"10:00:00  run_start\n",
		"10:05:00  message/plan     w1 → lead  ready\n",
		"── 2026-05-02 ──\n",
		"09:00:00  run_stop\n",
	} {
		if !strings.Contains(gotLog, want) {
			t.Errorf("renderLog missing %q in:\n%s", want, gotLog)
		}
	}

	out.Reset()
	renderRing(&v1.RingAgentResponse{Agent: &v1.Agent{Name: "w1"}, Unread: 0}, &out)
	renderRing(&v1.RingAgentResponse{Agent: &v1.Agent{Name: "w1"}, Unread: 2, Rung: true, Via: "socket"}, &out)
	renderRing(&v1.RingAgentResponse{Agent: &v1.Agent{Name: "w1"}, Unread: 1, Error: "human_draft"}, &out)
	gotRing := out.String()
	for _, want := range []string{
		"w1 has no unread messages.\n",
		"rung w1 (2 unread via socket)\n",
		"not rung w1 (1 unread): human_draft\n",
	} {
		if !strings.Contains(gotRing, want) {
			t.Errorf("renderRing missing %q in:\n%s", want, gotRing)
		}
	}
}

// renderPair draws the link as a QR code of half blocks above the link itself.
func TestRenderPairDrawsAQRCodeAndTheLink(t *testing.T) {
	uri := "agentmesh://pair?v=1&h=host&t=tok"
	var out bytes.Buffer
	renderPair(&v1.CreatePairingResponse{Uri: uri, HostName: "box", CertFingerprint: "AB:CD"}, &out)
	lines := strings.Split(out.String(), "\n")
	var qr []string
	for _, l := range lines {
		if l == "" {
			break
		}
		qr = append(qr, l)
	}
	if len(qr) < 10 {
		t.Fatalf("QR has %d lines:\n%s", len(qr), out.String())
	}
	width := len([]rune(qr[0]))
	for _, l := range qr {
		if len([]rune(l)) != width {
			t.Errorf("QR rows differ in width: %q", l)
		}
		if strings.Trim(l, " █▀▄") != "" {
			t.Errorf("QR row holds more than blocks: %q", l)
		}
	}
	for _, want := range []string{uri, "host: box", "certificate: AB:CD"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if !hasText("pair") {
		t.Error("pair has no --format flag")
	}
}
