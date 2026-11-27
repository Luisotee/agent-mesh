package daemon

import (
	"context"
	"strings"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
)

// GetAgentProgress says what an agent waits on (for every CLI) and where the daemon acts on its context (Claude's), and
// reads its plan (its newest TodoWrite list) and its conversation's size from its CLI's own record. Only Claude Code's
// record is read: for any other CLI, or an agent with no conversation yet, the plan and sizes are empty (and `supported`
// says whether the record could be read at all).
func (d *Daemon) GetAgentProgress(ctx context.Context, req *connect.Request[v1.GetAgentProgressRequest]) (*connect.Response[v1.GetAgentProgressResponse], error) {
	a, err := d.findAgentCrossRun(d.store.Read(ctx), auth.ActorFrom(ctx), req.Msg.Agent)
	if err != nil {
		return nil, err
	}
	out := &v1.GetAgentProgressResponse{Wait: d.agentWait(ctx, a)}
	// The marks are policy, known before the conversation has a record: the window comes from the model's name.
	if a.Kind == claude.Kind {
		out.Marks = d.contextMarks(d.store.Read(ctx), a, contextWindow(a.Model))
	}
	if a.Kind != claude.Kind || a.SessionId == "" || a.Worktree == "" {
		return connect.NewResponse(out), nil
	}
	path := claude.TranscriptPath(d.cliBaseEnv(), a.Worktree, a.SessionId)
	if path == "" {
		return connect.NewResponse(out), nil
	}
	p, err := claude.ReadProgress(path)
	if err != nil {
		return connect.NewResponse(out), nil // no record yet: nothing to show
	}
	out.Supported = true
	out.ContextTokens, out.ContextWindow = p.ContextTokens, contextWindow(a.Model)
	for _, it := range p.Plan {
		out.Plan = append(out.Plan, &v1.PlanItem{Content: it.Content, Status: it.Status})
	}
	return connect.NewResponse(out), nil
}

// contextWindow is the window a Claude model has, guessed from its name: 1,000,000 tokens for a `1m` variant, else 200,000.
func contextWindow(model string) int64 {
	m := strings.ToLower(model)
	if strings.Contains(m, "[1m]") || strings.Contains(m, "-1m") || strings.HasSuffix(m, "1m") {
		return 1_000_000
	}
	return 200_000
}
