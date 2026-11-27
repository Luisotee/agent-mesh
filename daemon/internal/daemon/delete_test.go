package daemon_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
	"github.com/Luisotee/agent-mesh/daemon/internal/testutil/shortdir"
)


func TestDeleteRunRefusesARunWithAProcessAndForgetsAnEndedOne(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	run, a, _ := w.plainRoot(t)
	del := func(mid string) (*connect.Response[v1.DeleteRunResponse], error) {
		return w.device.Mesh.DeleteRun(ctx, connect.NewRequest(&v1.DeleteRunRequest{Meta: meta(mid), RunId: run.Id}))
	}
	if _, err := del("live"); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "stop the run first") {
		t.Fatalf("deleting a run with a live agent: %v", err)
	}
	if _, err := w.d.Store().Read(ctx).GetRun(run.Id); err != nil {
		t.Fatalf("a refused delete touched the run: %v", err)
	}
	if _, err := os.Stat(a.Worktree); err != nil {
		t.Fatalf("a refused delete touched the worktree: %v", err)
	}

	if _, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta("stop"), RunId: run.Id})); err != nil {
		t.Fatal(err)
	}
	runDir, agentDir := filepath.Join(w.state, "runs", run.Id), filepath.Join(w.state, "agents", a.Id)
	if _, err := os.Stat(runDir); err != nil {
		t.Fatalf("the run's folder before the delete: %v", err)
	}
	res, err := del("ended")
	if err != nil {
		t.Fatal(err)
	}
	r := w.d.Store().Read(ctx)
	if _, err := r.GetRun(run.Id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("run after delete: %v", err)
	}
	if as, _ := r.ListAgents(run.Id); len(as) != 0 {
		t.Errorf("agents after delete: %v", as)
	}
	for _, p := range []string{runDir, agentDir, a.Worktree} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s is still there (%v)", p, err)
		}
	}
	if len(res.Msg.Worktrees) != 1 || !res.Msg.Worktrees[0].WorktreeRemoved {
		t.Errorf("outcomes = %v", res.Msg.Worktrees)
	}
	// The one event left of the run tells a client that was away to drop it.
	evs, err := r.ListEvents(run.Id)
	if err != nil || len(evs) != 1 || evs[0].GetRunDeleted() == nil {
		t.Errorf("events of the deleted run = %v, %v", evs, err)
	}
	// A retry of the same call gets its answer; another run id, or this one under a new call, is not found.
	if again, err := del("ended"); err != nil || len(again.Msg.Worktrees) != 1 {
		t.Errorf("a retry = %v, %v", again, err)
	}
	if _, err := del("later"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("deleting it again: %v", err)
	}
}

func TestDeleteRunKeepsAWorktreeThatHoldsWork(t *testing.T) {
	w := startReset(t, daemon.ResetTimings{})
	ctx := context.Background()
	run, a, _ := w.plainRoot(t)
	if err := os.WriteFile(filepath.Join(a.Worktree, "draft.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, a.Worktree, "add", "draft.txt")
	if _, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta("stop"), RunId: run.Id})); err != nil {
		t.Fatal(err)
	}
	res, err := w.device.Mesh.DeleteRun(ctx, connect.NewRequest(&v1.DeleteRunRequest{Meta: meta("del"), RunId: run.Id}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.Worktree, "draft.txt")); err != nil {
		t.Errorf("uncommitted work was removed: %v", err)
	}
	if !strings.Contains(runGit(t, a.Repo, "branch", "--list", a.Branch), a.Branch) {
		t.Errorf("the branch was removed")
	}
	if len(res.Msg.Worktrees) != 1 || res.Msg.Worktrees[0].WorktreeRemoved || !res.Msg.Worktrees[0].BranchKept || res.Msg.Worktrees[0].Reason == "" {
		t.Errorf("outcomes = %v: they must say what stayed and why", res.Msg.Worktrees)
	}
	if _, err := w.d.Store().Read(ctx).GetRun(run.Id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the run is still known: %v", err)
	}
}

func TestDeleteRunOfTheSetupRunLetsStartSetupMakeAnother(t *testing.T) {
	share := shareWithSetup(t)
	h := startWithAgents(t, shortdir.State(t), t.TempDir(), func(c *daemon.AgentConfig) { c.Share = share })
	ctx := context.Background()
	first := startSetup(t, h)
	if _, err := h.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta("stop"), RunId: first.Run.Id})); err != nil {
		t.Fatal(err)
	}
	if _, err := h.device.Mesh.DeleteRun(ctx, connect.NewRequest(&v1.DeleteRunRequest{Meta: meta("del"), RunId: first.Run.Id})); err != nil {
		t.Fatal(err)
	}
	second, err := h.device.Mesh.StartSetup(ctx, connect.NewRequest(&v1.StartSetupRequest{Meta: meta("setup-after")}))
	if err != nil {
		t.Fatal(err)
	}
	if second.Msg.AlreadyRunning || second.Msg.Run.Id == first.Run.Id || !second.Msg.Run.Setup || second.Msg.Agent.State == v1.AgentState_AGENT_STATE_EXITED {
		t.Fatalf("StartSetup after the delete = %+v", second.Msg)
	}
}

// A client that was away when a run was deleted resumes from its old cursor: the run's own events are gone, so it must
// still be told, by the RunDeleted event, to drop the run (and a snapshot is not what it gets: the log is the same one).
func TestAClientResumingAfterARunWasDeletedGetsTheRunDeletedEvent(t *testing.T) {
	h := start(t, "")
	ctx := context.Background()
	run := startRun(t, h.device.Mesh, "")
	s, err := h.device.Mesh.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	snap := recv(t, s).GetSnapshot()
	if snap == nil || len(snap.Runs) != 1 {
		t.Fatalf("first frame %v", snap)
	}
	cursor := snap.Cursor
	s.Close()

	for _, call := range []func() error{
		func() error {
			_, err := h.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta("stop"), RunId: run.Id}))
			return err
		},
		func() error {
			_, err := h.device.Mesh.DeleteRun(ctx, connect.NewRequest(&v1.DeleteRunRequest{Meta: meta("del"), RunId: run.Id}))
			return err
		},
	} {
		if err := call(); err != nil {
			t.Fatal(err)
		}
	}
	s2, err := h.device.Mesh.SubscribeEvents(ctx, connect.NewRequest(&v1.SubscribeEventsRequest{Cursor: cursor}))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if r := recv(t, s2).GetResumed(); r == nil {
		t.Fatalf("expected a resume, not a snapshot")
	}
	if ev := recv(t, s2).GetEvent(); ev.GetRunDeleted() == nil || ev.RunId != run.Id {
		t.Fatalf("the event after the resume = %v", ev)
	}
}
