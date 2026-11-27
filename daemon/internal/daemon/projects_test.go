package daemon_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
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
	prs, _ := json.Marshal([]map[string]any{{"number": 7, "title": "Fix the thing", "headRefName": "mesh/x/fix", "url": "https://github.com/acme/demo/pull/7", "isDraft": true},
		{"number": 8, "title": "From a fork", "headRefName": "main", "url": "https://github.com/acme/demo/pull/8", "isCrossRepository": true}})
	w.setRaw(prs)

	res, err := w.device.Mesh.ListProjects(context.Background(), connect.NewRequest(&v1.ListProjectsRequest{Repo: w.repo}))
	if err != nil || len(res.Msg.Projects) != 1 {
		t.Fatalf("%v, %v", res, err)
	}
	p := res.Msg.Projects[0]
	if len(p.PullRequests) != 2 || p.PullRequests[1].Number != 8 || !p.PullRequests[1].CrossRepository || p.PullRequests[0].CrossRepository {
		t.Errorf("a fork's pull request must say so: %v", p.PullRequests)
	}
	if p.PullRequests[0].Number != 7 || p.PullRequests[0].HeadRef != "mesh/x/fix" || !p.PullRequests[0].Draft || p.PullRequests[0].Title != "Fix the thing" {
		t.Errorf("pull requests: %v", p.PullRequests)
	}
	seen := map[string]int{}
	for _, b := range p.Branches {
		seen[b]++
	}
	// A local branch by its name, the remote's as origin/<name>: refs a worktree can start from.
	if seen["main"] != 1 || seen["older"] != 1 || seen["origin/main"] != 1 || seen["origin/HEAD"] != 0 || seen["origin"] != 0 {
		t.Errorf("branches %v: each once, the remote's as origin/<name>, no origin/HEAD", p.Branches)
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

func TestListProjectsForOneRepoFetchesForTheUserButNotForAReadOnlyDevice(t *testing.T) {
	w := startPR(t)
	bare := filepath.Join(filepath.Dir(w.repo), "origin.git")
	w.setRaw([]byte("[]"))
	runGit(t, bare, "branch", "fresh", "main") // a branch pushed by someone else: only a fetch brings it
	ctx := context.Background()
	branches := func(c interface {
		ListProjects(context.Context, *connect.Request[v1.ListProjectsRequest]) (*connect.Response[v1.ListProjectsResponse], error)
	}) ([]string, int) {
		t.Helper()
		n := int(w.calls.Load())
		res, err := c.ListProjects(ctx, connect.NewRequest(&v1.ListProjectsRequest{Repo: w.repo}))
		if err != nil || len(res.Msg.Projects) != 1 {
			t.Fatalf("%v, %v", res, err)
		}
		return res.Msg.Projects[0].Branches, int(w.calls.Load()) - n
	}

	ro := readOnlyDevice(t, w)
	got, gh := branches(ro)
	if slices.Contains(got, "origin/fresh") || gh != 0 {
		t.Errorf("a read-only device fetched or asked GitHub: %v, %d gh calls", got, gh)
	}
	if got, gh := branches(w.device.Mesh); !slices.Contains(got, "origin/fresh") || gh == 0 {
		t.Errorf("the user's device: %v, %d gh calls", got, gh)
	}
}

func TestListProjectsForOneRepoIsRestrictedToListedReposOnlyForAReadOnlyDevice(t *testing.T) {
	w := startPR(t)
	other := t.TempDir() // a git repo that is in no repos.dir and that no agent used
	runGit(t, "", "init", "--initial-branch=main", other)
	ctx := context.Background()
	ro := readOnlyDevice(t, w)
	_, err := ro.ListProjects(ctx, connect.NewRequest(&v1.ListProjectsRequest{Repo: other}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a read-only device naming an unlisted repo: %v, want not found", err)
	}
	// The user's device may read any repo it names (a freshly cloned one), and so may a root.
	for name, c := range map[string]agentmeshv1connect.MeshServiceClient{"device": w.device.Mesh, "root": w.as(w.rootTok)} {
		res, err := c.ListProjects(ctx, connect.NewRequest(&v1.ListProjectsRequest{Repo: other}))
		if err != nil || len(res.Msg.Projects) != 1 || res.Msg.Projects[0].Path == "" {
			t.Errorf("the %s naming an unlisted repo: %v, %v", name, res, err)
		}
	}
	// A folder inside a listed repo is that repo, by the path the list gives.
	sub := filepath.Join(w.repo, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := ro.ListProjects(ctx, connect.NewRequest(&v1.ListProjectsRequest{Repo: sub}))
	if err != nil || len(res.Msg.Projects) != 1 || res.Msg.Projects[0].Path != w.repo {
		t.Errorf("a subfolder of a listed repo: %v, %v", res, err)
	}
}

func TestListProjectsForOneRepoKeepsTheListedPathThroughASymlink(t *testing.T) {
	w := startPR(t)
	real := t.TempDir()
	runGit(t, "", "init", "--initial-branch=main", filepath.Join(real, "app"))
	link := filepath.Join(t.TempDir(), "projects")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	v, _ := structpb.NewValue([]any{link})
	ctx := context.Background()
	if _, err := w.device.Mesh.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{Meta: meta(""), Key: "repos.dir", Value: v})); err != nil {
		t.Fatal(err)
	}
	listed := filepath.Join(link, "app")
	res, err := w.device.Mesh.ListProjects(ctx, connect.NewRequest(&v1.ListProjectsRequest{Repo: listed}))
	if err != nil || len(res.Msg.Projects) != 1 || res.Msg.Projects[0].Path != listed || !res.Msg.Projects[0].InReposDir {
		t.Errorf("got %v, %v: want the path the list gives (%s), in repos.dir", res, err, listed)
	}
}

// readOnlyDevice is a client of a read-only paired device.
func readOnlyDevice(t *testing.T, w *prWorld) agentmeshv1connect.MeshServiceClient {
	t.Helper()
	ctx := context.Background()
	cp, err := w.device.Mesh.CreatePairing(ctx, connect.NewRequest(&v1.CreatePairingRequest{DirectUrl: testDirectURL, Meta: meta("pair-ro"), Name: "ro", ReadOnly: true}))
	if err != nil {
		t.Fatal(err)
	}
	red, err := pairingClient(w.state, "").RedeemPairing(ctx, connect.NewRequest(&v1.RedeemPairingRequest{Token: cp.Msg.Token}))
	if err != nil {
		t.Fatal(err)
	}
	return w.as(red.Msg.DeviceToken)
}
