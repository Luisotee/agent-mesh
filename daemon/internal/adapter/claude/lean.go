package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
)

// Lean sessions (docs/design.md §11): a Claude agent starts without the tools, skills, plugins and MCP servers
// nothing in a mesh uses, which are about 39k tokens of the first call, re-read on every call after it. The
// daemon picks what stays out (`claude.*` settings, LeanLaunch); this file turns that into Claude's flags and
// reads what Claude's own config holds. Checked on Claude Code 2.1.332 (bin/mesh passes the same keys the same
// way): `--settings` overrides the user's own `skillOverrides`, `enabledPlugins` and `syncClaudeAiSkills` for
// this session only, and `--strict-mcp-config` loads only the servers of `--mcp-config`.

// MCP modes of `claude.mcp`: which servers a lean agent loads besides the always-loaded ones.
const (
	MCPRepo    = "repo"    // the repo's own (its project entry in ~/.claude.json, its approved .mcp.json)
	MCPNone    = "none"    // only the always-loaded ones
	MCPInherit = "inherit" // not lean about MCP: whatever the user's config loads
)

// readObject is a JSON file's top-level object; {} when it is missing, unreadable or not an object.
func readObject(path string) map[string]any {
	b, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil || m == nil {
		return map[string]any{}
	}
	return m
}

func object(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func names(v any) []string {
	var out []string
	xs, _ := v.([]any)
	for _, x := range xs {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// configHome is Claude's own config folder: $CLAUDE_CONFIG_DIR, else ~/.claude.
func configHome(env map[string]string) string {
	if d := env["CLAUDE_CONFIG_DIR"]; d != "" {
		return d
	}
	if h := env["HOME"]; h != "" {
		return filepath.Join(h, ".claude")
	}
	return ""
}

// UserPlugins are the plugins the user's settings enable (`name@marketplace`), sorted.
func UserPlugins(env map[string]string) []string {
	home := configHome(env)
	if home == "" {
		return nil
	}
	var out []string
	for k, v := range object(readObject(filepath.Join(home, "settings.json"))["enabledPlugins"]) {
		if on, _ := v.(bool); on {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// leanPlugins are the enabled plugins whose name (before the `@marketplace`) is in off.
func leanPlugins(env map[string]string, off []string) []string {
	skip := map[string]bool{}
	for _, n := range off {
		skip[n] = true
	}
	var out []string
	for _, k := range UserPlugins(env) {
		if skip[strings.SplitN(k, "@", 2)[0]] {
			out = append(out, k)
		}
	}
	return out
}

// leanSettings are the keys a lean launch adds to its `--settings`.
func leanSettings(env map[string]string, l *adapterv1.LeanLaunch) map[string]any {
	out := map[string]any{}
	if len(l.HiddenSkills) > 0 {
		skills := map[string]string{}
		for _, s := range l.HiddenSkills {
			skills[s] = "user-invocable-only"
		}
		out["skillOverrides"] = skills
	}
	if l.HideClaudeaiSkills {
		out["syncClaudeAiSkills"] = false
	}
	if plugins := leanPlugins(env, l.OffPlugins); len(plugins) > 0 {
		off := map[string]bool{}
		for _, p := range plugins {
			off[p] = false
		}
		out["enabledPlugins"] = off
	}
	if l.McpConfigPath != "" {
		out["env"] = map[string]string{"ENABLE_CLAUDEAI_MCP_SERVERS": "false"}
	}
	return out
}

// LeanArgs are the flags of a lean launch: the strict MCP config.
func LeanArgs(l *adapterv1.LeanLaunch) []string {
	if l == nil || l.McpConfigPath == "" {
		return nil
	}
	return []string{"--strict-mcp-config", "--mcp-config", l.McpConfigPath}
}

// MCPServer is a server the user's Claude config defines, with where it comes from.
type MCPServer struct {
	Config map[string]any
	// Scope is `user` (the global config's own), `local` (the repo's project entry there) or `project` (its
	// .mcp.json, only when the user approved the server).
	Scope string
}

// KnownMCPServers are the servers the user's Claude config defines for a repo, by name. A later scope wins a
// name, as in Claude Code.
func KnownMCPServers(env map[string]string, repo string) map[string]MCPServer {
	found := map[string]MCPServer{}
	cfg, err := configFile(env)
	if err != nil || cfg == "" {
		return found
	}
	state := readObject(cfg)
	add := func(servers any, scope string, allow func(string) bool) {
		for n, c := range object(servers) {
			if cm, ok := c.(map[string]any); ok && allow(n) {
				found[n] = MCPServer{Config: cm, Scope: scope}
			}
		}
	}
	always := func(string) bool { return true }
	add(state["mcpServers"], "user", always)
	if repo != "" {
		project := object(object(state["projects"])[repo])
		every, yes, no := approvedProject(repo, project)
		add(readObject(filepath.Join(repo, ".mcp.json"))["mcpServers"], "project",
			func(n string) bool { return !no[n] && (every || yes[n]) })
		add(project["mcpServers"], "local", always)
	}
	return found
}

// approvedProject is which of a repo's `.mcp.json` servers the user approved, from its project entry in the
// global config and the repo's own `.claude/settings*.json`, as Claude Code asks the user once per server.
func approvedProject(repo string, project map[string]any) (every bool, yes, no map[string]bool) {
	yes, no = map[string]bool{}, map[string]bool{}
	for _, src := range []map[string]any{project, readObject(filepath.Join(repo, ".claude", "settings.json")),
		readObject(filepath.Join(repo, ".claude", "settings.local.json"))} {
		if b, _ := src["enableAllProjectMcpServers"].(bool); b {
			every = true
		}
		for _, n := range names(src["enabledMcpjsonServers"]) {
			yes[n] = true
		}
		for _, n := range names(src["disabledMcpjsonServers"]) {
			no[n] = true
		}
	}
	return every, yes, no
}

// LeanMCPConfig is the `--mcp-config` file of a lean agent: with mode `repo` the repo's own servers (`local` and
// approved `project` ones), then, in any mode, the servers of `keep` and `extra` the user's config defines. A
// name the config doesn't define is skipped. The second result is the names asked for but not defined.
func LeanMCPConfig(env map[string]string, repo, mode string, keep, extra []string) ([]byte, []string, error) {
	known := KnownMCPServers(env, repo)
	want := map[string]any{}
	if mode == MCPRepo {
		for n, s := range known {
			if s.Scope != "user" {
				want[n] = s.Config
			}
		}
	}
	var missing []string
	for _, n := range append(append([]string{}, keep...), extra...) {
		if s, ok := known[n]; ok {
			want[n] = s.Config
		} else {
			missing = append(missing, n)
		}
	}
	b, err := json.MarshalIndent(map[string]any{"mcpServers": want}, "", " ")
	return b, missing, err
}
