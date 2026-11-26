package daemon_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
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
