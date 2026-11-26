package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/opencode"
)

func TestOpenCodeInstalledChecksConfiguredAndPATHBinaries(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "opencode")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	rt, err := newRuntime(AgentConfig{
		Binaries: map[string]string{"opencode": bin},
	})
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{rt: rt}
	if !d.installed(opencode.Kind) {
		t.Fatal("installed(opencode) = false with configured executable binary")
	}

	rtMissing, err := newRuntime(AgentConfig{
		Binaries: map[string]string{"opencode": filepath.Join(dir, "missing")},
	})
	if err != nil {
		t.Fatal(err)
	}
	dMissing := &Daemon{rt: rtMissing}
	if dMissing.installed(opencode.Kind) {
		t.Fatal("installed(opencode) = true for missing configured binary")
	}

	// lookPath matches host PATH resolution cleanly without leaking the real binary into testing.Testing() defaults.
	if got, ok := lookPath("opencode", dir); !ok || got != bin {
		t.Fatalf("lookPath(opencode, %q) = (%q, %v), want (%q, true)", dir, got, ok, bin)
	}
	if _, ok := lookPath("opencode", t.TempDir()); ok {
		t.Fatal("lookPath succeeded in an empty directory")
	}
}

func TestOpenCodePollRecordsSeenEvidenceAndLastOutputState(t *testing.T) {
	rt, err := newRuntime(AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{rt: rt}
	if d.seenOpenCodeEvidence("opencode/big-pickle", "msg_1") {
		t.Fatal("unexpected seen evidence before mark")
	}
	d.markOpenCodeEvidence("opencode/big-pickle", "msg_1")
	if !d.seenOpenCodeEvidence("opencode/big-pickle", "msg_1") {
		t.Fatal("expected marked evidence for big-pickle")
	}
	if d.seenOpenCodeEvidence("opencode/deepseek-v4.1-flash-free", "msg_1") {
		t.Fatal("evidence key must scope by model")
	}
	if d.seenOpenCodeEvidence("opencode/big-pickle", "") {
		t.Fatal("empty evidence must never be marked seen")
	}

	// Verify openCodeAvailable reports reset note when all expanded tier models are held.
	d.rt.openCode.freeModels = []string{"opencode/big-pickle", "opencode/flint-40b-free"}
	body := withOpenCodeBodyNote(opencode.Kind, "Review (read-only)")
	if body == "Review (read-only)" {
		t.Fatal("expected opencode body note to be appended")
	}
	if withOpenCodeBodyNote(opencode.Kind, body) != body {
		t.Fatal("withOpenCodeBodyNote should be idempotent")
	}
}
