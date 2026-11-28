package daemon_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

// leanArgs reads what a launch's argv says about the lean session: the deny list (every value of the one
// --disallowedTools), the MCP config path and whether --strict-mcp-config is there.
func leanArgs(argv []string) (deny []string, mcpPath string, strict bool) {
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "--strict-mcp-config":
			strict = true
		case "--mcp-config":
			mcpPath = argv[i+1]
		case "--disallowedTools":
			for _, a := range argv[i+1:] {
				if strings.HasPrefix(a, "--") {
					break
				}
				deny = append(deny, a)
			}
		}
	}
	return deny, mcpPath, strict
}

func (w *reopenWorld) userMCPServers(t *testing.T, servers ...string) {
	t.Helper()
	m := map[string]any{}
	for _, s := range servers {
		m[s] = map[string]any{"type": "stdio", "command": s + "-cmd", "env": map[string]string{"TOKEN": "secret-" + s}}
	}
	b, _ := json.Marshal(map[string]any{"mcpServers": m})
	if err := os.WriteFile(filepath.Join(w.home, ".claude.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mcpFileServers(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		McpServers map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	var out []string
	for n := range doc.McpServers {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

func TestAClaudeAgentLaunchesLeanWithTheKeptAndTheNamedMCPServersAndARootKeepsAskUserQuestion(t *testing.T) {
	w := startReopen(t)
	w.userMCPServers(t, "context7", "playwright", "sentry")
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	lead, asLead := w.root(t, run.Id)

	rootLaunch := w.launches(t, w.agent(lead.Id).SessionId)
	if len(rootLaunch) != 1 {
		t.Fatalf("root launches %v", rootLaunch)
	}
	deny, path, strict := leanArgs(rootLaunch[0])
	if slices.Contains(deny, "AskUserQuestion") || !slices.Contains(deny, "Agent") || !strict {
		t.Errorf("the root's lean flags: deny %v strict %v", deny, strict)
	}
	if got := mcpFileServers(t, path); !slices.Equal(got, []string{"context7"}) {
		t.Errorf("the root's MCP servers %v", got)
	}

	// A worker named playwright for its task: it gets context7 and playwright, never sentry.
	res, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker", Repo: gitRepo(t), Slug: "ui",
		Tier: "mid", Mcp: []string{"playwright"}, Task: "build the page"}))
	if err != nil {
		t.Fatal(err)
	}
	worker := w.waitState(res.Msg.Agent.Id, v1.AgentState_AGENT_STATE_IDLE)
	if !slices.Equal(worker.Mcp, []string{"playwright"}) {
		t.Errorf("the agent's mcp %v", worker.Mcp)
	}
	launch := w.launches(t, worker.SessionId)
	deny, path, strict = leanArgs(launch[0])
	if !slices.Contains(deny, "AskUserQuestion") || !slices.Contains(deny, "Artifact") || !strict {
		t.Errorf("the worker's lean flags: deny %v strict %v", deny, strict)
	}
	if got := mcpFileServers(t, path); !slices.Equal(got, []string{"context7", "playwright"}) {
		t.Errorf("the worker's MCP servers %v", got)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the MCP file's mode: %v %v", info, err)
	}
	if !strings.Contains(strings.Join(launch[0], " "), "ENABLE_CLAUDEAI_MCP_SERVERS") {
		t.Errorf("claude.ai's connectors are not turned off: %v", launch[0])
	}

	// A reopen keeps its servers.
	if _, err := w.device.Mesh.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{Key: "claude.mcp_keep",
		Value: val(t, []any{"context7", "sentry"})})); err != nil {
		t.Fatal(err)
	}
	if _, err := w.device.Mesh.SwitchAgent(ctx, connect.NewRequest(&v1.SwitchAgentRequest{Agent: worker.Id, Model: "opus"})); err != nil {
		t.Fatal(err)
	}
	w.waitReopened(t, worker.Id, worker.TerminalId)
	all := w.launches(t, worker.SessionId)
	_, path, _ = leanArgs(all[len(all)-1])
	if got := mcpFileServers(t, path); !slices.Equal(got, []string{"context7", "playwright", "sentry"}) {
		t.Errorf("after the reopen: %v", got)
	}
}

func TestSpawnRefusesMCPServersTheUsersConfigLacksAndOnesAnAgentCannotTake(t *testing.T) {
	w := startReopen(t)
	w.userMCPServers(t, "context7")
	ctx := context.Background()
	run := startRun(t, w.device.Mesh, "")
	_, asLead := w.root(t, run.Id)
	spawn := func(kind string, mcp ...string) error {
		_, err := asLead.SpawnAgent(ctx, connect.NewRequest(&v1.SpawnAgentRequest{About: "test worker", Repo: gitRepo(t), Slug: "x" + kind,
			Kind: kind, Tier: "mid", Mcp: mcp}))
		return err
	}
	if err := spawn("claude", "nope"); code(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "nope") {
		t.Errorf("an unknown server: %v", err)
	}
	if err := spawn("agy", "context7"); code(err) != connect.CodeInvalidArgument {
		t.Errorf("an agy agent: %v", err)
	}
	if _, err := w.device.Mesh.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{Key: "claude.lean",
		Value: val(t, false)})); err != nil {
		t.Fatal(err)
	}
	if err := spawn("claude", "context7"); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("with claude.lean off: %v", err)
	}
}

func TestWithClaudeLeanOffALaunchIsAsBefore(t *testing.T) {
	w := startReopen(t)
	w.userMCPServers(t, "context7")
	ctx := context.Background()
	if _, err := w.device.Mesh.SetSetting(ctx, connect.NewRequest(&v1.SetSettingRequest{Key: "claude.lean",
		Value: val(t, false)})); err != nil {
		t.Fatal(err)
	}
	run := startRun(t, w.device.Mesh, "")
	lead, _ := w.root(t, run.Id)
	argv := w.launches(t, w.agent(lead.Id).SessionId)[0]
	if j := strings.Join(argv, " "); strings.Contains(j, "--strict-mcp-config") || strings.Contains(j, "--disallowedTools") ||
		strings.Contains(j, "skillOverrides") {
		t.Errorf("a non-lean launch: %v", argv)
	}
}
