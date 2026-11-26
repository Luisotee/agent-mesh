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

// filterAdded removes any item that the agent added back via `mesh tools add`. A plugin skill has two spellings, the
// bare `<name>` and the namespaced `agent-mesh-daemon:<name>` (matchBarePluginSkill): adding either un-hides both.
func filterAdded(xs, added []string, matchBarePluginSkill bool) []string {
	if len(added) == 0 || len(xs) == 0 {
		return xs
	}
	have := make(map[string]bool, len(added))
	for _, n := range added {
		have[n] = true
		if matchBarePluginSkill {
			have[strings.TrimPrefix(n, pluginSkillPrefix)] = true
		}
	}
	var out []string
	for _, x := range xs {
		if have[x] || matchBarePluginSkill && have[strings.TrimPrefix(x, pluginSkillPrefix)] {
			continue
		}
		out = append(out, x)
	}
	return out
}

const pluginSkillPrefix = "agent-mesh-daemon:"

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
	// A server this launch loads unnamed (its repo's own) that another agent holds stays out of the file.
	held, err := d.heldUnnamed(r, a, env)
	if err != nil {
		return nil, err
	}
	var except []string
	for n, h := range held {
		except = append(except, n)
		d.log.Info("an exclusive MCP server another agent holds is left out", "agent", a.Name, "server", n, "holder", h.Name)
	}
	cfg, missing, err := claude.LeanMCPConfig(env, a.Repo, mode, keep, a.Mcp, except)
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

// exclusiveHolders maps each currently held exclusive MCP server name to a live agent (not skipID) holding it: one
// that named it (`mcp`, `mcp_all`), or whose launch loads it unnamed (a server of its repo under `claude.mcp` `repo`,
// everything under `inherit` or with `claude.lean` off). Must be called inside a store transaction when enforcing
// mutual exclusion.
func (d *Daemon) exclusiveHolders(r store.Reader, skipID string) (map[string]*v1.Agent, error) {
	exclSet, err := d.exclusiveSet(r)
	if err != nil || len(exclSet) == 0 {
		return nil, err
	}
	all, err := r.AllAgents()
	if err != nil {
		return nil, err
	}
	lean := d.leanSetting(r)
	holders := map[string]*v1.Agent{}
	cache := map[string]map[string]any{} // ~/.claude.json, read once for all the agents asked about
	hold := func(n string, a *v1.Agent) {
		if _, ok := holders[n]; !ok {
			holders[n] = a
		}
	}
	for _, other := range all {
		if other.Id == skipID || !hasProcess(other) {
			continue
		}
		if other.McpAll {
			for n := range exclSet {
				hold(n, other)
			}
		}
		for _, n := range other.Mcp {
			if exclSet[n] {
				hold(n, other)
			}
		}
		if other.Kind != claude.Kind {
			continue
		}
		env := d.agentEnv(other, "")
		if !lean {
			for n := range claude.KnownMCPServersCached(env, other.Repo, cache) {
				if exclSet[n] {
					hold(n, other)
				}
			}
			continue
		}
		_, loaded, _, err := d.mcpLoad(r, other, env, cache)
		if err != nil {
			return nil, err
		}
		for _, n := range loaded {
			if exclSet[n] {
				hold(n, other)
			}
		}
	}
	return holders, nil
}

// heldUnnamed maps the exclusive servers a's lean launch would load without a name (its repo's own, under
// `claude.mcp` `repo`) that another live agent holds, to that agent. Servers it names are the lease check's, and
// under `inherit` (or `mcp_all`) nothing is cut, so none are listed.
func (d *Daemon) heldUnnamed(r store.Reader, a *v1.Agent, env map[string]string) (map[string]*v1.Agent, error) {
	if a.Kind != claude.Kind {
		return nil, nil
	}
	if !d.leanSetting(r) {
		return nil, nil
	}
	mode, loaded, _, err := d.mcpLoad(r, a, env, nil)
	if err != nil || mode == claude.MCPInherit {
		return nil, err
	}
	holders, err := d.exclusiveHolders(r, a.Id)
	if err != nil || len(holders) == 0 {
		return nil, err
	}
	out := map[string]*v1.Agent{}
	for _, n := range loaded {
		if h, ok := holders[n]; ok && !slices.Contains(a.Mcp, n) {
			out[n] = h
		}
	}
	return out, nil
}

// noteHeldUnnamed tells a launched agent which servers of its repo another agent holds, so stayed out of its MCP
// file (leanLaunch); false when none did.
func (d *Daemon) noteHeldUnnamed(tx *store.Tx, a *v1.Agent) (bool, error) {
	held, err := d.heldUnnamed(tx.Reader, a, d.agentEnv(a, ""))
	if err != nil || len(held) == 0 {
		return false, err
	}
	var left []string
	for n, h := range held {
		left = append(left, fmt.Sprintf("%s (held by %s)", n, holderLabelOrElse(h, a.RunId)))
	}
	slices.Sort(left)
	_, err = tellTx(tx, a, "left this repo's exclusive MCP server(s) out of your MCP file: "+strings.Join(left, ", ")+" (`mesh tools add <name>` once the holder is done)")
	return err == nil, err
}

// leanSetting reads `claude.lean` (an unreadable setting keeps the default: lean).
func (d *Daemon) leanSetting(r store.Reader) bool {
	if v, err := d.Setting(r, "claude.lean"); err == nil {
		if on, ok := v.(bool); ok {
			return on
		}
	}
	return true
}

// checkMCPLease refuses when any requested server in names is an exclusive MCP server currently held by another live agent.
func (d *Daemon) checkMCPLease(r store.Reader, names []string, skipID string, viewer *v1.Actor) error {
	if len(names) == 0 {
		return nil
	}
	holders, err := d.exclusiveHolders(r, skipID)
	if err != nil || len(holders) == 0 {
		return err
	}
	for _, n := range dedupe(names) {
		if holder, ok := holders[n]; ok {
			if label := holderLabel(holder, viewerRun(viewer)); label != "" {
				return failedPrecondition("%s is one shared server, so one agent at a time holds it: %s does (`mesh retire %s` once it is done, or run without %s)",
					n, label, holder.Name, n)
			}
			return failedPrecondition("%s is one shared server, so one agent at a time holds it, and one of another run does (ask your parent, or run without %s)", n, n)
		}
	}
	return nil
}

// dropHeldExclusiveMCP drops any exclusive MCP server (or `mcp_all`) carried over on an implicit resume when another
// live agent now holds it, returning a short explanatory note if anything was dropped.
func (d *Daemon) dropHeldExclusiveMCP(r store.Reader, a *v1.Agent) (string, bool, error) {
	holders, err := d.exclusiveHolders(r, a.Id)
	if err != nil || len(holders) == 0 {
		return "", false, err
	}
	// A server it loads unnamed (its repo's own) leaves its MCP file at launch (leanLaunch): say so.
	unnamed, err := d.heldUnnamed(r, a, d.agentEnv(a, ""))
	if err != nil {
		return "", false, err
	}
	var left []string
	for n, h := range unnamed {
		left = append(left, fmt.Sprintf("%s (held by %s)", n, holderLabelOrElse(h, a.RunId)))
	}
	slices.Sort(left)
	leftNote := ""
	if len(left) > 0 {
		leftNote = "left this repo's exclusive MCP server(s) out of its MCP file: " + strings.Join(left, ", ") + " (`mesh tools add <name>` once the holder is done)"
	}
	if len(a.Mcp) == 0 && !a.McpAll {
		return leftNote, leftNote != "", nil
	}
	var kept []string
	var dropped []string
	for _, n := range a.Mcp {
		if h, ok := holders[n]; ok {
			dropped = append(dropped, fmt.Sprintf("%s (held by %s)", n, holderLabelOrElse(h, a.RunId)))
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
		return leftNote, leftNote != "", nil
	}
	a.Mcp = kept
	var held []string
	for n, h := range holders {
		held = append(held, fmt.Sprintf("%s by %s", n, holderLabelOrElse(h, a.RunId)))
	}
	slices.Sort(held)
	if len(dropped) == 0 {
		return withNote(fmt.Sprintf("dropped claude.ai full MCP connectors on resume (exclusive servers held: %s; get them back with `mesh tools add claude.ai` once the holder is done)", strings.Join(held, ", ")), leftNote), true, nil
	}
	note := "dropped exclusive MCP server(s) on resume: " + strings.Join(dropped, ", ")
	if droppedAll {
		note += "; and claude.ai full MCP connectors (exclusive servers held: " + strings.Join(held, ", ") + "; get them back with `mesh tools add claude.ai` once the holder is done)"
	}
	return withNote(note, leftNote), true, nil
}

func withNote(note, more string) string {
	if more == "" {
		return note
	}
	return note + "; and " + more
}

// viewerRun is the run an actor may see agents of: its own when it is an agent, any (empty) for a user or device.
func viewerRun(actor *v1.Actor) string {
	if actor != nil && IsAgent(actor.Class) {
		return actor.RunId
	}
	return ""
}

// holderLabel names the agent holding a server for someone who may see agents of run viewer (empty: any): the
// name and run within that run, else "" (an agent of another run is not named to a worker).
func holderLabel(h *v1.Agent, viewer string) string {
	if viewer != "" && h.RunId != viewer {
		return ""
	}
	if viewer != "" {
		return h.Name
	}
	return fmt.Sprintf("%s (run %s)", h.Name, h.RunId)
}

func holderLabelOrElse(h *v1.Agent, viewer string) string {
	if l := holderLabel(h, viewer); l != "" {
		return l
	}
	return "an agent of another run"
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
