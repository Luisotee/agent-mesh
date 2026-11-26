package daemon

import (
	"context"
	"strings"

	"connectrpc.com/connect"

	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// HoldPullRequest holds a pull request's merge, or releases it (docs/apps.md, "Autonomy per project": "Hold merge"
// pauses one PR at any level). The hold is a flag of the PR's record in `agent_prs`, kept across reads of GitHub; the
// root reads it in `mesh prs` and the brief and never merges a held PR (ORCHESTRATE). Holding a PR the daemon doesn't
// list is refused: nothing would carry the flag to the root.
func (d *Daemon) HoldPullRequest(ctx context.Context, req *connect.Request[v1.HoldPullRequestRequest]) (*connect.Response[v1.HoldPullRequestResponse], error) {
	url := strings.TrimSpace(req.Msg.Url)
	if url == "" {
		return nil, invalid("name the pull request (url), as `mesh prs` shows it")
	}
	out := &v1.HoldPullRequestResponse{}
	_, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceHoldPullRequestProcedure, out, func(t *store.Tx) error {
		rows, err := t.PRsByURL(url)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return notFound("no pull request %s among the ones the host lists (`mesh prs`)", url)
		}
		for _, p := range rows {
			if hold := !req.Msg.Release; p.Held != hold {
				p.Held = hold
				if err := t.PutAgentPR(p); err != nil {
					return err
				}
				a, err := t.GetAgent(p.AgentId)
				if err != nil {
					return err
				}
				// The event carries the PR as it is now, with its state and checks unchanged: a client replaces the PR it holds.
				if _, err := t.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: a.Id,
					Payload: &v1.Event_PullRequestChanged{PullRequestChanged: &v1.PullRequestChanged{PullRequest: p, FromState: p.State, FromChecks: p.Checks}}}); err != nil {
					return err
				}
			}
			out.PullRequests = append(out.PullRequests, p)
		}
		return nil
	})
	return connect.NewResponse(out), err
}
