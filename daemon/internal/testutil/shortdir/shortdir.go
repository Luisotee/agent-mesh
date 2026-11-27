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
)

var (
	baseOnce sync.Once
	base     string
)

// Base is where the short dirs go: /tmp when it can be written, else the OS temp dir.
func Base() string {
	baseOnce.Do(func() {
		base = os.TempDir()
		if fi, err := os.Stat("/tmp"); err == nil && fi.IsDir() {
			if f, err := os.CreateTemp("/tmp", "am-probe"); err == nil {
				f.Close()
				os.Remove(f.Name())
				base = "/tmp"
			}
		}
	})
	return base
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
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// State is a state dir for a meshd in a short dir of its own: the dir itself does not exist yet, as meshd makes it.
func State(t testing.TB) string {
	t.Helper()
	return filepath.Join(New(t), "state")
}
