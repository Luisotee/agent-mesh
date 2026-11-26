package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// An agy found on PATH is not run while its token file is missing: a logged-out agy opens the user's login in a
// browser, and a meshd with a temp HOME (a dev or test one) that reaches the real agy this way must not do it.
func TestAnAgyFoundOnPathIsNotRunWithoutItsTokenFile(t *testing.T) {
	home := t.TempDir()
	d := &Daemon{rt: &runtime{cfg: AgentConfig{Env: map[string]string{"HOME": home}}}}
	if !d.agyProbeBlocked() {
		t.Fatal("agy on PATH with no token file would be run")
	}
	d.rt.cfg.Binaries = map[string]string{"agy": "/fake/agy"}
	if d.agyProbeBlocked() {
		t.Error("an agy the config names (a fake in tests) is blocked")
	}
	d.rt.cfg.Binaries = nil
	dir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "antigravity-oauth-token"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if d.agyProbeBlocked() {
		t.Error("agy with its token file is blocked")
	}
}
