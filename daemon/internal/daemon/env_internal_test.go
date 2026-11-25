package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLookPathAndTheAgentEnvAllowlist(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "notexec"), []byte("x"), 0o644)
	if p, ok := lookPath("claude", "/nonexistent:"+dir); !ok || p != bin {
		t.Errorf("lookPath: %q, %v", p, ok)
	}
	if _, ok := lookPath("notexec", dir); ok {
		t.Error("a file without the execute bit was found")
	}
	if _, ok := lookPath("claude", ""); ok {
		t.Error("found on an empty PATH")
	}
	have := map[string]bool{}
	for _, k := range inheritedEnv {
		have[k] = true
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR", "HTTPS_PROXY", "NO_PROXY",
		"CLAUDE_CODE_USE_BEDROCK", "AWS_PROFILE", "CLAUDE_CODE_USE_VERTEX"} {
		if !have[k] {
			t.Errorf("%s is not inherited by agents", k)
		}
	}
	for _, k := range []string{"CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "AGENT_MESH_TOKEN", "MESH_RUN"} {
		if have[k] {
			t.Errorf("%s must never be inherited", k)
		}
	}
}
