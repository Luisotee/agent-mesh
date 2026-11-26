package daemon

// The host's limits on what it launches (design §17): a cap on live agent processes, and a memory floor. Both
// refuse with ResourceExhausted, never queue: the caller (the root, a person) decides what to retire or wait for.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

const (
	// AgentMB is what one idle agent with its MCP servers holds (22 sessions held 8.4 GB on 2026-10-27).
	AgentMB = 400
	// ReserveFor is how long a launched agent counts as AgentMB used: about how long it takes to reach its idle
	// size, until when MemAvailable doesn't show it yet, so four launches in a row don't read the same free memory.
	ReserveFor = 120 * time.Second
	// minFreeHeadroomMB and minFreeLeastMB shape `min_free_mb: auto`: 10% of RAM plus 10% of swap, plus room for
	// the agent itself and a build in it, and at least this.
	minFreeHeadroomMB = 1024
	minFreeLeastMB    = 2048
)

// guard is the memory reservations and the lock that keeps a check and its reservation together.
type guard struct {
	mu sync.Mutex
	// reserved is when each agent being launched was reserved for, by agent id: one agent holds one reservation
	// however often its launch is tried, and a launch that fails releases it.
	reserved map[string]time.Time
}

// release gives back an agent's reservation (its launch failed after the guard passed).
func (g *guard) release(agentID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.reserved, agentID)
}

// refresh restarts an existing reservation's clock at the actual launch (reopen may wait after the resume
// requested it). Swaps that took no reservation (approve, switch) stay unreserved.
func (g *guard) refresh(agentID string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.reserved[agentID]; ok {
		g.reserved[agentID] = now
	}
}

// seed restores a reservation for a live terminal adopted at boot if it is still within ReserveFor.
func (g *guard) seed(agentID string, startedAt, now time.Time) {
	if startedAt.IsZero() || now.Sub(startedAt) >= ReserveFor {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.reserved == nil {
		g.reserved = map[string]time.Time{}
	}
	if _, ok := g.reserved[agentID]; !ok {
		g.reserved[agentID] = startedAt
	}
}

func exhausted(format string, args ...any) error {
	return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf(format, args...))
}

// hasProcess reports whether an agent counts against the host's cap: its process runs, is starting, or a reopen
// of a stopped agent was asked for and hasn't launched yet.
func hasProcess(a *v1.Agent) bool {
	return !stopped(a) || a.PendingReopen != nil
}

// liveAgents counts the agent processes of every run, except the agent named (the one being resumed: it adds one
// process, however many reopens it has pending).
func liveAgents(r store.Reader, except string) (int, error) {
	agents, err := r.ListAgents("")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range agents {
		if a.Id != except && hasProcess(a) {
			n++
		}
	}
	return n, nil
}

// capGuard refuses a launch that would add an agent process past the cap (`caps.agents`; agents still starting
// count, so a burst of spawns can't all pass).
func (d *Daemon) capGuard(r store.Reader, what, except string) error {
	capV, err := d.Setting(r, "caps.agents")
	if err != nil {
		return err
	}
	limit := int(capV.(float64))
	if limit <= 0 {
		return nil
	}
	n, err := liveAgents(r, except)
	if err != nil {
		return err
	}
	if n >= limit {
		return exhausted("not starting %s: %d agents run on this host, at the limit (caps.agents %d). Retire agents "+
			"that have reported, or raise the cap (`meshctl set --key caps.agents --value <n>`; 0 is no cap)",
			what, n, limit)
	}
	return nil
}

// mayLaunch is capGuard and the memory guard, for a launch that adds a process: a spawn, a resume. Called inside
// the write that stores the new agent or its reopen, where the store's one writer makes the count and the
// record one step: two launches can't both read the cap with room left.
func (d *Daemon) mayLaunch(r store.Reader, what, agentID string) error {
	if err := d.capGuard(r, what, agentID); err != nil {
		return err
	}
	return d.memoryGuard(r, what, agentID)
}

// memInfo reads /proc/meminfo (or the configured file) in kB by field; nil where it can't be read.
func (d *Daemon) memInfo() map[string]int64 {
	path := d.rt.cfg.MeminfoPath
	if path == "" {
		path = "/proc/meminfo"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	info := map[string]int64{}
	for _, line := range strings.Split(string(b), "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if f := strings.Fields(rest); len(f) > 0 {
			if n, err := strconv.ParseInt(f[0], 10, 64); err == nil {
				info[strings.TrimSpace(key)] = n
			}
		}
	}
	return info
}

// memoryPressure is the share of the last minute in which every runnable task stalled on memory (`full avg60`
// of /proc/pressure/memory), in percent; ok is false without the file (no PSI: no signal).
func (d *Daemon) memoryPressure() (float64, bool) {
	path := d.rt.cfg.PressurePath
	if path == "" {
		path = "/proc/pressure/memory"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != "full" {
			continue
		}
		for _, kv := range f[1:] {
			if v, ok := strings.CutPrefix(kv, "avg60="); ok {
				n, err := strconv.ParseFloat(v, 64)
				return n, err == nil
			}
		}
	}
	return 0, false
}

// memoryGuard refuses a launch while memory plus swap is under the floor (`min_free_mb`: MB, "auto", or 0 for
// off) or while the memory pressure is over `memory_pressure_max`: with a large swap, memory plus swap stays
// over the floor long after the machine has started thrashing. A launch that passes reserves AgentMB for
// ReserveFor, under its agent's id.
func (d *Daemon) memoryGuard(r store.Reader, what, agentID string) error {
	return d.checkMemory(r, what, agentID, true)
}

// checkMemoryGuard checks the memory floor and pressure without taking a reservation (used early in SpawnAgent
// before creating a worktree).
func (d *Daemon) checkMemoryGuard(r store.Reader, what string) error {
	return d.checkMemory(r, what, "", false)
}

func (d *Daemon) checkMemory(r store.Reader, what, agentID string, reserve bool) error {
	pmax, err := d.Setting(r, "memory_pressure_max")
	if err != nil {
		return err
	}
	if limit := pmax.(float64); limit > 0 {
		if p, ok := d.memoryPressure(); ok && p > limit {
			return exhausted("not starting %s: memory pressure is high (/proc/pressure/memory full avg60 %.1f%%, over "+
				"memory_pressure_max %.0f): the machine is thrashing. It clears by itself as the pressure drops (avg60 "+
				"decays over a minute or two), so try again later; retiring agents that have reported helps", what, p, limit)
		}
	}
	floorV, err := d.Setting(r, "min_free_mb")
	if err != nil {
		return err
	}
	info := d.memInfo()
	if _, ok := info["MemAvailable"]; !ok {
		return nil // no /proc: nothing to tell by
	}
	floor := int64(minFreeLeastMB)
	switch v := floorV.(type) {
	case float64:
		floor = int64(v)
	default: // "auto"
		total := info["MemTotal"] + info["SwapTotal"]
		floor = max(minFreeLeastMB, total/10/1024+minFreeHeadroomMB)
	}
	if floor == 0 {
		return nil
	}
	g := &d.rt.guard
	g.mu.Lock()
	defer g.mu.Unlock()
	now := d.now()
	if g.reserved == nil {
		g.reserved = map[string]time.Time{}
	}
	held := 0
	for id, t := range g.reserved {
		switch {
		case now.Sub(t) >= ReserveFor:
			delete(g.reserved, id)
		case !reserve || id != agentID:
			held++
		}
	}
	free := (info["MemAvailable"]+info["SwapFree"])/1024 - int64(held)*AgentMB
	if free < floor {
		starting := ""
		if held > 0 {
			starting = fmt.Sprintf(", %d agent(s) still starting", held)
		}
		return exhausted("not starting %s: %d MB of memory and swap left%s, under the %d MB floor (min_free_mb). Retire "+
			"agents that have reported and stop leftover dev servers, then try again, or lower `min_free_mb` (0 turns the "+
			"guard off)", what, max(free, 0), starting, floor)
	}
	if reserve {
		g.reserved[agentID] = now
	}
	return nil
}
