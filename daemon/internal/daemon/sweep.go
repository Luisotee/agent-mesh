package daemon

// Worktree process sweep (design §17): on retire and run stop, find leftover process trees in a worker's
// worktree that hold no terminal and no agent CLI and hang off init/systemd, send SIGTERM, wait briefly, then
// send SIGKILL to any survivor. Never touches main checkouts, root worktrees, or user shells/tmux panes.

import (
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/proc"
)

const (
	sweepAgentTries = 20
	sweepAgentPoll  = 500 * time.Millisecond
	sweepKillTries  = 10
	sweepKillPoll   = 300 * time.Millisecond
)

var (
	agentBinaries = map[string]bool{
		"claude":       true,
		"agy":          true,
		"copilot":      true,
		"opencode":     true,
		"opencode.exe": true,
	}
	interpreters = map[string]bool{
		"node": true,
		"bun":  true,
		"deno": true,
	}
	initNames = map[string]bool{
		"init":    true,
		"systemd": true,
	}
)

func isAgentBinary(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	first := filepath.Base(argv[0])
	if interpreters[first] && len(argv) > 1 {
		first = filepath.Base(argv[1])
	}
	return agentBinaries[first]
}

func resolvePath(p string) string {
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(real)
	}
	// If the leaf doesn't exist yet (or was removed), resolve its longest existing ancestor.
	cur := abs
	var tail []string
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				real = filepath.Join(real, tail[i])
			}
			return filepath.Clean(real)
		}
	}
}

func pathInside(path, top string) bool {
	if path == "" || top == "" {
		return false
	}
	p := resolvePath(path)
	t := resolvePath(top)
	if p == t {
		return true
	}
	if t == string(filepath.Separator) {
		return strings.HasPrefix(p, t)
	}
	return strings.HasPrefix(p, t+string(filepath.Separator))
}

// sweepable returns the worker worktree path safe to sweep: inside worktreesRoot at least `<run>/<slug>` deep,
// never a repo's main checkout, never a root agent, and never holding any root agent's cwd/worktree.
func sweepable(worktreesRoot string, a *v1.Agent, rootCwds []string) (string, bool) {
	if a == nil || a.Role == v1.AgentRole_AGENT_ROLE_ROOT || a.Worktree == "" {
		return "", false
	}
	top := resolvePath(a.Worktree)
	home := resolvePath(worktreesRoot)
	rel, err := filepath.Rel(home, top)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	if len(strings.Split(rel, string(filepath.Separator))) < 2 {
		return "", false
	}
	if a.Repo != "" && resolvePath(a.Repo) == top {
		return "", false
	}
	for _, rc := range rootCwds {
		if pathInside(rc, top) {
			return "", false
		}
	}
	return top, true
}

func ownAgent(p proc.Info, a *v1.Agent) bool {
	if a.SessionId == "" {
		return true
	}
	for _, arg := range p.Argv {
		if strings.Contains(arg, a.SessionId) {
			return true
		}
	}
	return false
}

// leftoverProcesses finds processes inside `top` whose chain of parents inside `top` holds no agent CLI and
// terminates at pid 1 or an init/systemd subreaper, and where no process in that subtree holds a terminal or is
// an agent CLI. Matches bin/mesh's `leftover_processes`.
func leftoverProcesses(top string, table []proc.Info) []proc.Info {
	byPID := make(map[int]proc.Info, len(table))
	children := make(map[int][]proc.Info)
	for _, p := range table {
		byPID[p.PID] = p
		children[p.PPID] = append(children[p.PPID], p)
	}

	owned := func(root proc.Info) bool {
		todo := []proc.Info{root}
		seen := make(map[int]bool)
		for len(todo) > 0 {
			node := todo[len(todo)-1]
			todo = todo[:len(todo)-1]
			if seen[node.PID] {
				continue
			}
			seen[node.PID] = true
			if node.TTY != 0 || isAgentBinary(node.Argv) {
				return true
			}
			todo = append(todo, children[node.PID]...)
		}
		return false
	}

	initNode := proc.Info{PID: 1, Argv: []string{"init"}, Cwd: "/"}
	var found []proc.Info
	for _, p := range table {
		if !pathInside(p.Cwd, top) {
			continue
		}
		cur := p
		hasNode := true
		chainTop := p
		seen := make(map[int]bool)
		broke := false
		for hasNode && pathInside(cur.Cwd, top) && !seen[cur.PID] {
			seen[cur.PID] = true
			if isAgentBinary(cur.Argv) {
				broke = true
				break
			}
			chainTop = cur
			switch {
			case cur.PPID > 1:
				next, ok := byPID[cur.PPID]
				cur, hasNode = next, ok
			case cur.PPID == 1:
				cur, hasNode = initNode, true
			default:
				hasNode = false
			}
		}
		if broke || !hasNode {
			continue
		}
		isInit := cur.PID == 1
		if !isInit && len(cur.Argv) > 0 {
			isInit = initNames[filepath.Base(cur.Argv[0])]
		}
		if isInit && !owned(chainTop) {
			found = append(found, p)
		}
	}
	return found
}

// sweepWorktrees stops leftover process trees in the given non-root agents' worktrees (TERM, brief wait, KILL).
// Never fails retire or run stop: errors are logged.
func (d *Daemon) sweepWorktrees(ctx context.Context, agents []*v1.Agent) {
	if len(agents) == 0 {
		return
	}
	r := d.store.Read(context.WithoutCancel(ctx))
	rootByRun := make(map[string][]string)
	for _, a := range agents {
		if a == nil || a.Role == v1.AgentRole_AGENT_ROLE_ROOT || a.Worktree == "" {
			continue
		}
		if _, ok := rootByRun[a.RunId]; ok {
			continue
		}
		runAgents, err := r.ListAgents(a.RunId)
		if err != nil {
			d.log.Warn("reading run agents before process sweep", "run", a.RunId, "err", err)
			return
		}
		var cwds []string
		for _, ra := range runAgents {
			if ra.Role == v1.AgentRole_AGENT_ROLE_ROOT && ra.Worktree != "" {
				cwds = append(cwds, ra.Worktree)
			}
		}
		rootByRun[a.RunId] = cwds
	}

	type target struct {
		agent *v1.Agent
		top   string
	}
	wtRoot := d.worktrees()
	var targets []target
	for _, a := range agents {
		if a == nil {
			continue
		}
		if top, ok := sweepable(wtRoot, a, rootByRun[a.RunId]); ok {
			targets = append(targets, target{agent: a, top: top})
		}
	}
	if len(targets) == 0 {
		return
	}

	sys := d.rt.sys
	agentPoll := d.rt.cfg.SweepAgentPoll
	if agentPoll == 0 {
		agentPoll = sweepAgentPoll
	}
	killPoll := d.rt.cfg.SweepKillPoll
	if killPoll == 0 {
		killPoll = sweepKillPoll
	}

	table, err := sys.ProcTable()
	if err != nil {
		d.log.Warn("reading process table for sweep", "err", err)
		return
	}
	for try := 0; try < sweepAgentTries; try++ {
		ownRunning := false
		for _, t := range targets {
			for _, p := range table {
				if isAgentBinary(p.Argv) && pathInside(p.Cwd, t.top) && ownAgent(p, t.agent) {
					ownRunning = true
					break
				}
			}
			if ownRunning {
				break
			}
		}
		if !ownRunning {
			break
		}
		sys.Sleep(agentPoll)
		if next, err := sys.ProcTable(); err == nil {
			table = next
		}
	}

	for _, t := range targets {
		procs := leftoverProcesses(t.top, table)
		if len(procs) == 0 {
			continue
		}
		for _, p := range procs {
			_ = sys.SignalProc(p.PID, p.StartTicks, syscall.SIGTERM)
		}
		for try := 0; try < sweepKillTries; try++ {
			anyAlive := false
			for _, p := range procs {
				if sys.ProcAlive(p.PID, p.StartTicks) {
					anyAlive = true
					break
				}
			}
			if !anyAlive {
				break
			}
			sys.Sleep(killPoll)
		}
		var pids, forced []int
		var cmds []string
		for _, p := range procs {
			pids = append(pids, p.PID)
			cmd := strings.Join(p.Argv, " ")
			if len(cmd) > 80 {
				cmd = cmd[:80]
			}
			cmds = append(cmds, cmd)
			if sys.ProcAlive(p.PID, p.StartTicks) {
				forced = append(forced, p.PID)
				_ = sys.SignalProc(p.PID, p.StartTicks, syscall.SIGKILL)
			}
		}
		d.log.Info("swept leftover processes", "agent", t.agent.Name, "cwd", t.top,
			"pids", pids, "commands", cmds, "forced", forced)
	}
}
