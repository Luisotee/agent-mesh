package daemon_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

func seedConflictRun(t *testing.T, h *harness, runID string, agents ...*v1.Agent) {
	t.Helper()
	err := h.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		if err := tx.PutRun(&v1.Run{Id: runID, Title: "conflicts", Status: v1.RunStatus_RUN_STATUS_OPEN, CreatedAt: timestamppb.Now()}); err != nil {
			return err
		}
		for _, a := range agents {
			if err := tx.PutAgent(a); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func addWorktreeBranch(t *testing.T, repo, runID, slug, base string) (wt, branch, baseCommit string) {
	t.Helper()
	if base == "" {
		base = "HEAD"
	}
	baseCommit = gitIn(t, repo, "rev-parse", "--verify", base+"^{commit}")
	branch = "mesh/" + runID + "/" + slug
	wt = filepath.Join(t.TempDir(), slug)
	gitIn(t, repo, "worktree", "add", "-b", branch, wt, baseCommit)
	return wt, branch, baseCommit
}

func commitFileIn(t *testing.T, dir, rel, content, msg string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", rel)
	gitIn(t, dir, "commit", "-q", "-m", msg)
}

func branchSlug(b string) string {
	i := strings.LastIndex(b, "/")
	if i < 0 {
		return b
	}
	return b[i+1:]
}

func pairKey(a, b string) string {
	sa, sb := branchSlug(a), branchSlug(b)
	if sa > sb {
		sa, sb = sb, sa
	}
	return sa + "|" + sb
}

func TestConflictsFindsOverlappingBranchesAndSkipsStackedOnes(t *testing.T) {
	h := start(t, "")
	repo := gitRepo(t)
	commitFileIn(t, repo, "a.txt", "1\n", "base")

	wt1, b1, base1 := addWorktreeBranch(t, repo, "r1", "b1", "")
	commitFileIn(t, wt1, "a.txt", "2\n", "b1")
	wt2, b2, base2 := addWorktreeBranch(t, repo, "r1", "b2", "")
	commitFileIn(t, wt2, "a.txt", "3\n", "b2")
	wt3, b3, base3 := addWorktreeBranch(t, repo, "r1", "b3", "")
	commitFileIn(t, wt3, "c.txt", "x\n", "b3")
	wt4, b4, base4 := addWorktreeBranch(t, repo, "r1", "b4", b1) // stacked on b1
	commitFileIn(t, wt4, "a.txt", "4\n", "b4")
	wt5, b5, base5 := addWorktreeBranch(t, repo, "r1", "b5", "") // no commits: not checked

	seedConflictRun(t, h, "r1",
		&v1.Agent{Id: "a1", RunId: "r1", Name: "repo-b1", Role: v1.AgentRole_AGENT_ROLE_WORKER, Repo: repo, Worktree: wt1, Branch: b1, BaseCommit: base1, CreatedAt: timestamppb.Now()},
		&v1.Agent{Id: "a2", RunId: "r1", Name: "repo-b2", Role: v1.AgentRole_AGENT_ROLE_WORKER, Repo: repo, Worktree: wt2, Branch: b2, BaseCommit: base2, CreatedAt: timestamppb.Now()},
		&v1.Agent{Id: "a3", RunId: "r1", Name: "repo-b3", Role: v1.AgentRole_AGENT_ROLE_WORKER, Repo: repo, Worktree: wt3, Branch: b3, BaseCommit: base3, CreatedAt: timestamppb.Now()},
		&v1.Agent{Id: "a4", RunId: "r1", Name: "repo-b4", Role: v1.AgentRole_AGENT_ROLE_WORKER, Repo: repo, Worktree: wt4, Branch: b4, BaseCommit: base4, CreatedAt: timestamppb.Now()},
		&v1.Agent{Id: "a5", RunId: "r1", Name: "repo-b5", Role: v1.AgentRole_AGENT_ROLE_WORKER, Repo: repo, Worktree: wt5, Branch: b5, BaseCommit: base5, CreatedAt: timestamppb.Now()},
		&v1.Agent{Id: "ap", RunId: "r1", Name: "peer-x", Role: v1.AgentRole_AGENT_ROLE_ASKER, Repo: repo, Worktree: t.TempDir(), Peer: true, CreatedAt: timestamppb.Now()},
	)

	res, err := h.device.Mesh.CheckConflicts(context.Background(), connect.NewRequest(&v1.CheckConflictsRequest{RunId: "r1"}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.CheckedBranches != 4 {
		t.Fatalf("checked = %d, want 4", res.Msg.CheckedBranches)
	}
	if len(res.Msg.Overlaps) != 0 {
		t.Fatalf("unexpected overlaps: %+v", res.Msg.Overlaps)
	}
	gotPairs := map[string][]string{}
	for _, c := range res.Msg.Conflicts {
		if c.Kind != v1.ConflictKind_CONFLICT_KIND_CONFLICT {
			t.Fatalf("expected conflict kind, got %v", c.Kind)
		}
		gotPairs[pairKey(c.Branch, c.Against)] = c.Files
	}
	if len(gotPairs) != 2 || !slices.Equal(gotPairs["b1|b2"], []string{"a.txt"}) || !slices.Equal(gotPairs["b2|b4"], []string{"a.txt"}) {
		t.Fatalf("conflicts = %+v", gotPairs)
	}
}

func TestConflictsWarnsAboutUncommittedUntrackedAndStagedRenameOverlaps(t *testing.T) {
	h := start(t, "")
	repo := gitRepo(t)
	commitFileIn(t, repo, "a.txt", "1\n", "base")
	commitFileIn(t, repo, "r.txt", "r\n", "base")

	wt1, b1, base1 := addWorktreeBranch(t, repo, "r1", "b1", "")
	os.WriteFile(filepath.Join(wt1, "a.txt"), []byte("2\n"), 0o644)   // unstaged
	os.WriteFile(filepath.Join(wt1, "new.txt"), []byte("n\n"), 0o644) // untracked

	wt2, b2, base2 := addWorktreeBranch(t, repo, "r1", "b2", "")
	commitFileIn(t, wt2, "a.txt", "3\n", "b2") // committed

	wt3, b3, base3 := addWorktreeBranch(t, repo, "r1", "b3", "")
	os.WriteFile(filepath.Join(wt3, "new.txt"), []byte("m\n"), 0o644)
	gitIn(t, wt3, "mv", "r.txt", "s.txt") // staged rename

	wt4, b4, base4 := addWorktreeBranch(t, repo, "r1", "b4", "")
	os.WriteFile(filepath.Join(wt4, "r.txt"), []byte("changed\n"), 0o644)

	wt5, b5, base5 := addWorktreeBranch(t, repo, "r1", "b5", b2) // stacked on b2
	os.WriteFile(filepath.Join(wt5, "a.txt"), []byte("5\n"), 0o644)

	seedConflictRun(t, h, "r1",
		&v1.Agent{Id: "a1", RunId: "r1", Name: "repo-b1", Repo: repo, Worktree: wt1, Branch: b1, BaseCommit: base1, CreatedAt: timestamppb.Now()},
		&v1.Agent{Id: "a2", RunId: "r1", Name: "repo-b2", Repo: repo, Worktree: wt2, Branch: b2, BaseCommit: base2, CreatedAt: timestamppb.Now()},
		&v1.Agent{Id: "a3", RunId: "r1", Name: "repo-b3", Repo: repo, Worktree: wt3, Branch: b3, BaseCommit: base3, CreatedAt: timestamppb.Now()},
		&v1.Agent{Id: "a4", RunId: "r1", Name: "repo-b4", Repo: repo, Worktree: wt4, Branch: b4, BaseCommit: base4, CreatedAt: timestamppb.Now()},
		&v1.Agent{Id: "a5", RunId: "r1", Name: "repo-b5", Repo: repo, Worktree: wt5, Branch: b5, BaseCommit: base5, CreatedAt: timestamppb.Now()},
	)

	res, err := h.device.Mesh.CheckConflicts(context.Background(), connect.NewRequest(&v1.CheckConflictsRequest{RunId: "r1"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Msg.Conflicts) != 0 || res.Msg.CheckedBranches != 1 {
		t.Fatalf("conflicts=%+v checked=%d", res.Msg.Conflicts, res.Msg.CheckedBranches)
	}
	got := map[string][]string{}
	for _, o := range res.Msg.Overlaps {
		if o.Kind != v1.ConflictKind_CONFLICT_KIND_OVERLAP {
			t.Fatalf("expected overlap kind, got %v", o.Kind)
		}
		got[pairKey(o.Branch, o.Against)] = o.Files
	}
	want := map[string][]string{
		"b1|b2": {"a.txt"},
		"b1|b3": {"new.txt"},
		"b3|b4": {"r.txt"},
		"b1|b5": {"a.txt"},
	}
	if len(got) != len(want) {
		t.Fatalf("overlaps = %+v, want %+v", got, want)
	}
	for k, wf := range want {
		if !slices.Equal(got[k], wf) {
			t.Fatalf("overlap[%s] = %v, want %v", k, got[k], wf)
		}
	}
}

func TestConflictsCountsOnlyWorkersOwnCommitsAndRebasesAndRenames(t *testing.T) {
	h := start(t, "")
	repo := gitRepo(t)
	var lines strings.Builder
	for i := 0; i < 20; i++ {
		lines.WriteString("line\n")
	}
	commitFileIn(t, repo, "a.txt", "1\n", "base")
	commitFileIn(t, repo, "r.txt", lines.String(), "base r")
	commitFileIn(t, repo, "café.txt", "1\n", "base cafe")

	wt2, b2, base2 := addWorktreeBranch(t, repo, "r1", "b2", "")
	commitFileIn(t, wt2, "a.txt", "2\n", "b2")
	wt5, b5, base5 := addWorktreeBranch(t, repo, "r1", "b5", b2)
	commitFileIn(t, wt2, "x.txt", "x\n", "b2 again") // b2 moves past b5's base
	os.WriteFile(filepath.Join(wt5, "a.txt"), []byte("5\n"), 0o644)
	commitFileIn(t, wt5, "y.txt", "y\n", "b5")

	// Two branches sharing a non-default base without touching its files don't overlap.
	wtC1, bC1, baseC1 := addWorktreeBranch(t, repo, "r1", "c1", b2)
	wtC2, bC2, baseC2 := addWorktreeBranch(t, repo, "r1", "c2", b2)
	// Committed rename + non-ASCII path vs unstaged edit.
	wtR1, bR1, baseR1 := addWorktreeBranch(t, repo, "r1", "r1", "")
	gitIn(t, wtR1, "mv", "r.txt", "s.txt")
	commitFileIn(t, wtR1, "café.txt", "r1\n", "rename & cafe")
	wtR2, bR2, baseR2 := addWorktreeBranch(t, repo, "r1", "r2", "")
	os.WriteFile(filepath.Join(wtR2, "r.txt"), []byte("changed\n"), 0o644)
	os.WriteFile(filepath.Join(wtR2, "café.txt"), []byte("r2\n"), 0o644)

	// Missing recorded base produces a warning instead of failing the call.
	wtBad, bBad, _ := addWorktreeBranch(t, repo, "r1", "bad", "")
	os.WriteFile(filepath.Join(wtBad, "a.txt"), []byte("bad\n"), 0o644)

	seedConflictRun(t, h, "r1",
		&v1.Agent{Id: "a2", RunId: "r1", Name: "repo-b2", Repo: repo, Worktree: wt2, Branch: b2, BaseCommit: base2, CreatedAt: timestamppb.Now()},
		&v1.Agent{Id: "a5", RunId: "r1", Name: "repo-b5", Repo: repo, Worktree: wt5, Branch: b5, BaseCommit: base5, CreatedAt: timestamppb.Now()},
		&v1.Agent{Id: "c1", RunId: "r1", Name: "repo-c1", Repo: repo, Worktree: wtC1, Branch: bC1, BaseCommit: baseC1, CreatedAt: timestamppb.Now()},
		&v1.Agent{Id: "c2", RunId: "r1", Name: "repo-c2", Repo: repo, Worktree: wtC2, Branch: bC2, BaseCommit: baseC2, CreatedAt: timestamppb.Now()},
		&v1.Agent{Id: "r1", RunId: "r1", Name: "repo-r1", Repo: repo, Worktree: wtR1, Branch: bR1, BaseCommit: baseR1, CreatedAt: timestamppb.Now()},
		&v1.Agent{Id: "r2", RunId: "r1", Name: "repo-r2", Repo: repo, Worktree: wtR2, Branch: bR2, BaseCommit: baseR2, CreatedAt: timestamppb.Now()},
		&v1.Agent{Id: "bad", RunId: "r1", Name: "repo-bad", Repo: repo, Worktree: wtBad, Branch: bBad, BaseCommit: strings.Repeat("0", 40), CreatedAt: timestamppb.Now()},
	)

	res, err := h.device.Mesh.CheckConflicts(context.Background(), connect.NewRequest(&v1.CheckConflictsRequest{RunId: "r1"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Msg.Warnings) != 1 || !strings.Contains(res.Msg.Warnings[0], bBad) {
		t.Fatalf("warnings = %v", res.Msg.Warnings)
	}
	overlaps := map[string][]string{}
	for _, o := range res.Msg.Overlaps {
		overlaps[pairKey(o.Branch, o.Against)] = o.Files
	}
	if _, hasStacked := overlaps["b2|b5"]; hasStacked {
		t.Fatalf("stacked pair should not overlap: %+v", overlaps)
	}
	if _, hasShared := overlaps["c1|c2"]; hasShared {
		t.Fatalf("shared base without edits should not overlap: %+v", overlaps)
	}
	if !slices.Equal(overlaps["r1|r2"], []string{"café.txt", "r.txt"}) {
		t.Fatalf("rename/non-ascii overlap = %+v", overlaps)
	}
}

func TestConflictsReadsWorktreeWithoutWritingIndexAndRespectsScoping(t *testing.T) {
	h := start(t, "")
	repo := gitRepo(t)
	commitFileIn(t, repo, "a.txt", "1\n", "base")
	wt1, b1, base1 := addWorktreeBranch(t, repo, "r1", "b1", "")

	indexPath := gitIn(t, wt1, "rev-parse", "--path-format=absolute", "--git-path", "index")
	// Change stat mtime without changing contents: `git status` without GIT_OPTIONAL_LOCKS=0 would rewrite the index.
	past := time.Unix(1_000_000_000, 0)
	if err := os.Chtimes(filepath.Join(wt1, "a.txt"), past, past); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := h.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		return tx.PutRun(&v1.Run{Id: "r1", Title: "conflicts", Status: v1.RunStatus_RUN_STATUS_OPEN, CreatedAt: timestamppb.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	tok := h.seedAgent("r1", "a1", "repo-b1", "", v1.AgentRole_AGENT_ROLE_WORKER)
	err = h.d.Store().Write(context.Background(), nil, "", func(tx *store.Tx) error {
		return tx.PutAgent(&v1.Agent{
			Id: "a1", RunId: "r1", Name: "repo-b1", Role: v1.AgentRole_AGENT_ROLE_WORKER,
			Repo: repo, Worktree: wt1, Branch: b1, BaseCommit: base1, CreatedAt: timestamppb.Now(),
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	// Worker can inspect its own run, but not another run.
	if _, err := h.as(tok).CheckConflicts(context.Background(), connect.NewRequest(&v1.CheckConflictsRequest{})); err != nil {
		t.Fatal(err)
	}
	if _, err := h.as(tok).CheckConflicts(context.Background(), connect.NewRequest(&v1.CheckConflictsRequest{RunId: "other"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("other run err = %v", err)
	}

	after, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("CheckConflicts modified the worker's git index")
	}
	// Verify control: `git status --porcelain` without GIT_OPTIONAL_LOCKS=0 DOES rewrite it.
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = wt1
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	control, _ := os.ReadFile(indexPath)
	if bytes.Equal(before, control) {
		t.Fatal("expected control `git status` to refresh index")
	}
}

func TestConflictsNamesANonASCIIConflictedFileUnquoted(t *testing.T) {
	h := start(t, "")
	repo := gitRepo(t)
	commitFileIn(t, repo, "docs/café ü.md", "1\n", "base")

	wt1, b1, base1 := addWorktreeBranch(t, repo, "r1", "b1", "")
	commitFileIn(t, wt1, "docs/café ü.md", "2\n", "b1")
	wt2, b2, base2 := addWorktreeBranch(t, repo, "r1", "b2", "")
	commitFileIn(t, wt2, "docs/café ü.md", "3\n", "b2")

	seedConflictRun(t, h, "r1",
		&v1.Agent{Id: "a1", RunId: "r1", Name: "repo-b1", Repo: repo, Worktree: wt1, Branch: b1, BaseCommit: base1, CreatedAt: timestamppb.Now()},
		&v1.Agent{Id: "a2", RunId: "r1", Name: "repo-b2", Repo: repo, Worktree: wt2, Branch: b2, BaseCommit: base2, CreatedAt: timestamppb.Now()},
	)
	res, err := h.device.Mesh.CheckConflicts(context.Background(), connect.NewRequest(&v1.CheckConflictsRequest{RunId: "r1"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Msg.Conflicts) != 1 || !slices.Equal(res.Msg.Conflicts[0].Files, []string{"docs/café ü.md"}) {
		t.Fatalf("conflicts = %+v", res.Msg.Conflicts)
	}
	if len(res.Msg.Overlaps) != 0 {
		t.Fatalf("a conflicted file also reported as an overlap: %+v", res.Msg.Overlaps)
	}
}
