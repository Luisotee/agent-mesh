package daemon

import (
	"math"
	"testing"
	"time"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

func TestClaudeCallCostPrices(t *testing.T) {
	const m = int64(1_000_000)
	cases := []struct {
		name                     string
		model                    string
		input, w5, w1, read, out int64
		want                     float64
	}{
		{"sonnet 5.5 cache read is $0.10", "claude-sonnet-5-5", 0, 0, 0, m, 0, 0.10},
		{"sonnet 5.5 other rates", "claude-sonnet-5-5", m, m, m, 0, m, 2.0 + 2.5 + 4.0 + 10.0},
		{"older sonnet keeps $0.20 reads", "claude-sonnet-5", 0, 0, 0, m, 0, 0.20},
		{"opus reads unchanged", "claude-opus-5-5", 0, 0, 0, m, 0, 0.20},
		{"haiku 5.5 up to 100K", "claude-haiku-5-5", 40_000, 0, 0, 60_000, 0, 40_000*0.10/1e6 + 60_000*0.01/1e6},
		{"haiku 5.5 over 100K bills 5x", "claude-haiku-5-5", 0, 0, 0, 200_000, 1_000_000, 200_000*0.05/1e6 + 2.50},
		{"haiku 5.5 over 100K counts writes", "claude-haiku-5-5", 50_000, 60_000, 0, 0, 0, 50_000*0.50/1e6 + 60_000*0.625/1e6},
		{"other haiku has no long-prompt tier", "claude-haiku-4-5", 0, 0, 0, 200_000, 0, 200_000 * 0.01 / 1e6},
		{"fable 5.2 has its own rates", "claude-fable-5-2", m, m, m, m, m, 8 + 10 + 16 + 0.20 + 40},
		{"fable 5.1 keeps the table's", "claude-fable-5-1", m, m, m, m, m, 10 + 12.5 + 20 + 0.25 + 50},
		{"agy is free", "agy", m, 0, 0, m, m, 0},
	}
	for _, c := range cases {
		got := claudeCallCost(c.model, c.input, c.w5, c.w1, c.read, c.out)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// A cold resume is a Claude cache story: an OpenCode call that writes cache after a long pause is not one, and its
// writes count in the tokens (and the total) like any CLI's.
func TestTallyCountsOpenCodeCacheWritesButNeverAsACold(t *testing.T) {
	t0 := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	calls := []costCall{
		{t: t0, model: "p/m", cli: "opencode", input: 100, w5: 30_000, output: 50},
		{t: t0.Add(3 * time.Hour), model: "p/m", cli: "opencode", input: 100, w5: 30_000, output: 50},
	}
	ct := newCostTally(false)
	place := func(costCall) *placedCall { return &placedCall{kind: "worker", agent: agentKey{run: "r", name: "a"}} }
	tallySession(ct, "s", calls, false, place, time.Time{}, time.Time{}, map[string]bool{})
	b := ct.agents[agentKey{run: "r", name: "a"}]
	if b.cold != 0 {
		t.Fatalf("cold = %d, want 0 for OpenCode", b.cold)
	}
	if b.tokens() != 60_300 {
		t.Fatalf("tokens = %d, want 60300 (input + cache write + output)", b.tokens())
	}
}

func TestTokenTotalLeavesCacheReadsApart(t *testing.T) {
	if got := tokenTotal(10, 20, 30, 40); got != 100 {
		t.Fatalf("tokenTotal = %d", got)
	}
	r := buildCostReport("run x", func() *costTally {
		ct := newCostTally(true)
		ct.add("s", costCall{model: "claude-opus-5-5", input: 1, w1: 2, w5: 3, read: 1_000_000, output: 4}, "worker", agentKey{name: "a"}, "", false, false, false, "")
		return ct
	}(), newCostTally(false), nil, 0, nil, nil)
	if r.TotalTokens != 10 || r.TotalRead != 1_000_000 || r.Agents[0].Total != 10 {
		t.Fatalf("total = %d, read = %d, agent = %+v", r.TotalTokens, r.TotalRead, r.Agents[0])
	}
}

// The lists GetCost no longer fills say so to every client, not only in a comment.
func TestTheEmptiedTokenListsAreMarkedDeprecated(t *testing.T) {
	file := v1.File_agentmesh_v1_report_proto
	for _, name := range []protoreflect.Name{"TokenKindCost", "TokenModelPhaseCost"} {
		md := file.Messages().ByName(name)
		if md == nil || !md.Options().(*descriptorpb.MessageOptions).GetDeprecated() {
			t.Errorf("message %s is not marked deprecated", name)
		}
	}
	resp := file.Messages().ByName("GetCostResponse")
	for _, name := range []protoreflect.Name{"token_kinds", "tokens_by_model"} {
		fd := resp.Fields().ByName(name)
		if fd == nil || !fd.Options().(*descriptorpb.FieldOptions).GetDeprecated() {
			t.Errorf("field %s is not marked deprecated", name)
		}
	}
}

// The 5m share counts only Claude's writes: an OpenCode call's cache write sits in w5 with no TTL behind it.
func TestWrite5mShareIsClaudesOnly(t *testing.T) {
	tally := newCostTally(true)
	tokens := newCostTally(false)
	tally.add("c", costCall{model: "claude-opus-5-5", w1: 300, w5: 100}, "worker", agentKey{name: "a"}, "", false, false, false, "")
	tokens.add("o", costCall{model: "p/m", cli: "opencode", w5: 5000}, "opencode worker", agentKey{name: "b"}, "", false, false, false, "")
	r := buildCostReport("run x", tally, tokens, nil, 0, nil, nil)
	for _, k := range r.Kinds {
		switch k.Kind {
		case "worker":
			if k.Write_5MShare != 25 {
				t.Errorf("worker 5m share = %v, want 25", k.Write_5MShare)
			}
		case "opencode worker":
			if k.Write_5MShare != 0 || k.W5 != 5000 {
				t.Errorf("opencode worker = %+v, want its writes counted and no 5m share", k)
			}
		}
	}
}
