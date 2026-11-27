package daemon

import (
	"context"
	"regexp"
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
	base, suffixed := a.Branch, false
	if m := stackSuffixRE.FindStringSubmatch(a.Branch); m != nil {
		base, suffixed = m[1], true
	}
	foreign := map[string]bool{}
	if others, err := r.ListAgents(a.RunId); err == nil {
		for _, o := range others {
			if o.Id != a.Id && o.Branch != "" {
				foreign[o.Branch] = true
			}
		}
	}
	// An agent recorded on `<base>-<n>` while another agent of the run is recorded on `<base>` itself (slugs `foo` and
	// `foo-2`) can't tell `foo`'s later branches from its own: it keeps its recorded branch and the one its worktree is on.
	expand := !(suffixed && foreign[base])
	seen := map[string]bool{a.Branch: true}
	branches := []string{a.Branch}
	add := func(b string) {
		b = strings.TrimSpace(b)
		if b != "" && !seen[b] && !foreign[b] {
			seen[b] = true
			branches = append(branches, b)
		}
	}
	if !expand {
		// no look at the other local branches
	} else if out, err := git(ctx, dir, "for-each-ref", "--format=%(refname:short)", "refs/heads/"+base, "refs/heads/"+base+"-*"); err == nil {
		for _, b := range strings.Fields(out) {
			if b == base || (strings.HasPrefix(b, base+"-") && isDigits(strings.TrimPrefix(b, base+"-"))) {
				add(b)
			}
		}
	}
	if a.Worktree != "" && dir == a.Worktree {
		if out, err := git(ctx, dir, "branch", "--show-current"); err == nil && strings.HasPrefix(strings.TrimSpace(out), "mesh/"+a.RunId+"/") {
			add(out)
		}
	}
	number := func(b string) int {
		if m := stackSuffixRE.FindStringSubmatch(b); m != nil && strings.HasPrefix(b, base+"-") && b != base {
			n, _ := strconv.Atoi(m[2])
			return n
		}
		return 1
	}
	sort.SliceStable(branches, func(i, j int) bool { return number(branches[i]) > number(branches[j]) })
	if len(branches) > stackBranchCap {
		branches = branches[:stackBranchCap]
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
