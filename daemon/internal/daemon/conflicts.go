package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

// conflictBranch tracks one branch in a repo for CheckConflicts.
type conflictBranch struct {
	repo      string
	worktree  string
	branch    string
	base      string
	tip       string
	committed bool
}

// CheckConflicts finds which run branches with commits conflict with their repo's default branch or with each
// other (via `git merge-tree --write-tree`), and which workers' changes, committed or not, touch the same files.
func (d *Daemon) CheckConflicts(ctx context.Context, req *connect.Request[v1.CheckConflictsRequest]) (*connect.Response[v1.CheckConflictsResponse], error) {
	runID, err := scopedRun(auth.ActorFrom(ctx), req.Msg.RunId)
	if err != nil {
		return nil, err
	}
	if runID == "" {
		return nil, invalid("no run given")
	}
	r := d.store.Read(ctx)
	if _, err := r.GetRun(runID); errors.Is(err, store.ErrNotFound) {
		return nil, notFound("no run %s", runID)
	} else if err != nil {
		return nil, err
	}
	agents, err := r.ListAgents(runID)
	if err != nil {
		return nil, err
	}
	wts, err := r.ListWorktrees(runID)
	if err != nil {
		return nil, err
	}

	byRepo := map[string]map[string]conflictBranch{}
	addBranch := func(repo, worktree, branch, base string) {
		if branch == "" || repo == "" {
			return // e.g. a peer in a detached worktree
		}
		tip, err := gitRO(ctx, repo, "rev-parse", "--verify", "refs/heads/"+branch)
		if err != nil {
			return // deleted
		}
		seen := byRepo[repo]
		if seen == nil {
			seen = map[string]conflictBranch{}
			byRepo[repo] = seen
		}
		prev, exists := seen[branch]
		if !exists || (!isDir(prev.worktree) && isDir(worktree)) {
			seen[branch] = conflictBranch{
				repo:      repo,
				worktree:  worktree,
				branch:    branch,
				base:      base,
				tip:       tip,
				committed: tip != base,
			}
		}
	}
	for _, a := range agents {
		addBranch(a.Repo, a.Worktree, a.Branch, a.BaseCommit)
	}
	for _, wt := range wts {
		addBranch(wt.Repo, wt.Path, wt.Branch, wt.BaseCommit)
	}

	repos := make([]string, 0, len(byRepo))
	for repo := range byRepo {
		repos = append(repos, repo)
	}
	sort.Strings(repos)

	out := &v1.CheckConflictsResponse{}
	for _, repo := range repos {
		wts := byRepo[repo]
		target := defaultBaseRO(ctx, repo)
		tips := map[string]string{}
		branches := make([]string, 0, len(wts))
		for b, wt := range wts {
			tips[b] = wt.tip
			branches = append(branches, b)
		}
		sort.Strings(branches)
		anc := newAncestry(repo, tips)

		var committed []string
		for _, b := range branches {
			if wts[b].committed {
				committed = append(committed, b)
				out.CheckedBranches++
			}
		}

		// Stacked branches (one built on the other's commits) are meant to overlap: skip those pairs.
		type pair struct{ a, b string }
		var apart []pair
		for i, a := range branches {
			for _, b := range branches[i+1:] {
				s1, err := stacked(ctx, anc, wts[a], wts[b], target)
				if err != nil {
					return nil, err
				}
				s2, err := stacked(ctx, anc, wts[b], wts[a], target)
				if err != nil {
					return nil, err
				}
				if !s1 && !s2 {
					apart = append(apart, pair{a, b})
				}
			}
		}

		var mergePairs []pair
		for _, b := range committed {
			mergePairs = append(mergePairs, pair{b, target})
		}
		for i, a := range committed {
			for _, b := range committed[i+1:] {
				ab, err := anc.contains(ctx, a, b)
				if err != nil {
					return nil, err
				}
				ba, err := anc.contains(ctx, b, a)
				if err != nil {
					return nil, err
				}
				if !ab && !ba {
					mergePairs = append(mergePairs, pair{a, b})
				}
			}
		}

		conflicting := map[pair]map[string]bool{}
		repoName := filepath.Base(repo)
		for _, p := range mergePairs {
			ours := p.b
			if wt, ok := wts[p.b]; ok {
				ours = wt.tip
			}
			files, hasConflict, err := mergeConflicts(ctx, repo, ours, wts[p.a].tip)
			if err != nil {
				return nil, err
			}
			if hasConflict {
				out.Conflicts = append(out.Conflicts, &v1.BranchConflict{
					Repo:    repoName,
					Branch:  p.a,
					Against: p.b,
					Files:   files,
					Kind:    v1.ConflictKind_CONFLICT_KIND_CONFLICT,
				})
				set := map[string]bool{}
				for _, f := range files {
					set[f] = true
				}
				conflicting[p] = set
			}
		}

		touched := map[string]map[string]bool{}
		for _, b := range branches {
			files, err := changedFiles(ctx, anc, wts[b], target)
			if err != nil {
				out.Warnings = append(out.Warnings, fmt.Sprintf("mesh conflicts: no overlap check for %s: %v", b, err))
				continue
			}
			touched[b] = files
		}
		for _, p := range apart {
			ta, oka := touched[p.a]
			tb, okb := touched[p.b]
			if !oka || !okb {
				continue
			}
			cset := conflicting[p]
			var shared []string
			for f := range ta {
				if tb[f] && !cset[f] {
					shared = append(shared, f)
				}
			}
			sort.Strings(shared)
			if len(shared) > 0 {
				out.Overlaps = append(out.Overlaps, &v1.BranchConflict{
					Repo:    repoName,
					Branch:  p.a,
					Against: p.b,
					Files:   shared,
					Kind:    v1.ConflictKind_CONFLICT_KIND_OVERLAP,
				})
			}
		}
	}
	return connect.NewResponse(out), nil
}

func isDir(p string) bool {
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// gitROEnv returns os.Environ() with GIT_OPTIONAL_LOCKS=0 so read-only git calls never take a worker's index.lock.
func gitROEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, "GIT_OPTIONAL_LOCKS=") {
			out = append(out, e)
		}
	}
	return append(out, "GIT_OPTIONAL_LOCKS=0")
}

func gitRORaw(ctx context.Context, dir string, args ...string) (stdout []byte, stderr string, code int, err error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitROEnv()
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			return outBuf.Bytes(), errBuf.String(), ee.ExitCode(), runErr
		}
		return outBuf.Bytes(), errBuf.String(), -1, runErr
	}
	return outBuf.Bytes(), errBuf.String(), 0, nil
}

func gitRO(ctx context.Context, dir string, args ...string) (string, error) {
	out, stderr, _, err := gitRORaw(ctx, dir, args...)
	if err != nil {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(string(out)), nil
}

func defaultBaseRO(ctx context.Context, top string) string {
	if ref, err := gitRO(ctx, top, "rev-parse", "--abbrev-ref", "origin/HEAD"); err == nil && ref != "" && ref != "origin/HEAD" {
		if _, err := gitRO(ctx, top, "rev-parse", "--verify", "-q", ref+"^{commit}"); err == nil {
			return ref
		}
	}
	return "HEAD"
}

func mergeConflicts(ctx context.Context, repo, ours, theirs string) ([]string, bool, error) {
	// -z: names come NUL-separated and unquoted (without it git C-quotes a non-ASCII name); the first record is
	// the merged tree's OID.
	stdout, stderr, code, err := gitRORaw(ctx, repo, "merge-tree", "--write-tree", "--name-only", "--no-messages", "-z", ours, theirs)
	if code == 0 && err == nil {
		return nil, false, nil
	}
	if code == 1 {
		records := strings.Split(string(stdout), "\x00")
		var files []string
		if len(records) > 1 {
			for _, rec := range records[1:] {
				if rec != "" {
					files = append(files, rec)
				}
			}
		}
		return files, true, nil
	}
	if strings.Contains(stderr, "write-tree") || strings.Contains(stderr, "usage") {
		return nil, false, failedPrecondition("`mesh conflicts` needs git 2.38 or newer (`git merge-tree --write-tree`)")
	}
	return nil, false, fmt.Errorf("git merge-tree %s %s: %s", ours, theirs, strings.TrimSpace(stderr))
}

type commitPair struct{ a, b string }

type ancestry struct {
	repo    string
	commits map[string]string
	known   map[commitPair]bool
}

func newAncestry(repo string, tips map[string]string) *ancestry {
	commits := make(map[string]string, len(tips)*2)
	for ref, tip := range tips {
		commits[ref] = tip
		commits[tip] = tip
	}
	return &ancestry{
		repo:    repo,
		commits: commits,
		known:   map[commitPair]bool{},
	}
}

func (a *ancestry) commit(ctx context.Context, ref string) string {
	if ref == "" {
		return ""
	}
	if c, ok := a.commits[ref]; ok {
		return c
	}
	c, err := gitRO(ctx, a.repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		c = ""
	}
	a.commits[ref] = c
	return c
}

func (a *ancestry) contains(ctx context.Context, ancestorRef, descRef string) (bool, error) {
	cA, cB := a.commit(ctx, ancestorRef), a.commit(ctx, descRef)
	if cA == "" || cB == "" {
		return false, nil
	}
	if cA == cB {
		return true, nil
	}
	pair := commitPair{cA, cB}
	if v, ok := a.known[pair]; ok {
		return v, nil
	}
	_, stderr, code, err := gitRORaw(ctx, a.repo, "merge-base", "--is-ancestor", cA, cB)
	if code != 0 && code != 1 {
		msg := strings.TrimSpace(stderr)
		if msg == "" && err != nil {
			msg = err.Error()
		}
		return false, fmt.Errorf("git merge-base --is-ancestor %s %s: %s", cA, cB, msg)
	}
	ans := code == 0
	a.known[pair] = ans
	return ans, nil
}

func stacked(ctx context.Context, anc *ancestry, parent, child conflictBranch, target string) (bool, error) {
	if parent.committed {
		inTarget, err := anc.contains(ctx, parent.branch, target)
		if err != nil {
			return false, err
		}
		if !inTarget {
			inChild, err := anc.contains(ctx, parent.branch, child.branch)
			if err != nil {
				return false, err
			}
			if inChild {
				return true, nil
			}
		}
	}
	if child.base == "" || parent.base == "" || child.base == parent.base {
		return false, nil
	}
	ownInBase, err := anc.contains(ctx, parent.base, child.base)
	if err != nil || !ownInBase {
		return false, err
	}
	baseInTarget, err := anc.contains(ctx, child.base, target)
	if err != nil || baseInTarget {
		return false, err
	}
	return anc.contains(ctx, child.base, parent.branch)
}

func changedFiles(ctx context.Context, anc *ancestry, wt conflictBranch, target string) (map[string]bool, error) {
	start, err := gitRO(ctx, anc.repo, "merge-base", target, wt.tip)
	if err != nil {
		return nil, err
	}
	if wt.base != "" {
		own, err := gitRO(ctx, anc.repo, "merge-base", wt.base, wt.tip)
		if err != nil {
			return nil, err
		}
		inStart, err := anc.contains(ctx, own, start)
		if err != nil {
			return nil, err
		}
		if !inStart {
			start = own
		}
	}
	diffOut, stderr, code, err := gitRORaw(ctx, anc.repo, "diff", "--name-only", "-z", "--no-renames", start, wt.tip)
	if code != 0 || err != nil {
		return nil, fmt.Errorf("git diff %s %s: %s", start, wt.tip, strings.TrimSpace(stderr))
	}
	files := map[string]bool{}
	for _, raw := range bytes.Split(diffOut, []byte{0}) {
		if len(raw) > 0 {
			files[string(raw)] = true
		}
	}
	if isDir(wt.worktree) {
		statusOut, _, sCode, sErr := gitRORaw(ctx, wt.worktree, "status", "--porcelain=v2", "-z", "--untracked-files=all")
		if sCode == 0 && sErr == nil {
			fields := bytes.Split(statusOut, []byte{0})
			for i := 0; i < len(fields); i++ {
				field := string(fields[i])
				if field == "" {
					continue
				}
				switch field[0] {
				case '1':
					parts := strings.SplitN(field, " ", 9)
					if len(parts) == 9 && parts[8] != "" {
						files[parts[8]] = true
					}
				case '2':
					parts := strings.SplitN(field, " ", 10)
					if len(parts) == 10 && parts[9] != "" {
						files[parts[9]] = true
					}
					if i+1 < len(fields) {
						i++
						if orig := string(fields[i]); orig != "" {
							files[orig] = true
						}
					}
				case 'u':
					parts := strings.SplitN(field, " ", 11)
					if len(parts) == 11 && parts[10] != "" {
						files[parts[10]] = true
					}
				case '?':
					if len(field) > 2 {
						files[field[2:]] = true
					}
				}
			}
		}
	}
	return files, nil
}
