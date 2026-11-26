package daemon

import (
	"math"
	"testing"
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
		{"agy is free", "agy", m, 0, 0, m, m, 0},
	}
	for _, c := range cases {
		got := claudeCallCost(c.model, c.input, c.w5, c.w1, c.read, c.out)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
