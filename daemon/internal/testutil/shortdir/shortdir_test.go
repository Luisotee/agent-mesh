package shortdir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Luisotee/agent-mesh/daemon/internal/testutil/cleanenv"
)

func TestADirIsShortWhateverTMPDIRAndTheTestNameAre(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir()+"/"+strings.Repeat("a-very-long-name", 8))
	dir := State(t)
	if len(dir) > 40 {
		t.Fatalf("%q is %d bytes", dir, len(dir))
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the state dir should not exist yet: %v", err)
	}
}

func TestADirIsRemovedWhenTheTestEnds(t *testing.T) {
	var dir string
	t.Run("inner", func(t *testing.T) {
		dir = New(t)
		if err := os.WriteFile(dir+"/f", nil, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("%s still exists: %v", dir, err)
	}
}

func TestADirFollowsTheGatesBase(t *testing.T) {
	base := t.TempDir()
	t.Setenv(EnvBase, base)
	if dir := New(t); filepath.Dir(dir) != base {
		t.Fatalf("%q is not under %q", dir, base)
	}
	t.Setenv(EnvBase, filepath.Join(base, "gone"))
	if Base() == filepath.Join(base, "gone") {
		t.Fatal("a base that does not exist is used")
	}
}

// A killed run leaves its dirs; the next start removes the old ones and leaves the young (a run in progress) and
// anybody else's, however they are named.
func TestSweepRemovesOnlyOldLeftovers(t *testing.T) {
	base := t.TempDir()
	t.Setenv(EnvBase, base)
	t.Setenv("TMPDIR", base)
	made := func(prefix string) string {
		d, err := Make(prefix)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	old, oldOdd, young := made("am"), made("ibx"), made("reopen-")
	oldBins, stranger, strangerAm := filepath.Join(base, "meshd-bins-789"), filepath.Join(base, "keepme"), filepath.Join(base, "am999")
	for _, d := range []string{oldBins, stranger, strangerAm} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-5 * time.Hour)
	for _, d := range []string{old, oldOdd, oldBins, stranger, strangerAm} {
		os.Chtimes(d, past, past)
	}
	Sweep(3 * time.Hour)
	for d, want := range map[string]bool{old: false, oldOdd: false, oldBins: false, young: true, stranger: true, strangerAm: true} {
		if _, err := os.Stat(d); (err == nil) != want {
			t.Errorf("%s: exists=%v, want %v", filepath.Base(d), err == nil, want)
		}
	}
}

// The suites' TestMains clear AGENT_MESH_* before they sweep and make dirs: the gate's base must survive that.
func TestTheGatesBaseSurvivesCleanenv(t *testing.T) {
	base := t.TempDir()
	t.Setenv(EnvBase, base)
	cleanenv.Agent()
	if Base() != base {
		t.Fatalf("Base() = %q after cleanenv.Agent(), want %q", Base(), base)
	}
}
