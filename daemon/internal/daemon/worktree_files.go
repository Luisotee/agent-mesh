package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
)

var excludeMu sync.Mutex

// writeWorktreeFiles writes adapter-requested files inside worktree before the CLI starts, keeping
// git-excluded files out of git status (via <git-common-dir>/info/exclude) and neutralizing pre-existing
// tracked files with --skip-worktree.
func (d *Daemon) writeWorktreeFiles(ctx context.Context, worktree string, files []*adapterv1.WorktreeFile) error {
	if len(files) == 0 {
		return nil
	}
	if worktree == "" {
		return fmt.Errorf("cannot write worktree files: empty worktree")
	}
	realWT, err := evalExisting(worktree)
	if err != nil {
		return err
	}
	for _, f := range files {
		rel := filepath.Clean(f.Path)
		if rel == "" || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("invalid worktree file path %q", f.Path)
		}
		target := filepath.Join(worktree, rel)
		parent := filepath.Dir(target)
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return err
		}
		realTarget, err := evalExisting(target)
		if err != nil {
			return err
		}
		if realTarget != realWT && !strings.HasPrefix(realTarget, realWT+string(filepath.Separator)) {
			return fmt.Errorf("worktree file %q escapes worktree %q", f.Path, worktree)
		}
		if f.GitExclude {
			slashRel := filepath.ToSlash(rel)
			if err := excludeWorktreeFile(ctx, worktree, slashRel); err != nil {
				return err
			}
		}
		mode := os.FileMode(f.Mode)
		if mode == 0 {
			mode = 0o600
		}
		if err := fsutil.WriteAtomic(target, f.Content, mode); err != nil {
			return err
		}
	}
	return nil
}

// evalExisting resolves symlinks on the longest existing ancestor of p and appends the rest.
func evalExisting(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	cur := abs
	var tail []string
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			parts := append([]string{real}, tail...)
			return filepath.Join(parts...), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
	}
}

func excludeWorktreeFile(ctx context.Context, worktree, slashRel string) error {
	commonDir, err := git(ctx, worktree, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if err := addGitExclude(commonDir, "/"+slashRel); err != nil {
		return err
	}
	// If the repo tracks this file (e.g. an untrusted .agents/hooks.json in a PR head), mark it skip-worktree
	// before overwriting it so our replacement holds without dirtying git status or blocking worktree removal.
	ls := exec.CommandContext(ctx, "git", "-C", worktree, "ls-files", "--error-unmatch", "--", slashRel)
	if ls.Run() == nil {
		if _, err := git(ctx, worktree, "update-index", "--skip-worktree", "--", slashRel); err != nil {
			return err
		}
	}
	return nil
}

func addGitExclude(commonDir, pattern string) error {
	excludeMu.Lock()
	defer excludeMu.Unlock()
	infoDir := filepath.Join(commonDir, "info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil {
		return err
	}
	excludePath := filepath.Join(infoDir, "exclude")
	existing, err := os.ReadFile(excludePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == pattern {
			return nil
		}
	}
	var b strings.Builder
	b.Write(existing)
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		b.WriteByte('\n')
	}
	b.WriteString(pattern)
	b.WriteByte('\n')
	return fsutil.WriteAtomic(excludePath, []byte(b.String()), 0o644)
}
