package daemon

// How many agents this host runs and how many workers the root keeps working, sized from the host (`caps.agents`
// and `caps.working` set to "auto", their default): memory and swap give a ceiling on live agents, the cores
// and what a working agent measured uses give the working workers. The daemon enforces the first (capGuard); the
// second is advice the root reads in `mesh brief` and `mesh spawn`.

import (
	"context"
	"fmt"
	goruntime "runtime"
	"sync"
	"time"

	"connectrpc.com/connect"

	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/proc"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

const (
	// capacityEvery is how often the agents' memory and CPU are measured.
	capacityEvery = 30 * time.Second
	// cpuPerWorkingDefault is what a working agent is taken to use (cores) until enough passes have measured it.
	cpuPerWorkingDefault = 0.75
	// cpuSamplesNeeded passes with a working agent measured before its CPU use replaces the default.
	cpuSamplesNeeded = 3
	// oversubscribe: a working agent waits on its model most of the time, so the cores may be promised twice over.
	oversubscribe = 2.0
	// agentsPerWorking: each working worker has about one more agent beside it (a reviewer, an idle one).
	agentsPerWorking = 2
	minAutoWorking   = 2
	minAutoAgents    = 4
	maxAuto          = 64
	// ewma weighs the newest pass in the running averages.
	ewma = 0.3
)

// capSampler keeps what the passes measured: the mean resident memory of an agent (with its process tree) and the
// mean CPU a WORKING agent used. Memory only: a restart measures again.
type capSampler struct {
	mu            sync.Mutex
	mbPerAgent    float64 // 0 until a pass measured an agent
	cpuPerWorking float64 // cores; 0 until a pass measured a working agent
	cpuSamples    int
	last          map[string]cpuMark // agent id → its tree's CPU at the last pass
}

type cpuMark struct {
	ticks uint64
	at    time.Time
}

// hostInputs are what the auto values are worked out from.
type hostInputs struct {
	Cores                   int
	MemTotalMB, SwapTotalMB int64
	FloorMB                 int64
	MBPerAgent              float64 // measured; 0 = not yet
	CPUPerWorking           float64 // measured, in cores; 0 = not yet
	CPUSamples              int
}

func (in hostInputs) agentMB() int64 {
	return max(AgentMB, int64(in.MBPerAgent+0.5))
}

func (in hostInputs) cpuPerWorking() float64 {
	if in.CPUSamples < cpuSamplesNeeded || in.CPUPerWorking <= 0 {
		return cpuPerWorkingDefault
	}
	return min(max(in.CPUPerWorking, 0.25), 4)
}

// autoWorking is the workers to keep working at once: the cores, promised twice over, shared by what a working
// agent uses.
func autoWorking(in hostInputs) int {
	return min(max(minAutoWorking, int(float64(max(in.Cores, 1))*oversubscribe/in.cpuPerWorking())), maxAuto)
}

// autoAgents is the live agents the host holds: what its memory (and half its swap, above the floor) holds of
// agents of the measured size, but no more than agentsPerWorking beside each working worker.
func autoAgents(in hostInputs, working int) int {
	memCap := int((in.MemTotalMB + in.SwapTotalMB/2 - in.FloorMB) / in.agentMB())
	return min(max(minAutoAgents, min(memCap, agentsPerWorking*working)), maxAuto)
}

// capacity is the host's limits and what is left of them.
type capacity struct {
	Agents, Working         int // 0 is no cap, no limit
	AgentsAuto, WorkingAuto bool
	Live, WorkingNow        int
	MoreFit                 int
	AgentMB                 int64
	Cores                   int
	Measured                bool // the sizes come from measured agents, not the defaults
}

func (c capacity) autoNote(auto bool) string {
	if !auto {
		return ""
	}
	return fmt.Sprintf(", auto: %d cores, %d MB an agent", c.Cores, c.AgentMB)
}

// hostInputs reads the host: cores, memory and swap, the guard's floor, and what the sampler measured.
func (d *Daemon) hostInputs(r store.Reader, info map[string]int64) (hostInputs, error) {
	floorV, err := d.Setting(r, "min_free_mb")
	if err != nil {
		return hostInputs{}, err
	}
	s := &d.rt.sampler
	s.mu.Lock()
	defer s.mu.Unlock()
	cores := d.rt.cfg.Cores
	if cores <= 0 {
		cores = goruntime.NumCPU()
	}
	return hostInputs{Cores: cores, MemTotalMB: info["MemTotal"] / 1024, SwapTotalMB: info["SwapTotal"] / 1024,
		FloorMB: memFloorMB(floorV, info), MBPerAgent: s.mbPerAgent, CPUPerWorking: s.cpuPerWorking, CPUSamples: s.cpuSamples}, nil
}

// capacity works out `caps.agents` and `caps.working` (a number as set, "auto" from the host), the live count
// against them and how many more agents fit now. Without /proc (no memory figures) auto sizes by the cores alone.
func (d *Daemon) capacity(r store.Reader) (capacity, error) {
	agentsV, err := d.Setting(r, "caps.agents")
	if err != nil {
		return capacity{}, err
	}
	workingV, err := d.Setting(r, "caps.working")
	if err != nil {
		return capacity{}, err
	}
	info := d.memInfo()
	in, err := d.hostInputs(r, info)
	if err != nil {
		return capacity{}, err
	}
	c := capacity{Cores: in.Cores, AgentMB: in.agentMB(), Measured: in.CPUSamples >= cpuSamplesNeeded || in.MBPerAgent > 0}
	if n, ok := workingV.(float64); ok {
		c.Working = int(n)
	} else {
		c.WorkingAuto, c.Working = true, autoWorking(in)
	}
	if n, ok := agentsV.(float64); ok {
		c.Agents = int(n)
	} else {
		c.AgentsAuto = true
		if _, ok := info["MemAvailable"]; ok {
			w := c.Working
			if w <= 0 {
				w = autoWorking(in)
			}
			c.Agents = autoAgents(in, w)
		} else {
			c.Agents = min(max(minAutoAgents, agentsPerWorking*autoWorking(in)), maxAuto)
		}
	}
	agents, err := r.ListAgents("")
	if err != nil {
		return capacity{}, err
	}
	for _, a := range agents {
		if hasProcess(a) && !isAdopted(a) {
			c.Live++
		}
		if a.Role == v1.AgentRole_AGENT_ROLE_WORKER && a.State == v1.AgentState_AGENT_STATE_WORKING {
			c.WorkingNow++
		}
	}
	c.MoreFit = 1 << 20
	if c.Agents > 0 {
		c.MoreFit = max(c.Agents-c.Live, 0)
	}
	if _, ok := info["MemAvailable"]; ok {
		free := (info["MemAvailable"]+info["SwapFree"])/1024 - in.FloorMB - int64(d.heldReservations())*AgentMB
		c.MoreFit = min(c.MoreFit, int(max(free, 0)/c.AgentMB))
	}
	return c, nil
}

// heldReservations counts the launches still reserving their memory (ReserveFor).
func (d *Daemon) heldReservations() int {
	g := &d.rt.guard
	g.mu.Lock()
	defer g.mu.Unlock()
	now := d.now()
	n := 0
	for _, t := range g.reserved {
		if now.Sub(t) < ReserveFor {
			n++
		}
	}
	return n
}

// Line is what `mesh brief` and `mesh spawn` tell the root: how many more agents fit and how many workers work.
func (c capacity) Line() string {
	fit := fmt.Sprintf("%d more agents fit now", c.MoreFit)
	switch {
	case c.MoreFit >= 1<<20:
		fit = "no cap on agents"
	case c.MoreFit == 1:
		fit = "1 more agent fits now"
	}
	agents := fmt.Sprintf("%d agents live", c.Live)
	if c.Agents > 0 {
		agents = fmt.Sprintf("%d of %d agents live, caps.agents %s", c.Live, c.Agents, autoWord(c.AgentsAuto))
	}
	working := fmt.Sprintf("%d workers working", c.WorkingNow)
	if c.Working > 0 {
		working = fmt.Sprintf("%d of %d workers working, caps.working %s", c.WorkingNow, c.Working, autoWord(c.WorkingAuto))
	}
	line := fmt.Sprintf("%s (%s); %s", fit, agents, working)
	if c.AgentsAuto || c.WorkingAuto {
		line += fmt.Sprintf(" [auto: %d cores, %d MB an agent]", c.Cores, c.AgentMB)
	}
	return line
}

func autoWord(auto bool) string {
	if auto {
		return "auto"
	}
	return "set"
}

// capacityLine is Line, or "" when the host can't be read.
func (d *Daemon) capacityLine(ctx context.Context) string {
	c, err := d.capacity(d.store.Read(ctx))
	if err != nil {
		return ""
	}
	return c.Line()
}

// sampleCapacity measures every live agent's process tree: resident memory, and for the WORKING ones the CPU used
// since the last pass. The terminal host knows each agent's pid; /proc the tree below it.
func (d *Daemon) sampleCapacity(ctx context.Context) { d.sampleCapacityAt(ctx, d.now()) }

func (d *Daemon) sampleCapacityAt(ctx context.Context, now time.Time) {
	if d.rt.term == nil {
		return
	}
	res, err := d.rt.term.API.ListTerminals(ctx, connect.NewRequest(&termv1.ListTerminalsRequest{}))
	if err != nil {
		return
	}
	pids := map[string]int{}
	for _, t := range res.Msg.Terminals {
		if t.Pid > 0 {
			pids[t.Id] = int(t.Pid)
		}
	}
	agents, err := d.store.Read(ctx).ListAgents("")
	if err != nil {
		return
	}
	table, err := d.rt.sys.ProcTable()
	if err != nil {
		return
	}
	// /proc is read without the sampler's lock: capGuard reads the measurements inside the store's write
	// transaction, and a spawn must not wait there for a pass that is still reading every agent's tree.
	type reading struct {
		id      string
		rssKB   uint64
		ticks   uint64
		working bool
	}
	var got []reading
	for _, a := range agents {
		pid, ok := pids[a.TerminalId]
		if !ok || stopped(a) {
			continue
		}
		var rssKB, ticks uint64
		for _, p := range proc.Tree(pid, table) {
			rss, cpu, err := d.rt.sys.ProcUsage(p)
			if err != nil {
				continue // exited since the table was read
			}
			rssKB += rss
			ticks += cpu
		}
		if rssKB == 0 {
			continue
		}
		got = append(got, reading{a.Id, rssKB, ticks, a.State == v1.AgentState_AGENT_STATE_WORKING})
	}
	var mbSum, cpuSum float64
	var measured, cpuN int
	seen := map[string]bool{}
	s := &d.rt.sampler
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		s.last = map[string]cpuMark{}
	}
	for _, g := range got {
		seen[g.id] = true
		mbSum += float64(g.rssKB) / 1024
		measured++
		prev, had := s.last[g.id]
		s.last[g.id] = cpuMark{g.ticks, now}
		if dt := now.Sub(prev.at).Seconds(); had && dt >= 5 && g.ticks >= prev.ticks && g.working {
			cpuSum += float64(g.ticks-prev.ticks) / proc.ClockTicks / dt
			cpuN++
		}
	}
	for id := range s.last {
		if !seen[id] {
			delete(s.last, id)
		}
	}
	if measured > 0 {
		mean := mbSum / float64(measured)
		if s.mbPerAgent == 0 {
			s.mbPerAgent = mean
		} else {
			s.mbPerAgent += ewma * (mean - s.mbPerAgent)
		}
	}
	if cpuN > 0 {
		mean := cpuSum / float64(cpuN)
		if s.cpuSamples == 0 {
			s.cpuPerWorking = mean
		} else {
			s.cpuPerWorking += ewma * (mean - s.cpuPerWorking)
		}
		s.cpuSamples++
	}
}
