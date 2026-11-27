package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
	"google.golang.org/protobuf/proto"
)

// A worker moves to a new branch for its next PR (the daemon does it after a merge, and a worker may `git checkout
// -b` itself), and the branch stored on its agent row is the one it was spawned on. What `check`, `agents` and the
// work evidence say is of the branch its worktree has checked out now.

// headBranch is the branch a worktree has checked out, read from its HEAD file (no git process): "" for a detached
// HEAD, a missing worktree or one git cannot read.
func headBranch(worktree string) string {
	if worktree == "" {
		return ""
	}
	gitdir := filepath.Join(worktree, ".git")
	if b, err := os.ReadFile(gitdir); err == nil { // a linked worktree: ".git" is a file naming its gitdir
		dir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
		if !ok {
			return ""
		}
		gitdir = strings.TrimSpace(dir)
		if !filepath.IsAbs(gitdir) {
			gitdir = filepath.Join(worktree, gitdir)
		}
	}
	head, err := os.ReadFile(filepath.Join(gitdir, "HEAD"))
	if err != nil {
		return ""
	}
	ref, ok := strings.CutPrefix(strings.TrimSpace(string(head)), "ref: refs/heads/")
	if !ok {
		return ""
	}
	return ref
}

// movedBranch is the branch a change worker's worktree is on now when that is not the one stored, else "".
func movedBranch(a *v1.Agent) string {
	if a.Peer || a.Repo == "" || a.Worktree == "" || a.Role == v1.AgentRole_AGENT_ROLE_ROOT || adapter.ReadOnly(a.Role) {
		return ""
	}
	if cur := headBranch(a.Worktree); cur != "" && cur != a.Branch {
		return cur
	}
	return ""
}

// onItsBranch is the agent as it stands on the branch its worktree has now, with the base re-derived (where that
// branch left the default branch), or the agent itself when it is on the branch stored. The stored base belongs to
// the first branch: counting from it would call the merged PR's commits the new branch's.
func onItsBranch(ctx context.Context, a *v1.Agent) *v1.Agent {
	cur := movedBranch(a)
	if cur == "" {
		return a
	}
	out := proto.Clone(a).(*v1.Agent)
	out.Branch = cur
	// The default branch, else (no origin/HEAD) the repo's own HEAD, its main checkout: either way not the first
	// branch's start, which would count the merged PR's commits again.
	if tip, err := gitRO(ctx, a.Repo, "rev-parse", "--verify", "refs/heads/"+cur); err == nil {
		if mb, err := gitRO(ctx, a.Repo, "merge-base", defaultBaseRO(ctx, a.Repo), tip); err == nil && mb != "" {
			out.BaseCommit = mb
		}
	}
	return out
}

// followBranch records the branch a change worker's worktree is on now, with the base derived for it (where it left the
// default branch), on its row when the worker moved on itself (`git checkout -b fix-2`): the apps, the PR stack, the
// finished-PR move and a worktree recreated from the branch read the recorded one. Only a later branch of the worker's own
// series is taken (`inSeriesOf` and a higher `-N`: `fix`, then `fix-2`, `fix-3`), never `main`, an earlier branch it went
// back to look at, nor one another agent of the repo records or has checked out (`worker_review.go`: a switch onto another
// worker's branch does not make that PR the worker's own); and only when the base can be derived (else the next idle
// tries again). It runs after the idle's hook has replied, under the worktree's lock (a daemon move that failed and rolled
// its branch back is not recorded half-way), outside any write: the base is read with git. The row is read again in the
// write, so a move the daemon made meanwhile is not undone.
func (d *Daemon) followBranch(ctx context.Context, a *v1.Agent) {
	if a.Worktree == "" || !laterInSeries(a.Branch, movedBranch(a)) {
		return // most idles: nothing moved, no lock taken
	}
	mu := d.worktreeLock(a.Worktree)
	mu.Lock()
	defer mu.Unlock()
	cur := movedBranch(a) // read again under the lock
	if !laterInSeries(a.Branch, cur) || d.otherAgentHasBranch(ctx, a, cur) {
		return
	}
	tip, err := gitRO(ctx, a.Repo, "rev-parse", "--verify", "refs/heads/"+cur)
	if err != nil {
		return
	}
	base, err := gitRO(ctx, a.Repo, "merge-base", defaultBaseRO(ctx, a.Repo), tip)
	if err != nil || base == "" {
		return
	}
	err = d.store.Write(ctx, nil, "", func(t *store.Tx) error {
		row, err := t.GetAgent(a.Id)
		if err != nil || row.Branch != a.Branch || row.State == v1.AgentState_AGENT_STATE_RETIRED {
			return err
		}
		row.Branch, row.BaseCommit = cur, base
		return t.PutAgent(row)
	})
	if err != nil {
		d.log.Warn("recording the branch a worker moved to", "agent", a.Name, "branch", cur, "err", err)
	}
}

// seriesNumber is the `-N` a branch's slug ends in (`fix-3` is 3), 0 for the bare slug.
func seriesNumber(branch string) int {
	i := strings.LastIndex(branch, "-")
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(branch[i+1:])
	if err != nil {
		return 0
	}
	return n
}

// laterInSeries reports whether `cur` is a branch of `recorded`'s series (same `mesh/<run>/<slug>`) made after it.
func laterInSeries(recorded, cur string) bool {
	return cur != "" && recorded != "" && inSeriesOf(recorded, cur) && seriesNumber(cur) > seriesNumber(recorded)
}

// otherAgentHasBranch reports whether another agent of the same repo records the branch or has it checked out.
func (d *Daemon) otherAgentHasBranch(ctx context.Context, a *v1.Agent, branch string) bool {
	others, err := d.store.Read(ctx).ListAgents(a.RunId)
	if err != nil {
		return true // cannot tell: record nothing
	}
	for _, o := range others {
		if o.Id != a.Id && o.Repo == a.Repo && (o.Branch == branch || headBranch(o.Worktree) == branch) {
			return true
		}
	}
	return false
}
