package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeTrackedReplacesAgentMeshAndDeepMergesOtherKeys(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(repo, ".agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	committed := `{
  "agent-mesh": {
    "PreInvocation": [{"type": "command", "command": "old-mesh-pre"}],
    "PreToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": "old-mesh-tool", "timeout": 5}]}]
  },
  "repo": {
    "PreInvocation": [{"type": "command", "command": "repo-pre"}],
    "PreToolUse": [{"matcher": "read_file", "hooks": [{"type": "command", "command": "repo-tool", "timeout": 5}]}]
  },
  "other": {
    "Stop": [{"type": "command", "command": "other-stop"}]
  }
}`
	if err := os.WriteFile(filepath.Join(repo, ".agents", "hooks.json"), []byte(committed), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".agents/hooks.json")
	git("commit", "-q", "-m", "init")

	ours := `{
  "agent-mesh": {
    "SessionStart": [{"type": "command", "command": "mesh-start", "timeout": 10}],
    "PreInvocation": [{"type": "command", "command": "mesh-pre", "timeout": 10}],
    "PreToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": "mesh-tool", "timeout": 10}]}]
  },
  "repo": {
    "PreInvocation": [{"type": "command", "command": "extra-pre"}],
    "Stop": [{"type": "command", "command": "extra-stop"}]
  }
}`
	merged := mergeTracked(context.Background(), repo, ".agents/hooks.json", []byte(ours))

	var got map[string]struct {
		SessionStart  []json.RawMessage `json:"SessionStart"`
		PreInvocation []json.RawMessage `json:"PreInvocation"`
		PreToolUse    []json.RawMessage `json:"PreToolUse"`
		Stop          []json.RawMessage `json:"Stop"`
	}
	if err := json.Unmarshal(merged, &got); err != nil {
		t.Fatalf("unmarshal merged: %v\n%s", err, merged)
	}
	if len(got["other"].Stop) != 1 {
		t.Errorf("lost unrelated top-level key: %s", merged)
	}
	am := got["agent-mesh"]
	if len(am.SessionStart) != 1 || len(am.PreInvocation) != 1 || len(am.PreToolUse) != 1 {
		t.Fatalf("expected agent-mesh key replaced wholesale, got %s", merged)
	}
	if strings.Contains(string(am.PreInvocation[0]), "old-mesh-pre") || !strings.Contains(string(am.PreInvocation[0]), "mesh-pre") {
		t.Errorf("agent-mesh PreInvocation = %s, want only mesh-pre", am.PreInvocation[0])
	}
	rp := got["repo"]
	if len(rp.PreInvocation) != 2 || len(rp.PreToolUse) != 1 || len(rp.Stop) != 1 {
		t.Fatalf("expected non-mesh key deep-merged, got %s", merged)
	}
	if !strings.Contains(string(rp.PreInvocation[0]), "repo-pre") || !strings.Contains(string(rp.PreInvocation[1]), "extra-pre") {
		t.Errorf("repo PreInvocation did not preserve both entries: %s", merged)
	}
}
