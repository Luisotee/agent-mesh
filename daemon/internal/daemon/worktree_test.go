package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

func TestCreateWorktreeIsRecordedCheckedForConflictsAndCleanedOnStopRun(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	worker := w.spawnUnder(t, asLead, "feat", &v1.SpawnAgentRequest{})

	// Only a device or a root may create a standalone worktree; a worker cannot.
	if _, err := w.as(w.token(t, worker.Id)).CreateWorktree(ctx, connect.NewRequest(&v1.CreateWorktreeRequest{
		Meta: meta(""), Repo: worker.Repo, Slug: "denied",
	})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("worker CreateWorktree: %v, want PermissionDenied", err)
	}

	// Root creates two standalone worktrees in its own run: one stays untouched, one gets a commit, one is dirty.
	res1, err := asLead.CreateWorktree(ctx, connect.NewRequest(&v1.CreateWorktreeRequest{
		Meta: meta("wt-1"), Repo: worker.Repo, Slug: "integ-clean",
	}))
	if err != nil {
		t.Fatal(err)
	}
	wt1 := res1.Msg.Worktree
	if wt1.Branch != "mesh/"+run.Id+"/integ-clean" || wt1.RunId != run.Id || wt1.BaseCommit == "" {
		t.Fatalf("wt1 = %+v", wt1)
	}
	if _, err := os.Stat(wt1.Path); err != nil {
		t.Fatalf("wt1 path missing: %v", err)
	}
	// Idempotent replay returns the same worktree without error.
	if replay, err := asLead.CreateWorktree(ctx, connect.NewRequest(&v1.CreateWorktreeRequest{
		Meta: meta("wt-1"), Repo: worker.Repo, Slug: "integ-clean",
	})); err != nil || replay.Msg.Worktree.Id != wt1.Id {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	// Duplicate slug with a new mutation id is refused.
	if _, err := asLead.CreateWorktree(ctx, connect.NewRequest(&v1.CreateWorktreeRequest{
		Meta: meta("wt-dup"), Repo: worker.Repo, Slug: "integ-clean",
	})); code(err) != connect.CodeAlreadyExists {
		t.Fatalf("duplicate slug: %v, want AlreadyExists", err)
	}

	res2, err := asLead.CreateWorktree(ctx, connect.NewRequest(&v1.CreateWorktreeRequest{
		Meta: meta("wt-2"), Repo: worker.Repo, Slug: "integ-committed", Base: "HEAD",
	}))
	if err != nil {
		t.Fatal(err)
	}
	wt2 := res2.Msg.Worktree
	commitFileIn(t, wt2.Path, "shared.txt", "from wt2\n", "integ commit")
	commitFileIn(t, worker.Worktree, "shared.txt", "from worker\n", "worker commit")

	res3, err := w.device.Mesh.CreateWorktree(ctx, connect.NewRequest(&v1.CreateWorktreeRequest{
		Meta: meta("wt-3"), RunId: run.Id, Repo: worker.Repo, Slug: "integ-dirty",
	}))
	if err != nil {
		t.Fatal(err)
	}
	wt3 := res3.Msg.Worktree
	if err := os.WriteFile(filepath.Join(wt3.Path, "dirty.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}

	// CheckConflicts sees standalone worktree branches alongside agent branches.
	conf, err := asLead.CheckConflicts(ctx, connect.NewRequest(&v1.CheckConflictsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	foundConflict := false
	for _, c := range conf.Msg.Conflicts {
		if (c.Branch == wt2.Branch && c.Against == worker.Branch) || (c.Branch == worker.Branch && c.Against == wt2.Branch) {
			foundConflict = true
		}
	}
	if !foundConflict {
		t.Fatalf("expected conflict between %s and %s, got %+v", wt2.Branch, worker.Branch, conf.Msg.Conflicts)
	}

	// WorktreeCreated events are recorded.
	evs, err := w.d.Store().Read(ctx).EventsAfter(0, run.Id, 1000)
	if err != nil {
		t.Fatal(err)
	}
	createdEvents := 0
	for _, ev := range evs {
		if wc := ev.GetWorktreeCreated(); wc != nil {
			createdEvents++
		}
	}
	if createdEvents != 3 {
		t.Fatalf("WorktreeCreated events = %d, want 3", createdEvents)
	}

	// StopRun cleans wt1 (worktree + untouched branch), removes wt2 (keeping committed branch), and keeps dirty wt3.
	stopRes, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta("stop-wt"), RunId: run.Id}))
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]*v1.WorktreeOutcome{}
	for _, o := range stopRes.Msg.Worktrees {
		byPath[o.Worktree] = o
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	branches := gitIn(t, worker.Repo, "branch", "--list", "mesh/*")
	if o := byPath[wt1.Path]; o == nil || !o.WorktreeRemoved || o.BranchKept || exists(wt1.Path) || strings.Contains(branches, wt1.Branch) {
		t.Errorf("wt1 outcome = %+v (branches %s)", o, branches)
	}
	if o := byPath[wt2.Path]; o == nil || !o.WorktreeRemoved || !o.BranchKept || exists(wt2.Path) || !strings.Contains(branches, wt2.Branch) {
		t.Errorf("wt2 outcome = %+v (branches %s)", o, branches)
	}
	if o := byPath[wt3.Path]; o == nil || o.WorktreeRemoved || !o.BranchKept || !exists(wt3.Path) {
		t.Errorf("wt3 outcome = %+v", o)
	}

	// Removed standalone worktrees (wt1, wt2) had their DB rows deleted; dirty wt3 stays so a later StopRun sees only wt3.
	leftWTs, err := w.d.Store().Read(ctx).ListWorktrees(run.Id)
	if err != nil {
		t.Fatal(err)
	}
	if len(leftWTs) != 1 || leftWTs[0].Id != wt3.Id {
		t.Fatalf("remaining worktree rows = %+v, want [%s]", leftWTs, wt3.Id)
	}

	// Replaying StopRun with the same mutation id returns the stored response cleanly without repeating worktree loops.
	replayStop, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta("stop-wt"), RunId: run.Id}))
	if err != nil {
		t.Fatal(err)
	}
	if len(replayStop.Msg.Worktrees) != 0 {
		t.Fatalf("replayed StopRun worktrees = %+v, want empty (stored response)", replayStop.Msg.Worktrees)
	}

	// A second StopRun with a new mutation id sees wt3 (still dirty) and never repeats deleted wt1/wt2.
	secondStop, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta("stop-wt-2"), RunId: run.Id}))
	if err != nil {
		t.Fatal(err)
	}
	seenSecond := map[string]bool{}
	for _, o := range secondStop.Msg.Worktrees {
		seenSecond[o.Worktree] = true
	}
	if seenSecond[wt1.Path] || seenSecond[wt2.Path] || !seenSecond[wt3.Path] {
		t.Fatalf("second StopRun worktrees = %+v, want wt3 without wt1/wt2", secondStop.Msg.Worktrees)
	}
}
