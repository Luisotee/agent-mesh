package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// .worktreeinclude: a file at the top of a repo that lists, in .gitignore syntax, the git-ignored files a new worktree
// should get a copy of (a `.env`, a local config, a seeded database): a worktree is a fresh checkout and has none of
// them. Only files git ignores are copied (a tracked file is in the worktree already, and an unignored untracked one
// is somebody's work in progress), regular files only, and nothing that resolves outside the repo.

const (
	worktreeIncludeFile = ".worktreeinclude"
	// worktreeIncludeMaxFile bounds the list itself.
	worktreeIncludeMaxFile = 64 << 10
	// worktreeIncludeMaxFiles bounds how many files one worktree gets, whatever they weigh.
	worktreeIncludeMaxFiles = 5000
	// worktreeIncludeMB is the default `worktree.include_max_mb`: the most one worktree gets copied in total.
	worktreeIncludeMB = 50
)

// WorktreeInclude is what copying the files of .worktreeinclude did.
type WorktreeInclude struct {
	Copied  int
	Bytes   int64
	Skipped []string // what was left out, with the reason
}

// copyWorktreeInclude copies the repo's .worktreeinclude files into a worktree that was just made from it, within
// maxBytes. A failure to read the list or a file is reported in Skipped and never fails the worktree.
func copyWorktreeInclude(ctx context.Context, repo, worktree string, maxBytes int64) WorktreeInclude {
	var out WorktreeInclude
	skip := func(format string, a ...any) { out.Skipped = append(out.Skipped, fmt.Sprintf(format, a...)) }
	if maxBytes <= 0 {
		return out // a cap of 0 copies nothing, not even an empty file
	}
	list := filepath.Join(repo, worktreeIncludeFile)
	st, err := os.Lstat(list)
	if err != nil {
		return out // no list: nothing to do
	}
	if !st.Mode().IsRegular() || st.Size() > worktreeIncludeMaxFile {
		skip("%s is not a regular file under %d KB", worktreeIncludeFile, worktreeIncludeMaxFile>>10)
		return out
	}
	root, err := filepath.EvalSymlinks(repo)
	if err != nil {
		skip("%s: %v", repo, err)
		return out
	}
	// The files git would ignore by the list's patterns (a directory that matches whole comes as one entry), then only
	// those the repo's own ignore rules cover too.
	raw, _, _, err := gitRORaw(ctx, repo, "ls-files", "-z", "--others", "--ignored", "--directory", "--exclude-from="+list)
	if err != nil {
		skip("git ls-files: %v", err)
		return out
	}
	var candidates []string
	for _, p := range strings.Split(string(raw), "\x00") {
		if p != "" && unsafeRel(p) == "" {
			candidates = append(candidates, p)
		}
	}
	ignored, err := gitIgnored(ctx, repo, candidates)
	if err != nil {
		skip("git check-ignore: %v", err)
		return out
	}
	// A directory the list names whole but the repo ignores only in part (`config/.env` is ignored, `config/` is not): its files
	// are looked at one by one.
	isIgnored := map[string]bool{}
	for _, p := range ignored {
		isIgnored[p] = true
	}
	for _, c := range candidates {
		if !strings.HasSuffix(c, "/") || isIgnored[c] {
			continue
		}
		inside, _, _, err := gitRORaw(ctx, repo, "ls-files", "-z", "--others", "--ignored", "--exclude-from="+list, "--", c)
		if err != nil {
			skip("git ls-files %s: %v", c, err)
			continue
		}
		var files []string
		for _, p := range strings.Split(string(inside), "\x00") {
			if p != "" && unsafeRel(p) == "" && len(files) < worktreeIncludeMaxFiles {
				files = append(files, p)
			}
		}
		more, err := gitIgnored(ctx, repo, files)
		if err != nil {
			skip("git check-ignore %s: %v", c, err)
			continue
		}
		ignored = append(ignored, more...)
	}
	sort.Strings(ignored)
	// Everything is written through a handle on the worktree that refuses a path leaving it: the checked-out branch may hold a
	// symlink where the repo has a directory, and a write through it would land wherever it points.
	dest, err := os.OpenRoot(worktree)
	if err != nil {
		skip("%s: %v", worktree, err)
		return out
	}
	defer dest.Close()
	for _, rel := range ignored {
		walkInclude(root, dest, strings.TrimSuffix(rel, "/"), maxBytes, &out, skip)
	}
	return out
}

// unsafeRel says why a path git listed can't be used ("" when it can): absolute, or climbing out of the repo.
func unsafeRel(p string) string {
	c := filepath.Clean(filepath.FromSlash(p))
	if filepath.IsAbs(c) || c == ".." || strings.HasPrefix(c, ".."+string(filepath.Separator)) {
		return "outside the repo"
	}
	return ""
}

// gitIgnored keeps the paths the repo's ignore rules cover.
func gitIgnored(ctx context.Context, repo string, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "check-ignore", "-z", "--stdin")
	cmd.Env = gitROEnv()
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if ok := asExit(err, &ee); ok && ee.ExitCode() == 1 {
			return nil, nil // none of them is ignored
		}
		return nil, err
	}
	var out []string
	for _, p := range strings.Split(stdout.String(), "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

func asExit(err error, ee **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*ee = e
	}
	return ok
}

// walkInclude copies one listed path: a file, or every regular file under a directory. A symlink is never followed or copied,
// and a file whose real path leaves the repo is left out.
func walkInclude(root string, dest *os.Root, rel string, maxBytes int64, out *WorktreeInclude, skip func(string, ...any)) {
	src := filepath.Join(root, filepath.FromSlash(rel))
	_ = filepath.WalkDir(src, func(path string, de fs.DirEntry, err error) error {
		if err != nil {
			skip("%s: %v", rel, err)
			return nil
		}
		if de.Type()&fs.ModeSymlink != 0 {
			skip("%s: a symlink is not copied", relTo(root, path))
			return nil
		}
		if de.IsDir() {
			if de.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !de.Type().IsRegular() {
			return nil
		}
		r := relTo(root, path)
		if real, err := filepath.EvalSymlinks(path); err != nil || !insideDir(root, real) {
			skip("%s: resolves outside the repo", r)
			return nil
		}
		info, err := de.Info()
		if err != nil {
			skip("%s: %v", r, err)
			return nil
		}
		if out.Copied >= worktreeIncludeMaxFiles {
			skip("%s: more than %d files", r, worktreeIncludeMaxFiles)
			return filepath.SkipAll
		}
		if out.Bytes+info.Size() > maxBytes {
			skip("%s: over the %d MB cap", r, maxBytes>>20)
			return nil
		}
		to := filepath.FromSlash(r)
		if _, err := dest.Lstat(to); err == nil {
			return nil // the checkout has it already (a tracked file): never overwritten
		} else if !errors.Is(err, fs.ErrNotExist) {
			skip("%s: %v", r, err) // a path through a symlink or the like
			return nil
		}
		n, err := copyOne(path, dest, to, info.Mode().Perm(), maxBytes-out.Bytes)
		if err != nil {
			skip("%s: %v", r, err)
			return nil
		}
		out.Copied++
		out.Bytes += n
		return nil
	})
}

func relTo(root, path string) string {
	r, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(r)
}

func insideDir(root, p string) bool {
	r, err := filepath.Rel(root, p)
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}

// copyOne copies a file to `to`, inside dest only, creating nothing that exists, and at most `room` bytes: a file that grew since
// it was measured is left out, not truncated.
func copyOne(from string, dest *os.Root, to string, mode fs.FileMode, room int64) (int64, error) {
	if dir := filepath.Dir(to); dir != "." {
		if err := dest.MkdirAll(dir, 0o700); err != nil {
			return 0, err
		}
	}
	in, err := os.Open(from)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	f, err := dest.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, io.LimitReader(in, room+1))
	if err == nil && n > room {
		err = fmt.Errorf("grew past the cap while it was copied")
	}
	if err != nil {
		f.Close()
		_ = dest.Remove(to)
		return 0, err
	}
	return n, f.Close()
}

// includeInto copies the repo's .worktreeinclude files into a worktree just made, with the cap of `worktree.include_max_mb`, and logs
// what it did. Never fails the caller.
func (d *Daemon) includeInto(ctx context.Context, repo, worktree string) {
	max := int64(worktreeIncludeMB)
	if v, err := d.Setting(d.store.Read(ctx), "worktree.include_max_mb"); err == nil {
		if f, ok := v.(float64); ok && f >= 0 {
			max = int64(f)
		}
	}
	res := copyWorktreeInclude(ctx, repo, worktree, max<<20)
	if res.Copied == 0 && len(res.Skipped) == 0 {
		return
	}
	d.log.Info("worktree include", "worktree", worktree, "copied", res.Copied, "bytes", res.Bytes, "skipped", strings.Join(res.Skipped, "; "))
}
