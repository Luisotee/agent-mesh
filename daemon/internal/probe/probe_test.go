package probe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cli writes a shell script that stands in for a CLI and returns its path.
func cli(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fakecli")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAProbeThatOpensABrowserByPathIsKilledAndReportedAsNeedingALogin(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"xdg-open on PATH", `xdg-open https://example.invalid/login; sleep 30`},
		{"a Windows opener on PATH", `cmd.exe /c start https://example.invalid/login; sleep 30`},
		{"the BROWSER variable", `"$BROWSER" https://example.invalid/login; sleep 30`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			_, err := Run(context.Background(), Spec{Bin: cli(t, tc.body), Timeout: 20 * time.Second})
			if !errors.Is(err, ErrLoginNeeded) {
				t.Fatalf("err = %v, want ErrLoginNeeded", err)
			}
			if time.Since(start) > 5*time.Second {
				t.Errorf("the CLI was left running for %s after it tried to open a browser", time.Since(start))
			}
		})
	}
}

func TestAProbeSeesNoDisplayNoWindowsHostAndNoMntOnItsPath(t *testing.T) {
	t.Setenv("DISPLAY", ":0")
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	t.Setenv("WSL_INTEROP", "/run/WSL/1_interop")
	t.Setenv("PATH", "/mnt/c/Windows/System32:/usr/bin:/bin:/mnt/c")
	out, err := Run(context.Background(), Spec{Bin: cli(t, `echo "$PATH"; echo "d=$DISPLAY w=$WAYLAND_DISPLAY i=$WSL_INTEROP"`), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 || strings.Contains(lines[0], "/mnt") || !strings.Contains(lines[0], "/usr/bin") {
		t.Errorf("PATH = %q", lines)
	}
	if lines[1] != "d= w= i=" {
		t.Errorf("environment = %q", lines[1])
	}
}

func TestAProbeThatHangsIsKilledAtItsTimeoutWithWhatItStarted(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child")
	start := time.Now()
	_, err := Run(context.Background(), Spec{Bin: cli(t, `(sleep 30; touch `+marker+`) & sleep 30`), Timeout: 500 * time.Millisecond})
	if err == nil || errors.Is(err, ErrLoginNeeded) {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %s", time.Since(start))
	}
}

func TestAProbeReturnsWhatTheCLIPrinted(t *testing.T) {
	out, err := Run(context.Background(), Spec{Bin: cli(t, `echo "$1 $2"`), Args: []string{"-p", "/usage"}, Timeout: 10 * time.Second})
	if err != nil || strings.TrimSpace(string(out)) != "-p /usage" {
		t.Fatalf("out = %q, err = %v", out, err)
	}
}

// A probe given an environment with no PATH (a test's, a config's) still finds the programs the CLI runs.
func TestAProbeWithNoPathInItsEnvironmentUsesTheDaemons(t *testing.T) {
	out, err := Run(context.Background(), Spec{Bin: cli(t, `cat /dev/null; echo ok`), Env: []string{"HOME=/nonexistent"}, Timeout: 10 * time.Second})
	if err != nil || strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("out = %q, err = %v", out, err)
	}
}
