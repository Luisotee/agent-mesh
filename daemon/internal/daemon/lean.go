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
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
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

// filterAdded removes any item that the agent added back via `mesh tools add`. For namespaced plugin skills like
// `agent-mesh-daemon:<name>`, adding the bare `<name>` also un-hides the namespaced entry.
func filterAdded(xs, added []string, matchBarePluginSkill bool) []string {
	if len(added) == 0 || len(xs) == 0 {
		return xs
	}
	have := make(map[string]bool, len(added))
	for _, n := range added {
		have[n] = true
	}
	var out []string
	for _, x := range xs {
		if have[x] {
			continue
		}
		if matchBarePluginSkill {
			if prefix, rest, ok := strings.Cut(x, ":"); ok && prefix == "agent-mesh-daemon" && have[rest] {
				continue
			}
		}
		out = append(out, x)
	}
	return out
}

func filterAddedPlugins(xs, added []string) []string {
	if len(added) == 0 || len(xs) == 0 {
		return xs
	}
	have := make(map[string]bool, len(added))
	for _, n := range added {
		have[strings.SplitN(n, "@", 2)[0]] = true
	}
	var out []string
	for _, x := range xs {
		if have[strings.SplitN(x, "@", 2)[0]] {
			continue
		}
		out = append(out, x)
	}
	return out
}

// leanLaunch is the lean policy of a Claude agent's launch (design §11): the deny list of its role, the skills and
// plugins hidden, `--system-prompt-snapshot off` once a built-in tool was added back, and the file of the MCP
// servers it loads (`claude.mcp` `repo`, `claude.mcp_keep`, and the agent's `mcp`). nil when the agent isn't
// Claude or `claude.lean` is off. env is the agent's.
func (d *Daemon) leanLaunch(ctx context.Context, a *v1.Agent, env map[string]string) (*adapterv1.LeanLaunch, error) {
	if a.Kind != claude.Kind || !d.leanOn(ctx) {
		return nil, nil
	}
	r := d.store.Read(ctx)
	root := a.Role == v1.AgentRole_AGENT_ROLE_ROOT
	denyKey := "claude.deny_tools"
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
	skills := append([]string{}, hidden...)
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
	lean := &adapterv1.LeanLaunch{
		DenyTools:             filterAdded(deny, a.AddedTools, false),
		HiddenSkills:          filterAdded(dedupe(skills), a.AddedTools, true),
		OffPlugins:            filterAddedPlugins(plugins, a.AddedTools),
		HideClaudeaiSkills:    keepSkills != true && !slices.Contains(a.AddedTools, claude.ClaudeAISkills),
		DisablePromptSnapshot: a.ToolsAdded,
	}
	mode, err := d.settingString(r, "claude.mcp")
	if err != nil {
		return nil, err
	}
	if mode == claude.MCPInherit || a.McpAll {
		return lean, nil
	}
	keep, err := d.settingStrings(r, "claude.mcp_keep")
	if err != nil {
		return nil, err
	}
	if mode != claude.MCPNone {
		mode = claude.MCPRepo
	}
	cfg, missing, err := claude.LeanMCPConfig(env, a.Repo, mode, keep, a.Mcp)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		d.log.Warn("MCP servers the user's Claude config doesn't define are left out", "agent", a.Name, "servers", missing)
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

// parseMCPArg splits comma-separated names and treats a lone "none" as an explicit empty list.
func parseMCPArg(in []string) (names []string, none bool, err error) {
	var parts []string
	for _, raw := range in {
		for _, p := range strings.Split(raw, ",") {
			if p = strings.TrimSpace(p); p != "" {
				parts = append(parts, p)
			}
		}
	}
	parts = dedupe(parts)
	if len(parts) == 0 {
		return nil, false, nil
	}
	if slices.Contains(parts, "none") {
		if len(parts) > 1 {
			return nil, false, invalid("mcp \"none\" clears servers and cannot be combined with other names (%s)", strings.Join(parts, ", "))
		}
		return nil, true, nil
	}
	return parts, false, nil
}

// checkMCP refuses a spawn's or resume's `mcp` names the agent can't have: not a Claude agent, a launch that isn't
// lean (every server loads already), or a name the user's config doesn't define for the repo.
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

// exclusiveSet returns the set of `claude.mcp_exclusive` server names (excluding always-loaded `claude.mcp_keep`).
func (d *Daemon) exclusiveSet(r store.Reader) (map[string]bool, error) {
	excl, err := d.settingStrings(r, "claude.mcp_exclusive")
	if err != nil {
		return nil, err
	}
	keep, err := d.settingStrings(r, "claude.mcp_keep")
	if err != nil {
		return nil, err
	}
	keepSet := map[string]bool{}
	for _, n := range keep {
		keepSet[n] = true
	}
	out := map[string]bool{}
	for _, n := range excl {
		if !keepSet[n] {
			out[n] = true
		}
	}
	return out, nil
}

// exclusiveHolders maps each currently held exclusive MCP server name to a live agent (not skipID) holding it via
// `mcp` or `mcp_all`. Must be called inside a store transaction when enforcing mutual exclusion.
func (d *Daemon) exclusiveHolders(r store.Reader, skipID string) (map[string]*v1.Agent, error) {
	exclSet, err := d.exclusiveSet(r)
	if err != nil || len(exclSet) == 0 {
		return nil, err
	}
	all, err := r.AllAgents()
	if err != nil {
		return nil, err
	}
	holders := map[string]*v1.Agent{}
	for _, other := range all {
		if other.Id == skipID || !hasProcess(other) {
			continue
		}
		if other.McpAll {
			for n := range exclSet {
				if _, ok := holders[n]; !ok {
					holders[n] = other
				}
			}
		}
		for _, n := range other.Mcp {
			if exclSet[n] {
				if _, ok := holders[n]; !ok {
					holders[n] = other
				}
			}
		}
	}
	return holders, nil
}

// checkMCPLease refuses when any requested server in names is an exclusive MCP server currently held by another live agent.
func (d *Daemon) checkMCPLease(r store.Reader, names []string, skipID string) error {
	if len(names) == 0 {
		return nil
	}
	holders, err := d.exclusiveHolders(r, skipID)
	if err != nil || len(holders) == 0 {
		return err
	}
	for _, n := range dedupe(names) {
		if holder, ok := holders[n]; ok {
			return failedPrecondition("%s is one shared server, so one agent at a time holds it: %s (run %s) does (`mesh retire %s` once it is done, or run without %s)",
				n, holder.Name, holder.RunId, holder.Name, n)
		}
	}
	return nil
}

// dropHeldExclusiveMCP drops any exclusive MCP server (or `mcp_all`) carried over on an implicit resume when another
// live agent now holds it, returning a short explanatory note if anything was dropped.
func (d *Daemon) dropHeldExclusiveMCP(r store.Reader, a *v1.Agent) (string, bool, error) {
	if len(a.Mcp) == 0 && !a.McpAll {
		return "", false, nil
	}
	holders, err := d.exclusiveHolders(r, a.Id)
	if err != nil || len(holders) == 0 {
		return "", false, err
	}
	var kept []string
	var dropped []string
	for _, n := range a.Mcp {
		if h, ok := holders[n]; ok {
			dropped = append(dropped, fmt.Sprintf("%s (held by %s, run %s)", n, h.Name, h.RunId))
			continue
		}
		kept = append(kept, n)
	}
	droppedAll := false
	if a.McpAll {
		a.McpAll = false
		droppedAll = true
	}
	if len(dropped) == 0 && !droppedAll {
		return "", false, nil
	}
	a.Mcp = kept
	if droppedAll && len(dropped) == 0 {
		var heldList []string
		for n, h := range holders {
			heldList = append(heldList, fmt.Sprintf("%s by %s", n, h.Name))
		}
		slices.Sort(heldList)
		return fmt.Sprintf("dropped claude.ai full MCP connectors on resume (exclusive servers held: %s)", strings.Join(heldList, ", ")), true, nil
	}
	return "dropped exclusive MCP server(s) on resume: " + strings.Join(dropped, ", "), true, nil
}

// dedupe drops repeats, keeping the first of each in order.
func dedupe(xs []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
