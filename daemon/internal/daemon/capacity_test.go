package daemon_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
)

// With caps.agents at its default ("auto") the host sets the limit: one core and plenty of memory give four
// agents, the fifth is refused saying how the number was sized, and every spawn answers how many more fit.
func TestAutoAgentCapIsTheHostsAndSpawnSaysWhatFits(t *testing.T) {
	dir := t.TempDir()
	mem := writeProc(t, dir, "meminfo", meminfoText(12000, 0, 16000, 0))
	psi := writeProc(t, dir, "pressure", "some avg10=0.00 avg60=0.00 avg300=0.00 total=0\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")
	w := startReopen(t, func(c *daemon.AgentConfig) { c.MeminfoPath, c.PressurePath, c.Cores = mem, psi, 1 })
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	repo := gitRepo(t)
	var last *v1.SpawnAgentResponse
	for _, slug := range []string{"one", "two", "three", "four"} {
		res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Meta: meta("sp-" + slug), RunId: run.Id,
			Repo: repo, Slug: slug, Tier: "mid"}))
		if err != nil {
			t.Fatalf("%s: %v", slug, err)
		}
		last = res.Msg
	}
	if !strings.Contains(last.Capacity, "0 more agents fit now") || !strings.Contains(last.Capacity, "4 of 4 agents live, caps.agents auto") ||
		!strings.Contains(last.Capacity, "1 cores") {
		t.Errorf("capacity after the fourth spawn: %q", last.Capacity)
	}
	_, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Meta: meta("sp-five"), RunId: run.Id, Repo: repo,
		Slug: "five", Tier: "mid"}))
	if code(err) != connect.CodeResourceExhausted || !strings.Contains(err.Error(), "caps.agents 4, auto: 1 cores") {
		t.Fatalf("a fifth agent: %v", err)
	}
	// A number the user sets wins, and 0 lifts the cap.
	w.set(t, "caps.agents", 5)
	if _, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{Meta: meta("sp-five-b"), RunId: run.Id, Repo: repo,
		Slug: "five", Tier: "mid"})); err != nil {
		t.Fatalf("with caps.agents 5: %v", err)
	}
	if line := w.d.CapacityLine(ctx); !strings.Contains(line, "5 of 5 agents live, caps.agents set") {
		t.Errorf("line %q", line)
	}
}

// A pass measures each agent's process tree: the memory it holds sizes the agent in the auto numbers, and a few
// passes over a WORKING agent show how much CPU one uses, which sets how many workers the cores carry.
func TestSamplingMeasuresMemoryAndCPUAndTheLineFollows(t *testing.T) {
	dir := t.TempDir()
	mem := writeProc(t, dir, "meminfo", meminfoText(12000, 0, 16000, 0))
	var ticks atomic.Uint64
	fs := newFakeSys()
	ops := fs.ops()
	ops.ProcUsage = func(pid int) (uint64, uint64, error) { return 600 * 1024, ticks.Add(200), nil }
	w := startReopen(t, func(c *daemon.AgentConfig) { c.MeminfoPath, c.Cores, c.Sys = mem, 4, ops })
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	a := w.spawn(run.Id, gitRepo(t), "worker", "")
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	w.putAgent(t, a.Id, func(ag *v1.Agent) { ag.State = v1.AgentState_AGENT_STATE_WORKING })
	if line := w.d.CapacityLine(ctx); !strings.Contains(line, "1 of 10 workers working") {
		t.Errorf("before any pass the default 0.75 cores a worker gives 10: %q", line)
	}
	now := time.Now()
	for i := 0; i < 4; i++ { // the first pass only marks the CPU; each later one adds a sample of 200 ticks in 10 s = 0.2 cores
		w.d.SampleCapacityAt(ctx, now.Add(time.Duration(i)*10*time.Second))
	}
	// 0.2 cores is under the floor of 0.25: 4 cores × 2 / 0.25 = 32 workers; 600 MB an agent.
	line := w.d.CapacityLine(ctx)
	if !strings.Contains(line, "of 32 workers working, caps.working auto") || !strings.Contains(line, "600 MB an agent") {
		t.Errorf("after the passes: %q", line)
	}
	// A number set by hand ignores the measurements.
	w.set(t, "caps.working", 3)
	if line := w.d.CapacityLine(ctx); !strings.Contains(line, "of 3 workers working, caps.working set") {
		t.Errorf("set: %q", line)
	}
}

// A pass reads every agent's /proc entries without the sampler's lock: the cap check inside a spawn's write
// transaction reads the measurements through it, and must not wait for a pass that is still reading.
func TestACapacityPassDoesNotBlockTheCapCheckWhileItReadsProc(t *testing.T) {
	dir := t.TempDir()
	mem := writeProc(t, dir, "meminfo", meminfoText(12000, 0, 16000, 0))
	var reading, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	fs := newFakeSys()
	ops := fs.ops()
	ops.ProcUsage = func(pid int) (uint64, uint64, error) {
		once.Do(func() { close(reading) })
		<-release
		return 600 * 1024, 1, nil
	}
	w := startReopen(t, func(c *daemon.AgentConfig) { c.MeminfoPath, c.Cores, c.Sys = mem, 4, ops })
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	a := w.spawn(run.Id, gitRepo(t), "worker", "")
	w.waitState(a.Id, v1.AgentState_AGENT_STATE_IDLE)
	passed := make(chan struct{})
	go func() { w.d.SampleCapacityAt(ctx, time.Now()); close(passed) }()
	select {
	case <-reading:
	case <-time.After(10 * time.Second):
		t.Fatal("the pass never read a process")
	}
	got := make(chan string, 1)
	go func() { got <- w.d.CapacityLine(ctx) }()
	select {
	case line := <-got:
		if line == "" {
			t.Errorf("no capacity line")
		}
	case <-time.After(5 * time.Second):
		t.Error("the capacity line waits for a pass that is reading /proc")
	}
	close(release)
	<-passed
}
