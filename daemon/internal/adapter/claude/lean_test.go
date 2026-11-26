package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	agentmeshv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
)

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// argvAfter is the values of a variadic flag: up to the next flag.
func argvAfter(argv []string, flag string) []string {
	i := slices.Index(argv, flag)
	if i < 0 {
		return nil
	}
	var out []string
	for _, a := range argv[i+1:] {
		if strings.HasPrefix(a, "--") {
			break
		}
		out = append(out, a)
	}
	return out
}

func TestALeanLaunchDeniesToolsHidesSkillsAndPluginsAndLoadsOnlyTheNamedMCPServers(t *testing.T) {
	home := t.TempDir()
	writeJSON(t, filepath.Join(home, ".claude", "settings.json"), map[string]any{"enabledPlugins": map[string]bool{
		"feature-dev@official": true, "pr-review-toolkit@official": false, "keepme@official": true}})
	lean := &adapterv1.LeanLaunch{DenyTools: []string{"Artifact", "AskUserQuestion"},
		HiddenSkills: []string{"loop", "delegate"}, OffPlugins: []string{"feature-dev", "pr-review-toolkit", "absent"},
		HideClaudeaiSkills: true, McpConfigPath: "/state/agents/a1/mcp.json"}
	cwd, repo := t.TempDir(), t.TempDir()
	res, err := New().PlanLaunch(context.Background(), connect.NewRequest(&adapterv1.PlanLaunchRequest{
		Role: agentmeshv1.AgentRole_AGENT_ROLE_ASKER, Cwd: cwd, Repo: repo, HookArgv: []string{"mesh", "hook"},
		Env: map[string]string{"HOME": home}, Lean: lean}))
	if err != nil {
		t.Fatal(err)
	}
	argv := res.Msg.Argv
	// One --disallowedTools: the read-only rules, then the lean tools.
	if n := strings.Count(strings.Join(argv, "\x00"), "--disallowedTools"); n != 1 {
		t.Fatalf("%d --disallowedTools in %q", n, argv)
	}
	deny := argvAfter(argv, "--disallowedTools")
	if !slices.Contains(deny, "Bash(git push:*)") || !slices.Contains(deny, "Artifact") || !slices.Contains(deny, "AskUserQuestion") {
		t.Errorf("deny %q", deny)
	}
	if got := argvAfter(argv, "--mcp-config"); len(got) != 1 || got[0] != "/state/agents/a1/mcp.json" || !slices.Contains(argv, "--strict-mcp-config") {
		t.Errorf("mcp flags in %q", argv)
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(argvAfter(argv, "--settings")[0]), &settings); err != nil {
		t.Fatal(err)
	}
	if got := settings["skillOverrides"]; !jsonEq(got, map[string]any{"loop": "user-invocable-only", "delegate": "user-invocable-only"}) {
		t.Errorf("skillOverrides %v", got)
	}
	// Only plugins the user's settings enable are turned off (a flag for one that isn't enabled would be noise).
	if got := settings["enabledPlugins"]; !jsonEq(got, map[string]any{"feature-dev@official": false}) {
		t.Errorf("enabledPlugins %v", got)
	}
	if settings["syncClaudeAiSkills"] != false {
		t.Errorf("syncClaudeAiSkills %v", settings["syncClaudeAiSkills"])
	}
	if got := settings["env"]; !jsonEq(got, map[string]any{"ENABLE_CLAUDEAI_MCP_SERVERS": "false"}) {
		t.Errorf("env %v", got)
	}
	if settings["hooks"] == nil {
		t.Error("the hooks are gone")
	}
}

// The share folder is loaded with --plugin-dir; a copy of the mesh's own plugin the user installed is turned off
// for that session, whatever its name (live: of another name it loads beside the inline one and doubles the skills;
// of the same name the inline one replaces it).
func TestALaunchWithTheShareTurnsOffTheUsersInstalledCopyOfTheMeshPlugin(t *testing.T) {
	for _, shareName := range []string{"agent-mesh-daemon", "agent-mesh", "mesh-future"} {
		t.Run(shareName, func(t *testing.T) {
			home, share := t.TempDir(), t.TempDir()
			writeJSON(t, filepath.Join(home, ".claude", "settings.json"), map[string]any{"enabledPlugins": map[string]bool{
				"agent-mesh@agent-mesh": true, "agent-mesh-daemon@x": true, "mesh-future@x": true, "feature-dev@official": true,
				"other@official": true, "disabled@official": false}})
			writeJSON(t, filepath.Join(share, ".claude-plugin", "plugin.json"), map[string]any{"name": shareName})
			lean := &adapterv1.LeanLaunch{OffPlugins: []string{"feature-dev"}}
			res, err := New().PlanLaunch(context.Background(), connect.NewRequest(&adapterv1.PlanLaunchRequest{
				Role: agentmeshv1.AgentRole_AGENT_ROLE_WORKER, Cwd: t.TempDir(), Repo: t.TempDir(), HookArgv: []string{"mesh", "hook"},
				Env: map[string]string{"HOME": home, "AGENT_MESH_SHARE": share}, Lean: lean}))
			if err != nil {
				t.Fatal(err)
			}
			if got := argvAfter(res.Msg.Argv, "--plugin-dir"); len(got) != 1 || got[0] != share {
				t.Fatalf("--plugin-dir %q", got)
			}
			var settings map[string]any
			if err := json.Unmarshal([]byte(argvAfter(res.Msg.Argv, "--settings")[0]), &settings); err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"agent-mesh@agent-mesh": false, "agent-mesh-daemon@x": false, "feature-dev@official": false}
			if shareName == "mesh-future" {
				want["mesh-future@x"] = false
			}
			if got := settings["enabledPlugins"]; !jsonEq(got, want) {
				t.Errorf("enabledPlugins %v, want %v", got, want)
			}
		})
	}
}

func TestALaunchWithoutAnInstalledMeshPluginAddsNoPluginSetting(t *testing.T) {
	home, share := t.TempDir(), t.TempDir()
	writeJSON(t, filepath.Join(home, ".claude", "settings.json"), map[string]any{"enabledPlugins": map[string]bool{"other@official": true}})
	writeJSON(t, filepath.Join(share, ".claude-plugin", "plugin.json"), map[string]any{"name": "agent-mesh"})
	res, err := New().PlanLaunch(context.Background(), connect.NewRequest(&adapterv1.PlanLaunchRequest{
		Role: agentmeshv1.AgentRole_AGENT_ROLE_WORKER, Cwd: t.TempDir(), Repo: t.TempDir(), HookArgv: []string{"mesh", "hook"},
		Env: map[string]string{"HOME": home, "AGENT_MESH_SHARE": share}}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(res.Msg.Argv, "\x00"), "enabledPlugins") {
		t.Errorf("argv %q", res.Msg.Argv)
	}
}

func jsonEq(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestALeanRootKeepsAskUserQuestionAndAWorkerWithoutMCPFileLoadsEveryServer(t *testing.T) {
	plan := func(lean *adapterv1.LeanLaunch) []string {
		res, err := New().PlanLaunch(context.Background(), connect.NewRequest(&adapterv1.PlanLaunchRequest{
			Role: agentmeshv1.AgentRole_AGENT_ROLE_ROOT, Cwd: t.TempDir(), HookArgv: []string{"mesh", "hook"},
			Env: map[string]string{"HOME": t.TempDir()}, Lean: lean}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.Argv
	}
	argv := plan(&adapterv1.LeanLaunch{DenyTools: []string{"Artifact", "Agent"}})
	if deny := argvAfter(argv, "--disallowedTools"); !slices.Equal(deny, []string{"Artifact", "Agent"}) {
		t.Errorf("deny %q", deny)
	}
	// MCP `inherit`: no config file, so no strict flag and claude.ai's connectors stay on.
	joined := strings.Join(argv, "\x00")
	if strings.Contains(joined, "--strict-mcp-config") || strings.Contains(joined, "ENABLE_CLAUDEAI_MCP_SERVERS") {
		t.Errorf("an inherit launch restricts MCP: %q", argv)
	}
	// Not lean: nothing added, as before.
	argv = plan(nil)
	if j := strings.Join(argv, "\x00"); strings.Contains(j, "--disallowedTools") || strings.Contains(j, "skillOverrides") || strings.Contains(j, "enabledPlugins") {
		t.Errorf("a non-lean launch carries lean flags: %q", argv)
	}
}

func TestLeanMCPConfigTakesTheRepoOwnServersTheKeptOnesAndTheNamedOnes(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	srv := func(cmd string) map[string]any { return map[string]any{"type": "stdio", "command": cmd} }
	writeJSON(t, filepath.Join(home, ".claude.json"), map[string]any{
		"mcpServers": map[string]any{"context7": srv("c7"), "playwright": srv("pw"), "sentry": srv("sn")},
		"projects": map[string]any{repo: map[string]any{
			"mcpServers":            map[string]any{"repo-local": srv("rl")},
			"enabledMcpjsonServers": []any{"approved"}}}})
	writeJSON(t, filepath.Join(repo, ".mcp.json"), map[string]any{"mcpServers": map[string]any{
		"approved": srv("ap"), "unapproved": srv("un")}})
	env := map[string]string{"HOME": home}
	names := func(mode string, keep, extra []string) ([]string, []string) {
		b, missing, err := LeanMCPConfig(env, repo, mode, keep, extra, nil)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			McpServers map[string]any `json:"mcpServers"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		var got []string
		for n := range doc.McpServers {
			got = append(got, n)
		}
		slices.Sort(got)
		return got, missing
	}
	if got, _ := names(MCPRepo, []string{"context7"}, nil); !slices.Equal(got, []string{"approved", "context7", "repo-local"}) {
		t.Errorf("repo mode: %v (a user-scope server is not the repo's, and an unapproved .mcp.json one stays out)", got)
	}
	if got, _ := names(MCPNone, []string{"context7"}, nil); !slices.Equal(got, []string{"context7"}) {
		t.Errorf("none mode: %v", got)
	}
	got, missing := names(MCPNone, []string{"context7", "gone"}, []string{"playwright", "nope"})
	if !slices.Equal(got, []string{"context7", "playwright"}) || !slices.Equal(missing, []string{"gone", "nope"}) {
		t.Errorf("named servers: %v, missing %v", got, missing)
	}
}
