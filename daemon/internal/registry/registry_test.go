package registry

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

var update = flag.Bool("update", false, "rewrite testdata/actors.golden")

// TestActorsGolden pins what each actor class may do. A change to who may call what shows up here as a diff
// to review, not as a silent change of an option. Run with -update to accept one.
func TestActorsGolden(t *testing.T) {
	reg, err := Mesh()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for c := v1.ActorClass(1); ; c++ {
		name, ok := v1.ActorClass_name[int32(c)]
		if !ok {
			break
		}
		actions, perms := reg.For(c)
		b.WriteString(name + "\n")
		for _, a := range actions {
			b.WriteString("  " + a + "\n")
		}
		for _, p := range perms {
			b.WriteString("  permission " + p + "\n")
		}
	}
	path := filepath.Join("testdata", "actors.golden")
	if *update {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != string(want) {
		t.Errorf("actor permissions changed; review, then go test ./internal/registry -update\n--- got\n%s--- want\n%s", b.String(), want)
	}
}

func TestEveryActionIsComplete(t *testing.T) {
	reg, err := Mesh()
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Actions()) == 0 {
		t.Fatal("empty registry")
	}
	for _, a := range reg.Actions() {
		if a.Summary == "" {
			t.Errorf("%s has no summary (the CLI's help)", a.Procedure)
		}
		if a.Write && a.Method.Input().Fields().ByName("meta") == nil {
			t.Errorf("%s writes but its request has no RequestMeta `meta`", a.Procedure)
		}
		if !a.Write && a.Method.Input().Fields().ByName("meta") != nil {
			t.Errorf("%s has a `meta` but isn't marked write", a.Procedure)
		}
	}
	if !reg.Has("terminal.input", v1.ActorClass_ACTOR_CLASS_DEVICE) || reg.Has("terminal.input", v1.ActorClass_ACTOR_CLASS_WORKER) {
		t.Error("terminal.input should be the device's only")
	}
	if reg.Allowed("/agentmesh.v1.MeshService/Nope", v1.ActorClass_ACTOR_CLASS_DEVICE) {
		t.Error("an unknown procedure was allowed")
	}
}

func TestAServiceWithoutActionOptionsIsRefused(t *testing.T) {
	_, err := New(termv1.File_agentmesh_term_v1_term_proto.Services().ByName("TermHostService"))
	if err == nil || !strings.Contains(err.Error(), "no (agentmesh.v1.action) option") {
		t.Errorf("err = %v", err)
	}
}

func TestKebab(t *testing.T) {
	for in, want := range map[string]string{"SpawnAgent": "spawn-agent", "Hello": "hello", "ReportHook": "report-hook"} {
		if got := Kebab(in); got != want {
			t.Errorf("Kebab(%q) = %q", in, got)
		}
	}
}
