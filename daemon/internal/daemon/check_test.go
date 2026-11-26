package daemon_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
)

type checker interface {
	CheckAgent(context.Context, *connect.Request[v1.CheckAgentRequest]) (*connect.Response[v1.CheckAgentResponse], error)
}

// mesh check is a worker's branch at a glance: commits past the base, the diff stat, pushed or not, the worktree's
// state and its PRs; a root, a device and a read-only device may ask, and an agent without a branch says so.
func TestCheckAgentShowsABranchInOneCall(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)
	worker := w.spawn(run.Id, gitRepo(t), "fix", "")
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	got, _ := w.device.Mesh.GetAgent(ctx, connect.NewRequest(&v1.GetAgentRequest{Agent: worker.Id}))
	wt := got.Msg.Agent.Worktree
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", wt, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	os.WriteFile(filepath.Join(wt, "a.txt"), []byte("one\ntwo\nthree\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "b.txt"), []byte("x\n"), 0o644)
	git("add", ".")
	git("commit", "-m", "add the files")
	os.WriteFile(filepath.Join(wt, "c.txt"), []byte("untracked\n"), 0o644)

	check := func(c checker, ref string) (string, error) {
		res, err := c.CheckAgent(ctx, connect.NewRequest(&v1.CheckAgentRequest{Agent: ref}))
		if err != nil {
			return "", err
		}
		return res.Msg.Text, nil
	}
	text, err := check(asLead, worker.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{worker.Name + " (worker, idle)", "branch " + got.Msg.Agent.Branch, "commits past the base: 1", "add the files",
		"diff against the base: 2 files, +4 -0", "+3 -0 a.txt", "tip NOT pushed", "worktree dirty", "?? c.txt", "PRs: "} {
		if !strings.Contains(text, want) {
			t.Errorf("the check lacks %q:\n%s", want, text)
		}
	}
	if text2, err := check(w.device.Mesh, worker.Id); err != nil || text2 != text {
		t.Errorf("a device sees %q, %v", text2, err)
	}
	if text, err := check(w.device.Mesh, lead.Name); err != nil || !strings.Contains(text, "no branch of its own") {
		t.Errorf("a root: %q, %v", text, err)
	}
	if _, err := check(w.device.Mesh, "nobody"); code(err) != connect.CodeNotFound {
		t.Errorf("an unknown agent: %v", err)
	}
	asWorker := w.as(w.token(t, worker.Id))
	if _, err := asWorker.CheckAgent(ctx, connect.NewRequest(&v1.CheckAgentRequest{Agent: worker.Id})); code(err) != connect.CodePermissionDenied {
		t.Errorf("a worker checked: %v", err)
	}
}

// A worker that merged the default branch in is credited only with its own work: the base moves to the newest
// commit of that branch it has merged, and merge commits are not counted.
func TestCheckAgentDoesNotCountWhatMainBrought(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	repo := gitRepo(t)
	gitIn(t, repo, "remote", "add", "origin", repo)
	gitIn(t, repo, "fetch", "origin")
	gitIn(t, repo, "remote", "set-head", "origin", "main")
	run := startRun(t, w.device.Mesh, "")
	worker := w.spawn(run.Id, repo, "fix", "")
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	got, _ := w.device.Mesh.GetAgent(ctx, connect.NewRequest(&v1.GetAgentRequest{Agent: worker.Id}))
	wt := got.Msg.Agent.Worktree
	os.WriteFile(filepath.Join(wt, "mine.txt"), []byte("one\n"), 0o644)
	gitIn(t, wt, "add", ".")
	gitIn(t, wt, "commit", "-m", "the worker's change")
	os.WriteFile(filepath.Join(repo, "theirs.txt"), []byte("a\nb\nc\nd\n"), 0o644)
	gitIn(t, repo, "add", ".")
	gitIn(t, repo, "commit", "-m", "main moved on")
	gitIn(t, repo, "fetch", "origin")
	gitIn(t, wt, "merge", "--no-edit", "origin/main")
	res, err := w.device.Mesh.CheckAgent(ctx, connect.NewRequest(&v1.CheckAgentRequest{Agent: worker.Id}))
	if err != nil {
		t.Fatal(err)
	}
	text := res.Msg.Text
	if !strings.Contains(text, "commits past the base: 1") || !strings.Contains(text, "the worker's change") ||
		strings.Contains(text, "main moved on") || strings.Contains(text, "theirs.txt") || !strings.Contains(text, "mine.txt") {
		t.Errorf("the check counts main's work as the worker's:\n%s", text)
	}
}

// A read-only device is shown the stored PRs: its call runs no gh (a phone polling it would spend the host's
// GitHub rate limit) and writes nothing.
func TestCheckAgentForAReadOnlyDeviceRunsNoGH(t *testing.T) {
	var ghCalls atomic.Int32
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Usage = &daemon.UsageOps{DisableClaudeNetwork: true, RunGH: func(context.Context, string, ...string) ([]byte, error) {
			ghCalls.Add(1)
			return []byte("[]"), nil
		}}
	})
	ctx := context.Background()
	repo := gitRepo(t)
	gitIn(t, repo, "remote", "add", "origin", "https://github.com/o/r.git")
	run := startRun(t, w.device.Mesh, "")
	worker := w.spawn(run.Id, repo, "fix", "")
	w.waitState(worker.Id, v1.AgentState_AGENT_STATE_IDLE)
	ghCalls.Store(0)
	ro := auth.WithActor(ctx, &v1.Actor{Id: "viewer", Class: v1.ActorClass_ACTOR_CLASS_READ_ONLY_DEVICE})
	res, err := w.d.CheckAgent(ro, connect.NewRequest(&v1.CheckAgentRequest{Agent: worker.Id}))
	if err != nil || !strings.Contains(res.Msg.Text, "PRs: ") {
		t.Fatalf("a read-only device: %v, %v", res, err)
	}
	if n := ghCalls.Load(); n != 0 {
		t.Errorf("a read-only device's check ran gh %d times", n)
	}
	if _, err := w.device.Mesh.CheckAgent(ctx, connect.NewRequest(&v1.CheckAgentRequest{Agent: worker.Id})); err != nil {
		t.Fatal(err)
	}
	if ghCalls.Load() == 0 {
		t.Errorf("the host's own device's check read nothing from GitHub (the test's remote is no GitHub repo?)")
	}
}
