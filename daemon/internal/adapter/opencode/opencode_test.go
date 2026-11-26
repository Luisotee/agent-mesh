package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"
	_ "modernc.org/sqlite"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	agentmeshv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

const sampleVerboseListing = `opencode/beam-free
{
  "id": "beam-free",
  "providerID": "opencode",
  "status": "active",
  "cost": {"input": 0, "output": 0, "cache": {"read": 0, "write": 0}},
  "capabilities": {"toolcall": true},
  "release_date": "2026-10-05",
  "variants": {"low": {"reasoningEffort": "low"}, "high": {"reasoningEffort": "high"}}
}
opencode/big-pickle
{
  "id": "big-pickle",
  "providerID": "opencode",
  "status": "active",
  "cost": {"input": 0, "output": 0},
  "capabilities": {"toolcall": true},
  "release_date": "2026-11-01"
}
opencode/gpt-paid
{
  "id": "gpt-paid",
  "providerID": "opencode",
  "status": "active",
  "cost": {"input": 1.25, "output": 10},
  "release_date": "2026-11-10"
}
opencode/no-tools
{
  "id": "no-tools",
  "providerID": "opencode",
  "status": "active",
  "cost": {"input": 0, "output": 0},
  "capabilities": {"toolcall": false},
  "release_date": "2026-11-15"
}
anthropic/claude-x
{
  "id": "claude-x",
  "providerID": "anthropic",
  "status": "active",
  "cost": {"input": 0, "output": 0},
  "release_date": "2026-11-12"
}
`

func TestParseModelsAndFreeModelsNewestFirst(t *testing.T) {
	listing := ParseModels(sampleVerboseListing)
	if len(listing) != 5 {
		t.Fatalf("parsed %d models, want 5: %+v", len(listing), listing)
	}
	free := FreeModels(listing)
	if strings.Join(free, ",") != "opencode/big-pickle,opencode/beam-free" {
		t.Fatalf("FreeModels = %v", free)
	}
	a := New()
	a.ListModels = func(context.Context, string, map[string]string) (string, error) {
		return sampleVerboseListing, nil
	}
	expanded, err := a.ExpandChain(context.Background(), "opencode", nil, []string{"anthropic/claude-x", "free", "opencode/beam-free"})
	if err != nil {
		t.Fatal(err)
	}
	want := "anthropic/claude-x,opencode/big-pickle,opencode/beam-free"
	if strings.Join(expanded, ",") != want {
		t.Fatalf("ExpandChain = %v, want %s", expanded, want)
	}
}

func TestPlanLaunchFreshResumeVariantsAndOrderedPermissions(t *testing.T) {
	a := New()
	a.ListModels = func(context.Context, string, map[string]string) (string, error) {
		return sampleVerboseListing, nil
	}
	root := t.TempDir()
	wt := filepath.Join(root, "wt", "feat")
	repo := filepath.Join(root, "proj")

	// Fresh planning launch on beam-free (variant high exists), with skip_permissions.
	res, err := a.PlanLaunch(context.Background(), connect.NewRequest(&adapterv1.PlanLaunchRequest{
		AgentId: "a1", Role: agentmeshv1.AgentRole_AGENT_ROLE_WORKER, Cwd: wt, Repo: repo,
		Binary: "/opt/opencode", Model: "opencode/beam-free", Effort: "high",
		SkipPermissions: true, DenyEdits: true, Env: map[string]string{"HOME": root},
	}))
	if err != nil {
		t.Fatal(err)
	}
	agent := AgentName("opencode/beam-free", "high")
	wantArgv := []string{"/opt/opencode", "--agent", agent, "-m", "opencode/beam-free", "--auto"}
	if strings.Join(res.Msg.Argv, "\x00") != strings.Join(wantArgv, "\x00") {
		t.Fatalf("argv = %v, want %v", res.Msg.Argv, wantArgv)
	}
	if res.Msg.Env["OPENCODE_DISABLE_AUTOUPDATE"] != "1" {
		t.Errorf("OPENCODE_DISABLE_AUTOUPDATE = %q", res.Msg.Env["OPENCODE_DISABLE_AUTOUPDATE"])
	}
	var cfg struct {
		Agent map[string]map[string]any `json:"agent"`
	}
	if err := json.Unmarshal([]byte(res.Msg.Env["OPENCODE_CONFIG_CONTENT"]), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Agent[agent]["model"] != "opencode/beam-free" || cfg.Agent[agent]["variant"] != "high" || cfg.Agent[agent]["mode"] != "primary" {
		t.Errorf("OPENCODE_CONFIG_CONTENT = %s", res.Msg.Env["OPENCODE_CONFIG_CONTENT"])
	}
	perm := res.Msg.Env["OPENCODE_PERMISSION"]
	// Verify edit keys preserve exact order: "*" -> "../*" -> "../../proj/*" (last match wins).
	iStar := strings.Index(perm, `"*":"deny"`)
	iUp := strings.Index(perm, `"../*":"allow"`)
	iRepo := strings.Index(perm, `"../../proj/*":"deny"`)
	if iStar < 0 || iUp <= iStar || iRepo <= iUp {
		t.Fatalf("OPENCODE_PERMISSION edit order wrong: %s", perm)
	}
	if !strings.Contains(perm, `"git push*":"deny"`) || !strings.Contains(perm, `"gh pr create*":"deny"`) {
		t.Errorf("OPENCODE_PERMISSION missing bash deny rules: %s", perm)
	}

	// Resume on big-pickle (no variant listed, writer role without DenyEdits): distinct agent name, -s, no OPENCODE_PERMISSION.
	res2, err := a.PlanLaunch(context.Background(), connect.NewRequest(&adapterv1.PlanLaunchRequest{
		AgentId: "a1", Role: agentmeshv1.AgentRole_AGENT_ROLE_WORKER, Cwd: wt, Repo: repo,
		ResumeSessionId: "ses_9", Model: "opencode/big-pickle", Effort: "medium",
		Env: map[string]string{"HOME": root},
	}))
	if err != nil {
		t.Fatal(err)
	}
	agent2 := AgentName("opencode/big-pickle", "medium")
	if agent2 == agent {
		t.Fatalf("distinct models/efforts produced same agent name %s", agent2)
	}
	wantArgv2 := []string{"opencode", "--agent", agent2, "-m", "opencode/big-pickle", "-s", "ses_9"}
	if strings.Join(res2.Msg.Argv, "\x00") != strings.Join(wantArgv2, "\x00") {
		t.Fatalf("resume argv = %v, want %v", res2.Msg.Argv, wantArgv2)
	}
	if _, hasPerm := res2.Msg.Env["OPENCODE_PERMISSION"]; hasPerm {
		t.Errorf("writer launch should not have OPENCODE_PERMISSION")
	}
	if strings.Contains(res2.Msg.Env["OPENCODE_CONFIG_CONTENT"], `"variant"`) {
		t.Errorf("unexpected variant in %s", res2.Msg.Env["OPENCODE_CONFIG_CONTENT"])
	}
}

func seedTestDB(t *testing.T, dbPath string, wal bool, fn func(*sql.DB)) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if wal {
		if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS session (id TEXT PRIMARY KEY, directory TEXT NOT NULL, parent_id TEXT, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL);
		CREATE TABLE IF NOT EXISTS message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL);
	`); err != nil {
		t.Fatal(err)
	}
	fn(db)
}

func TestLookupSessionAndInspectSessionWAL(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opencode.db")
	wt := filepath.Join(dir, "wt")
	repo := filepath.Join(dir, "repo")
	os.MkdirAll(wt, 0o755)
	os.MkdirAll(repo, 0o755)
	now := time.Now().Truncate(time.Millisecond)

	seedTestDB(t, dbPath, true, func(db *sql.DB) {
		db.Exec(`INSERT INTO session VALUES ('ses_old', ?, NULL, ?, ?)`, wt, now.Add(-time.Hour).UnixMilli(), now.Add(-time.Hour).UnixMilli())
		db.Exec(`INSERT INTO session VALUES ('ses_child', ?, 'ses_new', ?, ?)`, wt, now.Add(time.Second).UnixMilli(), now.Add(time.Second).UnixMilli())
		db.Exec(`INSERT INTO session VALUES ('ses_new', ?, NULL, ?, ?)`, wt, now.UnixMilli(), now.UnixMilli())
		db.Exec(`INSERT INTO session VALUES ('ses_repo', ?, NULL, ?, ?)`, repo, now.UnixMilli(), now.UnixMilli())

		// Old model error before turn start, and new model "Model is unavailable" error after turn start.
		oldErr, _ := json.Marshal(map[string]any{
			"role": "assistant", "providerID": "opencode", "modelID": "big-pickle",
			"time":  map[string]any{"created": now.Add(-time.Minute).UnixMilli(), "completed": now.Add(-50 * time.Second).UnixMilli()},
			"error": map[string]any{"name": "APIError", "data": map[string]any{"statusCode": 429, "message": "Rate limit"}},
		})
		db.Exec(`INSERT INTO message VALUES ('m_old', 'ses_new', ?, ?, ?)`, now.Add(-time.Minute).UnixMilli(), now.Add(-50*time.Second).UnixMilli(), string(oldErr))

		unavail, _ := json.Marshal(map[string]any{
			"role": "assistant", "providerID": "opencode", "modelID": "tidepool-alpha-free",
			"time": map[string]any{"created": now.Add(time.Second).UnixMilli(), "completed": now.Add(2 * time.Second).UnixMilli()},
			"error": map[string]any{
				"name": "APIError",
				"data": map[string]any{
					"statusCode":   400,
					"message":      "Upstream request failed: Model is unavailable.",
					"responseBody": `{"error":{"type":"server_error","message":"Upstream request failed: Model is unavailable."}}`,
				},
			},
		})
		db.Exec(`INSERT INTO message VALUES ('m_unavail', 'ses_new', ?, ?, ?)`, now.Add(time.Second).UnixMilli(), now.Add(2*time.Second).UnixMilli(), string(unavail))
	})

	a := New()
	env := map[string]string{"AGENT_MESH_OPENCODE_DB": dbPath}
	res, err := a.LookupSession(context.Background(), connect.NewRequest(&adapterv1.LookupSessionRequest{
		AgentId: "a1", Cwd: wt, Repo: repo, Env: env, NotBefore: timestamppb.New(now),
	}))
	if err != nil || res.Msg.SessionId != "ses_new" {
		t.Fatalf("LookupSession = %q, %v; want ses_new", res.Msg.SessionId, err)
	}
	// Shared checkout (cwd == repo) never matches.
	shared, err := a.LookupSession(context.Background(), connect.NewRequest(&adapterv1.LookupSessionRequest{
		AgentId: "a1", Cwd: repo, Repo: repo, Env: env, NotBefore: timestamppb.New(now),
	}))
	if err != nil || shared.Msg.SessionId != "" {
		t.Fatalf("shared checkout LookupSession = %q, %v; want empty", shared.Msg.SessionId, err)
	}

	// InspectSession ignores old-model error and classifies Model is unavailable on current model as limited.
	info, err := InspectSession(dbPath, "ses_new", wt, repo, "opencode/tidepool-alpha-free", now, now)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != "limited" || info.IsRate || info.ResetIn != DefaultLimitHold.String() || !strings.Contains(info.LimitMessage, "Model is unavailable") {
		t.Fatalf("InspectSession = %+v", info)
	}
	// When switched to another model since `now`, the old model's error does not count as the new model's limit.
	info2, err := InspectSession(dbPath, "ses_new", wt, repo, "opencode/beam-free", now, now)
	if err != nil {
		t.Fatal(err)
	}
	if info2.Status != "" || info2.HasModelReply {
		t.Fatalf("switched model should ignore old model reply: %+v", info2)
	}
}

func TestMatchPaneLimitAndQuotedNonMatches(t *testing.T) {
	nonMatches := []string{
		`# OpenCode Go's "<name> usage limit reached. It will reset in 3 hours …")`,
		`  The docs say a model's usage limit reached. It will reset in 3 hours when it does`,
		`  so a 429 shows as Too Many Requests [retrying in 2s attempt #3] in the pane`,
		`  ┃  Too Many Requests [retrying in 8s attempt #2]`,
		`Note: the usage limit reached. It will reset in 3 hours`,
		`  The error "Upstream request failed: Model is unavailable." happens sometimes`,
		`  ┃  Rate limit ` + strings.Repeat("x", 200) + ` [retrying in 2s attempt #3]`,
	}
	for _, line := range nonMatches {
		if hit, ok := MatchPaneLimit([]string{line}); ok {
			t.Errorf("MatchPaneLimit matched non-limit line %q -> %+v", line, hit)
		}
	}

	cases := []struct {
		line    string
		isRate  bool
		resetIn string
	}{
		{"  ┃  Kimi K2 usage limit reached. It will reset in 3 hours.", false, "3h0m0s"},
		{"  ┃  API Error: 429 [retrying in 2s attempt #10]", true, "10m0s"},
		{"  ┃  Free usage exceeded, subscribe to Go [retrying in 4h 12m attempt #1]", false, "4h12m0s"},
		{"  ┃  Error: Too Many Requests [retrying in 5s attempt #3]", true, "10m0s"},
		{"  ┃  opencode/kimi-k2 usage limit reached. It will reset in 3 hours.", false, "3h0m0s"},
		{"  ┃  Upstream request failed: Model is unavailable.", false, "5h0m0s"},
	}
	for _, c := range cases {
		hit, ok := MatchPaneLimit([]string{c.line})
		if !ok {
			t.Errorf("MatchPaneLimit missed %q", c.line)
			continue
		}
		if hit.IsRate != c.isRate || hit.ResetIn != c.resetIn {
			t.Errorf("%q -> %+v, want isRate=%v resetIn=%s", c.line, hit, c.isRate, c.resetIn)
		}
	}
}
