// Package shortdir makes the temp dirs a test starts a daemon in. t.TempDir puts the test's name in the path, and a
// long TMPDIR plus a long test name takes a unix socket path (about 107 bytes) over its limit: meshd then refuses
// to start ("timed out waiting for meshd.json"). These dirs sit under /tmp with a short name of their own, so a
// suite passes with any TMPDIR.
package shortdir

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var (
	probeOnce   sync.Once
	tmpWritable bool // whether /tmp can be written does not change in a test binary's life
)

// EnvBase names the dir the short dirs go under, when it is set and exists: a gate (daemon/scripts/check.sh) makes
// one and removes it when it ends, so what a killed or timed-out run left goes with it.
// Its name is outside AGENT_MESH_*, which cleanenv.Agent() unsets in the suites' TestMains.
const EnvBase = "MESH_TEST_SHORT_TMP"

// Base is where the short dirs go: $MESH_TEST_SHORT_TMP when it names a dir, else /tmp when it can be written,
// else the OS temp dir (read each time, so a test that sets TMPDIR is followed).
func Base() string {
	if d := os.Getenv(EnvBase); d != "" {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d
		}
	}
	probeOnce.Do(func() {
		if f, err := os.CreateTemp("/tmp", "am-probe"); err == nil {
			f.Close()
			os.Remove(f.Name())
			tmpWritable = true
		}
	})
	if tmpWritable {
		return "/tmp"
	}
	return os.TempDir()
}

// marker is the file every dir Make creates holds: what Sweep removes is told by it, whatever the dir is called.
const marker = ".shortdir"

// Make is os.MkdirTemp under Base, for a test that removes the dir itself (keep the prefix short). The dir holds a
// marker file, which is how Sweep knows it as the suites' own.
func Make(prefix string) (string, error) {
	dir, err := os.MkdirTemp(Base(), prefix)
	if err != nil {
		return "", err
	}
	if f, err := os.Create(filepath.Join(dir, marker)); err == nil {
		f.Close()
	}
	return dir, nil
}

// binsPatterns are the dirs the TestMains make with os.MkdirTemp (the built fakes, 54 MB each): their names are
// their own.
var binsPatterns = []string{"meshd-bins-*", "meshd-e2e-bins-*"}

// Sweep removes the dirs a killed or timed-out run left (its cleanups never ran) that are older than maxAge, from Base
// the OS temp dir and /tmp: every dir Make made (it holds the marker file) and the built-binaries dirs. The age is what
// keeps a run in progress in another process safe: pass more than the longest suite takes.
func Sweep(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	seen := map[string]bool{}
	for _, base := range []string{Base(), os.TempDir(), "/tmp"} { // /tmp too: a gate's own dirs are not where a killed hand-run left its
		if seen[base] {
			continue
		}
		seen[base] = true
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			n := filepath.Join(base, e.Name())
			ours := false
			for _, pat := range binsPatterns {
				if ok, _ := filepath.Match(pat, e.Name()); ok {
					ours = true
				}
			}
			if !ours {
				if _, err := os.Lstat(filepath.Join(n, marker)); err != nil {
					continue
				}
			}
			if st, err := os.Lstat(n); err == nil && st.ModTime().Before(cutoff) {
				os.RemoveAll(n)
			}
		}
	}
}

// New is a dir with a short path (empty but for its marker file), removed when the test ends (after the cleanups registered later, as
// t.TempDir's is).
func New(t testing.TB) string {
	t.Helper()
	dir, err := Make("am")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Retried: a process the test started may still be writing as its dir goes (the terminal host's last file).
		var err error
		for try := 0; try < 40; try++ {
			if err = os.RemoveAll(dir); err == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Logf("could not remove %s: %v", dir, err)
	})
	return dir
}

// State is a state dir for a meshd in a short dir of its own: the dir itself does not exist yet, as meshd makes it.
func State(t testing.TB) string {
	t.Helper()
	return filepath.Join(New(t), "state")
}
