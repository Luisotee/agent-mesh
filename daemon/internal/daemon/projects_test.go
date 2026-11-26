package daemon_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

func TestListProjectsFindsTheGitFoldersOfReposDirAndTheReposAgentsUsed(t *testing.T) {
	w := startPR(t)
	root := t.TempDir()
	for _, name := range []string{"shop", "api"} {
		runGit(t, "", "init", "--initial-branch=main", filepath.Join(root, name))
	}
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o755); err != nil { // a folder that is no repo
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".hidden", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	v, _ := structpb.NewValue([]any{root})
	if _, err := w.device.Mesh.SetSetting(context.Background(), connect.NewRequest(&v1.SetSettingRequest{Meta: meta(""), Key: "repos.dir", Value: v})); err != nil {
		t.Fatal(err)
	}

	res, err := w.device.Mesh.ListProjects(context.Background(), connect.NewRequest(&v1.ListProjectsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*v1.Project{}
	for _, p := range res.Msg.Projects {
		byName[p.Name] = p
	}
	// The repo the seeded worker uses comes first (it was used), then repos.dir's by name.
	if len(res.Msg.Projects) != 3 || res.Msg.Projects[0].Path != w.repo {
		t.Fatalf("projects %v", res.Msg.Projects)
	}
	if p := byName["repo"]; p.InReposDir || p.LastUsed == nil || p.DefaultBranch != "main" || len(p.Branches) == 0 || p.Branches[0] == "" {
		t.Errorf("the used repo: %v", p)
	}
	if p := byName["shop"]; !p.InReposDir || p.LastUsed != nil || p.Path != filepath.Join(root, "shop") {
		t.Errorf("a repos.dir repo: %v", p)
	}
	if byName["notes"] != nil || byName[".hidden"] != nil {
		t.Errorf("a plain or hidden folder was listed: %v", res.Msg.Projects)
	}
	for _, p := range res.Msg.Projects {
		if len(p.PullRequests) != 0 {
			t.Errorf("the full list reads no GitHub: %v", p)
		}
	}
}

func TestListProjectsForOneRepoAddsItsOpenPullRequestsAndBranchesNewestFirst(t *testing.T) {
	w := startPR(t)
	runGit(t, w.repo, "checkout", "-q", "-b", "older")
	runGit(t, w.repo, "checkout", "-q", "main")
	w.setList()
	w.mu.Lock()
	w.list = nil
	w.mu.Unlock()
	// gh's answer for `pr list --state open`.
	prs, _ := json.Marshal([]map[string]any{{"number": 7, "title": "Fix the thing", "headRefName": "mesh/x/fix", "url": "https://github.com/acme/demo/pull/7", "isDraft": true}})
	w.setRaw(prs)

	res, err := w.device.Mesh.ListProjects(context.Background(), connect.NewRequest(&v1.ListProjectsRequest{Repo: w.repo}))
	if err != nil || len(res.Msg.Projects) != 1 {
		t.Fatalf("%v, %v", res, err)
	}
	p := res.Msg.Projects[0]
	if len(p.PullRequests) != 1 || p.PullRequests[0].Number != 7 || p.PullRequests[0].HeadRef != "mesh/x/fix" || !p.PullRequests[0].Draft || p.PullRequests[0].Title != "Fix the thing" {
		t.Errorf("pull requests: %v", p.PullRequests)
	}
	seen := map[string]int{}
	for _, b := range p.Branches {
		seen[b]++
	}
	if seen["main"] != 1 || seen["older"] != 1 || seen["HEAD"] != 0 || seen["origin"] != 0 {
		t.Errorf("branches %v: each once, no origin/HEAD", p.Branches)
	}

	// A repo that isn't one is refused, and a GitHub that fails leaves the project without PRs.
	if _, err := w.device.Mesh.ListProjects(context.Background(), connect.NewRequest(&v1.ListProjectsRequest{Repo: t.TempDir()})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("not a repo: %v", err)
	}
	w.mu.Lock()
	w.fail = os.ErrDeadlineExceeded
	w.mu.Unlock()
	res, err = w.device.Mesh.ListProjects(context.Background(), connect.NewRequest(&v1.ListProjectsRequest{Repo: w.repo}))
	if err != nil || len(res.Msg.Projects[0].PullRequests) != 0 {
		t.Errorf("gh down: %v, %v", res, err)
	}
}
