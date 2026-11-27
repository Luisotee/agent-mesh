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

// ignoredEntries lists what a worktree holds that git ignores and `.worktreeinclude` does not bring back (an archived
// worktree is made again from its branch, so these are what archiving deletes): a file, or a whole ignored folder (`node_modules/`),
// with the bytes in it, largest first, at most maxIgnoredListed, and how many more there are. A failure to read gives nothing:
// this is information for the user, and what archiving refuses on is read elsewhere.
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
	restored := includedEntries(ctx, repo, worktree)
	var out []*v1.IgnoredEntry
	var unsized int32
	for _, p := range entries {
		if restoredByInclude(p, restored) {
			continue
		}
		if len(out) >= maxIgnoredEntries || ctx.Err() != nil {
			unsized++
			continue
		}
		e := &v1.IgnoredEntry{Path: p}
		e.Bytes, e.Partial = sizeOf(ctx, filepath.Join(worktree, filepath.FromSlash(p)))
		out = append(out, e)
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

// includedEntries is what the repo's `.worktreeinclude` names that the worktree holds untracked (a file, or a whole folder with a
// trailing slash): those a worktree made again gets back from the repo, up to the cap.
func includedEntries(ctx context.Context, repo, worktree string) []string {
	list := filepath.Join(repo, worktreeIncludeFile)
	if st, err := os.Lstat(list); err != nil || !st.Mode().IsRegular() || st.Size() > worktreeIncludeMaxFile {
		return nil
	}
	raw, _, _, err := gitRORaw(ctx, worktree, "ls-files", "-z", "--others", "--ignored", "--directory", "--exclude-from="+list)
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range strings.Split(string(raw), "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// restoredByInclude says whether an ignored entry is brought back by the list: it is named, or lies inside a folder the list names whole.
func restoredByInclude(entry string, included []string) bool {
	for _, in := range included {
		if entry == in || strings.HasSuffix(in, "/") && strings.HasPrefix(entry, in) {
			return true
		}
	}
	return false
}

// sizeOf is the bytes of the files under a path (a symlink counts as its own size and is never followed), counted up to maxIgnoredWalk files and
// the time left: `partial` says the count stopped.
func sizeOf(ctx context.Context, path string) (int64, bool) {
	st, err := os.Lstat(path)
	if err != nil {
		return 0, false
	}
	if !st.IsDir() {
		return st.Size(), false
	}
	var total int64
	files := 0
	partial := false
	_ = filepath.WalkDir(path, func(p string, de fs.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil {
			partial = partial || ctx.Err() != nil
			return nil
		}
		// A symlink counts as itself (WalkDir doesn't follow it): its target may be anywhere, outside the worktree too.
		if de.Type().IsRegular() || de.Type()&fs.ModeSymlink != 0 {
			if files++; files > maxIgnoredWalk {
				partial = true
				return filepath.SkipAll
			}
			if info, err := de.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total, partial
}
