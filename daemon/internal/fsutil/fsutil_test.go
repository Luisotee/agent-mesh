package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSecrets(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tok")
	if err := WriteAtomic(p, []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := ReadSecret(p); err != nil || s != "s3cret" {
		t.Errorf("%q, %v", s, err)
	}
	os.WriteFile(p, nil, 0o600)
	if _, err := ReadSecret(p); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an empty secret: %v", err)
	}
	os.Chmod(p, 0o644)
	if _, err := ReadSecret(p); err == nil {
		t.Error("a world-readable secret was read")
	}
}
