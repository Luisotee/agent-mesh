package daemon

import (
	"context"
	"strings"
	"time"

	"connectrpc.com/connect"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
	"github.com/Luisotee/agent-mesh/daemon/internal/proc"
)

// A Claude agent that ends its turn while a background command runs is waiting on it, declared or not: Claude
// Code starts each command as `bash -c 'source ~/.claude/shell-snapshots/… && eval …'`, a child of its own process
// (seen on a live agent with `ps --forest`), and nothing else it runs holds such a shell at a prompt (its MCP
// servers are `node` children). The accountability pass leaves such an agent alone (no nudge, no `silent`
// alert), for as long as a declared wait would hold it: `accountability.wait_max` from its last activity.

// hasBackgroundCommand reports whether the process tree under pid holds a Claude background command.
func hasBackgroundCommand(table []proc.Info, pid int) bool {
	byPID := map[int]proc.Info{}
	for _, p := range table {
		byPID[p.PID] = p
	}
	for _, id := range proc.Tree(pid, table) {
		p := byPID[id]
		if id == pid || len(p.Argv) < 3 {
			continue
		}
		switch base := p.Argv[0][strings.LastIndex(p.Argv[0], "/")+1:]; base {
		case "bash", "sh", "zsh", "dash":
			if p.Argv[1] == "-c" && strings.Contains(p.Argv[2], "/shell-snapshots/") {
				return true
			}
		}
	}
	return false
}

// backgroundWork reports whether a Claude agent has a background command running.
func (d *Daemon) backgroundWork(ctx context.Context, a *v1.Agent) bool {
	if a.Kind != claude.Kind || a.TerminalId == "" || d.rt.term == nil {
		return false
	}
	res, err := d.rt.term.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
	if err != nil {
		return false
	}
	pid := 0
	for _, t := range res.Msg.Terminals {
		if t.Id == a.TerminalId && !t.Exited {
			pid = int(t.Pid)
		}
	}
	if pid <= 0 {
		return false
	}
	table, err := d.rt.sys.ProcTable()
	return err == nil && hasBackgroundCommand(table, pid)
}

// waitsOnBackground is backgroundWork for an agent whose last activity is within waitMax.
func (d *Daemon) waitsOnBackground(ctx context.Context, a *v1.Agent, waitMax time.Duration) bool {
	if a.LastActivity == nil || d.now().Sub(a.LastActivity.AsTime()) >= waitMax {
		return false // no activity to measure the wait from, or it is over
	}
	return d.backgroundWork(ctx, a)
}
