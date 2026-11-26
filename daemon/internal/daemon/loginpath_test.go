package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveLoginPathReadsSentinelOverNoisyProfileAndAddsFallbacks(t *testing.T) {
	home := t.TempDir()
	localBin := filepath.Join(home, ".local", "bin")
	customBin := filepath.Join(home, "custom-node", "bin")
	os.MkdirAll(localBin, 0o755)
	os.MkdirAll(customBin, 0o755)

	shell := filepath.Join(t.TempDir(), "fake-shell")
	script := fmt.Sprintf("#!/bin/sh\necho 'noisy greeting from profile'\nprintf 'stderr warning\n' >&2\nexport PATH=%q\nif [ \"$1\" = \"-lc\" ]; then shift; fi\neval \"$1\"\n", customBin+":/usr/bin")
	if err := os.WriteFile(shell, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveLoginPath(context.Background(), shell, home, "/bin:/usr/bin")
	if err != nil {
		t.Fatalf("ResolveLoginPath: %v", err)
	}
	parts := filepath.SplitList(got)
	if len(parts) < 3 || parts[0] != customBin {
		t.Fatalf("expected %s first in %v", customBin, parts)
	}
	if strings.Contains(got, "noisy") {
		t.Fatalf("profile stdout leaked into PATH: %q", got)
	}
	foundLocal := false
	for _, p := range parts {
		if p == localBin {
			foundLocal = true
		}
	}
	if !foundLocal {
		t.Fatalf("expected existing fallback %s in %v", localBin, parts)
	}
}

func TestResolveLoginPathFallsBackWhenShellFailsOrTimesOut(t *testing.T) {
	home := t.TempDir()
	localBin := filepath.Join(home, ".local", "bin")
	os.MkdirAll(localBin, 0o755)

	badShell := filepath.Join(t.TempDir(), "bad-shell")
	os.WriteFile(badShell, []byte("#!/bin/sh\nexit 17\n"), 0o755)
	got, err := resolveLoginPathWithTimeout(context.Background(), badShell, home, "/sbin:/bin", 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected error from failing shell")
	}
	if !strings.Contains(got, "/sbin") || !strings.Contains(got, localBin) {
		t.Fatalf("fallback missing inherited or home dirs: %q", got)
	}

	slowShell := filepath.Join(t.TempDir(), "slow-shell")
	os.WriteFile(slowShell, []byte("#!/bin/sh\nsleep 5\n"), 0o755)
	start := time.Now()
	got, err = resolveLoginPathWithTimeout(context.Background(), slowShell, home, "/sbin", 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error from slow shell")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("slow shell took too long to cancel: %v", elapsed)
	}
	if !strings.Contains(got, "/sbin") {
		t.Fatalf("timeout fallback missing inherited PATH: %q", got)
	}
}
