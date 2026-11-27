// Package shortdir makes the temp dirs a test starts a daemon in. t.TempDir puts the test's name in the path, and a
// long TMPDIR plus a long test name takes a unix socket path (about 107 bytes) over its limit: meshd then refuses
// to start ("timed out waiting for meshd.json"). These dirs sit under /tmp with a short name of their own, so a
// suite passes with any TMPDIR.
package shortdir

import (
	"os"
	"path/filepath"
	"strings"
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
const EnvBase = "AGENT_MESH_SHORT_TMP"

// Base is where the short dirs go: $AGENT_MESH_SHORT_TMP when it names a dir, else /tmp when it can be written,
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

// leftovers are the names of the dirs the suites make under Base and the OS temp dir. A run that is killed or times
// out (the panic skips every cleanup) leaves them; Sweep removes the old ones at the next start.
var leftovers = []string{"am[0-9]*", "reopen-*", "meshd-bins-*", "meshd-e2e-bins-*"}

func pathExists(p string) bool { _, err := os.Lstat(p); return err == nil }

// Sweep removes the dirs a killed run left (see leftovers) that are older than maxAge, from Base and the OS temp
// dir. The age is what keeps a run in progress in another process safe: pass more than the longest suite takes.
func Sweep(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	seen := map[string]bool{}
	for _, base := range []string{Base(), os.TempDir()} {
		if seen[base] {
			continue
		}
		seen[base] = true
		for _, pat := range leftovers {
			names, _ := filepath.Glob(filepath.Join(base, pat))
			for _, n := range names {
				st, err := os.Lstat(n)
				if err != nil || !st.IsDir() || !st.ModTime().Before(cutoff) {
					continue
				}
				// A generic name (am123) is the suites' only if it holds what they put there.
				if strings.HasPrefix(filepath.Base(n), "am") && !pathExists(filepath.Join(n, "state")) {
					continue
				}
				os.RemoveAll(n)
			}
		}
	}
}

// Make is os.MkdirTemp under Base, for a test that removes the dir itself (keep the prefix short).
func Make(prefix string) (string, error) { return os.MkdirTemp(Base(), prefix) }

// New is an empty dir with a short path, removed when the test ends (after the cleanups registered later, as
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
