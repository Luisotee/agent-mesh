package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"
	_ "modernc.org/sqlite"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	termv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/term/v1/termv1connect"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/opencode"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
	"github.com/Luisotee/agent-mesh/daemon/internal/termclient"
)

func TestOpenCodeInstalledChecksConfiguredAndLookPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "opencode")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	rt, err := newRuntime(AgentConfig{
		Binaries: map[string]string{"opencode": bin},
		LookPath: func(string) (string, bool) { return "", false },
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
		LookPath: func(string) (string, bool) { return "", false },
	})
	if err != nil {
		t.Fatal(err)
	}
	if (&Daemon{rt: rtMissing}).installed(opencode.Kind) {
		t.Fatal("installed(opencode) = true for missing configured binary")
	}

	rtInjectedMiss, err := newRuntime(AgentConfig{
		LookPath: func(string) (string, bool) { return "", false },
	})
	if err != nil {
		t.Fatal(err)
	}
	if (&Daemon{rt: rtInjectedMiss}).installed(opencode.Kind) {
		t.Fatal("installed(opencode) = true when LookPath reports missing")
	}

	rtInjectedHit, err := newRuntime(AgentConfig{
		LookPath: func(name string) (string, bool) { return bin, name == opencode.Kind },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !(&Daemon{rt: rtInjectedHit}).installed(opencode.Kind) {
		t.Fatal("installed(opencode) = false when LookPath resolves opencode")
	}

	rtPATH, err := newRuntime(AgentConfig{
		Env: map[string]string{"PATH": dir},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := (&Daemon{rt: rtPATH}).opencodeBinary(); !ok || got != bin {
		t.Fatalf("opencodeBinary via PATH = (%q, %v), want (%q, true)", got, ok, bin)
	}
}

func TestOpenCodeAPIKeysOnlyInheritedByOpenCodeAgents(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-openai-test")
	t.Setenv("OPENROUTER_API_KEY", "sk-openrouter-test")
	t.Setenv("OPENCODE_API_KEY", "sk-opencode-test")

	for _, k := range inheritedEnv {
		for _, secret := range opencodeInheritedEnv {
			if k == secret {
				t.Fatalf("%s must not be in global inheritedEnv", k)
			}
		}
	}

	rt, err := newRuntime(AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{rt: rt, paths: Paths{Dir: t.TempDir()}}

	ocEnv := d.agentEnv(&v1.Agent{Id: "a-oc", RunId: "r1", Kind: opencode.Kind}, "tok")
	for _, k := range opencodeInheritedEnv {
		if ocEnv[k] == "" {
			t.Errorf("opencode agentEnv missing %s", k)
		}
	}
	for _, kind := range []string{"claude", "agy"} {
		otherEnv := d.agentEnv(&v1.Agent{Id: "a-" + kind, RunId: "r1", Kind: kind}, "tok")
		for _, k := range opencodeInheritedEnv {
			if _, has := otherEnv[k]; has {
				t.Errorf("%s agentEnv must not inherit %s", kind, k)
			}
		}
	}
}

func TestOpenCodeSeenEvidenceScopedAndCleared(t *testing.T) {
	rt, err := newRuntime(AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{rt: rt}
	if d.seenOpenCodeEvidence("a1", "t1", "opencode/big-pickle", "pane:limit") {
		t.Fatal("unexpected seen evidence before mark")
	}
	d.markOpenCodeEvidence("a1", "t1", "opencode/big-pickle", "pane:limit")
	d.markOpenCodeEvidence("a2", "t2", "opencode/big-pickle", "pane:limit")

	if !d.seenOpenCodeEvidence("a1", "t1", "opencode/big-pickle", "pane:limit") {
		t.Fatal("expected marked evidence for a1/t1/big-pickle")
	}
	if d.seenOpenCodeEvidence("a1", "t3", "opencode/big-pickle", "pane:limit") {
		t.Fatal("evidence key must scope by terminal id")
	}
	if d.seenOpenCodeEvidence("a3", "t1", "opencode/big-pickle", "pane:limit") {
		t.Fatal("evidence key must scope by agent id")
	}
	if d.seenOpenCodeEvidence("a1", "t1", "opencode/deepseek-v4.1-flash-free", "pane:limit") {
		t.Fatal("evidence key must scope by model")
	}
	if d.seenOpenCodeEvidence("a1", "t1", "opencode/big-pickle", "") {
		t.Fatal("empty evidence must never be marked seen")
	}

	// Clearing a1 leaves a2 intact.
	d.clearOpenCodeEvidence("a1")
	if d.seenOpenCodeEvidence("a1", "t1", "opencode/big-pickle", "pane:limit") {
		t.Fatal("clearOpenCodeEvidence did not clear a1")
	}
	if !d.seenOpenCodeEvidence("a2", "t2", "opencode/big-pickle", "pane:limit") {
		t.Fatal("clearOpenCodeEvidence cleared unrelated agent a2")
	}

	// Clearing terminal t2 clears a2's t2 entry.
	d.clearOpenCodeTerminal("t2")
	if d.seenOpenCodeEvidence("a2", "t2", "opencode/big-pickle", "pane:limit") {
		t.Fatal("clearOpenCodeTerminal did not clear t2")
	}

	body := withOpenCodeBodyNote(opencode.Kind, "Review (read-only)")
	if body == "Review (read-only)" {
		t.Fatal("expected opencode body note to be appended")
	}
	if withOpenCodeBodyNote(opencode.Kind, body) != body {
		t.Fatal("withOpenCodeBodyNote should be idempotent")
	}
}

type stubListTerm struct {
	termv1connect.TermHostServiceClient
	terminals []*termv1.Terminal
}

func (s *stubListTerm) ListTerminals(_ context.Context, _ *connect.Request[termv1.ListTerminalsRequest]) (*connect.Response[termv1.ListTerminalsResponse], error) {
	return connect.NewResponse(&termv1.ListTerminalsResponse{Terminals: s.terminals}), nil
}

func TestOpenCodePollOnlyAdvancesLastOutputOnSuccess(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	dbPath := filepath.Join(dir, "opencode.db")
	now := time.Now().Truncate(time.Millisecond)
	rt, err := newRuntime(AgentConfig{
		Env:      map[string]string{"AGENT_MESH_OPENCODE_DB": dbPath},
		LookPath: func(string) (string, bool) { return "", false },
	})
	if err != nil {
		t.Fatal(err)
	}
	outTime := now.Add(2 * time.Second)
	rt.term = &termclient.Client{
		API: &stubListTerm{terminals: []*termv1.Terminal{{
			Id:         "t1",
			StartedAt:  timestamppb.New(now),
			LastOutput: timestamppb.New(outTime),
		}}},
	}
	d := &Daemon{
		store: st,
		paths: Paths{Dir: dir},
		rt:    rt,
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:   func() time.Time { return now.Add(3 * time.Second) },
	}

	ctx := context.Background()
	wt := filepath.Join(dir, "wt")
	if err := os.MkdirAll(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, nil, "", func(tx *store.Tx) error {
		if err := tx.PutRun(&v1.Run{Id: "r1", Status: v1.RunStatus_RUN_STATUS_OPEN, CreatedAt: timestamppb.New(now)}); err != nil {
			return err
		}
		return tx.PutAgent(&v1.Agent{
			Id: "a1", RunId: "r1", Name: "repo-oc", Kind: opencode.Kind, Role: v1.AgentRole_AGENT_ROLE_WORKER,
			Model: "opencode/big-pickle", Worktree: wt, TerminalId: "t1", SessionId: "ses_1",
			State: v1.AgentState_AGENT_STATE_WORKING, CreatedAt: timestamppb.New(now), LastActivity: timestamppb.New(now),
		})
	}); err != nil {
		t.Fatal(err)
	}

	// 1. opencode.db does not exist yet: InspectSession returns os.ErrNotExist, so ps.lastOutput must NOT advance.
	d.pollOpenCode(ctx)
	d.rt.openCode.mu.Lock()
	ps := d.rt.openCode.polled["a1"]
	if ps == nil || !ps.lastOutput.IsZero() {
		d.rt.openCode.mu.Unlock()
		t.Fatalf("lastOutput advanced despite DB error: %+v", ps)
	}
	d.rt.openCode.mu.Unlock()

	// 2. applyOpenCodeEvents returns an error when store.Write fails (canceled ctx).
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := d.applyOpenCodeEvents(canceled, "a1", "t1", []*adapterv1.AgentEvent{{
		Time: timestamppb.New(now),
		Kind: &adapterv1.AgentEvent_TurnEnded{TurnEnded: &adapterv1.TurnEnded{}},
	}}); err == nil {
		t.Fatal("applyOpenCodeEvents succeeded with canceled context")
	}

	// 3. Once opencode.db appears with a completed turn, the next poll (with the SAME tm.LastOutput!) transitions
	// WORKING -> IDLE and advances ps.lastOutput.
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL, parent_id TEXT, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL);
		CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL);
	`)
	if err != nil {
		t.Fatal(err)
	}
	msgAt := now.Add(time.Second).UnixMilli()
	raw, _ := json.Marshal(map[string]any{
		"role": "assistant", "providerID": "opencode", "modelID": "big-pickle", "finish": "stop",
		"time": map[string]any{"created": msgAt, "completed": msgAt + 50},
	})
	if _, err := db.Exec(`INSERT INTO session VALUES ('ses_1', ?, NULL, ?, ?)`, wt, msgAt, msgAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO message VALUES ('m1', 'ses_1', ?, ?, ?)`, msgAt, msgAt+50, string(raw)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	d.pollOpenCode(ctx)
	a, err := st.Read(ctx).GetAgent("a1")
	if err != nil {
		t.Fatal(err)
	}
	if a.State != v1.AgentState_AGENT_STATE_IDLE {
		t.Fatalf("agent state = %v, want IDLE after DB became readable", a.State)
	}
	d.rt.openCode.mu.Lock()
	gotLast := d.rt.openCode.polled["a1"].lastOutput
	d.rt.openCode.mu.Unlock()
	if !gotLast.Equal(outTime) {
		t.Fatalf("lastOutput = %v, want %v after successful poll", gotLast, outTime)
	}
}
