package daemon

import (
	"context"
	"strings"
	"sync"
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
// alert) for backgroundGrace from its last activity (at most `accountability.wait_max`, as a declared wait is);
// after that a command still running is one the agent forgot (a dev server, a watcher), and it is nudged once to
// declare a wait or stop it, as an agent with nothing running would be.

// backgroundGrace is how long a background command is let run, from the agent's last activity, before the agent
// is nudged about it. A variable for tests.
var backgroundGrace = 30 * time.Minute

// backgroundNudge is the nudge of an agent that ended a turn with a background command still running, past the
// grace.
func backgroundNudge(name string) string {
	return nudgeTag + " Your turn ended without a report to your parent, and a background command you started still runs. " +
		"If you are waiting on it, declare that now: `mesh send parent --type wait --body \"<what>\"` (a wait ends when something wakes you: " +
		"if you end that turn still waiting, declare a fresh wait first). If you are not, stop it. If the task is finished, run `mesh inbox`, " +
		"then report it: `mesh send parent --type done --body @$(mesh dir)/reports/" + name + ".md`."
}

// hasBackgroundCommand reports whether the process tree under pid holds a Claude background command.
func hasBackgroundCommand(table []proc.Info, pid int) bool {
	_, ok := backgroundCommand(table, pid)
	return ok
}

// backgroundCommand is the command line of a Claude background command in the process tree under pid, as the user
// would read it (`make test`), and whether there is one. A command whose line can't be told apart from the snapshot
// plumbing comes back empty with true.
func backgroundCommand(table []proc.Info, pid int) (string, bool) {
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
				return evalLine(p.Argv[2]), true
			}
		}
	}
	return "", false
}

// evalLine is what a snapshot shell evaluates: the quoted text after its `eval`, with the quotes taken off.
func evalLine(script string) string {
	i := strings.Index(script, "eval ")
	if i < 0 {
		return ""
	}
	cmd := strings.TrimSpace(script[i+len("eval "):])
	if len(cmd) >= 2 && (cmd[0] == '\'' || cmd[0] == '"') {
		cmd = unquoteFirst(cmd)
	}
	return strings.TrimSpace(cmd)
}

// unquoteFirst is the shell word `s` opens with, unquoted: single-quoted parts as they are, double-quoted ones with a
// backslash escaping a quote, a backslash, a dollar, a backtick or a newline, and a backslash outside quotes escaping the next character, joined until a
// space ends the word (what follows, the `< /dev/null` and the cwd plumbing, is left out). Claude Code writes a quote
// inside a command as `'"'"'` (close, a double-quoted quote, reopen), a shell as `'\”`: both read as a quote. An unclosed
// quote takes the rest.
func unquoteFirst(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				b.WriteString(s[i+1:])
				return b.String()
			}
			b.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case '"':
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`\n", s[i+1]) >= 0 {
					i++
				}
				b.WriteByte(s[i])
			}
		case '\\':
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
		case ' ', '\t', '\n':
			return b.String()
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// backgroundWork reports whether a Claude agent has a background command running.
func (d *Daemon) backgroundWork(ctx context.Context, a *v1.Agent) bool {
	_, ok := d.backgroundCommandOf(ctx, a)
	return ok
}

// backgroundCommandOf is the command line of the background command a Claude agent has running, and whether it has one.
// It reads the terminals and the process table now: the accountability pass asks it.
func (d *Daemon) backgroundCommandOf(ctx context.Context, a *v1.Agent) (string, bool) {
	if a.Kind != claude.Kind || a.TerminalId == "" || d.rt.term == nil {
		return "", false
	}
	snap, ok := d.readProcs(ctx)
	if !ok {
		return "", false
	}
	return snap.backgroundCommand(a)
}

// shownBackgroundCommand is backgroundCommandOf for what an app shows (GetAgentProgress): the apps poll it for every
// open chat, so it reuses a read of the terminals and the process table up to waitReadTTL old.
func (d *Daemon) shownBackgroundCommand(ctx context.Context, a *v1.Agent) (string, bool) {
	if a.Kind != claude.Kind || a.TerminalId == "" || d.rt.term == nil {
		return "", false
	}
	snap, ok := d.recentProcs(ctx)
	if !ok {
		return "", false
	}
	return snap.backgroundCommand(a)
}

// waitReadTTL is how old a read of the terminals and the process table may be when an app asks what an agent waits on.
const waitReadTTL = 5 * time.Second

// procSnapshot is what the terminal host and the process table said at one moment: the pid of each live terminal.
type procSnapshot struct {
	pids  map[string]int
	table []proc.Info
}

func (s procSnapshot) backgroundCommand(a *v1.Agent) (string, bool) {
	pid := s.pids[a.TerminalId]
	if pid <= 0 {
		return "", false
	}
	return backgroundCommand(s.table, pid)
}

// readProcs reads the live terminals' pids and the process table now.
func (d *Daemon) readProcs(ctx context.Context) (procSnapshot, bool) {
	res, err := d.rt.term.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
	if err != nil {
		return procSnapshot{}, false
	}
	pids := map[string]int{}
	for _, t := range res.Msg.Terminals {
		if !t.Exited {
			pids[t.Id] = int(t.Pid)
		}
	}
	table, err := d.rt.sys.ProcTable()
	if err != nil {
		return procSnapshot{}, false
	}
	return procSnapshot{pids: pids, table: table}, true
}

// procReads is the read recentProcs keeps: one for the whole daemon, shared by every agent asked about.
type procReads struct {
	mu   sync.Mutex
	at   time.Time
	snap procSnapshot
}

// recentProcs is readProcs, or the last read when it is less than waitReadTTL old. Callers at once make one read.
func (d *Daemon) recentProcs(ctx context.Context) (procSnapshot, bool) {
	d.procs.mu.Lock()
	defer d.procs.mu.Unlock()
	now := d.now()
	if age := now.Sub(d.procs.at); !d.procs.at.IsZero() && age >= 0 && age < waitReadTTL {
		return d.procs.snap, true
	}
	snap, ok := d.readProcs(ctx)
	if !ok {
		return procSnapshot{}, false
	}
	d.procs.at, d.procs.snap = now, snap
	return snap, true
}

// withinBackgroundGrace reports whether a running background command is still let run: the agent's last activity
// is within the grace (and within waitMax). With no activity to measure from it is not.
func (d *Daemon) withinBackgroundGrace(a *v1.Agent, waitMax time.Duration) bool {
	if a.LastActivity == nil {
		return false
	}
	return d.now().Sub(a.LastActivity.AsTime()) < min(backgroundGrace, waitMax)
}
