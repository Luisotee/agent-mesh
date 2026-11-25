// Package fsutil holds the small file-system primitives the daemon and the terminal host share: atomic writes,
// a held lock, and secrets kept in 0600 files.
package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// WriteAtomic writes data to path through a temp file and a rename, so a reader never sees a partial file.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	// The rename itself must survive a crash too.
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		dir.Sync()
		dir.Close()
	}
	return nil
}

// Lock is an exclusive flock held for the life of a process: liveness and "one instance" in one.
type Lock struct {
	f *os.File
}

// ErrLocked means another live process holds the lock.
var ErrLocked = errors.New("lock held by another process")

// TryLock takes the lock at path without waiting.
func TryLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Held reports whether a lock at path is held by some process, without taking it.
func Held(path string) bool {
	l, err := TryLock(path)
	if err != nil {
		return errors.Is(err, ErrLocked)
	}
	l.Release()
	return false
}

// Release drops the lock.
func (l *Lock) Release() {
	if l != nil && l.f != nil {
		l.f.Close()
		l.f = nil
	}
}

// ReadSecret reads a secret file, refusing one that others can read. An empty file (a crash while it was first
// written) reads as missing, so it is made again.
func ReadSecret(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is readable by others (mode %v): refusing it", path, st.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("%s is empty: %w", path, os.ErrNotExist)
	}
	return s, nil
}

// EnsureDir makes dir with mode 0700 if it is missing.
func EnsureDir(dir string) error {
	return os.MkdirAll(dir, 0o700)
}
