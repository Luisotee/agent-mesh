package daemon_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// genMetadata is a `gen_metadata` row as agy writes it: field 1 holds the model id (19) and labels (20), among them
// the index of the last step the generation was given.
func genMetadata(model string, lastStep int) []byte {
	label := pbBytes(20, append(pbBytes(1, []byte("last_step_index")), pbBytes(2, []byte(fmt.Sprint(lastStep)))...))
	return pbBytes(1, append(pbBytes(19, []byte(model)), label...))
}

func TestCostCountsCopilotAndNamesAnAgyModel(t *testing.T) {
	home := t.TempDir()
	h := startCostHarness(t, home)
	ctx := context.Background()
	if err := h.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error {
		if err := tx.PutRun(&v1.Run{Id: "r1", Title: "one", Status: v1.RunStatus_RUN_STATUS_OPEN, CreatedAt: costTS(0)}); err != nil {
			return err
		}
		// Neither agent has a model on record (the CLI's own default ran).
		for _, a := range []*v1.Agent{
			{Id: "g1", RunId: "r1", Name: "repo-agy", Role: v1.AgentRole_AGENT_ROLE_WORKER, Kind: "agy", Worktree: "/wt1", Repo: "/repo", SessionId: "conv1", CreatedAt: costTS(1)},
			{Id: "c1", RunId: "r1", Name: "repo-copilot", Role: v1.AgentRole_AGENT_ROLE_WORKER, Kind: "copilot", Worktree: "/wt2", Repo: "/repo", SessionId: "cop-1", CreatedAt: costTS(1)},
		} {
			if err := tx.PutAgent(a); err != nil {
				return err
			}
			if _, err := tx.AppendEvent(&v1.Event{RunId: "r1", AgentId: a.Id, Payload: &v1.Event_AgentSpawned{AgentSpawned: &v1.AgentSpawned{Agent: a}}}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// agy: steps 1 and 3 are model steps; the generation before step 3 ran another model.
	convDir := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatal(err)
	}
	adb, err := sql.Open("sqlite", filepath.Join(convDir, "conv1.db"))
	if err != nil {
		t.Fatal(err)
	}
	adb.Exec(`CREATE TABLE steps (idx integer, step_type integer, status integer, metadata blob)`)
	adb.Exec(`CREATE TABLE gen_metadata (idx integer, data blob)`)
	usagePB := append(append(pbInt(2, 1000), pbInt(5, 20_000)...), pbInt(3, 300)...)
	for i, m := range []int{2, 20} {
		sec := uint64(costT0.Unix()) + uint64(m*60)
		adb.Exec(`INSERT INTO steps VALUES (?, 15, 3, ?)`, 1+2*i, append(pbBytes(1, pbInt(1, sec)), pbBytes(9, usagePB)...))
	}
	adb.Exec(`INSERT INTO gen_metadata VALUES (0, ?)`, genMetadata("gemini-4-neon", 0))
	adb.Exec(`INSERT INTO gen_metadata VALUES (1, ?)`, genMetadata("gemini-4-hydrogen", 2))
	adb.Close()

	// Copilot: two calls of the worker (the second with a cache write), a half-written line, and an event that isn't a call.
	usage := func(id string, minutes float64, model string, in, out, read, write int64) string {
		raw, _ := json.Marshal(map[string]any{"type": "session.usage_record", "id": id, "timestamp": costISO(minutes),
			"data": map[string]any{"usage": map[string]any{"model": model, "inputTokens": in, "outputTokens": out, "cacheReadTokens": read, "cacheWriteTokens": write, "reasoningTokens": 3, "cost": 1.0}}})
		return string(raw) + "\n"
	}
	dir := filepath.Join(home, ".copilot", "session-state", "cop-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	events := usage("u1", 3, "gpt-5.6-luna", 14378, 6, 9777, 4598) + usage("u2", 4, "gpt-5.6-luna", 10_000, 500, 9_000, 0) +
		`{"type":"assistant.turn_end","data":{},"id":"t1","timestamp":"` + costISO(5) + `"}` + "\n" +
		`{"type":"session.usage_record","data":{"usage":{"inputTokens":99`
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(events), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := h.device.Mesh.GetCost(ctx, connect.NewRequest(&v1.GetCostRequest{RunId: "r1"}))
	if err != nil {
		t.Fatal(err)
	}
	byAgent := map[string]*v1.AgentCost{}
	for _, a := range res.Msg.Agents {
		byAgent[a.Name] = a
	}
	// Copilot's inputTokens holds its cache reads and writes: the fresh input is what is left of it.
	if a := byAgent["repo-copilot"]; a == nil || a.Kind != "copilot worker" || a.Calls != 2 || a.Input != 3+1000 || a.W5 != 4598 || a.Read != 9777+9000 || a.Output != 506 || a.Usd != 0 {
		t.Fatalf("repo-copilot = %+v", a)
	}
	byCLI := map[string]*v1.CliCost{}
	for _, c := range res.Msg.Clis {
		byCLI[c.Cli] = c
	}
	if c := byCLI["copilot"]; c == nil || c.Calls != 2 || c.Basis == "" || !strings.Contains(c.Basis, "premium request") {
		t.Fatalf("copilot cli row = %+v", byCLI["copilot"])
	}
	// agy: the first step ran the first generation's model; the second the one that had seen step 2.
	models := map[string]int64{}
	for _, m := range res.Msg.ByModel {
		models[m.Model] += m.Calls
	}
	if models["gemini-4-neon"] != 1 || models["gemini-4-hydrogen"] != 1 || models["?"] != 0 || models["agy"] != 0 {
		t.Fatalf("by_model = %+v", res.Msg.ByModel)
	}
}
