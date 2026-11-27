package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1/termv1connect"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
	"github.com/Luisotee/agent-mesh/daemon/internal/proc"
	"github.com/Luisotee/agent-mesh/daemon/internal/termclient"
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

// listTerm answers ListTerminals with one live terminal and counts the calls.
type listTerm struct {
	termv1connect.TermHostServiceClient
	calls atomic.Int32
}

func (l *listTerm) ListTerminals(context.Context, *connect.Request[termv1.ListTerminalsRequest]) (*connect.Response[termv1.ListTerminalsResponse], error) {
	l.calls.Add(1)
	return connect.NewResponse(&termv1.ListTerminalsResponse{Terminals: []*termv1.Terminal{{Id: "t1", Pid: 10}, {Id: "t2", Pid: 20, Exited: true}}}), nil
}

// What an app shows an agent waits on reads the terminals and the process table once for every agent and call within
// waitReadTTL; the accountability pass reads them each time it asks.
func TestTheWaitAnAppShowsSharesOneReadOfTheProcessTable(t *testing.T) {
	const snap = "source /home/u/.claude/shell-snapshots/snapshot-bash-1.sh 2>/dev/null || true && eval 'make test'"
	table := []proc.Info{{PID: 10, PPID: 1, Argv: []string{"claude"}}, {PID: 11, PPID: 10, Argv: []string{"bash", "-c", snap}}}
	var reads atomic.Int32
	term := &listTerm{}
	now := time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC)
	d := &Daemon{now: func() time.Time { return now }, rt: &runtime{term: &termclient.Client{API: term},
		sys: &SystemOps{ProcTable: func() ([]proc.Info, error) { reads.Add(1); return table, nil }}}}
	a := &v1.Agent{Id: "a1", Kind: claude.Kind, TerminalId: "t1"}
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if cmd, ok := d.shownBackgroundCommand(ctx, a); !ok || cmd != "make test" {
			t.Fatalf("shown %d: %q, %v", i, cmd, ok)
		}
	}
	if reads.Load() != 1 || term.calls.Load() != 1 {
		t.Errorf("five shown waits read the process table %d times and the terminals %d times, want once each", reads.Load(), term.calls.Load())
	}
	// Past the ttl it reads again, and a command that ended is no longer shown.
	now = now.Add(waitReadTTL)
	table = table[:1]
	if cmd, ok := d.shownBackgroundCommand(ctx, a); ok {
		t.Errorf("a command that ended is still shown after the ttl: %q", cmd)
	}
	if reads.Load() != 2 {
		t.Errorf("%d reads after the ttl, want 2", reads.Load())
	}
	// The accountability pass is never served an old read.
	table = []proc.Info{{PID: 10, PPID: 1, Argv: []string{"claude"}}, {PID: 11, PPID: 10, Argv: []string{"bash", "-c", snap}}}
	if _, ok := d.backgroundCommandOf(ctx, a); !ok || reads.Load() != 3 {
		t.Errorf("backgroundCommandOf: found %v after %d reads, want a fresh read that finds it", ok, reads.Load())
	}
	// An exited terminal has no command, and another agent's terminal id is not this one's.
	if _, ok := d.shownBackgroundCommand(ctx, &v1.Agent{Id: "a2", Kind: claude.Kind, TerminalId: "t2"}); ok {
		t.Errorf("an exited terminal has a background command")
	}
}

// A terminal host that does not answer is asked once per waitReadTTL, not by every caller that waits for it.
func TestAFailedReadOfTheProcessTableIsKeptForTheTTLToo(t *testing.T) {
	var reads atomic.Int32
	now := time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC)
	d := &Daemon{now: func() time.Time { return now }, rt: &runtime{term: &termclient.Client{API: &listTerm{}},
		sys: &SystemOps{ProcTable: func() ([]proc.Info, error) { reads.Add(1); return nil, errors.New("no /proc") }}}}
	a := &v1.Agent{Id: "a1", Kind: claude.Kind, TerminalId: "t1"}
	for i := 0; i < 4; i++ {
		if _, ok := d.shownBackgroundCommand(context.Background(), a); ok {
			t.Fatal("a command from a failed read")
		}
	}
	if reads.Load() != 1 {
		t.Errorf("%d reads for four calls within the ttl, want 1", reads.Load())
	}
	now = now.Add(waitReadTTL)
	d.shownBackgroundCommand(context.Background(), a)
	if reads.Load() != 2 {
		t.Errorf("%d reads after the ttl, want 2", reads.Load())
	}
}
