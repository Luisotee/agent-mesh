package daemon

import (
	"testing"

	"github.com/Luisotee/agent-mesh/daemon/internal/proc"
)

// A Claude background command is a `bash -c` that sources a shell snapshot, under the agent's own process; its
// MCP servers and any other shell are not.
func TestHasBackgroundCommandFindsOnlyClaudesShellSnapshotShells(t *testing.T) {
	const snap = "source /home/u/.claude/shell-snapshots/snapshot-bash-1.sh 2>/dev/null || true && eval 'make test'"
	claude := proc.Info{PID: 10, PPID: 1, Argv: []string{"claude", "--resume", "x"}}
	for _, tc := range []struct {
		name  string
		procs []proc.Info
		want  bool
	}{
		{"a background command", []proc.Info{claude, {PID: 11, PPID: 10, Argv: []string{"/bin/bash", "-c", snap}}}, true},
		{"under a wrapper", []proc.Info{claude, {PID: 11, PPID: 10, Argv: []string{"env", "x"}}, {PID: 12, PPID: 11, Argv: []string{"sh", "-c", snap}}}, true},
		{"only an MCP server", []proc.Info{claude, {PID: 11, PPID: 10, Argv: []string{"node", "/x/mcp/index.js", "--flag"}}}, false},
		{"a shell with no snapshot", []proc.Info{claude, {PID: 11, PPID: 10, Argv: []string{"bash", "-c", "make test"}}}, false},
		{"a snapshot shell of another process", []proc.Info{claude, {PID: 20, PPID: 1, Argv: []string{"bash", "-c", snap}}}, false},
		{"nothing running", []proc.Info{claude}, false},
		{"a snapshot named by a non-shell", []proc.Info{claude, {PID: 11, PPID: 10, Argv: []string{"node", "-c", snap}}}, false},
	} {
		if got := hasBackgroundCommand(tc.procs, 10); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}
