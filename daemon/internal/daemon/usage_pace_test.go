package daemon_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/hook"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// `mesh usage` says how far through its window each pool is, warns when use runs ahead of it, keeps each reading
// for the size estimate, lists OpenCode's held models, and says what is not measured yet.
func TestUsageShowsPaceKeepsReadingsAndListsOpenCodeHolds(t *testing.T) {
	ctx := context.Background()
	reset := time.Now().Add(6 * 24 * time.Hour).UTC().Format(time.RFC3339)
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Usage = &daemon.UsageOps{
			DisableClaudeNetwork: true,
			ReadAgyUsage: func(context.Context) (string, error) {
				return "Gemini 4 Neon\tNeon\t2%\t4h\n", nil
			},
			ReadAgyModels: func(context.Context) (string, error) {
				return "gemini-4-neon-high\tGemini 4 Neon (High)\n", nil
			},
			FetchClaudeUsage: func(context.Context) (*hook.ClaudeUsageSnapshot, error) {
				return &hook.ClaudeUsageSnapshot{
					Windows: map[string]hook.ClaudeUsageWindow{
						"five_hour": {Used: 10, Resets: time.Now().Add(4 * time.Hour).UTC().Format(time.RFC3339)},
						"seven_day": {Used: 80, Resets: reset},
					},
					Source: "live",
				}, nil
			},
		}
	})
	if err := w.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error {
		return tx.PutModelLimit("opencode", "opencode/beam-free", time.Now().Add(3*time.Hour))
	}); err != nil {
		t.Fatal(err)
	}
	res, err := w.device.Mesh.GetUsage(ctx, connect.NewRequest(&v1.GetUsageRequest{Refresh: true}))
	if err != nil {
		t.Fatal(err)
	}
	u := res.Msg
	var week, short *v1.ClaudeUsageWindow
	for _, win := range u.Claude.Windows {
		switch win.Key {
		case "seven_day":
			week = win
		case "five_hour":
			short = win
		}
	}
	if week == nil || short == nil {
		t.Fatalf("windows %v", u.Claude.Windows)
	}
	if week.Elapsed < 10 || week.Elapsed > 18 || !strings.Contains(week.Pace, "runs out in") {
		t.Errorf("a week at 80%% used with 6 days left: elapsed %.1f, pace %q", week.Elapsed, week.Pace)
	}
	if short.Pace != "" {
		t.Errorf("a five-hour window at 10%%: %q", short.Pace)
	}
	if week.Worth != "not measured yet" {
		t.Errorf("worth before any measure: %q", week.Worth)
	}
	if len(u.AgyRows) != 1 || !strings.Contains(u.AgyRows[0].Pace, "runs out in") {
		t.Errorf("agy at 98%% used with 4h to the reset: %v", u.AgyRows)
	}
	if u.Unit != "a Claude week" {
		t.Errorf("unit %q", u.Unit)
	}
	if len(u.Held) != 1 || !strings.Contains(u.Held[0], "opencode/beam-free until") {
		t.Errorf("held %v", u.Held)
	}
	for _, pool := range []string{"claude:seven_day", "claude:five_hour", "agy:Gemini 4 Neon:short"} {
		s, err := w.d.Store().Read(ctx).UsageSamples(pool, time.Now().Add(-time.Hour))
		if err != nil || len(s) != 1 {
			t.Errorf("samples of %s: %v %v", pool, s, err)
		}
	}
}
