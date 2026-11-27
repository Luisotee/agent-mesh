package agy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// `agy models` is not run while the daemon holds agy (it needs a login): the cache answers, or an error does.
func TestAgyModelsIsNotRunWhileHeld(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")
	bin := filepath.Join(dir, "agy")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ntouch "+ran+"\nprintf 'gemini-4-neon-high\\tGemini 4 Neon (High)\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HOME": dir, "PATH": os.Getenv("PATH")}
	held := true
	notified := 0
	a := New()
	a.Hold = func() bool { return held }
	a.LoginNeeded = func() { notified++ }
	if _, err := a.listModelsDirect(context.Background(), bin, env, true); err == nil {
		t.Error("a held agy listed models")
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("agy was run while held")
	}
	held = false
	if _, err := a.listModelsDirect(context.Background(), bin, env, true); err != nil {
		t.Fatalf("a free agy: %v", err)
	}
	if _, err := os.Stat(ran); err != nil {
		t.Error("agy was not run when free")
	}
}

// `agy models` that answers clears the login record (agy is logged in); one that is held, or fails, does not.
func TestAgyModelsAnsweringClearsTheLoginRecord(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agy")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf 'gemini-4-neon-high\\tGemini 4 Neon (High)\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HOME": dir, "PATH": os.Getenv("PATH")}
	cleared := 0
	held := true
	a := New()
	a.Hold = func() bool { return held }
	a.LoginCleared = func() { cleared++ }
	if _, err := a.listModelsDirect(context.Background(), bin, env, true); err == nil || cleared != 0 {
		t.Fatalf("held: err %v, cleared %d", err, cleared)
	}
	held = false
	if _, err := a.listModelsDirect(context.Background(), bin, env, true); err != nil || cleared != 1 {
		t.Fatalf("answered: err %v, cleared %d", err, cleared)
	}
}
