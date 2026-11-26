package daemon

import (
	"strings"
	"testing"
)

// This host (3 cores, 16 GB RAM, 32 GB swap, no measurements yet) works out to the numbers PR #166 fixed by hand:
// 8 workers working, 16 agents.
func TestAutoSizesFollowTheHost(t *testing.T) {
	in := hostInputs{Cores: 3, MemTotalMB: 16000, SwapTotalMB: 32000, FloorMB: 2600}
	w := autoWorking(in)
	if w != 8 {
		t.Errorf("working = %d, want 8", w)
	}
	if a := autoAgents(in, w); a != 16 {
		t.Errorf("agents = %d, want 16", a)
	}
	// More cores, more workers; the agents follow (two to a worker), up to memory.
	big := hostInputs{Cores: 16, MemTotalMB: 64000, SwapTotalMB: 8000, FloorMB: 8000}
	if w := autoWorking(big); w != 42 || autoAgents(big, w) != 64 {
		t.Errorf("16 cores: working %d agents %d, want 42 and 64 (the ceiling)", autoWorking(big), autoAgents(big, autoWorking(big)))
	}
	// Memory limits first on a small host: 4 GB, no swap, a 2 GB floor holds 5 agents of 400 MB.
	small := hostInputs{Cores: 8, MemTotalMB: 4096, FloorMB: 2048}
	if a := autoAgents(small, autoWorking(small)); a != 5 {
		t.Errorf("small host agents = %d, want 5", a)
	}
	// And never below the least a root, a worker and a reviewer need.
	tiny := hostInputs{Cores: 1, MemTotalMB: 2048, FloorMB: 2048}
	if w, a := autoWorking(tiny), autoAgents(tiny, autoWorking(tiny)); w != 2 || a != 4 {
		t.Errorf("tiny host: working %d agents %d, want 2 and 4", w, a)
	}
}

// Measured numbers replace the defaults only once enough passes saw a working agent; memory measured above the
// default size counts, below it doesn't.
func TestMeasuredUseReplacesTheDefaultsOnceThereAreSamples(t *testing.T) {
	base := hostInputs{Cores: 4, MemTotalMB: 16000, SwapTotalMB: 16000, FloorMB: 2600}
	if w := autoWorking(base); w != 10 { // 4 × 2 / 0.75
		t.Errorf("default: %d", w)
	}
	hungry := base
	hungry.CPUPerWorking, hungry.CPUSamples = 2.0, 2
	if w := autoWorking(hungry); w != 10 {
		t.Errorf("two samples are not enough: %d", w)
	}
	hungry.CPUSamples = 3
	if w := autoWorking(hungry); w != 4 { // 4 × 2 / 2.0
		t.Errorf("measured 2 cores each: %d", w)
	}
	hungry.CPUPerWorking = 100 // a runaway test run doesn't zero the limit
	if w := autoWorking(hungry); w != 2 {
		t.Errorf("clamped: %d", w)
	}
	big := base
	big.MBPerAgent = 900
	if big.agentMB() != 900 {
		t.Errorf("agentMB = %d", big.agentMB())
	}
	big.MBPerAgent = 120
	if big.agentMB() != AgentMB {
		t.Errorf("a light agent still holds the default: %d", big.agentMB())
	}
}

func TestTheCapacityLineSaysWhatFitsAndHowItWasSized(t *testing.T) {
	c := capacity{Agents: 16, Working: 8, AgentsAuto: true, WorkingAuto: true, Live: 9, WorkingNow: 6, MoreFit: 7, AgentMB: 420, Cores: 3}
	got := c.Line()
	for _, want := range []string{"7 more agents fit now", "9 of 16 agents live, caps.agents auto", "6 of 8 workers working, caps.working auto", "3 cores, 420 MB"} {
		if !strings.Contains(got, want) {
			t.Errorf("line %q lacks %q", got, want)
		}
	}
	set := capacity{Agents: 4, Working: 0, Live: 3, WorkingNow: 2, MoreFit: 1}
	got = set.Line()
	if !strings.Contains(got, "1 more agent fits now") || !strings.Contains(got, "3 of 4 agents live, caps.agents set") ||
		!strings.Contains(got, "2 workers working") || strings.Contains(got, "auto:") {
		t.Errorf("line %q", got)
	}
}
