package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
)

// Claude Code shows a folder-trust dialog ("Quick safety check … Yes, I trust this folder", the default "No,
// exit") for any folder it hasn't seen, even under --dangerously-skip-permissions, and fires no hook before it is
// answered, so a new worktree's agent would sit at STARTING. It records an answer in the global config file,
// `projects[<folder>].hasTrustDialogAccepted`, and reads it back for the folder, its canonical git root (a
// worktree's main checkout) and every parent of the folder. Pretrust writes that entry before the launch.

// trustLockWait bounds how long Pretrust waits for the config file's lock; trustLockStale is when Claude's lock
// library (proper-lockfile) calls a lock abandoned.
const (
	trustLockWait  = 3 * time.Second
	trustLockStale = 10 * time.Second
)

// configFile is the global config file Claude Code reads for an environment: `<config home>/.config.json` when
// that legacy file exists, else `.claude.json` in $CLAUDE_CONFIG_DIR or the home directory.
func configFile(env map[string]string) (string, error) {
	home := env["HOME"]
	if home == "" {
		return "", nil
	}
	configDir := env["CLAUDE_CONFIG_DIR"]
	legacyHome := configDir
	if legacyHome == "" {
		legacyHome = filepath.Join(home, ".claude")
	}
	if _, err := os.Stat(filepath.Join(legacyHome, ".config.json")); err == nil {
		return filepath.Join(legacyHome, ".config.json"), nil
	}
	if configDir == "" {
		configDir = home
	}
	return filepath.Join(configDir, ".claude.json"), nil
}

// canonicalRoot is the main checkout of a linked worktree (Claude keys a project by it), or "" when dir isn't
// one: its `.git` file names a gitdir whose `commondir` is the main checkout's `.git`.
func canonicalRoot(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err != nil {
		return ""
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return ""
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	c, err := os.ReadFile(filepath.Join(gitdir, "commondir"))
	if err != nil {
		return ""
	}
	common := strings.TrimSpace(string(c))
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitdir, common)
	}
	common = filepath.Clean(common)
	if filepath.Base(common) != ".git" {
		return common // a bare repo's worktree: the common dir itself is the identity
	}
	return filepath.Dir(common)
}

// trustedFolder reports whether the config trusts a folder the way Claude reads it.
func trustedFolder(projects map[string]json.RawMessage, dir string) bool {
	trusted := func(key string) bool {
		var p struct {
			Trusted bool `json:"hasTrustDialogAccepted"`
		}
		return json.Unmarshal(projects[key], &p) == nil && p.Trusted
	}
	if root := canonicalRoot(dir); root != "" && trusted(root) {
		return true
	}
	for cur := dir; ; cur = filepath.Dir(cur) {
		if trusted(cur) {
			return true
		}
		if filepath.Dir(cur) == cur {
			return false
		}
	}
}

// defaultProject is what Claude's own trust dialog saves for a folder it had no entry for.
const defaultProject = `{"allowedTools":[],"mcpContextUris":[],"mcpServers":{},"enabledMcpjsonServers":[],` +
	`"disabledMcpjsonServers":[],"hasTrustDialogAccepted":true,"projectOnboardingSeenCount":0,` +
	`"hasClaudeMdExternalIncludesApproved":false,"hasClaudeMdExternalIncludesWarningShown":false}`

// pretrustLocks serialise the Pretrust calls of this process per config file: the file lock alone cannot tell a
// stale lock from a live one made by a concurrent call, and would break it.
var pretrustLocks sync.Map // file → *sync.Mutex

// Pretrust makes Claude Code trust the folder it is about to be launched in, so its folder-trust dialog never
// shows: it adds `projects[<resolved cwd>].hasTrustDialogAccepted` to the global config of the agent's
// environment, unless the folder, its canonical git root or a parent is trusted already. It writes under the lock
// Claude takes for that file (`<file>.lock`, a directory) and keeps every other key as it was. Without a config
// file (Claude's first run, which shows its onboarding anyway) or no HOME in the environment it does nothing.
func Pretrust(env map[string]string, cwd string) error {
	if cwd == "" {
		return nil
	}
	file, err := configFile(env)
	if err != nil || file == "" {
		return err // with no HOME in the launch environment there is no file to know: never the process's own
	}
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	dir, err := filepath.Abs(cwd)
	if err != nil {
		return err
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	mu, _ := pretrustLocks.LoadOrStore(file, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	unlock, err := lockConfig(file)
	if err != nil {
		return err
	}
	defer unlock()
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return fmt.Errorf("%s is not readable JSON: %w", file, err)
	}
	if top == nil { // a file holding `null`
		top = map[string]json.RawMessage{}
	}
	projects := map[string]json.RawMessage{}
	if raw, ok := top["projects"]; ok {
		if err := json.Unmarshal(raw, &projects); err != nil {
			return fmt.Errorf("%s: projects: %w", file, err)
		}
		if projects == nil { // `"projects": null`
			projects = map[string]json.RawMessage{}
		}
	}
	if trustedFolder(projects, dir) {
		return nil
	}
	var entry map[string]json.RawMessage
	if raw, ok := projects[dir]; ok {
		if err := json.Unmarshal(raw, &entry); err != nil {
			return fmt.Errorf("%s: project %s: %w", file, dir, err)
		}
	}
	if entry != nil {
		// An entry without the trust flag: set it and keep what else it holds. A `null` one counts as absent.
		entry["hasTrustDialogAccepted"] = json.RawMessage("true")
		if projects[dir], err = marshal(entry); err != nil {
			return err
		}
	} else {
		projects[dir] = json.RawMessage(defaultProject)
	}
	if top["projects"], err = marshal(projects); err != nil {
		return err
	}
	out, err := marshal(top)
	if err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if st, err := os.Stat(file); err == nil {
		mode = st.Mode().Perm()
	}
	return fsutil.WriteAtomic(file, out, mode)
}

// marshal is json.Marshal with indentation and no HTML escaping, so values kept as they were stay readable.
func marshal(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// lockConfig takes Claude's lock on a config file: it makes the directory `<file>.lock`, breaking one older than
// trustLockStale (its holder died), and waits up to trustLockWait for a live one.
func lockConfig(file string) (func(), error) {
	lock := file + ".lock"
	deadline := time.Now().Add(trustLockWait)
	for {
		err := os.Mkdir(lock, 0o755)
		if err == nil {
			return func() { os.Remove(lock) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if st, serr := os.Stat(lock); serr == nil && time.Since(st.ModTime()) > trustLockStale {
			os.Remove(lock)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s is locked", file)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
