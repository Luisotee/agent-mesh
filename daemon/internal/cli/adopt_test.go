package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Luisotee/agent-mesh/daemon/internal/proc"
	"github.com/Luisotee/agent-mesh/daemon/internal/registry"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
)

func withAncestors(t *testing.T, pids ...int) {
	t.Helper()
	old := ancestorsOf
	ancestorsOf = func() []int { return pids }
	t.Cleanup(func() { ancestorsOf = old })
}

// A `mesh` run from an adopted session takes the identity the daemon kept for it, found through the processes above
// it; a token already in the environment, a stale file (the pid now another process) or no file change nothing.
func TestAnAdoptedSessionsMeshActsAsItsRoot(t *testing.T) {
	state := t.TempDir()
	self := os.Getpid()
	start, err := proc.StartTicks(self)
	if err != nil {
		t.Skip("no /proc")
	}
	paths := statedir.Paths{Dir: state}
	os.MkdirAll(filepath.Dir(paths.Adopted(self)), 0o700)
	write := func(start uint64) {
		b := []byte(`{"pid":` + strconv.Itoa(self) + `,"start":` + strconv.FormatUint(start, 10) +
			`,"agent":"a1","env":{"AGENT_MESH_TOKEN":"tok","AGENT_MESH_AGENT":"a1","AGENT_MESH_RUN":"r1","OTHER":"no"}}`)
		if err := os.WriteFile(paths.Adopted(self), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	base := Env{Getenv: func(k string) string { return map[string]string{"HOME": "/h"}[k] }}
	withAncestors(t, 1234567, self)
	write(start)
	env := withAdoptedIdentity(base, state)
	if env.Getenv("AGENT_MESH_TOKEN") != "tok" || env.Getenv("AGENT_MESH_RUN") != "r1" || env.Getenv("HOME") != "/h" || env.Getenv("OTHER") != "" {
		t.Errorf("identity: token %q run %q home %q other %q", env.Getenv("AGENT_MESH_TOKEN"), env.Getenv("AGENT_MESH_RUN"), env.Getenv("HOME"), env.Getenv("OTHER"))
	}
	// A token of its own wins: an agent started by the daemon is itself.
	own := Env{Getenv: func(k string) string { return map[string]string{"AGENT_MESH_TOKEN": "mine"}[k] }}
	if got := withAdoptedIdentity(own, state).Getenv("AGENT_MESH_TOKEN"); got != "mine" {
		t.Errorf("an agent's own token: %q", got)
	}
	write(start + 1) // the pid now belongs to another process
	if got := withAdoptedIdentity(base, state).Getenv("AGENT_MESH_TOKEN"); got != "" {
		t.Errorf("a stale file: %q", got)
	}
	withAncestors(t, 1234567)
	write(start)
	if got := withAdoptedIdentity(base, state).Getenv("AGENT_MESH_TOKEN"); got != "" {
		t.Errorf("no adopted session above: %q", got)
	}
}

// `mesh adopt` finds the Claude Code session above it (a process with a record under the Claude config folder) and
// fills its pid and start; --pid given by hand is kept.
func TestAdoptFindsTheClaudeSessionAboveIt(t *testing.T) {
	self := os.Getpid()
	start, err := proc.StartTicks(self)
	if err != nil {
		t.Skip("no /proc")
	}
	cfg := t.TempDir()
	os.MkdirAll(filepath.Join(cfg, "sessions"), 0o700)
	os.WriteFile(filepath.Join(cfg, "sessions", strconv.Itoa(self)+".json"), []byte(`{"pid":`+strconv.Itoa(self)+`}`), 0o600)
	env := Env{Getenv: func(k string) string { return map[string]string{"CLAUDE_CONFIG_DIR": cfg}[k] }}
	reg, err := registry.Mesh()
	if err != nil {
		t.Fatal(err)
	}
	a := find(reg, "adopt")
	if a == nil {
		t.Fatal("no adopt command")
	}
	parse := func(args ...string) (pid int64, ticks uint64) {
		cmd := newCommand(a, env, "/s")
		req, err := cmd.parse(args)
		if err != nil {
			t.Fatal(err)
		}
		fillAdoptRequest(env, req)
		m := req.ProtoReflect()
		return m.Get(m.Descriptor().Fields().ByName("pid")).Int(), m.Get(m.Descriptor().Fields().ByName("pid_start_ticks")).Uint()
	}
	withAncestors(t, 1234567, self)
	if pid, ticks := parse("--run-id", "r1"); pid != int64(self) || ticks != start {
		t.Errorf("found pid %d start %d, want %d %d", pid, ticks, self, start)
	}
	if pid, _ := parse("--run-id", "r1", "--pid", "42"); pid != 42 {
		t.Errorf("--pid was overwritten: %d", pid)
	}
	withAncestors(t, 1234567)
	if pid, _ := parse("--run-id", "r1"); pid != 0 {
		t.Errorf("no session above, yet pid %d", pid)
	}
}
