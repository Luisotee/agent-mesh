package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	var full []string
	if dir != "" {
		full = append(full, "-C", dir)
	}
	full = append(full, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func githubBackedRepo(t *testing.T) (repo string, bare string) {
	t.Helper()
	root := t.TempDir()
	bare = filepath.Join(root, "origin.git")
	repo = filepath.Join(root, "repo")
	runGit(t, "", "init", "--bare", "--initial-branch=main", bare)
	runGit(t, "", "init", "--initial-branch=main", repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "initial")
	runGit(t, repo, "remote", "add", "origin", "https://github.com/acme/demo.git")
	runGit(t, repo, "config", "url."+bare+".insteadOf", "https://github.com/acme/demo.git")
	runGit(t, repo, "push", "-u", "origin", "main")
	runGit(t, repo, "remote", "set-head", "origin", "main")
	return repo, bare
}

func retireStoppedWorker(t *testing.T, w *reopenWorld, a *v1.Agent) *v1.Agent {
	t.Helper()
	if _, err := w.device.Mesh.RetireAgent(context.Background(), connect.NewRequest(&v1.RetireAgentRequest{
		Meta: meta(""), Agent: a.Id,
	})); err != nil {
		t.Fatal(err)
	}
	return w.waitState(a.Id, v1.AgentState_AGENT_STATE_RETIRED)
}

func TestResumeMovesOffMergedPROntoFetchedDefaultBranch(t *testing.T) {
	repo, bare := githubBackedRepo(t)
	var ghResp atomic.Value
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Usage = &daemon.UsageOps{
			DisableClaudeNetwork: true,
			RunGH: func(_ context.Context, _ string, args ...string) ([]byte, error) {
				v, _ := ghResp.Load().(string)
				return []byte(v), nil
			},
		}
	})
	run := startRun(t, w.device.Mesh, "")
	res, err := w.device.Mesh.SpawnAgent(context.Background(), connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: repo, Slug: "fix", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_WORKER,
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)

	// Commit work on the worker branch and advance origin/main on the bare remote to simulate the PR merging.
	if err := os.WriteFile(filepath.Join(a.Worktree, "fix.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, a.Worktree, "add", "fix.txt")
	runGit(t, a.Worktree, "commit", "-m", "fix")
	prHead := runGit(t, a.Worktree, "rev-parse", "HEAD")

	other := filepath.Join(t.TempDir(), "other")
	runGit(t, "", "clone", bare, other)
	if err := os.WriteFile(filepath.Join(other, "upstream.txt"), []byte("merged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, other, "add", "upstream.txt")
	runGit(t, other, "commit", "-m", "merged pr upstream")
	runGit(t, other, "push", "origin", "main")
	newMain := runGit(t, other, "rev-parse", "HEAD")

	a = retireStoppedWorker(t, w, a)

	b, _ := json.Marshal([]map[string]any{{
		"number": 12, "url": "https://github.com/acme/demo/pull/12", "state": "MERGED", "headRefOid": prHead,
	}})
	ghResp.Store(string(b))

	oldTerm := a.TerminalId
	rres, err := w.device.Mesh.ResumeAgent(context.Background(), connect.NewRequest(&v1.ResumeAgentRequest{
		Meta: meta(""), Agent: a.Id, Task: "follow-up fix",
	}))
	if err != nil {
		t.Fatalf("ResumeAgent: %v", err)
	}
	if rres.Msg.LeftPr != "https://github.com/acme/demo/pull/12" {
		t.Errorf("LeftPr = %q", rres.Msg.LeftPr)
	}
	if len(rres.Msg.Notes) == 0 || !strings.Contains(rres.Msg.Notes[0], "merged") {
		t.Errorf("Notes = %v", rres.Msg.Notes)
	}
	if rres.Msg.Agent.Branch != "mesh/"+run.Id+"/fix-2" || rres.Msg.Agent.BaseCommit != newMain {
		t.Errorf("branch/base = %q / %q, want mesh/%s/fix-2 / %q", rres.Msg.Agent.Branch, rres.Msg.Agent.BaseCommit, run.Id, newMain)
	}
	if !strings.Contains(rres.Msg.Message.GetBody(), "PR #12") {
		t.Errorf("task note missing in %q", rres.Msg.Message.GetBody())
	}
	if curHead := runGit(t, a.Worktree, "rev-parse", "HEAD"); curHead != newMain {
		t.Errorf("worktree HEAD = %q, want %q (fetched origin/main)", curHead, newMain)
	}
	_ = w.waitReopened(t, a.Id, oldTerm)
}

func TestResumeClosedPROpenPRDirtyMergeAndSkippedCases(t *testing.T) {
	ctx := context.Background()

	// 1. Closed PR moves onto a new branch from the closed PR's head; open PR stays put; gh error skips cleanly.
	repo, bare := githubBackedRepo(t)
	var ghErr error
	var ghResp atomic.Value
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Usage = &daemon.UsageOps{
			DisableClaudeNetwork: true,
			RunGH: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
				if ghErr != nil {
					return nil, ghErr
				}
				v, _ := ghResp.Load().(string)
				return []byte(v), nil
			},
		}
	})
	run := startRun(t, w.device.Mesh, "")
	res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: repo, Slug: "cls", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_WORKER,
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	if err := os.WriteFile(filepath.Join(a.Worktree, "a.txt"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, a.Worktree, "add", "a.txt")
	runGit(t, a.Worktree, "commit", "-m", "pr head")
	prHead := runGit(t, a.Worktree, "rev-parse", "HEAD")

	// Open PR: stays on existing branch.
	openJSON, _ := json.Marshal([]map[string]any{{
		"number": 20, "url": "https://github.com/acme/demo/pull/20", "state": "OPEN", "headRefOid": prHead,
	}})
	ghResp.Store(string(openJSON))
	a = retireStoppedWorker(t, w, a)
	oldBranch := a.Branch
	oldTerm := a.TerminalId
	rOpen, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Meta: meta(""), Agent: a.Id, Task: "more on open pr",
	}))
	if err != nil || rOpen.Msg.LeftPr != "" || rOpen.Msg.Agent.Branch != oldBranch {
		t.Fatalf("open pr unexpectedly moved: %+v err=%v", rOpen, err)
	}
	a = w.waitReopened(t, a.Id, oldTerm)

	// Closed PR: moves to -2 branch from closed PR head.
	closedJSON, _ := json.Marshal([]map[string]any{{
		"number": 20, "url": "https://github.com/acme/demo/pull/20", "state": "CLOSED", "headRefOid": prHead,
	}})
	ghResp.Store(string(closedJSON))
	a = retireStoppedWorker(t, w, a)
	oldTerm = a.TerminalId
	rClosed, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Meta: meta(""), Agent: a.Id, Task: "reopen after close",
	}))
	if err != nil || rClosed.Msg.LeftPr != "https://github.com/acme/demo/pull/20" ||
		rClosed.Msg.Agent.Branch != "mesh/"+run.Id+"/cls-2" {
		t.Fatalf("closed pr move failed: %+v err=%v", rClosed, err)
	}
	a = w.waitReopened(t, a.Id, oldTerm)

	// Dirty worktree after merge: commit prHead2 on cls-2, leave dirty.txt uncommitted, warn in task note.
	if err := os.WriteFile(filepath.Join(a.Worktree, "b.txt"), []byte("2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, a.Worktree, "add", "b.txt")
	runGit(t, a.Worktree, "commit", "-m", "pr2 head")
	prHead2 := runGit(t, a.Worktree, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(a.Worktree, "dirty.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mergedJSON, _ := json.Marshal([]map[string]any{{
		"number": 21, "url": "https://github.com/acme/demo/pull/21", "state": "MERGED", "headRefOid": prHead2,
	}})
	ghResp.Store(string(mergedJSON))
	a = retireStoppedWorker(t, w, a)
	oldTerm = a.TerminalId
	rDirty, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Meta: meta(""), Agent: a.Id, Task: "check dirty",
	}))
	if err != nil || rDirty.Msg.LeftPr != "" || rDirty.Msg.Agent.Branch != "mesh/"+run.Id+"/cls-2" ||
		!strings.Contains(rDirty.Msg.Message.GetBody(), "local changes") {
		t.Fatalf("dirty merge handling failed: %+v err=%v", rDirty, err)
	}
	a = w.waitReopened(t, a.Id, oldTerm)

	// gh error doesn't block resume; read-only asker never calls gh.
	askerRes, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: repo, Slug: "ask", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_ASKER,
	}))
	if err != nil {
		t.Fatal(err)
	}
	ask := w.waitState(askerRes.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	ask = retireStoppedWorker(t, w, ask)
	ghErr = errors.New("gh unavailable")
	a = retireStoppedWorker(t, w, a)
	if _, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Meta: meta(""), Agent: a.Id, Task: "resumes despite gh error",
	})); err != nil {
		t.Fatalf("gh error blocked resume: %v", err)
	}
	if _, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Meta: meta(""), Agent: ask.Id, Task: "read-only question",
	})); err != nil {
		t.Fatalf("asker resume failed: %v", err)
	}
	_ = bare
}

func TestResumeRecreatesRemovedWorktreeAndSkipsNonGitHub(t *testing.T) {
	ctx := context.Background()
	repo := gitRepo(t)
	var ghCalls atomic.Int32
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Usage = &daemon.UsageOps{
			DisableClaudeNetwork: true,
			RunGH: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
				ghCalls.Add(1)
				return nil, nil
			},
		}
	})
	run := startRun(t, w.device.Mesh, "")
	res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: repo, Slug: "rec", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_WORKER,
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	wt := a.Worktree

	// StopRun removes clean untouched worktree and deletes its branch.
	if _, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{
		Meta: meta(""), RunId: run.Id,
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("expected StopRun to remove %s, stat=%v", wt, err)
	}

	// Open a fresh follow-up run and resume the stopped worker into it: worktree is recreated and gh was not called.
	run2 := startRun(t, w.device.Mesh, "")
	rres, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Meta: meta(""), Agent: a.Id, RunId: run2.Id, Task: "continue on recreated worktree",
	}))
	if err != nil {
		t.Fatalf("ResumeAgent: %v", err)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Errorf("worktree %s was not recreated: %v", wt, err)
	}
	if len(rres.Msg.Notes) == 0 || !strings.Contains(rres.Msg.Notes[0], "recreated worktree") {
		t.Errorf("Notes = %v, want recreated worktree", rres.Msg.Notes)
	}
	if ghCalls.Load() != 0 {
		t.Errorf("gh called %d times on non-GitHub repo", ghCalls.Load())
	}
}

func TestFinishedPRMissingRemoteHeadCommitAndOpenPRPrecedence(t *testing.T) {
	ctx := context.Background()
	repo, bare := githubBackedRepo(t)
	var ghResp atomic.Value
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Usage = &daemon.UsageOps{
			DisableClaudeNetwork: true,
			RunGH: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
				v, _ := ghResp.Load().(string)
				return []byte(v), nil
			},
		}
	})
	run := startRun(t, w.device.Mesh, "")
	res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: repo, Slug: "fix", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_WORKER,
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)

	if err := os.WriteFile(filepath.Join(a.Worktree, "fix.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, a.Worktree, "add", "fix.txt")
	runGit(t, a.Worktree, "commit", "-m", "fix")
	localHead := runGit(t, a.Worktree, "rev-parse", "HEAD")

	other := filepath.Join(t.TempDir(), "other")
	runGit(t, "", "clone", bare, other)
	if err := os.WriteFile(filepath.Join(other, "upstream.txt"), []byte("merged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, other, "add", "upstream.txt")
	runGit(t, other, "commit", "-m", "merged pr upstream")
	runGit(t, other, "push", "origin", "main")
	newMain := runGit(t, other, "rev-parse", "HEAD")

	// Open PR with a non-local headRefOid must NOT be filtered out, and must prevent leaving the branch even
	// if an older MERGED PR exists on the same branch.
	openAndOldMerged, _ := json.Marshal([]map[string]any{
		{"number": 30, "url": "https://github.com/acme/demo/pull/30", "state": "MERGED", "headRefOid": localHead},
		{"number": 31, "url": "https://github.com/acme/demo/pull/31", "state": "OPEN", "headRefOid": "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},
	})
	ghResp.Store(string(openAndOldMerged))
	a = retireStoppedWorker(t, w, a)
	oldBranch := a.Branch
	oldTerm := a.TerminalId
	rOpen, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Meta: meta(""), Agent: a.Id, Task: "keep open pr",
	}))
	if err != nil || rOpen.Msg.LeftPr != "" || rOpen.Msg.Agent.Branch != oldBranch {
		t.Fatalf("open PR with missing local SHA did not block move: %+v err=%v", rOpen, err)
	}
	a = w.waitReopened(t, a.Id, oldTerm)

	// Merged PR whose headRefOid is missing locally (e.g. squash-merged / updated via GitHub UI) STILL counts.
	mergedRemoteOnly, _ := json.Marshal([]map[string]any{{
		"number": 32, "url": "https://github.com/acme/demo/pull/32", "state": "MERGED",
		"headRefOid": "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
	}})
	ghResp.Store(string(mergedRemoteOnly))
	a = retireStoppedWorker(t, w, a)
	oldTerm = a.TerminalId
	rMerged, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Meta: meta(""), Agent: a.Id, Task: "follow-up after squash merge",
	}))
	if err != nil {
		t.Fatalf("ResumeAgent: %v", err)
	}
	if rMerged.Msg.LeftPr != "https://github.com/acme/demo/pull/32" {
		t.Errorf("LeftPr = %q, want PR #32", rMerged.Msg.LeftPr)
	}
	if rMerged.Msg.Agent.Branch != "mesh/"+run.Id+"/fix-2" || rMerged.Msg.Agent.BaseCommit != newMain {
		t.Errorf("branch/base = %q / %q, want mesh/%s/fix-2 / %q", rMerged.Msg.Agent.Branch, rMerged.Msg.Agent.BaseCommit, run.Id, newMain)
	}
	_ = w.waitReopened(t, a.Id, oldTerm)
}

func TestEnsureWorktreePrunesManuallyRemovedDir(t *testing.T) {
	ctx := context.Background()
	repo := gitRepo(t)
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Usage = &daemon.UsageOps{DisableClaudeNetwork: true}
	})
	run := startRun(t, w.device.Mesh, "")
	res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: repo, Slug: "manualrm", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_WORKER,
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	wt := a.Worktree
	a = retireStoppedWorker(t, w, a)

	// Delete the directory directly outside `git worktree remove` so git still lists it as missing.
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	rres, err := w.device.Mesh.ResumeAgent(ctx, connect.NewRequest(&v1.ResumeAgentRequest{
		Meta: meta(""), Agent: a.Id, Task: "work again",
	}))
	if err != nil {
		t.Fatalf("ResumeAgent after manual rm -rf: %v", err)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Errorf("worktree %s was not recreated: %v", wt, err)
	}
	if len(rres.Msg.Notes) == 0 || !strings.Contains(rres.Msg.Notes[0], "recreated worktree") {
		t.Errorf("Notes = %v, want recreated worktree", rres.Msg.Notes)
	}
}

func TestSendMessageWorktreePrepErrorsAndFailedReviveBranchNote(t *testing.T) {
	ctx := context.Background()
	repo, _ := githubBackedRepo(t)
	var ghResp atomic.Value
	w := startReopen(t, func(c *daemon.AgentConfig) {
		c.Usage = &daemon.UsageOps{
			DisableClaudeNetwork: true,
			RunGH: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
				v, _ := ghResp.Load().(string)
				return []byte(v), nil
			},
		}
	})
	run := startRun(t, w.device.Mesh, "")
	leadRes, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: repo, Slug: "lead", Role: v1.AgentRole_AGENT_ROLE_ROOT,
	}))
	if err != nil {
		t.Fatal(err)
	}
	_ = w.waitState(leadRes.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)

	res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		RunId: run.Id, Repo: repo, Slug: "cap", Tier: "mid", Role: v1.AgentRole_AGENT_ROLE_WORKER,
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	if err := os.WriteFile(filepath.Join(a.Worktree, "x.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, a.Worktree, "add", "x.txt")
	runGit(t, a.Worktree, "commit", "-m", "pr commit")
	prHead := runGit(t, a.Worktree, "rev-parse", "HEAD")

	b, _ := json.Marshal([]map[string]any{{
		"number": 44, "url": "https://github.com/acme/demo/pull/44", "state": "MERGED", "headRefOid": prHead,
	}})
	ghResp.Store(string(b))
	a = retireStoppedWorker(t, w, a)
	oldBranch := a.Branch

	// Hit caps.agents=1 (lead holds the 1 live slot) during SendMessage resume preparation:
	// prep fails/rolls back in revive -> ResumeError returned, message kept WITHOUT stale branch note.
	w.set(t, "caps.agents", 1)
	sres, err := w.device.Mesh.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{
		Meta: meta(""), To: a.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "Direct task\ntry under cap",
	}))
	if err != nil {
		t.Fatalf("SendMessage under cap failed unexpectedly: %v", err)
	}
	if sres.Msg.ResumeError == "" {
		t.Fatalf("expected ResumeError under cap, got resumed=%+v", sres.Msg.Resumed)
	}
	if strings.Contains(sres.Msg.Message.GetBody(), "moved onto new branch") {
		t.Errorf("stored message kept stale branch-move note after refused resume: %q", sres.Msg.Message.GetBody())
	}
	if cur := runGit(t, a.Worktree, "symbolic-ref", "--short", "HEAD"); cur != oldBranch {
		t.Errorf("worktree branch = %q, want rolled-back %q", cur, oldBranch)
	}

	// Lift cap, delete both the worktree AND the repo so prepareWorktreeForTask errors:
	// SendMessage keeps the message and reports preparing worktree in ResumeError.
	w.set(t, "caps.agents", 8)
	if err := os.RemoveAll(a.Worktree); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	sres2, err := w.device.Mesh.SendMessage(ctx, connect.NewRequest(&v1.SendMessageRequest{
		Meta: meta(""), To: a.Id, Type: v1.MessageType_MESSAGE_TYPE_TASK, Body: "Direct task\nmissing repo",
	}))
	if err != nil {
		t.Fatalf("SendMessage with missing repo returned RPC error instead of ResumeError: %v", err)
	}
	if !strings.Contains(sres2.Msg.ResumeError, "preparing worktree") {
		t.Errorf("ResumeError = %q, want preparing worktree error", sres2.Msg.ResumeError)
	}
	if sres2.Msg.Message == nil {
		t.Error("expected message to be stored despite worktree prep error")
	}
}
