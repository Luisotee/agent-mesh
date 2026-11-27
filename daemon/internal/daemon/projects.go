package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Luisotee/agent-mesh/daemon/internal/auth"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

const (
	projectBranches = 40  // branches listed for a project
	projectLimit    = 300 // projects listed
	projectPRs      = "30"
	projectGit      = 8 // repos asked of git at once
)

// ListProjects lists the repos work can start in: the git folders directly under the `repos.dir` folders, and every repo
// an agent has worked in, each with its default branch and branches (from git, no network). With `repo` set it is that
// one of those, fetched from its origin first, with its open pull requests read from GitHub (the workspace dialog
// starts from one): a read-only device gets the refs last fetched and no pull requests, and only for a listed repo.
func (d *Daemon) ListProjects(ctx context.Context, req *connect.Request[v1.ListProjectsRequest]) (*connect.Response[v1.ListProjectsResponse], error) {
	actor := auth.ActorFrom(ctx)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	paths, used, inDir, err := d.projectPaths(ctx)
	if err != nil {
		return nil, err
	}
	// With `repo`, that one project, by the path the list gives it when it is listed (git's resolved top may differ
	// through a symlink). A read-only device reads only listed projects, and updates nothing: it gets the refs last
	// fetched and no pull requests (they come from GitHub). A device (the user) may name any repo on the host, and a
	// root too: they get fresh refs, so a branch or a PR's head they pick is there to start from (a failed fetch is not
	// an error: the refs last fetched are listed).
	fetch := false
	if repo := req.Msg.Repo; repo != "" {
		top, err := d.repoTop(ctx, repo)
		if err != nil {
			return nil, err
		}
		readOnly := actor.GetClass() == v1.ActorClass_ACTOR_CLASS_READ_ONLY_DEVICE
		if i := slices.IndexFunc(paths, func(p string) bool { return sameDir(p, top) }); i >= 0 {
			top = paths[i]
		} else if readOnly {
			return nil, notFound("%s is not one of the projects the host lists (`mesh projects`)", repo)
		}
		paths = []string{top}
		if fetch = !readOnly; fetch {
			fctx, stop := context.WithTimeout(ctx, 15*time.Second)
			_, _ = git(fctx, top, "fetch", "--quiet", "origin")
			stop()
		}
	}
	seen := map[string]bool{}
	var uniq []string
	for _, p := range paths {
		if !seen[p] {
			seen[p] = true
			uniq = append(uniq, p)
		}
	}
	sort.Slice(uniq, func(i, j int) bool {
		a, b := used[uniq[i]], used[uniq[j]]
		return a.After(b) || (a.Equal(b) && strings.ToLower(filepath.Base(uniq[i])) < strings.ToLower(filepath.Base(uniq[j])))
	})
	if len(uniq) > projectLimit {
		uniq = uniq[:projectLimit]
	}
	out := make([]*v1.Project, len(uniq))
	sem := make(chan struct{}, projectGit)
	var wg sync.WaitGroup
	for i, p := range uniq {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			pr := &v1.Project{Path: p, Name: filepath.Base(p), InReposDir: inDir[p]}
			if t, ok := used[p]; ok {
				pr.LastUsed = timestamppb.New(t)
			}
			pr.DefaultBranch, pr.Branches = projectBranchesOf(ctx, p)
			if fetch {
				pr.PullRequests = d.projectPullRequests(ctx, p)
			}
			out[i] = pr
		}()
	}
	wg.Wait()
	// A path that stopped being a repo (git found no branches and no default) is still listed: the folder is there.
	return connect.NewResponse(&v1.ListProjectsResponse{Projects: out}), nil
}

// projectPaths are the repos work can start in (the list `ListProjects` shows, before it is sorted and cut): the git
// folders under the `repos.dir` folders (`inDir`) and every repo an agent worked in, with when each was last used.
func (d *Daemon) projectPaths(ctx context.Context) (paths []string, used map[string]time.Time, inDir map[string]bool, err error) {
	r := d.store.Read(ctx)
	used = map[string]time.Time{}
	agents, err := r.AllAgents()
	if err != nil {
		return nil, nil, nil, err
	}
	for _, a := range agents {
		if a.Repo != "" {
			if t := a.CreatedAt.AsTime(); t.After(used[filepath.Clean(a.Repo)]) {
				used[filepath.Clean(a.Repo)] = t
			}
		}
	}
	inDir = map[string]bool{}
	dirs, err := d.Setting(r, "repos.dir")
	if err != nil {
		return nil, nil, nil, err
	}
	for _, e := range dirs.([]any) {
		for _, p := range gitFolders(expandHome(e.(string))) {
			inDir[p] = true
			paths = append(paths, p)
		}
	}
	for p := range used {
		paths = append(paths, p)
	}
	return paths, used, inDir, nil
}

// gitFolders are the folders directly under dir that are a git repo's top (a `.git` folder, or a file for a worktree).
func gitFolders(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), ".git")); err == nil {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// projectBranchesOf is a repo's default branch (the remote's HEAD as last fetched) and its newest branches by commit
// date, as refs a worktree can start from: a local branch by its name, the remote's as origin/<name>.
func projectBranchesOf(ctx context.Context, dir string) (string, []string) {
	def := ""
	if ref, err := git(ctx, dir, "symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"); err == nil {
		def = strings.TrimPrefix(ref, "origin/")
	}
	out, err := git(ctx, dir, "for-each-ref", "--sort=-committerdate", "--format=%(refname:short)", "refs/heads", "refs/remotes/origin")
	if err != nil {
		return def, nil
	}
	seen := map[string]bool{}
	var branches []string
	for _, line := range strings.Split(out, "\n") {
		b := strings.TrimSpace(line)
		if b == "" || b == "origin" || b == "origin/HEAD" || seen[b] {
			continue
		}
		seen[b] = true
		branches = append(branches, b)
		if len(branches) == projectBranches {
			break
		}
	}
	return def, branches
}

// projectPullRequests are a repo's open pull requests as GitHub says (`gh` infers the repo from the remote); none when
// `gh` can't be run or says nothing usable, so a workspace can still start from a branch.
func (d *Daemon) projectPullRequests(ctx context.Context, dir string) []*v1.ProjectPullRequest {
	out, err := d.runGH(ctx, dir, "pr", "list", "--state", "open", "--json", "number,title,headRefName,url,isDraft,isCrossRepository", "--limit", projectPRs)
	if err != nil {
		d.log.Debug("project pull requests", "repo", dir, "err", err)
		return nil
	}
	var rows []struct {
		Number      int32  `json:"number"`
		Title       string `json:"title"`
		HeadRefName string `json:"headRefName"`
		URL         string `json:"url"`
		IsDraft     bool   `json:"isDraft"`
		CrossRepo   bool   `json:"isCrossRepository"`
	}
	if json.Unmarshal(out, &rows) != nil {
		return nil
	}
	prs := make([]*v1.ProjectPullRequest, 0, len(rows))
	for _, p := range rows {
		prs = append(prs, &v1.ProjectPullRequest{Number: p.Number, Title: p.Title, HeadRef: p.HeadRefName, Url: p.URL, Draft: p.IsDraft, CrossRepository: p.CrossRepo})
	}
	return prs
}
