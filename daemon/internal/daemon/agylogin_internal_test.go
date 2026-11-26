package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func loginDaemon(t *testing.T, now func() time.Time) *Daemon {
	t.Helper()
	return &Daemon{paths: Paths{Dir: t.TempDir()}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: now,
		rt: &runtime{cfg: AgentConfig{Env: map[string]string{"HOME": t.TempDir()}}}}
}

// A second probe that wants a login while the first record holds leaves the record alone: its time is the hold
// and the alert's key, and rewriting it would push both back every time.
func TestARecordedLoginIsNotRewrittenWhileItHolds(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	d := loginDaemon(t, func() time.Time { return now })
	d.noteAgyLogin()
	first, err := os.ReadFile(d.paths.AgyLoginNeeded())
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	d.noteAgyLogin()
	if second, _ := os.ReadFile(d.paths.AgyLoginNeeded()); string(second) != string(first) {
		t.Errorf("the record was rewritten within the hold: %q -> %q", first, second)
	}
	now = now.Add(time.Hour) // the hold is over: the next one is recorded again
	d.noteAgyLogin()
	if third, _ := os.ReadFile(d.paths.AgyLoginNeeded()); string(third) == string(first) {
		t.Errorf("the record was not renewed after the hold")
	}
}

// With nothing recorded the alert pass reads no clock and does nothing (it runs every pass, in every test and
// every meshd).
func TestTheLoginAlertPassWithNothingRecordedReadsNoClock(t *testing.T) {
	d := loginDaemon(t, func() time.Time { t.Error("the clock was read"); return time.Time{} })
	d.agyLoginAlertPass(context.Background())
}
