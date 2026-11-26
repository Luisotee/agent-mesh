package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"connectrpc.com/connect"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
	"github.com/Luisotee/agent-mesh/daemon/internal/fsutil"
)

// leanMCPFile is an agent's `--mcp-config` file, in its own state folder (mode 0600: a server's config may carry
// secrets, and the folder is 0700).
func (d *Daemon) leanMCPFile(agentID string) string {
	return filepath.Join(d.paths.Agent(agentID), "mcp.json")
}

// leanOn reports `claude.lean`.
func (d *Daemon) leanOn(ctx context.Context) bool {
	v, err := d.Setting(d.store.Read(ctx), "claude.lean")
	on, ok := v.(bool)
	return err != nil || !ok || on // an unreadable setting keeps the default: lean
}

// leanLaunch is the lean policy of a Claude agent's launch (design §11): the deny list of its role, the skills and
// plugins hidden, and the file of the MCP servers it loads: the repo's own with `claude.mcp` `repo`, `claude.mcp_keep`
// and the servers its spawn named. nil when the agent isn't Claude or `claude.lean` is off. env is the agent's.
func (d *Daemon) leanLaunch(ctx context.Context, a *v1.Agent, env map[string]string) (*adapterv1.LeanLaunch, error) {
	if a.Kind != claude.Kind || !d.leanOn(ctx) {
		return nil, nil
	}
	r := d.store.Read(ctx)
	root := a.Role == v1.AgentRole_AGENT_ROLE_ROOT
	denyKey, skills := "claude.deny_tools", []string{}
	if root {
		denyKey = "claude.root_deny_tools"
	}
	deny, err := d.settingStrings(r, denyKey)
	if err != nil {
		return nil, err
	}
	hidden, err := d.settingStrings(r, "claude.skills_hidden")
	if err != nil {
		return nil, err
	}
	skills = hidden
	if !root {
		more, err := d.settingStrings(r, "claude.worker_skills_hidden")
		if err != nil {
			return nil, err
		}
		skills = append(skills, more...)
	}
	plugins, err := d.settingStrings(r, "claude.plugins_off")
	if err != nil {
		return nil, err
	}
	keepSkills, err := d.Setting(r, "claude.claudeai_skills")
	if err != nil {
		return nil, err
	}
	lean := &adapterv1.LeanLaunch{DenyTools: deny, HiddenSkills: slices.Compact(skills), OffPlugins: plugins}
	lean.HideClaudeaiSkills = keepSkills != true
	mode, err := d.settingString(r, "claude.mcp")
	if err != nil {
		return nil, err
	}
	if mode == claude.MCPInherit {
		return lean, nil
	}
	keep, err := d.settingStrings(r, "claude.mcp_keep")
	if err != nil {
		return nil, err
	}
	if mode != claude.MCPNone {
		mode = claude.MCPRepo
	}
	cfg, _, err := claude.LeanMCPConfig(env, a.Repo, mode, keep, a.Mcp)
	if err != nil {
		return nil, err
	}
	path := d.leanMCPFile(a.Id)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := fsutil.WriteAtomic(path, cfg, 0o600); err != nil {
		return nil, err
	}
	lean.McpConfigPath = path
	return lean, nil
}

// checkMCP refuses a spawn's `mcp` names the agent can't have: not a Claude agent, a launch that isn't lean (every
// server loads already), or a name the user's config doesn't define for the repo.
func (d *Daemon) checkMCP(ctx context.Context, kind, repo string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	if kind != claude.Kind {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("mcp is for Claude agents: %s has no lean session to pass servers into", kind))
	}
	mode, err := d.settingString(d.store.Read(ctx), "claude.mcp")
	if err != nil {
		return err
	}
	if !d.leanOn(ctx) || mode == claude.MCPInherit {
		why := "claude.lean is off"
		if d.leanOn(ctx) {
			why = "claude.mcp is `inherit`"
		}
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("mcp has nothing to add: %s, so every agent loads all of the user's servers already", why))
	}
	known := claude.KnownMCPServers(d.agentEnv(&v1.Agent{}, ""), repo)
	var missing []string
	for _, n := range names {
		if _, ok := known[n]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		have := make([]string, 0, len(known))
		for n := range known {
			have = append(have, n)
		}
		slices.Sort(have)
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("no MCP server named %s in the user's config (it has: %s)",
			strings.Join(missing, ", "), strings.Join(have, ", ")))
	}
	return nil
}
