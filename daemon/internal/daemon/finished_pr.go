package daemon

// Recreating a removed worktree and moving off a merged/closed GitHub PR onto a fresh branch on resume
// (bin/mesh's ensure_worktree, finished_pr, leave_finished_pr; design §8).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter"
)

var githubRemoteRE = regexp.MustCompile(`github\.com[:/]([^/\s]+/[^/\s]+?)(?:\.git)?$`)

func (d *Daemon) runGH(ctx context.Context, cwd string, args ...string) ([]byte, error) {
	if d.rt.cfg.Usage != nil && d.rt.cfg.Usage.RunGH != nil {
		return d.rt.cfg.Usage.RunGH(ctx, cwd, args...)
	}
	if testing.Testing() {
		return nil, errors.New("gh disabled in tests without Usage.RunGH")
	}
	bin, err := exec.LookPath("gh")
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, bin, args...)
	cmd.Dir = cwd
	return cmd.Output()
}

type preparedWorktree struct {
	recreated       bool
	createdBranch   string // new branch created by ensureWorktree (to clean up on failure)
	movedFromBranch string // old branch before leaveFinishedPR (to restore on failure)
	movedToBranch   string
	newBranch       string
	newBaseCommit   string
	taskNote        string
	note            string
	leftPR          string
}

func (p *preparedWorktree) rollback(ctx context.Context, repo, worktree string) {
	if p == nil {
		return
	}
	if p.movedToBranch != "" && p.movedFromBranch != "" {
		_, _ = git(ctx, worktree, "checkout", p.movedFromBranch)
		_, _ = git(ctx, repo, "branch", "-D", p.movedToBranch)
	}
	if p.recreated && worktree != "" {
		_, _ = git(ctx, repo, "worktree", "remove", "--force", worktree)
		_ = os.RemoveAll(worktree)
		if p.createdBranch != "" {
			_, _ = git(ctx, repo, "branch", "-D", p.createdBranch)
		}
	}
}

func worktreeSlug(a *v1.Agent) string {
	prefix := "mesh/" + a.RunId + "/"
	if s, ok := strings.CutPrefix(a.Branch, prefix); ok && slugRE.MatchString(s) {
		return s
	}
	if a.Repo != "" {
		base := filepath.Base(a.Repo) + "-"
		if s, ok := strings.CutPrefix(a.Name, base); ok && slugRE.MatchString(s) {
			return s
		}
	}
	s := strings.ToLower(a.Name)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 40 {
		out = strings.TrimRight(out[:40], "-")
	}
	if out == "" {
		return "work"
	}
	return out
}

// ensureWorktree makes sure a change worker's worktree directory exists on disk before resume, recreating it
// from its recorded branch (or base commit when StopRun deleted an untouched branch). Returns whether a new
// worktree (and optionally branch) was created so the caller can clean up if the resume fails.
func ensureWorktree(ctx context.Context, repo, worktree, branch, baseCommit string) (recreated bool, createdBranch string, err error) {
	if worktree == "" || repo == "" {
		return false, "", nil
	}
	if _, serr := os.Stat(worktree); serr == nil {
		return false, "", nil
	} else if !os.IsNotExist(serr) {
		return false, "", serr
	}
	if _, rerr := os.Stat(repo); rerr != nil {
		return false, "", fmt.Errorf("repo %s is missing: %w", repo, rerr)
	}
	if err := os.MkdirAll(filepath.Dir(worktree), 0o700); err != nil {
		return false, "", err
	}
	// Clean up stale administrative state if the directory was removed outside `git worktree remove`.
	_, _ = git(ctx, repo, "worktree", "prune")
	if branch != "" {
		if _, berr := git(ctx, repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); berr == nil {
			if _, aerr := git(ctx, repo, "worktree", "add", worktree, branch); aerr != nil {
				return false, "", aerr
			}
			return true, "", nil
		}
	}
	startRef := strings.TrimSpace(baseCommit)
	if startRef == "" {
		startRef = defaultBase(ctx, repo)
	}
	if branch != "" {
		if _, aerr := git(ctx, repo, "worktree", "add", "-b", branch, worktree, startRef); aerr != nil {
			return false, "", aerr
		}
		return true, branch, nil
	}
	if _, aerr := git(ctx, repo, "worktree", "add", "--detach", worktree, startRef); aerr != nil {
		return false, "", aerr
	}
	return true, "", nil
}

// prepareWorktreeForTask ensures a change worker's worktree exists and, when `changeTask` is true and its
// branch's GitHub PR is already merged or closed (and none is open), moves it onto a fresh
// `mesh/<targetRunID>/<slug>` branch. Must run under d.worktreeLock(a.Worktree) and OUTSIDE SQLite write locks.
func (d *Daemon) prepareWorktreeForTask(ctx context.Context, a *v1.Agent, targetRunID string, changeTask bool) (*preparedWorktree, error) {
	if adapter.ReadOnly(a.Role) || a.Role == v1.AgentRole_AGENT_ROLE_ROOT || a.Worktree == "" || a.Repo == "" {
		return &preparedWorktree{}, nil
	}
	if targetRunID == "" {
		targetRunID = a.RunId
	}
	recreated, createdBranch, err := ensureWorktree(ctx, a.Repo, a.Worktree, a.Branch, a.BaseCommit)
	if err != nil {
		return nil, err
	}
	prep := &preparedWorktree{
		recreated:     recreated,
		createdBranch: createdBranch,
	}
	if recreated {
		prep.note = fmt.Sprintf("recreated worktree %s", a.Worktree)
		d.includeInto(ctx, a.Repo, a.Worktree)
	}
	if !changeTask {
		return prep, nil
	}
	branch := a.Branch
	if branch == "" {
		if cur, berr := git(ctx, a.Worktree, "symbolic-ref", "--quiet", "--short", "HEAD"); berr == nil {
			branch = cur
		}
	}
	if branch == "" || branch == "HEAD" {
		return prep, nil
	}
	remotes := githubRepos(ctx, a.Repo)
	if len(remotes) == 0 {
		return prep, nil
	}
	remote := prRemote(ctx, a.Repo, branch, remotes)
	if remote == "" {
		return prep, nil
	}
	state, num, prURL, headOID := d.finishedPR(ctx, a.Worktree, branch, a.BaseCommit, remotes[remote])
	if state == "" {
		return prep, nil
	}
	clean := worktreeClean(ctx, a.Worktree)
	if !clean && state == "MERGED" {
		note := fmt.Sprintf("PR #%d (%s) was merged, but %s has local changes; stayed on %s", num, prURL, a.Worktree, branch)
		prep.taskNote = "Note: " + note + "."
		if prep.note != "" {
			prep.note += "; " + note
		} else {
			prep.note = note
		}
		return prep, nil
	}

	newBranch := nextWorktreeBranch(ctx, a.Repo, remote, targetRunID, worktreeSlug(a))
	startRef := "HEAD"
	baseDesc := branch
	if state == "MERGED" {
		def := remoteDefault(ctx, a.Repo, remote)
		if def == "" {
			def = defaultBase(ctx, a.Repo)
		}
		candidate := "refs/remotes/" + remote + "/" + def
		if def != "" {
			if _, rerr := git(ctx, a.Repo, "rev-parse", "--verify", "--quiet", candidate); rerr == nil {
				startRef = candidate
				baseDesc = remote + "/" + def
			} else if _, rerr := git(ctx, a.Repo, "rev-parse", "--verify", "--quiet", def); rerr == nil {
				startRef = def
				baseDesc = def
			}
		}
	} else if clean && headOID != "" {
		if headSHA, herr := git(ctx, a.Worktree, "rev-parse", "--verify", "HEAD"); herr == nil && headSHA != headOID {
			if _, ferr := git(ctx, a.Worktree, "fetch", remote, fmt.Sprintf("refs/pull/%d/head", num)); ferr == nil {
				startRef = "FETCH_HEAD"
				baseDesc = fmt.Sprintf("PR #%d head", num)
			}
		}
	}
	newBase, _ := git(ctx, a.Worktree, "rev-parse", "--verify", startRef)
	var cerr error
	if startRef == "HEAD" {
		_, cerr = git(ctx, a.Worktree, "checkout", "-b", newBranch)
	} else {
		_, cerr = git(ctx, a.Worktree, "checkout", "-b", newBranch, startRef)
	}
	if cerr != nil {
		return prep, nil
	}
	prep.movedFromBranch = branch
	prep.movedToBranch = newBranch
	prep.newBranch = newBranch
	prep.newBaseCommit = newBase
	prep.leftPR = prURL
	lowerState := strings.ToLower(state)
	msg := fmt.Sprintf("left %s PR #%d (%s): moved branch %s -> %s", lowerState, num, prURL, branch, newBranch)
	if prep.note != "" {
		prep.note += "; " + msg
	} else {
		prep.note = msg
	}
	prep.taskNote = fmt.Sprintf("Note: PR #%d (%s) for branch %s was %s; your worktree was moved onto new branch %s (from %s).",
		num, prURL, branch, lowerState, newBranch, baseDesc)
	return prep, nil
}

func githubRepos(ctx context.Context, repo string) map[string]string {
	remotes := map[string]string{}
	if cfgOut, err := git(ctx, repo, "config", "--get-regexp", `^remote\.[^.]+\.url$`); err == nil && cfgOut != "" {
		for _, line := range strings.Split(cfgOut, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			name := strings.TrimSuffix(strings.TrimPrefix(fields[0], "remote."), ".url")
			if m := githubRemoteRE.FindStringSubmatch(fields[1]); m != nil && name != "" {
				remotes[name] = m[1]
			}
		}
	}
	if out, err := git(ctx, repo, "remote", "-v"); err == nil && out != "" {
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			if len(fields) >= 3 && fields[2] != "(fetch)" {
				continue
			}
			if m := githubRemoteRE.FindStringSubmatch(fields[1]); m != nil {
				remotes[fields[0]] = m[1]
			}
		}
	}
	return remotes
}

func prRemote(ctx context.Context, repo, branch string, remotes map[string]string) string {
	if r, err := git(ctx, repo, "config", "--get", "branch."+branch+".remote"); err == nil {
		r = strings.TrimSpace(r)
		if _, ok := remotes[r]; ok {
			return r
		}
	}
	if _, ok := remotes["origin"]; ok {
		return "origin"
	}
	names := make([]string, 0, len(remotes))
	for k := range remotes {
		names = append(names, k)
	}
	sort.Strings(names)
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

func remoteDefault(ctx context.Context, repo, remote string) string {
	_, _ = git(ctx, repo, "fetch", "--prune", remote)
	if ref, err := git(ctx, repo, "symbolic-ref", "--quiet", "refs/remotes/"+remote+"/HEAD"); err == nil {
		if b, ok := strings.CutPrefix(ref, "refs/remotes/"+remote+"/"); ok && b != "" {
			return b
		}
	}
	for _, cand := range []string{"main", "master"} {
		if _, err := git(ctx, repo, "show-ref", "--verify", "--quiet", "refs/remotes/"+remote+"/"+cand); err == nil {
			return cand
		}
	}
	return ""
}

type ghPR struct {
	Number     int    `json:"number"`
	URL        string `json:"url"`
	State      string `json:"state"`
	HeadRefOid string `json:"headRefOid"`
}

func gitIsAncestor(ctx context.Context, dir, ancestor, descendant string) bool {
	if ancestor == "" || descendant == "" {
		return false
	}
	_, err := gitRO(ctx, dir, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

// prOnBranch: whether a PR's head commit belongs to the branch (and not to the base before it). HEAD counts only
// where it is the branch (`atHead`: a worktree of the branch); a main checkout's HEAD is another branch.
func prOnBranch(ctx context.Context, worktree, oid, branch, baseCommit string, atHead bool) bool {
	if oid == "" {
		return true
	}
	if _, err := gitRO(ctx, worktree, "cat-file", "-e", oid+"^{commit}"); err != nil {
		return true // not in the local repo to compare (e.g. squash-merged PR head never fetched)
	}
	onBranch := gitIsAncestor(ctx, worktree, oid, "refs/heads/"+branch) || atHead && gitIsAncestor(ctx, worktree, oid, "HEAD")
	return onBranch && !(baseCommit != "" && gitIsAncestor(ctx, worktree, oid, baseCommit))
}

func (d *Daemon) finishedPR(ctx context.Context, worktree, branch, baseCommit, repoSlug string) (state string, number int, url string, headOID string) {
	args := []string{"pr", "list"}
	if repoSlug != "" {
		args = append(args, "--repo", repoSlug)
	}
	args = append(args, "--head", branch, "--state", "all", "--json", "number,url,state,headRefOid", "--limit", "20")
	out, err := d.runGH(ctx, worktree, args...)
	if err != nil || len(out) == 0 {
		return "", 0, "", ""
	}
	var prs []ghPR
	if json.Unmarshal(out, &prs) != nil || len(prs) == 0 {
		return "", 0, "", ""
	}
	var relevant []ghPR
	for _, pr := range prs {
		st := strings.ToUpper(strings.TrimSpace(pr.State))
		if st == "OPEN" {
			return "", 0, "", ""
		}
		if !prOnBranch(ctx, worktree, pr.HeadRefOid, branch, baseCommit, true) {
			continue
		}
		if st == "MERGED" || st == "CLOSED" {
			pr.State = st
			relevant = append(relevant, pr)
		}
	}
	if len(relevant) == 0 {
		return "", 0, "", ""
	}
	sort.Slice(relevant, func(i, j int) bool { return relevant[i].Number > relevant[j].Number })
	best := relevant[0]
	return best.State, best.Number, best.URL, best.HeadRefOid
}

func worktreeClean(ctx context.Context, worktree string) bool {
	out, err := git(ctx, worktree, "status", "--porcelain")
	return err == nil && strings.TrimSpace(out) == ""
}

func nextWorktreeBranch(ctx context.Context, repo, remote, runID, slug string) string {
	base := "mesh/" + runID + "/" + slug
	exists := func(b string) bool {
		if _, err := git(ctx, repo, "show-ref", "--verify", "--quiet", "refs/heads/"+b); err == nil {
			return true
		}
		if remote != "" {
			if _, err := git(ctx, repo, "show-ref", "--verify", "--quiet", "refs/remotes/"+remote+"/"+b); err == nil {
				return true
			}
		}
		return false
	}
	if !exists(base) {
		return base
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		if !exists(cand) {
			return cand
		}
	}
}
