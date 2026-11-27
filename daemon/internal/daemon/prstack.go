package daemon

import (
	"context"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// stackBranchCap is the most branches of one agent's stack whose PRs are asked about: the newest.
const stackBranchCap = 8

var stackSuffixRE = regexp.MustCompile(`^(.+)-([0-9]+)$`)

// stackBranches are the branches an agent's PRs may be on: the one recorded at its spawn, and the branches of its stack.
// A worker with a stack of PRs moves on to `<branch>-2`, `-3` … (the daemon's own `nextWorktreeBranch` names them the
// same way, and so does a worker that makes the next branch itself), so the recorded branch alone loses every later PR.
// The stack is the local branches `<base>` and `<base>-<n>`, where base is the recorded branch with a trailing `-<n>`
// taken off (the agent may have been recorded on a later one), plus the branch its worktree is on now; minus the branches
// another agent of the run records as its own (a slug `foo-2` is not a later branch of `foo`), the newest first, capped.
// An agent recorded on `foo-2` while another is recorded on `foo` looks at no other branch (`expand`).
func stackBranches(ctx context.Context, r store.Reader, a *v1.Agent, dir string) []string {
	if a.Branch == "" {
		return nil
	}
	// The recorded branch and what it was named after: `fix-2-2` was made after `fix-2`, which was made after `fix` (the
	// daemon names the next branch from the recorded one's slug), so each trailing `-<n>` taken off is a base to look from.
	bases := []string{a.Branch}
	for {
		m := stackSuffixRE.FindStringSubmatch(bases[len(bases)-1])
		if m == nil {
			break
		}
		bases = append(bases, m[1])
	}
	// What other agents of the same repo own: the branch each records now, and the head of every PR their own branch
	// has (an agent that moved on to `fix-3` no longer records `fix`, but its PR from there is still its own). A PR an
	// agent only reported (`report`) is no branch of its; and the branch names carry no repo, so another repo's agent
	// of the same slug owns none of ours.
	foreign := map[string]bool{}
	if others, err := r.ListAgents(a.RunId); err == nil {
		for _, o := range others {
			if o.Id == a.Id || o.Repo != a.Repo {
				continue
			}
			if o.Branch != "" {
				foreign[o.Branch] = true
			}
			if prs, err := r.AgentPRs(o.Id); err == nil {
				for _, p := range prs {
					if p.HeadRef != "" && p.Source == "branch" {
						foreign[p.HeadRef] = true
					}
				}
			}
		}
	}
	seen := map[string]bool{a.Branch: true}
	branches := []string{a.Branch}
	add := func(b string) bool {
		b = strings.TrimSpace(b)
		if b == "" || seen[b] || foreign[b] {
			return false
		}
		seen[b] = true
		branches = append(branches, b)
		return true
	}
	// An agent recorded on `<base>-<n>` while another owns `<base>` itself (slugs `foo` and `foo-2`) can't tell `foo`'s
	// later branches from its own: it looks from its own name and from no name past one another agent owns.
	for i, base := range bases {
		if i > 0 && foreign[base] {
			break
		}
		out, err := git(ctx, dir, "for-each-ref", "--format=%(refname:short)", "refs/heads/"+base, "refs/heads/"+base+"-*")
		if err != nil {
			continue
		}
		for _, b := range strings.Fields(out) {
			if b == base || (strings.HasPrefix(b, base+"-") && isDigits(strings.TrimPrefix(b, base+"-"))) {
				add(b)
			}
		}
	}
	current := ""
	if a.Worktree != "" && dir == a.Worktree {
		if out, err := git(ctx, dir, "branch", "--show-current"); err == nil && strings.HasPrefix(strings.TrimSpace(out), "mesh/"+a.RunId+"/") {
			// Only a branch the filter let in is the one that is never cut.
			if b := strings.TrimSpace(out); add(b) || seen[b] {
				current = b
			}
		}
	}
	number := func(b string) int {
		if m := stackSuffixRE.FindStringSubmatch(b); m != nil {
			n, _ := strconv.Atoi(m[2])
			return n
		}
		return 1
	}
	sort.SliceStable(branches, func(i, j int) bool { return number(branches[i]) > number(branches[j]) })
	if len(branches) > stackBranchCap {
		branches = branches[:stackBranchCap]
		// The branch the worktree is on is the one most likely to hold an open PR: it is never cut.
		if current != "" && !slices.Contains(branches, current) {
			branches[stackBranchCap-1] = current
		}
	}
	return branches
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
