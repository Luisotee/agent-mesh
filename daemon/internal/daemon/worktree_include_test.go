package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
)

func writeFile(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

// A new worktree gets a copy of the git-ignored files .worktreeinclude lists, and nothing else: not a file git does not
// ignore, not a symlink, not what would leave the repo, not past the cap.
func TestWorktreeIncludeCopiesOnlyIgnoredRegularFilesInsideTheRepo(t *testing.T) {
	repo := gitRepo(t)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeFile(t, outside, "not yours", 0o644)
	writeFile(t, filepath.Join(repo, ".gitignore"), ".env\n.env.*\nsecrets/\nbuild/\nnode_modules/\n", 0o644)
	writeFile(t, filepath.Join(repo, ".worktreeinclude"), "# local files\n.env\n.env.*\nsecrets/\nnotes.txt\nlink.txt\n", 0o644)
	writeFile(t, filepath.Join(repo, "tracked.txt"), "tracked", 0o644)
	gitIn(t, repo, "add", ".gitignore", ".worktreeinclude", "tracked.txt")
	gitIn(t, repo, "commit", "-q", "-m", "files")
	writeFile(t, filepath.Join(repo, ".env"), "TOKEN=1", 0o600)
	writeFile(t, filepath.Join(repo, ".env.local"), "LOCAL=1", 0o600)
	writeFile(t, filepath.Join(repo, "secrets", "key.pem"), "key", 0o600)
	writeFile(t, filepath.Join(repo, "secrets", "deep", "more.txt"), "more", 0o644)
	writeFile(t, filepath.Join(repo, "notes.txt"), "not ignored by git: somebody's work", 0o644) // listed, but git does not ignore it
	writeFile(t, filepath.Join(repo, "build", "out.bin"), "build output", 0o644)                 // ignored, but not listed
	if err := os.Symlink(outside, filepath.Join(repo, "secrets", "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, ".env.link")); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	gitIn(t, repo, "worktree", "add", "-q", "-b", "feature", wt)

	res := daemon.CopyWorktreeInclude(context.Background(), repo, wt, 1<<20)
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(wt, rel))
		if err != nil {
			return "<missing>"
		}
		return string(b)
	}
	for rel, want := range map[string]string{".env": "TOKEN=1", ".env.local": "LOCAL=1", "secrets/key.pem": "key", "secrets/deep/more.txt": "more"} {
		if got := read(rel); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
	for _, rel := range []string{"notes.txt", "build/out.bin", "secrets/escape", ".env.link"} {
		if got := read(rel); got != "<missing>" {
			t.Errorf("%s was copied: %q", rel, got)
		}
	}
	if st, err := os.Stat(filepath.Join(wt, ".env")); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf(".env mode: %v, %v", st, err)
	}
	if read("tracked.txt") != "tracked" {
		t.Errorf("the checkout's own file changed")
	}
	if res.Copied != 4 {
		t.Errorf("copied %d files, want 4", res.Copied)
	}
	if !strings.Contains(strings.Join(res.Skipped, "\n"), "a symlink is not copied") {
		t.Errorf("a symlink skipped without a word: %v", res.Skipped)
	}
}

func TestWorktreeIncludeStopsAtTheByteCapAndNeverOverwrites(t *testing.T) {
	repo := gitRepo(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), "data/\n", 0o644)
	writeFile(t, filepath.Join(repo, ".worktreeinclude"), "data/\n", 0o644)
	gitIn(t, repo, "add", ".gitignore", ".worktreeinclude")
	gitIn(t, repo, "commit", "-q", "-m", "files")
	writeFile(t, filepath.Join(repo, "data", "a.txt"), strings.Repeat("a", 600), 0o644)
	writeFile(t, filepath.Join(repo, "data", "b.txt"), strings.Repeat("b", 600), 0o644)
	wt := filepath.Join(t.TempDir(), "wt")
	gitIn(t, repo, "worktree", "add", "-q", "-b", "feature", wt)
	writeFile(t, filepath.Join(wt, "data", "a.txt"), "already here", 0o644)

	res := daemon.CopyWorktreeInclude(context.Background(), repo, wt, 1000)
	if b, _ := os.ReadFile(filepath.Join(wt, "data", "a.txt")); string(b) != "already here" {
		t.Errorf("an existing file was overwritten: %q", b)
	}
	// a.txt exists (not copied), b.txt fits; nothing is over the cap, so no skip is told either.
	if _, err := os.Stat(filepath.Join(wt, "data", "b.txt")); err != nil || res.Copied != 1 || res.Bytes != 600 {
		t.Errorf("copied %d files, %d bytes (b.txt: %v)", res.Copied, res.Bytes, err)
	}
	over := daemon.CopyWorktreeInclude(context.Background(), repo, filepath.Join(t.TempDir(), "other"), 700)
	if over.Copied != 1 || len(over.Skipped) != 1 || !strings.Contains(over.Skipped[0], "over the") {
		t.Errorf("over the cap: copied %d, skipped %v", over.Copied, over.Skipped)
	}
	if none := daemon.CopyWorktreeInclude(context.Background(), repo, filepath.Join(t.TempDir(), "none"), 0); none.Copied != 0 {
		t.Errorf("a cap of 0 copied %d files", none.Copied)
	}
}

func TestWorktreeIncludeIsNothingWithoutTheListOrWithAnUnusableOne(t *testing.T) {
	repo := gitRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	gitIn(t, repo, "worktree", "add", "-q", "-b", "feature", wt)
	if res := daemon.CopyWorktreeInclude(context.Background(), repo, wt, 1<<20); res.Copied != 0 || len(res.Skipped) != 0 {
		t.Errorf("no list: %+v", res)
	}
	target := filepath.Join(t.TempDir(), "list")
	writeFile(t, target, ".env\n", 0o644)
	if err := os.Symlink(target, filepath.Join(repo, ".worktreeinclude")); err != nil {
		t.Fatal(err)
	}
	if res := daemon.CopyWorktreeInclude(context.Background(), repo, wt, 1<<20); res.Copied != 0 || len(res.Skipped) != 1 {
		t.Errorf("a list that is a symlink: %+v", res)
	}
}
