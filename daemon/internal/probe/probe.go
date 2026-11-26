// Package probe runs a CLI the daemon asks a question of (`agy -p /usage`, `agy models`, `opencode models`) so
// that it can never open a login: no terminal, no display, and every way of opening a browser that a PATH lookup
// finds is a stub that records the attempt (an opener started by an absolute path is not a PATH lookup, and is not
// caught). The CLI is killed at once when it tries, and the caller is told the
// CLI needs a login by ErrLoginNeeded (never the URL: it can carry a login state).
package probe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrLoginNeeded is the CLI trying to open a browser (to log in) while it ran as a probe.
var ErrLoginNeeded = errors.New("the CLI tried to open a browser to log in")

// openers are the programs a CLI starts to show a page: the Linux ones, and the Windows ones a WSL host reaches
// through its PATH. Each becomes a stub that records the attempt and fails.
var openers = []string{"xdg-open", "wslview", "sensible-browser", "x-www-browser", "www-browser", "gnome-open",
	"kde-open", "open", "firefox", "google-chrome", "chromium", "explorer.exe", "cmd.exe", "powershell.exe"}

// dropped are the variables a probe never inherits: where a display or a Windows host would be found.
var dropped = map[string]bool{"DISPLAY": true, "WAYLAND_DISPLAY": true, "WSL_INTEROP": true, "WSL_DISTRO_NAME": true,
	"BROWSER": true, "DBUS_SESSION_BUS_ADDRESS": true}

// Spec is one probe.
type Spec struct {
	Bin     string
	Dir     string // where the stub openers are made, which must allow running programs; empty means the temp dir
	Args    []string
	Env     []string // KEY=VALUE; empty means the daemon's own environment
	Timeout time.Duration
}

// Run runs the probe and returns its standard output. It is killed (with everything it started) at the timeout,
// when ctx ends, or at the moment it tries to open a browser, which is ErrLoginNeeded.
func Run(ctx context.Context, s Spec) ([]byte, error) {
	if s.Dir != "" {
		if err := os.MkdirAll(s.Dir, 0o700); err != nil {
			return nil, err
		}
		sweep(s.Dir)
	}
	dir, err := os.MkdirTemp(s.Dir, "meshprobe-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	stubs := filepath.Join(dir, "bin")
	if err := os.Mkdir(stubs, 0o700); err != nil {
		return nil, err
	}
	opened := filepath.Join(dir, "opened")
	script := "#!/bin/sh\n: > '" + opened + "'\nexit 1\n"
	for _, name := range openers {
		if err := os.WriteFile(filepath.Join(stubs, name), []byte(script), 0o700); err != nil {
			return nil, err
		}
	}
	// A stub that can't be run (a noexec mount) would let a lookup fall through to the real opener: not run at all.
	if err := syscall.Access(filepath.Join(stubs, openers[0]), 1); err != nil {
		return nil, fmt.Errorf("the probe's stub openers can't be run (is %s mounted noexec?): %w", filepath.Dir(stubs), err)
	}
	env := s.Env
	if len(env) == 0 {
		env = os.Environ()
	}
	env = Env(env, stubs)

	if s.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, s.Bin, s.Args...)
	cmd.Env = env
	cmd.Stdin = nil // /dev/null
	var out bytes.Buffer
	var stderr capped
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // no controlling terminal, its own process group
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 500 * time.Millisecond
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	var mu sync.Mutex
	reaped := false // once Wait has returned, its process group id may be someone else's: nothing is killed
	go func() {
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if _, err := os.Stat(opened); err == nil {
					mu.Lock()
					if !reaped {
						syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
					}
					mu.Unlock()
					return
				}
			}
		}
	}()
	werr := cmd.Wait()
	mu.Lock()
	reaped = true
	mu.Unlock()
	close(done)
	if _, err := os.Stat(opened); err == nil {
		return nil, ErrLoginNeeded
	}
	if werr != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			werr = fmt.Errorf("%w: %s", werr, msg)
		}
	}
	return out.Bytes(), werr
}

// sweep removes the stub directories of probes that were killed before they could clean up (meshd itself
// killed): those older than an hour, which no running probe has.
func sweep(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "meshprobe-") {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > time.Hour {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

// capped keeps the first 2 KiB written to it: what a failed probe said, for the error.
type capped struct{ b bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if room := 2048 - c.b.Len(); room > 0 {
		c.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (c *capped) String() string { return c.b.String() }

// Env returns env for a probe: no display or Windows host variables, BROWSER and PATH pointing first at the
// stub directory, and (on a WSL host) no PATH entry under /mnt, where the Windows programs are.
func Env(env []string, stubs string) []string {
	var out []string
	path := ""
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case k == "PATH":
			path = v
		case dropped[k]:
		default:
			out = append(out, kv)
		}
	}
	if path == "" { // none given: the daemon's own, as an exec with no PATH of its own would find programs by
		path = os.Getenv("PATH")
	}
	var keep []string
	for _, p := range filepath.SplitList(path) {
		if p != "" && p != "/mnt" && !strings.HasPrefix(p, "/mnt/") {
			keep = append(keep, p)
		}
	}
	return append(out, "PATH="+strings.Join(append([]string{stubs}, keep...), ":"),
		"BROWSER="+filepath.Join(stubs, "xdg-open"))
}
