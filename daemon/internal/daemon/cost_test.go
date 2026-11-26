package daemon_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"
	_ "modernc.org/sqlite"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/daemon"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

var costT0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func costTS(minutes float64) *timestamppb.Timestamp {
	return timestamppb.New(costT0.Add(time.Duration(minutes * float64(time.Minute))))
}

func costISO(minutes float64) string {
	return costT0.Add(time.Duration(minutes * float64(time.Minute))).Format(time.RFC3339)
}

func startCostHarness(t *testing.T, home string) *harness {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	d, err := daemon.New(daemon.Config{
		StateDir: state,
		TCP:      "127.0.0.1:0",
		Now:      func() time.Time { return costT0 },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Agents: daemon.AgentConfig{
			LookPath: func(string) (string, bool) { return "", false },
			Env:      map[string]string{"HOME": home},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return serve(t, d, state)
}

func sanitizeCwd(cwd string) string {
	return regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(cwd, "-")
}

func writeClaudeTranscript(t *testing.T, home, cwd, session, sub string, lines ...any) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", sanitizeCwd(cwd))
	p := filepath.Join(dir, session+".jsonl")
	if sub != "" {
		p = filepath.Join(dir, session, "subagents", sub+".jsonl")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, rec := range lines {
		raw, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func claudeReply(minutes float64, msgID, model string, input, w1, w5, read, output int64, blocks int, bash string) []any {
	if model == "" {
		model = "claude-opus-5-5"
	}
	if blocks <= 0 {
		blocks = 1
	}
	content := []any{map[string]any{"type": "text", "text": "ok"}}
	if bash != "" {
		content = []any{map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": bash}}}
	}
	rec := map[string]any{
		"type":      "assistant",
		"timestamp": costISO(minutes),
		"message": map[string]any{
			"id":      msgID,
			"model":   model,
			"content": content,
			"usage": map[string]any{
				"input_tokens":                input,
				"cache_creation_input_tokens": w1 + w5,
				"cache_read_input_tokens":     read,
				"output_tokens":               output,
				"cache_creation": map[string]any{
					"ephemeral_1h_input_tokens": w1,
					"ephemeral_5m_input_tokens": w5,
				},
			},
		},
	}
	out := make([]any, blocks)
	for i := 0; i < blocks; i++ {
		out[i] = rec
	}
	return out
}

func claudePrompt(minutes float64, text, entrypoint, cwd string) any {
	return map[string]any{
		"type":       "user",
		"timestamp":  costISO(minutes),
		"entrypoint": entrypoint,
		"cwd":        cwd,
		"message":    map[string]any{"role": "user", "content": text},
	}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 0.001 }

func pbVarint(n uint64) []byte {
	var out []byte
	for {
		b := byte(n & 0x7f)
		n >>= 7
		if n != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if n == 0 {
			return out
		}
	}
}

func pbBytes(num uint64, val []byte) []byte {
	out := pbVarint(num<<3 | 2)
	out = append(out, pbVarint(uint64(len(val)))...)
	return append(out, val...)
}

func pbInt(num, val uint64) []byte {
	out := pbVarint(num << 3)
	return append(out, pbVarint(val)...)
}

func TestCostColdResumesAndCopyDedupAndUnstoppedRunCap(t *testing.T) {
	home := t.TempDir()
	var now atomic.Int64
	now.Store(costT0.UnixNano())
	state := filepath.Join(t.TempDir(), "state")
	d, err := daemon.New(daemon.Config{
		StateDir: state,
		TCP:      "127.0.0.1:0",
		Now:      func() time.Time { return time.Unix(0, now.Load()).UTC() },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Agents: daemon.AgentConfig{
			LookPath: func(string) (string, bool) { return "", false },
			Env:      map[string]string{"HOME": home},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := serve(t, d, state)
	ctx := context.Background()

	// Unstopped run mold (min 0) capped by mnew (min 40) in /repo; plus cold resume & deduped copy.
	if err := h.d.Store().Write(ctx, nil, "", func(tx *store.Tx) error {
		tx.PutRun(&v1.Run{Id: "mold", Title: "old", Status: v1.RunStatus_RUN_STATUS_OPEN, CreatedAt: costTS(0)})
		tx.PutAgent(&v1.Agent{Id: "rold", RunId: "mold", Name: "repo-lead", Role: v1.AgentRole_AGENT_ROLE_ROOT, Kind: "claude", Worktree: "/repo", Repo: "/repo", SessionId: "rs1", CreatedAt: costTS(0)})
		tx.PutAgent(&v1.Agent{Id: "wold", RunId: "mold", Name: "repo-ask", Role: v1.AgentRole_AGENT_ROLE_ASKER, Kind: "claude", Worktree: "/wt", Repo: "/repo", SessionId: "orig", CreatedAt: costTS(0)})
		tx.PutRun(&v1.Run{Id: "mnew", Title: "new", Status: v1.RunStatus_RUN_STATUS_OPEN, CreatedAt: costTS(40)})
		tx.PutAgent(&v1.Agent{Id: "rnew", RunId: "mnew", Name: "repo-lead", Role: v1.AgentRole_AGENT_ROLE_ROOT, Kind: "claude", Worktree: "/repo", Repo: "/repo", SessionId: "rs1", CreatedAt: costTS(40)})
		return tx.PutAgent(&v1.Agent{Id: "wnew", RunId: "mnew", Name: "repo-ask", Role: v1.AgentRole_AGENT_ROLE_ASKER, Kind: "claude", Worktree: "/wt", Repo: "/repo", SessionId: "copy", CreatedAt: costTS(200)})
	}); err != nil {
		t.Fatal(err)
	}

	writeClaudeTranscript(t, home, "/repo", "rs1", "",
		append(claudeReply(10, "r-a", "", 0, 0, 0, 0, 1000, 1, ""), claudeReply(45, "r-b", "", 0, 0, 0, 0, 1000, 1, "")...)...)
	origCalls := append(claudeReply(2, "ca", "", 0, 30_000, 0, 0, 1000, 1, ""), claudeReply(3, "cb", "", 0, 0, 0, 0, 1000, 1, "")...)
	writeClaudeTranscript(t, home, "/wt", "orig", "", origCalls...)
	copyCalls := append(append([]any{}, origCalls...), claudeReply(201, "cc", "", 0, 40_000, 0, 0, 1000, 1, "")...)
	writeClaudeTranscript(t, home, "/wt", "copy", "", copyCalls...)

	resOld, err := h.device.Mesh.GetCost(ctx, connect.NewRequest(&v1.GetCostRequest{RunId: "mold"}))
	if err != nil {
		t.Fatal(err)
	}
	resNew, err := h.device.Mesh.GetCost(ctx, connect.NewRequest(&v1.GetCostRequest{RunId: "mnew"}))
	if err != nil {
		t.Fatal(err)
	}
	byName := func(list []*v1.AgentCost, name string) *v1.AgentCost {
		for _, a := range list {
			if a.Name == name {
				return a
			}
		}
		return nil
	}
	if r := byName(resOld.Msg.Agents, "repo-lead"); r == nil || r.Calls != 1 {
		t.Fatalf("mold root = %+v", r)
	}
	if r := byName(resNew.Msg.Agents, "repo-lead"); r == nil || r.Calls != 1 {
		t.Fatalf("mnew root = %+v", r)
	}
	if a := byName(resOld.Msg.Agents, "repo-ask"); a == nil || a.Calls != 2 {
		t.Fatalf("mold asker = %+v", a)
	}
	// Copy skips the two copied message ids (ca, cb) and flags cc at min 201 (>1h gap, 40k write) as cold!
	if a := byName(resNew.Msg.Agents, "repo-ask"); a == nil || a.Calls != 1 || a.Cold != 1 || !approx(a.ColdUsd, 40_000*8/1e6+1000*20/1e6) {
		t.Fatalf("mnew asker = %+v", a)
	}
	// ParseSince accepts relative spans and rejects garbage.
	if ts, err := daemon.ParseSince("7d", costT0); err != nil || !ts.Equal(costT0.Add(-7*24*time.Hour)) {
		t.Fatalf("ParseSince(7d) = %v, %v", ts, err)
	}
	if _, err := daemon.ParseSince("last week", costT0); err == nil {
		t.Fatal("expected ParseSince error")
	}
}

func TestCostComprehensiveAttributionAndWindows(t *testing.T) {
	home := t.TempDir()
	var now atomic.Int64
	now.Store(costT0.UnixNano())
	state := filepath.Join(t.TempDir(), "state")
	d, err := daemon.New(daemon.Config{
		StateDir: state,
		TCP:      "127.0.0.1:0",
		Now:      func() time.Time { return time.Unix(0, now.Load()).UTC() },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Agents: daemon.AgentConfig{
			LookPath: func(string) (string, bool) { return "", false },
			Env:      map[string]string{"HOME": home},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := serve(t, d, state)
	ctx := context.Background()

	// Helper to advance clock for Store writes.
	at := func(min float64, fn func(*store.Tx) error) {
		now.Store(costT0.Add(time.Duration(min * float64(time.Minute))).UnixNano())
		if err := h.d.Store().Write(ctx, nil, "", fn); err != nil {
			t.Fatal(err)
		}
	}

	// Run 1: root (r1s1), worker w1 (ws1, plans on opus until min 10, execs on sonnet until stop at min 60),
	// agy worker a1 (conv1, plans on argon, execs on hydrogen), opencode reviewer oc1 (ocses1),
	// peer p1 (ps1, spans r1 and r2).
	at(0, func(tx *store.Tx) error {
		if err := tx.PutRun(&v1.Run{Id: "r1", Title: "one", Status: v1.RunStatus_RUN_STATUS_OPEN, CreatedAt: costTS(0)}); err != nil {
			return err
		}
		if err := tx.PutAgent(&v1.Agent{Id: "root1", RunId: "r1", Name: "repo-lead", Role: v1.AgentRole_AGENT_ROLE_ROOT, Kind: "claude", Worktree: "/repo", Repo: "/repo", SessionId: "r1s1", CreatedAt: costTS(0)}); err != nil {
			return err
		}
		w1 := &v1.Agent{Id: "w1", RunId: "r1", Name: "repo-fix", Role: v1.AgentRole_AGENT_ROLE_WORKER, Kind: "claude", Worktree: "/wt1", Repo: "/repo", Model: "opus", Phase: v1.AgentPhase_AGENT_PHASE_PLANNING, SessionId: "ws1", CreatedAt: costTS(1)}
		if err := tx.PutAgent(w1); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(&v1.Event{RunId: "r1", AgentId: "w1", Payload: &v1.Event_AgentSpawned{AgentSpawned: &v1.AgentSpawned{Agent: w1}}}); err != nil {
			return err
		}
		a1 := &v1.Agent{Id: "a1", RunId: "r1", Name: "repo-agy", Role: v1.AgentRole_AGENT_ROLE_WORKER, Kind: "agy", Worktree: "/wt2", Repo: "/repo", Model: "gemini-4-argon-high", Phase: v1.AgentPhase_AGENT_PHASE_PLANNING, SessionId: "conv1", CreatedAt: costTS(1)}
		if err := tx.PutAgent(a1); err != nil {
			return err
		}
		if _, err := tx.AppendEvent(&v1.Event{RunId: "r1", AgentId: "a1", Payload: &v1.Event_AgentSpawned{AgentSpawned: &v1.AgentSpawned{Agent: a1}}}); err != nil {
			return err
		}
		if err := tx.PutAgent(&v1.Agent{Id: "oc1", RunId: "r1", Name: "repo-review-7-opencode", Role: v1.AgentRole_AGENT_ROLE_REVIEWER, Kind: "opencode", Worktree: "/wt3", Repo: "/src/repo", Model: "opencode/big-pickle", SessionId: "ocses1", CreatedAt: costTS(1)}); err != nil {
			return err
		}
		if err := tx.PutAgent(&v1.Agent{Id: "p1", RunId: "r1", Name: "peer-x", Role: v1.AgentRole_AGENT_ROLE_ASKER, Kind: "claude", Worktree: "/peer", Repo: "/peer", Peer: true, SessionId: "ps1", CreatedAt: costTS(1)}); err != nil {
			return err
		}
		return tx.PutAsk(&v1.Ask{
			Id: "k1", RunId: "r1", CallerId: "root1", Kind: v1.AskKind_ASK_KIND_REVIEW,
			Status: v1.AskStatus_ASK_STATUS_FINISHED, Pr: "https://github.com/o/repo/pull/7", PrNumber: 7, Repo: "/src/repo",
			CreatedAt: costTS(1),
			Members: []*v1.AskMember{
				{AgentId: "oc1", Name: "repo-review-7-opencode", Kind: "opencode", Model: "opencode/big-pickle", Tier: "mid", Status: v1.AskMemberStatus_ASK_MEMBER_STATUS_ANSWERED},
			},
			Verdicts: []*v1.Verdict{
				{Reviewer: "repo-review-7-opencode", Kind: "opencode", Model: "opencode/big-pickle", Tier: "mid", Agree: 2, Other: 1, Disagree: 1, Blocking: 1, At: costTS(5)},
				{Reviewer: "repo-review-7-opencode", Kind: "opencode", Model: "opencode/big-pickle", Tier: "mid", Agree: 0, Other: 0, Disagree: 4, Blocking: 0, At: costTS(6)},
			},
		})
	})

	at(10, func(tx *store.Tx) error {
		w1, _ := tx.GetAgent("w1")
		w1.Phase = v1.AgentPhase_AGENT_PHASE_EXECUTING
		w1.Model = "sonnet"
		tx.PutAgent(w1)
		tx.AppendEvent(&v1.Event{RunId: "r1", AgentId: "w1", Payload: &v1.Event_AgentPhaseChanged{
			AgentPhaseChanged: &v1.AgentPhaseChanged{From: v1.AgentPhase_AGENT_PHASE_PLANNING, To: v1.AgentPhase_AGENT_PHASE_EXECUTING, Why: "plan approved"},
		}})
		a1, _ := tx.GetAgent("a1")
		a1.Phase = v1.AgentPhase_AGENT_PHASE_EXECUTING
		a1.Model = "gemini-4-hydrogen-medium"
		tx.PutAgent(a1)
		tx.AppendEvent(&v1.Event{RunId: "r1", AgentId: "a1", Payload: &v1.Event_AgentPhaseChanged{
			AgentPhaseChanged: &v1.AgentPhaseChanged{From: v1.AgentPhase_AGENT_PHASE_PLANNING, To: v1.AgentPhase_AGENT_PHASE_EXECUTING, Why: "plan approved"},
		}})
		_, err := tx.AppendEvent(&v1.Event{RunId: "r1", AgentId: "a1", Payload: &v1.Event_AgentReopened{
			AgentReopened: &v1.AgentReopened{SessionId: "conv1", Reopen: &v1.Reopen{Model: "gemini-4-hydrogen-medium", Why: "plan approved"}},
		}})
		return err
	})

	at(40, func(tx *store.Tx) error {
		run, _ := tx.GetRun("r1")
		run.Status = v1.RunStatus_RUN_STATUS_CLOSED
		tx.UpdateRun(run)
		_, err := tx.AppendEvent(&v1.Event{RunId: "r1", Payload: &v1.Event_RunStopped{RunStopped: &v1.RunStopped{}}})
		return err
	})

	// Run 2 starts at min 100 in /repo, re-asks peer ps1 (with compaction at min 118!).
	at(100, func(tx *store.Tx) error {
		tx.PutRun(&v1.Run{Id: "r2", Title: "two", Status: v1.RunStatus_RUN_STATUS_OPEN, CreatedAt: costTS(100)})
		tx.PutAgent(&v1.Agent{Id: "root2", RunId: "r2", Name: "repo-lead", Role: v1.AgentRole_AGENT_ROLE_ROOT, Kind: "claude", Worktree: "/repo", Repo: "/repo", SessionId: "r2s1", CreatedAt: costTS(100)})
		return tx.PutAgent(&v1.Agent{Id: "p2", RunId: "r2", Name: "peer-x", Role: v1.AgentRole_AGENT_ROLE_ASKER, Kind: "claude", Worktree: "/peer", Repo: "/peer", Peer: true, SessionId: "ps1", CreatedAt: costTS(120)})
	})

	// Transcripts:
	// - root r1s1: plain work before run (min -5), in run (min 15), plus hand-/clear root session (min 25, bash "mesh approve repo-fix")
	writeClaudeTranscript(t, home, "/repo", "r1s1", "", append([]any{claudePrompt(-5, "plain", "cli", "/repo")},
		append(claudeReply(-4, "u0", "", 0, 0, 0, 0, 1000, 1, ""), claudeReply(15, "in1", "", 0, 0, 0, 0, 2000, 1, "")...)...)...)
	writeClaudeTranscript(t, home, "/repo", "hand", "", append([]any{claudePrompt(25, "go", "cli", "/repo")},
		claudeReply(26, "h1", "", 0, 0, 0, 0, 1000, 1, "mesh approve repo-fix")...)...)

	// - worker ws1: >100 KB line + 3x duplicated message.id on opus (min 2, output 1000 -> $0.02),
	//   then sonnet at min 20 (output 3000 -> $0.03), plus subagent with 5m write (w5 10_000 -> $0.025).
	bigToolResult := map[string]any{"type": "user", "timestamp": costISO(3), "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_result", "content": strings.Repeat("x", 150_000)},
	}}}
	wLines := append(claudeReply(2, "m1", "claude-opus-5-5", 0, 0, 0, 0, 1000, 3, ""), bigToolResult)
	wLines = append(wLines, claudeReply(20, "m2", "claude-sonnet-5-5", 0, 0, 0, 0, 3000, 2, "")...)
	wLines = append(wLines, map[string]any{"type": "assistant", "timestamp": costISO(21), "message": map[string]any{
		"id": "syn", "model": "<synthetic>", "usage": map[string]any{"output_tokens": 9999},
	}})
	writeClaudeTranscript(t, home, "/wt1", "ws1", "", wLines...)
	writeClaudeTranscript(t, home, "/wt1", "ws1", "agent-1", claudeReply(22, "sub1", "claude-opus-5-5", 0, 0, 10_000, 0, 0, 1, "")...)

	// - peer ps1: min 2 (output 1000 -> $0.02 in r1), min 118 compact (output 2000) + min 121 (output 4000) -> $0.12 in r2; plus cold resume at min 300 (w1=80_000 -> $0.64).
	pLines := append(claudeReply(2, "pa", "claude-opus-5-5", 0, 0, 0, 0, 1000, 1, ""),
		claudeReply(118, "pc", "claude-opus-5-5", 0, 0, 0, 0, 2000, 1, "")...)
	pLines = append(pLines, claudeReply(121, "pb", "claude-opus-5-5", 0, 0, 0, 0, 4000, 1, "")...)
	writeClaudeTranscript(t, home, "/peer", "ps1", "", pLines...)

	// - unmapped user/headless/worker sessions for window mode:
	writeClaudeTranscript(t, home, "/home/u/p", "user1", "", append([]any{claudePrompt(5, "hi", "cli", "/home/u/p")},
		claudeReply(6, "ux", "claude-opus-5-5", 0, 0, 0, 50_000, 0, 1, "")...)...)
	writeClaudeTranscript(t, home, "/home/u/p", "head1", "", append([]any{claudePrompt(5, "x", "sdk-cli", "/home/u/p")},
		claudeReply(6, "hx", "claude-opus-5-5", 0, 0, 0, 0, 10, 1, "")...)...)

	// - agy conv1 DB (steps at min 2 and min 20):
	convDir := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
	os.MkdirAll(convDir, 0o755)
	adb, err := sql.Open("sqlite", filepath.Join(convDir, "conv1.db"))
	if err != nil {
		t.Fatal(err)
	}
	adb.Exec(`CREATE TABLE steps (idx integer, step_type integer, status integer, metadata blob)`)
	usagePB := append(append(pbInt(2, 1000), pbInt(5, 20_000)...), pbInt(3, 300)...)
	for i, m := range []int{2, 20} {
		sec := uint64(costT0.Unix()) + uint64(m*60)
		metaPB := append(pbBytes(1, pbInt(1, sec)), pbBytes(9, usagePB)...)
		adb.Exec(`INSERT INTO steps VALUES (?, 15, 3, ?)`, i+1, metaPB)
	}
	adb.Close()

	// - OpenCode DB (message at min 3 with tokens + paid cost 0.05):
	ocDBPath := filepath.Join(home, ".local", "share", "opencode", "opencode.db")
	os.MkdirAll(filepath.Dir(ocDBPath), 0o755)
	odb, err := sql.Open("sqlite", ocDBPath)
	if err != nil {
		t.Fatal(err)
	}
	odb.Exec(`CREATE TABLE session (id text primary key, parent_id text, directory text, time_created integer, time_updated integer)`)
	odb.Exec(`CREATE TABLE message (id text primary key, session_id text, time_created integer, data text)`)
	t3 := costT0.Add(3 * time.Minute).UnixMilli()
	odb.Exec(`INSERT INTO session VALUES ('ocses1', NULL, '/wt3', ?, ?)`, t3, t3)
	msgJSON, _ := json.Marshal(map[string]any{
		"id": "om1", "role": "assistant", "providerID": "opencode", "modelID": "big-pickle",
		"cost": 0.05,
		"time": map[string]any{"created": t3},
		"tokens": map[string]any{
			"input": 1200, "output": 40, "reasoning": 10,
			"cache": map[string]any{"read": 30_000},
		},
	})
	odb.Exec(`INSERT INTO message VALUES ('om1', 'ocses1', ?, ?)`, t3, string(msgJSON))
	odb.Close()

	// Run 1 cost report:
	r1Res, err := h.device.Mesh.GetCost(ctx, connect.NewRequest(&v1.GetCostRequest{RunId: "r1"}))
	if err != nil {
		t.Fatal(err)
	}
	r1 := r1Res.Msg
	// Claude in r1: root (0.04 + 0.02 = 0.06), w1 (0.02 + 0.03 = 0.05 + sub 0.025 -> 0.08?), wait:
	// 10_000 w5 on opus = 10_000 * 5 / 1e6 = 0.05; peer ps1 = 0.02.
	// Total = 0.06 + 0.05 + 0.05 + 0.02 = 0.18!
	if !approx(r1.Usd, 0.18) {
		t.Fatalf("r1.Usd = %v, want 0.18; agents=%+v", r1.Usd, r1.Agents)
	}
	byAgent := map[string]*v1.AgentCost{}
	for _, a := range r1.Agents {
		byAgent[a.Name] = a
	}
	if a := byAgent["repo-fix"]; a == nil || !approx(a.PlanUsd, 0.02) || !approx(a.ExecUsd, 0.03) || !approx(a.SubUsd, 0.05) {
		t.Fatalf("repo-fix = %+v", a)
	}
	if a := byAgent["repo-lead"]; a == nil || !approx(a.Usd, 0.06) {
		t.Fatalf("repo-lead = %+v", a)
	}
	if a := byAgent["peer-x"]; a == nil || !approx(a.Usd, 0.02) || a.Kind != "peer" {
		t.Fatalf("peer-x r1 = %+v", a)
	}
	if len(r1.Verdicts) != 1 || r1.Verdicts[0].Findings != 8 || !r1.Verdicts[0].HasAgreedPct || r1.Verdicts[0].AgreedPct != 38 {
		t.Fatalf("verdicts = %+v", r1.Verdicts)
	}
	if r1.Reviews == nil || r1.Reviews.ReviewedPrs != 1 || r1.Reviews.ReviewersPerPr != 1.0 {
		t.Fatalf("reviews = %+v", r1.Reviews)
	}
	// agy & opencode tokens:
	foundTokenModels := map[string]int64{}
	var ocModelUSD float64
	for _, tm := range r1.TokensByModel {
		foundTokenModels[tm.Model+":"+tm.Phase] = tm.Calls
		if tm.Model == "opencode/big-pickle" {
			ocModelUSD = tm.Usd
		}
	}
	if foundTokenModels["argon:plan"] != 1 || foundTokenModels["hydrogen:exec"] != 1 || foundTokenModels["opencode/big-pickle:"] != 1 || !approx(ocModelUSD, 0.05) {
		t.Fatalf("tokens_by_model = %+v", r1.TokensByModel)
	}

	// Run 2 gets peer's min 118 compaction + min 121 call ($0.12).
	r2Res, err := h.device.Mesh.GetCost(ctx, connect.NewRequest(&v1.GetCostRequest{RunId: "r2"}))
	if err != nil {
		t.Fatal(err)
	}
	if !approx(r2Res.Msg.Usd, 0.12) {
		t.Fatalf("r2.Usd = %v, want 0.12", r2Res.Msg.Usd)
	}

	// Window mode includes pre-run user work, unmapped user & headless, omits agents unless --agents.
	winRes, err := h.device.Mesh.GetCost(ctx, connect.NewRequest(&v1.GetCostRequest{
		Since: costISO(-10), Until: costISO(200),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(winRes.Msg.Agents) != 0 {
		t.Fatalf("expected no agents without --agents, got %d", len(winRes.Msg.Agents))
	}
	byKind := map[string]*v1.KindCost{}
	for _, k := range winRes.Msg.Kinds {
		byKind[k.Kind] = k
	}
	if byKind["user"] == nil || byKind["headless"] == nil || byKind["worker subagents"] == nil {
		t.Fatalf("kinds = %+v", winRes.Msg.Kinds)
	}
	if !byKind["user"].HasFirstCtxMedian || byKind["user"].FirstCtxMedian != 50_000 {
		t.Fatalf("user first_ctx_median = %+v", byKind["user"])
	}

	// Inverted window is refused.
	if _, err := h.device.Mesh.GetCost(ctx, connect.NewRequest(&v1.GetCostRequest{
		Since: "2026-11-04", Until: "2026-11-01",
	})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("inverted window err = %v", err)
	}
	// Worker cannot read machine-wide window.
	wtok := h.seedAgent("r1", "w2", "repo-w2", "", v1.AgentRole_AGENT_ROLE_WORKER)
	if _, err := h.as(wtok).GetCost(ctx, connect.NewRequest(&v1.GetCostRequest{Since: "7d"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("worker window err = %v", err)
	}
}
