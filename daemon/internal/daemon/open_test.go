package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/creack/pty"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/cli"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

func TestOpenAgentStoppedAndLiveAcrossCLIsAndEnforcesGuards(t *testing.T) {
	w := startReopen(t)
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)

	// Spawn a read-only Claude asker (strict deny list + hooks + skipPermissions) and an agy worker.
	askRes, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{
		Repo: gitRepo(t), Slug: "ask", Role: v1.AgentRole_AGENT_ROLE_ASKER, Kind: "claude", Tier: "mid", Task: "read only question",
	}))
	if err != nil {
		t.Fatal(err)
	}
	asker := w.waitState(askRes.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)

	// Non-device actor cannot call OpenAgent.
	if _, err := asLead.OpenAgent(ctx, connect.NewRequest(&v1.OpenAgentRequest{Agent: asker.Id})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("root OpenAgent: %v, want PermissionDenied", err)
	}

	// Live agent: refuses --model, --effort, and --extra-args.
	if _, err := w.device.Mesh.OpenAgent(ctx, connect.NewRequest(&v1.OpenAgentRequest{Agent: asker.Id, Model: "opus"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("live with --model: %v, want InvalidArgument", err)
	}
	if _, err := w.device.Mesh.OpenAgent(ctx, connect.NewRequest(&v1.OpenAgentRequest{Agent: asker.Id, ExtraArgs: []string{"--verbose"}})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("live with extra_args: %v, want InvalidArgument", err)
	}

	// Live agent attaches via terminal ticket.
	live, err := w.device.Mesh.OpenAgent(ctx, connect.NewRequest(&v1.OpenAgentRequest{Agent: asker.Name}))
	if err != nil {
		t.Fatal(err)
	}
	if live.Msg.Mode != v1.OpenMode_OPEN_MODE_ATTACH || !strings.HasPrefix(live.Msg.TerminalPath, "/v1/term/"+asker.Id+"?ticket=") {
		t.Fatalf("live open = %+v", live.Msg)
	}

	// CLI attach via OpenAgent + real PTY + Ctrl-] detach.
	ptm, tty, err := pty.Open()
	if err != nil {
		t.Skipf("pty.Open: %v", err)
	}
	defer ptm.Close()
	defer tty.Close()
	if err := pty.Setsize(tty, &pty.Winsize{Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	var attachErr syncBuffer
	attachDone := make(chan int, 1)
	go func() {
		attachDone <- cli.Main([]string{"--state", w.state, "open", asker.Name}, cli.Env{
			Stdin:  tty,
			Stdout: tty,
			Stderr: &attachErr,
			Getenv: func(string) string { return "" },
		}, nil)
	}()
	// Wait for attached output, then detach with Ctrl-] (0x1d).
	waitFor(t, "pty attachment banner", func() bool {
		return strings.Contains(attachErr.String(), "Attached to "+asker.Name)
	})
	drainStop := make(chan struct{})
	go func() {
		defer close(drainStop)
		_, _ = io.Copy(io.Discard, ptm)
	}()
	time.Sleep(100 * time.Millisecond)
	if _, err := ptm.Write([]byte{0x1d}); err != nil {
		t.Fatal(err)
	}
	select {
	case exitCode := <-attachDone:
		if exitCode != cli.ExitOK {
			t.Fatalf("attach CLI exit=%d stderr=%q", exitCode, attachErr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CLI open attach did not exit after Ctrl-]")
	}
	if !strings.Contains(attachErr.String(), "[detached]") {
		t.Errorf("expected [detached] in stderr: %q", attachErr.String())
	}

	// Stop the asker, remove its clean worktree via StopRun, and test CLI open in EXEC mode:
	// worktree is recreated and Claude launch omits hooks, deny lists, lean flags, and --dangerously-skip-permissions.
	agyWorker := w.spawnUnder(t, asLead, "ag", &v1.SpawnAgentRequest{Kind: "agy", Tier: "strong"})
	agyWorker = w.agyTurn(t, agyWorker, "hi")
	hooksPath := filepath.Join(agyWorker.Worktree, ".agents", "hooks.json")
	if _, err := os.Stat(hooksPath); err != nil {
		t.Fatalf("expected agy spawn to write %s: %v", hooksPath, err)
	}
	// Keep agyWorker's worktree dirty so StopRun keeps it along with its generated .agents/hooks.json.
	if err := os.WriteFile(filepath.Join(agyWorker.Worktree, "wip.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta("stop"), RunId: run.Id})); err != nil {
		t.Fatal(err)
	}
	stoppedAsker := w.waitState(asker.Id, v1.AgentState_AGENT_STATE_RETIRED)
	agyWorker = w.waitState(agyWorker.Id, v1.AgentState_AGENT_STATE_RETIRED)

	// Stopped agent with PendingReopen set must be refused just like a running reopening agent.
	if err := w.d.Store().Write(ctx, &v1.Actor{Id: "test", Class: v1.ActorClass_ACTOR_CLASS_DEVICE}, "", func(tx *store.Tx) error {
		cur, gerr := tx.GetAgent(stoppedAsker.Id)
		if gerr != nil {
			return gerr
		}
		cur.PendingReopen = &v1.Reopen{Id: "reop-1", Why: "pending"}
		return tx.PutAgent(cur)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.device.Mesh.OpenAgent(ctx, connect.NewRequest(&v1.OpenAgentRequest{Agent: stoppedAsker.Id})); code(err) != connect.CodeUnavailable {
		t.Fatalf("stopped with PendingReopen: %v, want Unavailable (a client tries again)", err)
	}
	if err := w.d.Store().Write(ctx, &v1.Actor{Id: "test", Class: v1.ActorClass_ACTOR_CLASS_DEVICE}, "", func(tx *store.Tx) error {
		cur, gerr := tx.GetAgent(stoppedAsker.Id)
		if gerr != nil {
			return gerr
		}
		cur.PendingReopen = nil
		return tx.PutAgent(cur)
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(stoppedAsker.Worktree); !os.IsNotExist(err) {
		// Remove worktree explicitly if still around so recreation path is exercised too.
		_ = exec.Command("git", "-C", stoppedAsker.Repo, "worktree", "remove", "--force", stoppedAsker.Worktree).Run()
	}

	var out, errOut bytes.Buffer
	var execArgv, execEnv []string
	var execDir string
	exitCode := cli.Main([]string{"--state", w.state, "open", "--model", "opus", "--effort", "high", asker.Id, "--", "--verbose"}, cli.Env{
		Stdin:   strings.NewReader(""),
		Stdout:  &out,
		Stderr:  &errOut,
		Getenv:  func(string) string { return "" },
		Environ: func() []string { return []string{"HOME=/h", "PATH=/bin", "AGENT_MESH_TOKEN=leak", "CLAUDECODE=1"} },
		Exec: func(argv, envv []string, dir string) error {
			execArgv, execEnv, execDir = argv, envv, dir
			return nil
		},
	}, nil)
	if exitCode != cli.ExitOK {
		t.Fatalf("exec open exit=%d stderr=%q", exitCode, errOut.String())
	}
	if execDir != stoppedAsker.Worktree {
		t.Errorf("execDir = %q, want %q", execDir, stoppedAsker.Worktree)
	}
	if _, err := os.Stat(stoppedAsker.Worktree); err != nil {
		t.Errorf("worktree was not recreated: %v", err)
	}
	joined := strings.Join(execArgv, "\x00")
	for _, want := range []string{"--resume\x00" + stoppedAsker.SessionId, "--model\x00opus", "--effort\x00high", "--verbose"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %v missing %q", execArgv, want)
		}
	}
	for _, banned := range []string{"--settings", "--disallowedTools", "--dangerously-skip-permissions", "--plugin-dir"} {
		if slices.Contains(execArgv, banned) {
			t.Errorf("standalone Claude open kept %q in %v", banned, execArgv)
		}
	}
	for _, kv := range execEnv {
		if strings.HasPrefix(kv, "AGENT_MESH_") || strings.HasPrefix(kv, "CLAUDECODE=") {
			t.Errorf("leaked env %q", kv)
		}
	}
	if !strings.Contains(errOut.String(), "worktree was recreated") {
		t.Errorf("expected recreated note in stderr: %q", errOut.String())
	}

	// Stopped agy worker: `.agents/hooks.json` generated by the mesh run is removed so the user doesn't hit mesh hooks,
	// while any non-mesh hooks alongside it are preserved.
	if _, err := os.Stat(hooksPath); err != nil {
		t.Fatalf("expected StopRun to preserve dirty worktree's %s: %v", hooksPath, err)
	}
	jsonOut, jsonErr := &bytes.Buffer{}, &bytes.Buffer{}
	codeJSON := cli.Main([]string{"--state", w.state, "open", "--format", "json", agyWorker.Id}, cli.Env{
		Stdin:  strings.NewReader(""),
		Stdout: jsonOut,
		Stderr: jsonErr,
		Getenv: func(string) string { return "" },
	}, nil)
	if codeJSON != cli.ExitOK {
		t.Fatalf("open --format json exit=%d stderr=%q", codeJSON, jsonErr.String())
	}
	var respMap map[string]any
	if err := json.Unmarshal(jsonOut.Bytes(), &respMap); err != nil {
		t.Fatalf("invalid json %q: %v", jsonOut.String(), err)
	}
	if respMap["mode"] != "OPEN_MODE_EXEC" {
		t.Errorf("json mode = %v", respMap["mode"])
	}
	if _, err := os.Stat(hooksPath); !os.IsNotExist(err) {
		t.Errorf("expected mesh .agents/hooks.json removed before standalone agy open, stat=%v", err)
	}
	// If non-mesh keys sit beside "agent-mesh", only "agent-mesh" is stripped.
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hooksPath, []byte(`{"agent-mesh":{"Stop":[]},"custom":{"Stop":[{"type":"command","command":"echo hi"}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.device.Mesh.OpenAgent(ctx, connect.NewRequest(&v1.OpenAgentRequest{Agent: agyWorker.Id})); err != nil {
		t.Fatal(err)
	}
	kept, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(kept), "agent-mesh") || !strings.Contains(string(kept), "custom") {
		t.Errorf("unexpected hooks.json after strip: %s", kept)
	}

	// Tracked .agents/hooks.json with non-mesh keys: OpenAgent clears skip-worktree and preserves the original mode.
	repoTracked := gitRepo(t)
	commitFileIn(t, repoTracked, ".agents/hooks.json", "{\"custom\":{\"Stop\":[{\"type\":\"command\",\"command\":\"echo hi\"}]}}\n", "add repo hooks")
	run2 := startRun(t, w.device.Mesh, "")
	trackedWorker := w.spawnUnder(t, w.device.Mesh, "tracked-ag", &v1.SpawnAgentRequest{
		RunId: run2.Id, Repo: repoTracked, Kind: "agy", Tier: "strong",
	})
	trackedWorker = w.agyTurn(t, trackedWorker, "hi")
	if err := os.WriteFile(filepath.Join(trackedWorker.Worktree, "wip.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lsBefore := gitIn(t, trackedWorker.Worktree, "ls-files", "-v", "--", ".agents/hooks.json")
	if !strings.HasPrefix(lsBefore, "S ") {
		t.Fatalf("expected skip-worktree before open, ls-files=%q", lsBefore)
	}
	if _, err := w.device.Mesh.StopRun(ctx, connect.NewRequest(&v1.StopRunRequest{Meta: meta("stop-tracked"), RunId: run2.Id})); err != nil {
		t.Fatal(err)
	}
	w.waitState(trackedWorker.Id, v1.AgentState_AGENT_STATE_RETIRED)
	wtHooks := filepath.Join(trackedWorker.Worktree, ".agents", "hooks.json")
	if err := os.Chmod(wtHooks, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.device.Mesh.OpenAgent(ctx, connect.NewRequest(&v1.OpenAgentRequest{Agent: trackedWorker.Id})); err != nil {
		t.Fatal(err)
	}
	lsAfter := gitIn(t, trackedWorker.Worktree, "ls-files", "-v", "--", ".agents/hooks.json")
	if strings.HasPrefix(lsAfter, "S ") {
		t.Errorf("expected skip-worktree cleared on tracked hooks.json, ls-files=%q", lsAfter)
	}
	stAfter, err := os.Stat(wtHooks)
	if err != nil {
		t.Fatal(err)
	}
	if stAfter.Mode().Perm() != 0o644 {
		t.Errorf("tracked hooks.json mode = %o, want 0644", stAfter.Mode().Perm())
	}
	rawAfter, err := os.ReadFile(wtHooks)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawAfter), "agent-mesh") || !strings.Contains(string(rawAfter), "custom") {
		t.Errorf("unexpected tracked hooks.json after strip: %s", rawAfter)
	}
	_ = daemon.HintHeader
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
