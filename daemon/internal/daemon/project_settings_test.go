package daemon_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/testutil/shortdir"
)

// A read-only Claude agent on a PR head must not run that head's `.claude/settings.json` hooks (nobody trusted the
// head's code), where a worker, which works on its own repo, does. The fake runs project hooks the way Claude Code
// does unless `--setting-sources` leaves the project out.
func TestAReadOnlyClaudeAgentDoesNotRunTheHeadsProjectHooks(t *testing.T) {
	ctx := context.Background()
	w := startReopen(t)
	repo := gitRepo(t)
	logDir := shortdir.New(t)
	marker := filepath.Join(logDir, "hooks.log")
	settings := fmt.Sprintf(`{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"cat >> %s; echo >> %s"}]}]}}`, marker, marker)
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".claude", "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "a hook"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run := startRun(t, w.device.Mesh, "")
	spawn := func(slug string, role v1.AgentRole) *v1.Agent {
		res, err := w.device.Mesh.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker", RunId: run.Id, Repo: repo,
			Slug: slug, Tier: "mid", Role: role, Task: "Question (read-only): where?"}))
		if err != nil {
			t.Fatal(err)
		}
		return w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	}
	worker := spawn("worker", v1.AgentRole_AGENT_ROLE_WORKER)
	for _, role := range []v1.AgentRole{v1.AgentRole_AGENT_ROLE_ASKER, v1.AgentRole_AGENT_ROLE_REVIEWER} {
		spawn("ro-"+strings.ToLower(strings.TrimPrefix(role.String(), "AGENT_ROLE_")), role)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("not even the worker ran the project's hook (the fake does not read it?): %v", err)
	}
	ran := string(b)
	if !strings.Contains(ran, worker.Worktree) {
		t.Errorf("the worker's own repo hook did not run in %s: %s", worker.Worktree, ran)
	}
	if n := strings.Count(ran, `"hook_event_name":"SessionStart"`); n != 1 {
		t.Errorf("the project hook ran %d times, want once (the worker's): %s", n, ran)
	}
}
