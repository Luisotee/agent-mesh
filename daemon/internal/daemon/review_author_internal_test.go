package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
	"github.com/Luisotee/agent-mesh/daemon/internal/testutil/shortdir"
)

func TestAuthorLast(t *testing.T) {
	small := []string{"opencode", "agy", "claude"}
	large := []string{"agy", "claude", "opencode"}
	cases := []struct {
		name   string
		chain  []string
		author string
		want   []string
	}{
		{"small, claude author", small, "claude", []string{"opencode", "agy", "claude"}},
		{"small, agy author", small, "agy", []string{"opencode", "claude", "agy"}},
		{"small, opencode author goes after the others", small, "opencode", []string{"agy", "claude", "opencode"}},
		{"small, author not in the slot", small, "copilot", small},
		{"large, claude author", large, "claude", []string{"agy", "claude", "opencode"}},
		{"large, agy author", large, "agy", []string{"claude", "agy", "opencode"}},
		{"large, opencode author", large, "opencode", []string{"agy", "claude", "opencode"}},
		{"large, author not in the slot", large, "copilot", large},
		{"no author known", large, "", large},
		{"only OpenCode besides the author", []string{"opencode", "agy"}, "agy", []string{"opencode", "agy"}},
		{"no opencode in the slot", []string{"claude", "agy"}, "claude", []string{"agy", "claude"}},
	}
	for _, c := range cases {
		in := append([]string(nil), c.chain...)
		if got := authorLast(in, c.author); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
		if !reflect.DeepEqual(in, c.chain) {
			t.Errorf("%s: the input chain was changed to %v", c.name, in)
		}
	}
}

// The author of a PR is the worker on its head branch, or, when the worker opened it from a branch it moved to, the
// one whose worktree has that branch checked out.
func TestPRAuthorKindFindsAWorkerOnAMovedBranch(t *testing.T) {
	dir := shortdir.New(t)
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	repo, wt := filepath.Join(dir, "repo"), filepath.Join(dir, "wt")
	for _, args := range [][]string{{"init", "-q", "-b", "main", repo}, {"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "x"},
		{"-C", repo, "worktree", "add", "-q", "-b", "moved-on", wt}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	ctx := context.Background()
	if err := st.Write(ctx, nil, "", func(tx *store.Tx) error {
		if err := tx.PutRun(&v1.Run{Id: "r1", Status: v1.RunStatus_RUN_STATUS_OPEN}); err != nil {
			return err
		}
		return tx.PutAgent(&v1.Agent{Id: "a1", RunId: "r1", Name: "w", Kind: "agy", Role: v1.AgentRole_AGENT_ROLE_WORKER, Repo: repo,
			Branch: "mesh/r1/w", Worktree: wt})
	}); err != nil {
		t.Fatal(err)
	}
	r := st.Read(ctx)
	if got := prAuthorKind(ctx, r, repo, "mesh/r1/w"); got != "agy" {
		t.Errorf("registered branch: %q", got)
	}
	if got := prAuthorKind(ctx, r, repo, "moved-on"); got != "agy" {
		t.Errorf("moved branch: %q, want the worker whose worktree is on it", got)
	}
	if got := prAuthorKind(ctx, r, repo, "somebody-elses"); got != "" {
		t.Errorf("another branch: %q", got)
	}
	// The repo is the same directory through a symlink: the author is still found, by either branch.
	link := filepath.Join(dir, "repo-link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	for _, head := range []string{"mesh/r1/w", "moved-on"} {
		if got := prAuthorKind(ctx, r, link, head); got != "agy" {
			t.Errorf("repo through a symlink, %s: %q", head, got)
		}
	}
}
