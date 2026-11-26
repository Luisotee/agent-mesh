package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
)

var excludeMu sync.Mutex

// writeWorktreeFiles writes adapter-requested files inside worktree before the CLI starts, keeping git-excluded
// files out of git status (excludeWorktreeFile) and merging a tracked file into the committed version when asked
// (mergeTracked) and that version is the repo's own (ownVersion).
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
		slashRel := filepath.ToSlash(rel)
		tracked := exec.CommandContext(ctx, "git", "-C", worktree, "ls-files", "--error-unmatch", "--", slashRel).Run() == nil
		content := f.Content
		if f.MergeTracked && tracked && ownVersion(ctx, worktree, slashRel) {
			content = mergeTracked(ctx, worktree, slashRel, content)
		}
		if f.GitExclude {
			if err := excludeWorktreeFile(ctx, worktree, slashRel, tracked); err != nil {
				return err
			}
		}
		mode := os.FileMode(f.Mode)
		if mode == 0 {
			mode = 0o600
		}
		if err := fsutil.WriteAtomic(target, content, mode); err != nil {
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

// mergeTracked is the committed version of a JSON object file (as the index has it, not the working copy, which a
// launch before may have written) with content merged in: the mesh's own top-level "agent-mesh" key is replaced
// wholesale so a committed copy never runs hooks twice, while any other keys are deep-merged (objects merged
// recursively, arrays concatenated, scalars replaced); content alone when either isn't a JSON object.
func mergeTracked(ctx context.Context, worktree, slashRel string, content []byte) []byte {
	committed, err := exec.CommandContext(ctx, "git", "-C", worktree, "show", ":"+slashRel).Output()
	if err != nil {
		return content
	}
	base, ok1 := decodeJSONObject(committed)
	ours, ok2 := decodeJSONObject(content)
	if !ok1 || !ok2 {
		return content
	}
	merged := mergeJSONObject(base, ours, true)
	b, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return content
	}
	return append(b, '\n')
}

func decodeJSONObject(raw []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, false
	}
	obj, ok := v.(map[string]any)
	return obj, ok && obj != nil
}

func mergeJSONObject(base, ours map[string]any, replaceAgentMesh bool) map[string]any {
	out := make(map[string]any, len(base)+len(ours))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range ours {
		if replaceAgentMesh && k == "agent-mesh" {
			out[k] = v
			continue
		}
		if existing, ok := out[k]; ok {
			out[k] = mergeJSONValue(existing, v)
		} else {
			out[k] = v
		}
	}
	return out
}

func mergeJSONValue(base, ours any) any {
	if bm, ok := base.(map[string]any); ok {
		if om, ok := ours.(map[string]any); ok {
			return mergeJSONObject(bm, om, false)
		}
	}
	if ba, ok := base.([]any); ok {
		if oa, ok := ours.([]any); ok {
			combined := make([]any, 0, len(ba)+len(oa))
			combined = append(combined, ba...)
			combined = append(combined, oa...)
			return combined
		}
	}
	return ours
}

// ownVersion reports whether the version of a file the worktree's index has is the one on one of the repo's own
// refs: a local branch other than the mesh's (refs/heads/mesh/…, which agents write) or a branch of origin. A base
// from anywhere else (a fork's PR head fetched by sha or into refs/pull/…, another remote) brings a file nobody
// here chose to run.
func ownVersion(ctx context.Context, worktree, slashRel string) bool {
	ours, err := exec.CommandContext(ctx, "git", "-C", worktree, "rev-parse", "-q", "--verify", ":"+slashRel).Output()
	if err != nil {
		return false
	}
	refs, err := exec.CommandContext(ctx, "git", "-C", worktree, "for-each-ref", "--format=%(refname)",
		"refs/heads", "refs/remotes/origin").Output()
	if err != nil {
		return false
	}
	var in strings.Builder
	for _, ref := range strings.Fields(string(refs)) {
		if !strings.HasPrefix(ref, "refs/heads/mesh/") && ref != "refs/remotes/origin/HEAD" {
			in.WriteString(ref + ":" + slashRel + "\n")
		}
	}
	if in.Len() == 0 {
		return false
	}
	cmd := exec.CommandContext(ctx, "git", "-C", worktree, "cat-file", "--batch-check=%(objectname)")
	cmd.Stdin = strings.NewReader(in.String())
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	want := strings.TrimSpace(string(ours))
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// ignoreMarker heads a .gitignore the daemon wrote, so a later launch knows it may rewrite it.
const ignoreMarker = "# agent-mesh: files the daemon writes for its agents, kept out of git"

// excludeWorktreeFile keeps a file the daemon writes out of git status in this worktree alone. A tracked one (a
// repo's own .agents/hooks.json) is marked skip-worktree, so the replacement neither dirties the worktree nor
// blocks its removal. An untracked one gets a self-ignoring .gitignore beside it: the repo's shared
// <git-common-dir>/info/exclude would hide the same path in every checkout of the repo, the user's included. Only
// a file at the worktree's top, or beside a .gitignore the daemon didn't write, falls back to info/exclude.
func excludeWorktreeFile(ctx context.Context, worktree, slashRel string, tracked bool) error {
	if tracked {
		_, err := git(ctx, worktree, "update-index", "--skip-worktree", "--", slashRel)
		return err
	}
	if dir := path.Dir(slashRel); dir != "." {
		ignore := filepath.Join(worktree, filepath.FromSlash(dir), ".gitignore")
		existing, err := os.ReadFile(ignore)
		if os.IsNotExist(err) || err == nil && strings.HasPrefix(string(existing), ignoreMarker+"\n") {
			return addIgnoreLine(ignore, existing, "/"+path.Base(slashRel))
		}
		if err != nil {
			return err
		}
	}
	commonDir, err := git(ctx, worktree, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	return addGitExclude(commonDir, "/"+slashRel)
}

// addIgnoreLine writes the daemon's .gitignore with pattern in it (and itself, so it never shows either).
func addIgnoreLine(ignore string, existing []byte, pattern string) error {
	lines := []string{ignoreMarker, "/.gitignore"}
	if len(existing) > 0 {
		lines = strings.Split(strings.TrimRight(string(existing), "\n"), "\n")
	}
	for _, l := range lines {
		if l == pattern {
			return nil
		}
	}
	lines = append(lines, pattern)
	return fsutil.WriteAtomic(ignore, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
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

// clearAgyMeshHooks removes the daemon's `"agent-mesh"` hook entry from worktree/.agents/hooks.json before
// opening a stopped agy agent outside any run (`mesh open`), so the user's session isn't held by read-only
// `--check-only` rules or mesh hooks. Repo hooks in other keys (or tracked repo hooks) stay intact.
func clearAgyMeshHooks(ctx context.Context, worktree string) error {
	if worktree == "" {
		return nil
	}
	slashRel := ".agents/hooks.json"
	hooksPath := filepath.Join(worktree, filepath.FromSlash(slashRel))
	raw, err := os.ReadFile(hooksPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	obj, ok := decodeJSONObject(raw)
	if !ok {
		return nil
	}
	if _, hasMesh := obj["agent-mesh"]; !hasMesh {
		return nil
	}
	delete(obj, "agent-mesh")
	tracked := exec.CommandContext(ctx, "git", "-C", worktree, "ls-files", "--error-unmatch", "--", slashRel).Run() == nil
	if tracked {
		if len(obj) == 0 {
			_, _ = git(ctx, worktree, "update-index", "--no-skip-worktree", "--", slashRel)
			_, err = git(ctx, worktree, "checkout", "--", slashRel)
			return err
		}
		b, err := json.MarshalIndent(obj, "", "  ")
		if err != nil {
			return err
		}
		return fsutil.WriteAtomic(hooksPath, append(b, '\n'), 0o600)
	}
	if len(obj) == 0 {
		if err := os.Remove(hooksPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		ignorePath := filepath.Join(worktree, ".agents", ".gitignore")
		if ig, err := os.ReadFile(ignorePath); err == nil && strings.HasPrefix(string(ig), ignoreMarker+"\n") {
			_ = os.Remove(ignorePath)
			_ = os.Remove(filepath.Join(worktree, ".agents"))
		}
		return nil
	}
	b, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(hooksPath, append(b, '\n'), 0o600)
}
