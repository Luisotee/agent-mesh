package shortdir

import (
	"os"
	"strings"
	"testing"
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
