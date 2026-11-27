package daemon_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

func TestAgentProgressNamesTheWaitAndWhereTheDaemonActsOnTheContext(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	progress := func(id string) *v1.GetAgentProgressResponse {
		t.Helper()
		res, err := w.device.Mesh.GetAgentProgress(ctx, connect.NewRequest(&v1.GetAgentProgressRequest{Agent: id}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg
	}
	marks := func(r *v1.GetAgentProgressResponse) (out []int64) {
		for _, m := range r.Marks {
			out = append(out, m.Tokens)
		}
		return out
	}
	// A lead: its reset point, while the keeper resets by itself; and it waits for the user.
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	got := progress(lead.Id)
	if m := marks(got); len(m) != 1 || m[0] != 150000 || got.Marks[0].Setting != "root_reset.at" {
		t.Errorf("a lead's marks: %v", got.Marks)
	}
	// A worker idle with nothing owed waits for its lead's message, by name; at its own thresholds (the settings are the
	// host's), those past the 200000 window dropped.
	w.setSetting(t, "compact.followup", 100000.0)
	worker := w.spawnUnder(t, asLead, "fix", &v1.SpawnAgentRequest{})
	got = progress(worker.Id)
	if m := marks(got); len(m) != 1 || m[0] != 100000 || got.Marks[0].Setting != "compact.followup" {
		t.Errorf("a worker's marks: %v (compact.claude's 300000 is past the window)", got.Marks)
	}
	if got.Wait.Kind != v1.AgentWaitKind_AGENT_WAIT_KIND_MESSAGE || got.Wait.From != lead.Name {
		t.Errorf("a worker's wait: %v", got.Wait)
	}
	// With the reset turned off the lead has no mark.
	w.setSetting(t, "root_reset.auto", false)
	if m := marks(progress(lead.Id)); len(m) != 0 {
		t.Errorf("a lead the mesh does not reset still has marks: %v", m)
	}
	// A plain agent: nothing is done to it, so nothing is marked, and it waits for the user.
	_, plain, _ := w.plainRoot(t)
	got = progress(plain.Id)
	if len(got.Marks) != 0 || got.Wait.Kind != v1.AgentWaitKind_AGENT_WAIT_KIND_MESSAGE || got.Wait.From != "user" {
		t.Errorf("a plain agent: marks %v wait %v", got.Marks, got.Wait)
	}
}
