package tlsutil

import (
	"crypto/tls"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreatePersistsAcrossRestartsAndRefusesPartial(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	cfg1, fp1, err := LoadOrCreate(certPath, keyPath, "h-test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fp1, "sha256:") || len(fp1) != len("sha256:")+64 {
		t.Fatalf("unexpected fingerprint %q", fp1)
	}
	if len(cfg1.Certificates) != 1 {
		t.Fatalf("expected 1 cert, got %d", len(cfg1.Certificates))
	}
	for _, p := range []string{certPath, keyPath} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", p, st.Mode().Perm())
		}
	}

	// Second load reuses the same keypair and fingerprint.
	_, fp2, err := LoadOrCreate(certPath, keyPath, "h-test")
	if err != nil {
		t.Fatal(err)
	}
	if fp1 != fp2 {
		t.Errorf("fingerprint changed across reload: %s vs %s", fp1, fp2)
	}

	// Missing one half refuses to regenerate.
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreate(certPath, keyPath, "h-test"); err == nil {
		t.Fatal("expected error when key is missing while cert exists")
	}

	// Corrupt pair refuses to regenerate.
	if err := os.WriteFile(keyPath, []byte("not-a-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreate(certPath, keyPath, "h-test"); err == nil {
		t.Fatal("expected error when key is corrupt")
	}
}

func TestPinnedConfigAcceptsMatchAndRejectsMismatch(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	srvCfg, fp1, err := LoadOrCreate(filepath.Join(dir1, "tls.crt"), filepath.Join(dir1, "tls.key"), "h1")
	if err != nil {
		t.Fatal(err)
	}
	_, fp2, err := LoadOrCreate(filepath.Join(dir2, "tls.crt"), filepath.Join(dir2, "tls.key"), "h2")
	if err != nil {
		t.Fatal(err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.(*tls.Conn).Handshake()
			}(c)
		}
	}()

	goodCfg, err := PinnedConfig(fp1, "h2", "http/1.1")
	if err != nil {
		t.Fatal(err)
	}
	c, err := tls.Dial("tcp", ln.Addr().String(), goodCfg)
	if err != nil {
		t.Fatalf("pinned dial with matching fp failed: %v", err)
	}
	c.Close()

	badCfg, err := PinnedConfig(fp2, "h2", "http/1.1")
	if err != nil {
		t.Fatal(err)
	}
	if c, err := tls.Dial("tcp", ln.Addr().String(), badCfg); err == nil {
		c.Close()
		t.Fatal("pinned dial succeeded with wrong fingerprint")
	} else if !errors.Is(err, ErrPinMismatch) {
		t.Fatalf("expected ErrPinMismatch, got %v", err)
	}

	if _, err := PinnedConfig("sha256:short"); err == nil {
		t.Fatal("expected error for invalid fingerprint")
	}
}
