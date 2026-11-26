package cli

// `mesh adopt` and the identity of an adopted session: a user's own Claude Code session the daemon made a run's
// root has no AGENT_MESH_TOKEN in its environment (the daemon didn't start it). Its `mesh` finds the identity
// the daemon kept for it by walking up its own process tree to that session (daemon/internal/daemon/adopt.go).

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/Luisotee/agent-mesh/daemon/internal/client"
	"github.com/Luisotee/agent-mesh/daemon/internal/proc"
	"github.com/Luisotee/agent-mesh/daemon/internal/statedir"
)

// ancestorsOf is ancestors; tests replace it.
var ancestorsOf = ancestors

// ancestors are the processes above this one, nearest first (at most 24).
func ancestors() []int {
	var out []int
	for pid := os.Getppid(); pid > 1 && len(out) < 24; {
		out = append(out, pid)
		next, err := proc.PPID(pid)
		if err != nil {
			break
		}
		pid = next
	}
	return out
}

// claudeSessionAbove is the nearest process above this one that is a Claude Code session: one with a record
// under the Claude config folder (`sessions/<pid>.json`).
func claudeSessionAbove(env Env) (pid int, start uint64, ok bool) {
	home := env.Getenv("CLAUDE_CONFIG_DIR")
	if home == "" {
		if h := env.Getenv("HOME"); h != "" {
			home = filepath.Join(h, ".claude")
		}
	}
	if home == "" {
		return 0, 0, false
	}
	for _, p := range ancestorsOf() {
		if _, err := os.Stat(filepath.Join(home, "sessions", strconv.Itoa(p)+".json")); err != nil {
			continue
		}
		if t, err := proc.StartTicks(p); err == nil {
			return p, t, true
		}
	}
	return 0, 0, false
}

// withAdoptedIdentity makes a `mesh` run from an adopted session act as that session's root: when no
// AGENT_MESH_TOKEN is set and a process above is an adopted session (its file in the state dir, and the process
// still the one that started at the recorded time), the AGENT_MESH_* variables the daemon recorded are the
// environment's.
func withAdoptedIdentity(env Env, state string) Env {
	if state == "" || env.Getenv(client.TokenEnv) != "" {
		return env
	}
	paths := statedir.Paths{Dir: state}
	for _, p := range ancestorsOf() {
		f, err := paths.ReadAdopted(p)
		if err != nil || f.Pid != p || !proc.Same(p, f.Start) || f.Env[client.TokenEnv] == "" {
			continue
		}
		inner := env.Getenv
		env.Getenv = func(k string) string {
			if v := inner(k); v != "" {
				return v
			}
			if strings.HasPrefix(k, "AGENT_MESH_") {
				return f.Env[k]
			}
			return ""
		}
		return env
	}
	return env
}

// fillAdoptRequest gives `mesh adopt` the session it runs in, unless --pid was given.
func fillAdoptRequest(env Env, req proto.Message) {
	m := req.ProtoReflect()
	pidF := m.Descriptor().Fields().ByName("pid")
	startF := m.Descriptor().Fields().ByName("pid_start_ticks")
	if pidF == nil || startF == nil || m.Has(pidF) {
		return
	}
	if pid, start, ok := claudeSessionAbove(env); ok {
		m.Set(pidF, protoreflect.ValueOfInt32(int32(pid)))
		m.Set(startF, protoreflect.ValueOfUint64(start))
	}
}
