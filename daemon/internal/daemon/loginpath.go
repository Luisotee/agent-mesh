package daemon

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const loginPathTimeout = 3 * time.Second

// ResolveLoginPath resolves PATH from a login shell within a short bounded timeout, merging the result ahead
// of currentPath and existing standard fallback directories. If the login shell fails, times out, or produces
// no PATH, it returns the fallback-merged PATH along with the error so callers can log and proceed safely.
func ResolveLoginPath(ctx context.Context, shell, home, currentPath string) (string, error) {
	return resolveLoginPathWithTimeout(ctx, shell, home, currentPath, loginPathTimeout)
}

func resolveLoginPathWithTimeout(ctx context.Context, shell, home, currentPath string, timeout time.Duration) (string, error) {
	fallback := mergePaths(home, "", currentPath)
	if shell == "" {
		shell = "/bin/sh"
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	const marker = "__AGENT_MESH_LOGIN_PATH__"
	script := fmt.Sprintf(`printf "%s\0"; printenv PATH; printf "\0%s"`, marker, marker)
	cmd := exec.CommandContext(runCtx, shell, "-lc", script)
	cmd.Stdin = nil
	env := filteredLoginEnv(os.Environ(), home, shell, currentPath)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 300 * time.Millisecond

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if cmd.Process != nil {
		// Kill any background subshells spawned by shell profiles that might linger in the process group.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return fallback, fmt.Errorf("%s -lc failed: %w (%s)", shell, err, msg)
		}
		return fallback, fmt.Errorf("%s -lc failed: %w", shell, err)
	}
	raw := stdout.Bytes()
	start := bytes.Index(raw, []byte(marker+"\x00"))
	if start < 0 {
		return fallback, fmt.Errorf("%s -lc produced no PATH sentinel", shell)
	}
	rest := raw[start+len(marker)+1:]
	end := bytes.Index(rest, []byte("\x00"+marker))
	if end < 0 {
		return fallback, fmt.Errorf("%s -lc produced an incomplete PATH sentinel", shell)
	}
	loginPath := strings.TrimRight(string(rest[:end]), "\r\n")
	if loginPath == "" {
		return fallback, fmt.Errorf("%s -lc returned an empty PATH", shell)
	}
	return mergePaths(home, loginPath, currentPath), nil
}

func filteredLoginEnv(base []string, home, shell, currentPath string) []string {
	out := make([]string, 0, len(base)+4)
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "HOME", "SHELL", "PATH", "TERM":
			continue
		}
		out = append(out, kv)
	}
	if home != "" {
		out = append(out, "HOME="+home)
	}
	if shell != "" {
		out = append(out, "SHELL="+shell)
	}
	if currentPath != "" {
		out = append(out, "PATH="+currentPath)
	}
	out = append(out, "TERM=dumb")
	return out
}

func mergePaths(home, loginPath, currentPath string) string {
	seen := map[string]bool{}
	var out []string
	add := func(dir string, checkExists bool) {
		dir = strings.TrimSpace(dir)
		if dir == "" || seen[dir] {
			return
		}
		if checkExists {
			st, err := os.Stat(dir)
			if err != nil || !st.IsDir() {
				return
			}
		}
		seen[dir] = true
		out = append(out, dir)
	}
	for _, p := range filepath.SplitList(loginPath) {
		add(p, false)
	}
	for _, p := range filepath.SplitList(currentPath) {
		add(p, false)
	}
	if home != "" {
		add(filepath.Join(home, ".local", "bin"), true)
		add(filepath.Join(home, "bin"), true)
		add(filepath.Join(home, ".local", "go", "bin"), true)
	}
	for _, sysDir := range []string{"/usr/local/bin", "/usr/bin", "/bin"} {
		add(sysDir, true)
	}
	return strings.Join(out, string(os.PathListSeparator))
}
