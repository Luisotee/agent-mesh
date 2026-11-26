package daemon

import (
	"context"
	"strings"

	"connectrpc.com/connect"

	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// HoldPullRequest holds a pull request's merge, or releases it (docs/apps.md, "Autonomy per project": "Hold merge"
// pauses one PR at any level). The hold is the user's: kept by the PR's URL (`pr_holds`), so it outlives every record
// of the PR, and set on each record `mesh prs` and the brief read, where the root sees it and never merges a held PR
// (ORCHESTRATE). A root may hold a PR its own run has, never release one: only the user (a device) releases. Holding a
// PR the daemon doesn't list is refused: nothing would carry the flag to the root.
func (d *Daemon) HoldPullRequest(ctx context.Context, req *connect.Request[v1.HoldPullRequestRequest]) (*connect.Response[v1.HoldPullRequestResponse], error) {
	url := strings.TrimSpace(req.Msg.Url)
	if url == "" {
		return nil, invalid("name the pull request (url), as `mesh prs` shows it")
	}
	own := runScope(auth.ActorFrom(ctx))
	if own != "" && req.Msg.Release {
		return nil, denied("only the user releases a held pull request")
	}
	out := &v1.HoldPullRequestResponse{}
	_, err := d.write(ctx, req.Msg.Meta, agentmeshv1connect.MeshServiceHoldPullRequestProcedure, out, func(t *store.Tx) error {
		all, err := t.PRsByURL(url)
		if err != nil {
			return err
		}
		// A root names a PR of its own run (some agent of it owns the PR); the hold is then the PR's, on every record of it.
		rows, mine := all, own == ""
		for _, p := range all {
			a, err := t.GetAgent(p.AgentId)
			if err != nil {
				return err
			}
			mine = mine || a.RunId == own
		}
		if !mine {
			rows = nil
		}
		hold := !req.Msg.Release
		was, err := t.PRHeld(url)
		if err != nil {
			return err
		}
		// A hold whose records are gone can still be released; nothing else is told of a PR the host doesn't list.
		if len(rows) == 0 && !(was && !hold) {
			return notFound("no pull request %s among the ones the host lists (`mesh prs`)", url)
		}
		if err := t.SetPRHold(url, hold); err != nil {
			return err
		}
		for _, p := range rows {
			if p.Held == hold {
				out.PullRequests = append(out.PullRequests, p)
				continue
			}
			if err := t.PutAgentPR(p); err != nil { // sets the flag from the hold
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
			out.PullRequests = append(out.PullRequests, p)
		}
		return nil
	})
	return connect.NewResponse(out), err
}
