package daemon

// Resource confinement for launched non-root agents (design §17): pin each agent and its descendants to the
// least-held CPU window (`worker_cpus`) and, when `worker_memory_high` > 0, place them in a transient systemd
// user scope with MemoryHigh. Never fails a launch: confinement errors are logged and ignored.

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/proc"
)

const (
	pinPasses    = 3
	scopeTimeout = 10 * time.Second
)

// SystemOps abstracts OS process table, CPU affinity, signalling, and systemd scope operations so tests can
// substitute fakes and never invoke real busctl/systemd or kill unrelated host processes.
type SystemOps struct {
	ProcTable   func() ([]proc.Info, error)
	TaskIDs     func(pid int) []int
	MachineCPUs func() []int
	GetAffinity func(pid int) ([]int, error)
	SetAffinity func(tid int, cpus []int) error
	SignalProc  func(pid int, startTicks uint64, sig syscall.Signal) error
	ProcAlive   func(pid int, startTicks uint64) bool
	RunScope    func(ctx context.Context, argv []string) error
	Sleep       func(time.Duration)
}

func resolveSystemOps(in *SystemOps) *SystemOps {
	out := &SystemOps{
		ProcTable:   proc.Table,
		TaskIDs:     proc.TaskIDs,
		MachineCPUs: proc.MachineCPUs,
		GetAffinity: proc.GetAffinity,
		SetAffinity: proc.SetAffinity,
		SignalProc:  proc.Signal,
		ProcAlive:   proc.Same,
		RunScope:    runBusctlScope,
		Sleep:       time.Sleep,
	}
	if in == nil {
		return out
	}
	if in.ProcTable != nil {
		out.ProcTable = in.ProcTable
	}
	if in.TaskIDs != nil {
		out.TaskIDs = in.TaskIDs
	}
	if in.MachineCPUs != nil {
		out.MachineCPUs = in.MachineCPUs
	}
	if in.GetAffinity != nil {
		out.GetAffinity = in.GetAffinity
	}
	if in.SetAffinity != nil {
		out.SetAffinity = in.SetAffinity
	}
	if in.SignalProc != nil {
		out.SignalProc = in.SignalProc
	}
	if in.ProcAlive != nil {
		out.ProcAlive = in.ProcAlive
	}
	if in.RunScope != nil {
		out.RunScope = in.RunScope
	}
	if in.Sleep != nil {
		out.Sleep = in.Sleep
	}
	return out
}

func runBusctlScope(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("empty scope argv")
	}
	cctx, cancel := context.WithTimeout(ctx, scopeTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

type cpuHolder struct {
	pid        int
	startTicks uint64
	cpus       []int
}

type cpuTracker struct {
	mu   sync.Mutex
	at   int
	held []cpuHolder
}

// pickWindow chooses the window of `percent`% of machineCPUs (at least 1 core) whose cores are held by the
// fewest live agents, breaking ties forward from the end of the previous pick. Returns nil when the cap is off
// (0, 100, single-core, or MachineCPUs empty).
func (c *cpuTracker) pickWindow(sys *SystemOps, percent, pid int, startTicks uint64) []int {
	if percent <= 0 || percent >= 100 {
		return nil
	}
	cpus := sys.MachineCPUs()
	n := len(cpus)
	if n == 0 {
		return nil
	}
	size := max(1, n*percent/100)
	if size >= n {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	live := make([]cpuHolder, 0, len(c.held))
	for _, h := range c.held {
		if sys.ProcAlive(h.pid, h.startTicks) {
			live = append(live, h)
		}
	}
	load := make(map[int]int, n)
	for _, cpu := range cpus {
		load[cpu] = 0
	}
	for _, h := range live {
		for _, cpu := range h.cpus {
			if _, ok := load[cpu]; ok {
				load[cpu]++
			}
		}
	}
	at := c.at % n
	bestOffset := 0
	bestLoad := -1
	bestDist := 0
	for o := 0; o < n; o++ {
		wLoad := 0
		for i := 0; i < size; i++ {
			wLoad += load[cpus[(o+i)%n]]
		}
		dist := (o - at + n) % n
		if bestLoad < 0 || wLoad < bestLoad || (wLoad == bestLoad && dist < bestDist) {
			bestOffset = o
			bestLoad = wLoad
			bestDist = dist
		}
	}
	chosen := make([]int, size)
	for i := 0; i < size; i++ {
		chosen[i] = cpus[(bestOffset+i)%n]
	}
	c.at = (bestOffset + size) % n
	if pid > 0 {
		live = append(live, cpuHolder{pid: pid, startTicks: startTicks, cpus: chosen})
	}
	c.held = live
	return chosen
}

func (c *cpuTracker) drop(pid int, startTicks uint64) {
	if pid <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.held[:0]
	for _, h := range c.held {
		if h.pid == pid && h.startTicks == startTicks {
			continue
		}
		kept = append(kept, h)
	}
	c.held = kept
}

func (c *cpuTracker) seed(sys *SystemOps, pid int, startTicks uint64, cpus []int) {
	if pid <= 0 || len(cpus) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	live := make([]cpuHolder, 0, len(c.held)+1)
	already := false
	for _, h := range c.held {
		if sys != nil && !sys.ProcAlive(h.pid, h.startTicks) {
			continue
		}
		if h.pid == pid && h.startTicks == startTicks {
			already = true
		}
		live = append(live, h)
	}
	if !already {
		live = append(live, cpuHolder{pid: pid, startTicks: startTicks, cpus: append([]int(nil), cpus...)})
	}
	c.held = live
}

// pinCPUs sets every thread of pids to cpus over up to pinPasses passes (catching threads spawned during the
// first pass). Returns how many threads took the mask.
func pinCPUs(sys *SystemOps, pids, cpus []int) int {
	done := make(map[int]bool)
	seen := make(map[int]bool)
	for pass := 0; pass < pinPasses; pass++ {
		var newTIDs []int
		for _, pid := range pids {
			for _, tid := range sys.TaskIDs(pid) {
				if !seen[tid] {
					seen[tid] = true
					newTIDs = append(newTIDs, tid)
				}
			}
		}
		if len(newTIDs) == 0 {
			break
		}
		for _, tid := range newTIDs {
			if err := sys.SetAffinity(tid, cpus); err == nil {
				done[tid] = true
			}
		}
	}
	return len(done)
}

// scopeSlug sanitizes a name to lowercase ASCII [a-z0-9-], truncated to limit, matching bin/mesh's slugify.
func scopeSlug(text string, limit int) string {
	var b strings.Builder
	dash := false
	for i := 0; i < len(text); i++ {
		ch := text[i]
		if ch >= 'A' && ch <= 'Z' {
			ch += 'a' - 'A'
		}
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') {
			b.WriteByte(ch)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > limit {
		s = strings.TrimRight(s[:limit], "-")
	}
	if s == "" {
		return "x"
	}
	return s
}

func scopeArgv(unit string, pids []int, memHighMB int64) []string {
	argv := []string{
		"busctl", "--user", "call",
		"org.freedesktop.systemd1", "/org/freedesktop/systemd1", "org.freedesktop.systemd1.Manager",
		"StartTransientUnit", "ssa(sv)a(sa(sv))",
		unit, "fail",
		"2",
		"PIDs", "au", strconv.Itoa(len(pids)),
	}
	for _, p := range pids {
		argv = append(argv, strconv.Itoa(p))
	}
	argv = append(argv, "MemoryHigh", "t", strconv.FormatUint(uint64(memHighMB)<<20, 10), "0")
	return argv
}

// confineAgent pins a newly launched non-root agent and its descendants to a CPU window (`worker_cpus`) and,
// when `worker_memory_high` > 0, moves them into a transient systemd user scope. Errors are logged, never
// returned.
func (d *Daemon) confineAgent(ctx context.Context, pid int, startTicks uint64, name string) {
	if pid <= 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	r := d.store.Read(ctx)
	cpuVal, err := d.Setting(r, "worker_cpus")
	if err != nil {
		d.log.Warn("reading worker_cpus", "agent", name, "err", err)
		return
	}
	memVal, err := d.Setting(r, "worker_memory_high")
	if err != nil {
		d.log.Warn("reading worker_memory_high", "agent", name, "err", err)
		return
	}
	cpuPct := int(cpuVal.(float64))
	memHighMB := int64(memVal.(float64))
	cpus := d.rt.cpus.pickWindow(d.rt.sys, cpuPct, pid, startTicks)
	if len(cpus) == 0 && memHighMB <= 0 {
		return // both off: skip /proc scan
	}
	tree := []int{pid}
	if table, err := d.rt.sys.ProcTable(); err == nil {
		if t := proc.Tree(pid, table); len(t) > 0 {
			tree = t
		}
	}
	if len(cpus) > 0 {
		if pinCPUs(d.rt.sys, tree, cpus) == 0 {
			d.rt.cpus.drop(pid, startTicks)
		}
	}
	if memHighMB > 0 && len(tree) > 0 {
		unit := fmt.Sprintf("mesh-%s-%d.scope", scopeSlug(name, 60), tree[0])
		if err := d.rt.sys.RunScope(ctx, scopeArgv(unit, tree, memHighMB)); err != nil {
			d.log.Warn("systemd memory scope failed; leaving agent unconfined", "agent", name, "unit", unit, "err", err)
		}
	}
}

// adoptCPUHolder records an adopted live non-root terminal's CPU affinity in the tracker if the process is
// still live (matching startTicks) and pinned to a subset of the machine's cores, so launches after a daemon
// restart avoid cores already held without double-counting or keeping dead holders.
func (d *Daemon) adoptCPUHolder(ctx context.Context, a *v1.Agent, pid int, startTicks uint64) {
	if a.Role == v1.AgentRole_AGENT_ROLE_ROOT || pid <= 0 {
		return
	}
	if !d.rt.sys.ProcAlive(pid, startTicks) {
		return
	}
	v, err := d.Setting(d.store.Read(ctx), "worker_cpus")
	if err != nil {
		return
	}
	pct := int(v.(float64))
	if pct <= 0 || pct >= 100 {
		return
	}
	machine := d.rt.sys.MachineCPUs()
	if len(machine) == 0 {
		return
	}
	held, err := d.rt.sys.GetAffinity(pid)
	if err != nil || len(held) == 0 || len(held) >= len(machine) {
		return
	}
	if !d.rt.sys.ProcAlive(pid, startTicks) {
		return
	}
	d.rt.cpus.seed(d.rt.sys, pid, startTicks, held)
}
