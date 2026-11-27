package daemon

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

const (
	// maxIgnoredListed is how many ignored entries one answer lists; the rest are counted.
	maxIgnoredListed = 20
	// maxIgnoredEntries bounds how many entries are sized at all (a worktree with thousands of ignored files in a folder that
	// is only partly ignored): the rest are counted unsized.
	maxIgnoredEntries = 1000
	// maxIgnoredWalk bounds the files counted in one folder; past it the entry says `partial`.
	maxIgnoredWalk = 20000
	// ignoredBudget bounds the time spent sizing, whatever the worktree holds.
	ignoredBudget = 5 * time.Second
)

// ignoredEntries lists what a worktree holds that git ignores and a restore would not give back (an archived worktree is made again
// from its branch, and `.worktreeinclude` copies files from the repo's own checkout): a file, or a whole ignored folder
// (`node_modules/`), with the bytes that would be lost, largest first, at most maxIgnoredListed, and how many more there are. A file
// the list names is safe only when the repo holds the same file (the same bytes): the worktree's own copy of a `.env` that the repo
// lacks, or that the agent edited, is lost. A failure to read gives nothing: this is information for the user, and what archiving
// refuses on is read elsewhere.
func ignoredEntries(ctx context.Context, repo, worktree string) ([]*v1.IgnoredEntry, int32) {
	ctx, cancel := context.WithTimeout(ctx, ignoredBudget)
	defer cancel()
	raw, _, _, err := gitRORaw(ctx, worktree, "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory")
	if err != nil {
		return nil, 0
	}
	var entries []string
	for _, p := range strings.Split(string(raw), "\x00") {
		if p != "" && unsafeRel(p) == "" {
			entries = append(entries, p)
		}
	}
	safe := restoredFiles(ctx, repo, worktree)
	var out []*v1.IgnoredEntry
	var unsized int32
	for _, p := range entries {
		if len(out) >= maxIgnoredEntries || ctx.Err() != nil {
			unsized++
			continue
		}
		bytes, lost, partial := lostIn(ctx, worktree, strings.TrimSuffix(p, "/"), safe)
		if lost == 0 && !partial {
			continue // all of it comes back
		}
		out = append(out, &v1.IgnoredEntry{Path: p, Bytes: bytes, Partial: partial})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Bytes != out[j].Bytes {
			return out[i].Bytes > out[j].Bytes
		}
		return out[i].Path < out[j].Path
	})
	more := unsized
	if len(out) > maxIgnoredListed {
		more += int32(len(out) - maxIgnoredListed)
		out = out[:maxIgnoredListed]
	}
	return out, more
}

// restoredFiles says whether a file of the worktree (its path from the worktree's root) comes back when the worktree is made again:
// the repo's `.worktreeinclude` names it, the repo has it as a regular file it ignores, and that file has the same bytes (a
// restore never copies a symlink, and gives the repo's copy, not the worktree's). Nil when the repo has no usable list.
func restoredFiles(ctx context.Context, repo, worktree string) func(rel string) bool {
	list := filepath.Join(repo, worktreeIncludeFile)
	if st, err := os.Lstat(list); err != nil || !st.Mode().IsRegular() || st.Size() > worktreeIncludeMaxFile {
		return nil
	}
	named, ok := repoIgnoredByList(ctx, repo, list, func(string, ...any) {})
	if !ok || len(named) == 0 {
		return nil
	}
	return func(rel string) bool {
		covered := false
		for _, in := range named {
			if rel == in || strings.HasSuffix(in, "/") && strings.HasPrefix(rel, in) {
				covered = true
				break
			}
		}
		return covered && sameFile(filepath.Join(repo, filepath.FromSlash(rel)), filepath.Join(worktree, filepath.FromSlash(rel)))
	}
}

// sameFile says whether two paths are regular files with the same bytes (compared whole up to 1 MB; a larger one by its size).
func sameFile(a, b string) bool {
	sa, err := os.Lstat(a)
	if err != nil || !sa.Mode().IsRegular() {
		return false
	}
	sb, err := os.Lstat(b)
	if err != nil || !sb.Mode().IsRegular() || sa.Size() != sb.Size() {
		return false
	}
	if sa.Size() > 1<<20 {
		return true
	}
	ba, err1 := os.ReadFile(a)
	bb, err2 := os.ReadFile(b)
	return err1 == nil && err2 == nil && string(ba) == string(bb)
}

// lostIn is the bytes, and how many files, under a path of the worktree that a restore does not give back (`safe` says which do; nil:
// none do). A symlink counts as itself and is never followed. The count stops at maxIgnoredWalk files, at the time budget, or at
// a folder that can't be read: `partial` says so.
func lostIn(ctx context.Context, worktree, rel string, safe func(string) bool) (bytes int64, lost int, partial bool) {
	path := filepath.Join(worktree, filepath.FromSlash(rel))
	st, err := os.Lstat(path)
	if err != nil {
		return 0, 0, false
	}
	if !st.IsDir() {
		if safe != nil && safe(rel) {
			return 0, 0, false
		}
		return st.Size(), 1, false
	}
	files := 0
	_ = filepath.WalkDir(path, func(p string, de fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			partial = true
			return filepath.SkipAll
		}
		if err != nil {
			partial = true
			return nil
		}
		// A symlink counts as itself (WalkDir doesn't follow it): its target may be anywhere, outside the worktree too.
		if de.Type().IsRegular() || de.Type()&fs.ModeSymlink != 0 {
			if files++; files > maxIgnoredWalk {
				partial = true
				return filepath.SkipAll
			}
			if safe != nil && safe(relTo(worktree, p)) {
				return nil
			}
			if info, err := de.Info(); err == nil {
				bytes += info.Size()
				lost++
			}
		}
		return nil
	})
	return bytes, lost, partial
}
