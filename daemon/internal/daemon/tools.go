package daemon

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1/agentmeshv1connect"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter/claude"
	"github.com/Luisotee/agent-mesh/daemon/internal/auth"
	"github.com/Luisotee/agent-mesh/daemon/internal/ids"
	"github.com/Luisotee/agent-mesh/daemon/internal/repoarg"
	"github.com/Luisotee/agent-mesh/daemon/internal/store"
)

var claudeAIConnectorRE = regexp.MustCompile(`(?i)^claude[._ ]ai\b`)

// canAccessTools reports whether actor may inspect (when write is false) or modify (when write is true) a's lean tools.
func canAccessTools(actor *v1.Actor, a *v1.Agent, write bool) error {
	if !IsAgent(actor.Class) {
		return nil
	}
	if a.Id == actor.Id {
		return nil
	}
	if write {
		if actor.Class == v1.ActorClass_ACTOR_CLASS_ROOT && a.ParentId == actor.Id {
			return nil
		}
		return denied("%s may only add tools to itself (or a root to its children)", actor.Name)
	}
	if actor.RunId != "" && a.RunId != actor.RunId {
		return denied("cannot inspect agent %s from another run", a.Name)
	}
	return nil
}

func (d *Daemon) resolveTargetAgent(r store.Reader, actor *v1.Actor, ref string) (*v1.Agent, error) {
	if strings.TrimSpace(ref) == "" {
		if !IsAgent(actor.Class) {
			return nil, invalid("name an agent (--agent)")
		}
		return d.getAgent(r, actor.Id)
	}
	return d.FindAgent(r, actor, ref)
}

func (d *Daemon) leanCut(ctx context.Context, r store.Reader, a *v1.Agent, env map[string]string) (*v1.ListToolsResponse, error) {
	if !d.leanOn(ctx) {
		return &v1.ListToolsResponse{
			Lean:    false,
			McpMode: claude.MCPInherit,
		}, nil
	}
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
	pluginsRaw, err := d.settingStrings(r, "claude.plugins_off")
	if err != nil {
		return nil, err
	}
	keepSkills, err := d.Setting(r, "claude.claudeai_skills")
	if err != nil {
		return nil, err
	}
	toolsCut := dedupe(filterAdded(deny, a.AddedTools, false))
	skillsCut := dedupe(filterAdded(skills, a.AddedTools, true))
	var pluginNames []string
	for _, p := range claude.LeanPlugins(env, filterAddedPlugins(pluginsRaw, a.AddedTools)) {
		pluginNames = append(pluginNames, strings.SplitN(p, "@", 2)[0])
	}
	pluginNames = dedupe(pluginNames)
	slices.Sort(toolsCut)
	slices.Sort(skillsCut)
	slices.Sort(pluginNames)

	claudeaiSkills := ""
	if keepSkills != true && !slices.Contains(a.AddedTools, claude.ClaudeAISkills) {
		claudeaiSkills = claude.ClaudeAISkills
	}

	mode, mcpLoaded, mcpCut, err := d.mcpLoad(r, a, env, nil)
	if err != nil {
		return nil, err
	}

	connectors := ""
	if mode != claude.MCPInherit {
		connectors = claude.ClaudeAIConnectors
	}

	added := append([]string{}, a.AddedTools...)
	added = append(added, a.Mcp...)
	if a.McpAll {
		added = append(added, claude.ClaudeAIConnectors)
	}
	added = dedupe(added)
	slices.Sort(added)

	return &v1.ListToolsResponse{
		Lean:               true,
		Tools:              toolsCut,
		Skills:             skillsCut,
		Plugins:            pluginNames,
		ClaudeaiSkills:     claudeaiSkills,
		McpMode:            mode,
		McpLoaded:          mcpLoaded,
		McpCut:             mcpCut,
		ClaudeaiConnectors: connectors,
		Added:              added,
	}, nil
}

// mcpLoad is the MCP side of a lean launch: the mode it runs in (`inherit` with `mcp_all`, else `repo` or `none`)
// and which of the user's servers it loads and cuts, sorted.
func (d *Daemon) mcpLoad(r store.Reader, a *v1.Agent, env map[string]string, cache map[string]map[string]any) (mode string, loaded, cut []string, err error) {
	mode, err = d.settingString(r, "claude.mcp")
	if err != nil {
		return "", nil, nil, err
	}
	if a.McpAll {
		mode = claude.MCPInherit
	} else if mode != claude.MCPNone && mode != claude.MCPInherit {
		mode = claude.MCPRepo
	}
	known := claude.KnownMCPServersCached(env, a.Repo, cache)
	if mode == claude.MCPInherit {
		for n := range known {
			loaded = append(loaded, n)
		}
	} else {
		keep, err := d.settingStrings(r, "claude.mcp_keep")
		if err != nil {
			return "", nil, nil, err
		}
		want := map[string]bool{}
		if mode == claude.MCPRepo {
			for n, s := range known {
				if s.Scope != "user" {
					want[n] = true
				}
			}
		}
		for _, n := range append(append([]string{}, keep...), a.Mcp...) {
			if _, ok := known[n]; ok {
				want[n] = true
			}
		}
		for n := range known {
			if want[n] {
				loaded = append(loaded, n)
			} else {
				cut = append(cut, n)
			}
		}
	}
	slices.Sort(loaded)
	slices.Sort(cut)
	return mode, loaded, cut, nil
}

// toolsAddedWhy is the `Why` of the reopen `mesh tools add` requests.
const toolsAddedWhy = "tools added"

func resolveAddition(cut *v1.ListToolsResponse, name string) (kind string, canonical string, err error) {
	if slices.Contains(cut.Tools, name) {
		return "tool", name, nil
	}
	if slices.Contains(cut.Skills, name) {
		return "skill", name, nil
	}
	if bare := strings.SplitN(name, "@", 2)[0]; slices.Contains(cut.Plugins, bare) {
		return "plugin", bare, nil
	}
	if cut.ClaudeaiSkills != "" && name == cut.ClaudeaiSkills {
		return "claudeai", cut.ClaudeaiSkills, nil
	}
	if slices.Contains(cut.McpCut, name) {
		return "mcp", name, nil
	}
	if cut.ClaudeaiConnectors != "" && claudeAIConnectorRE.MatchString(name) {
		return "connectors", cut.ClaudeaiConnectors, nil
	}
	var sections []string
	addSec := func(label string, xs []string) {
		if len(xs) > 0 {
			sections = append(sections, label+": "+strings.Join(xs, ", "))
		}
	}
	addSec("tools", cut.Tools)
	addSec("skills", cut.Skills)
	addSec("plugins", cut.Plugins)
	addSec("mcp_cut", cut.McpCut)
	summary := strings.Join(sections, "; ")
	if summary == "" {
		summary = "nothing is cut"
	}
	if cut.ClaudeaiConnectors != "" {
		summary += "; claude.ai connectors: `claude.ai`"
	}
	if cut.ClaudeaiSkills != "" {
		summary += "; " + cut.ClaudeaiSkills
	}
	return "", "", invalid("%s is not something this session runs without (%s)", name, summary)
}

// ListTools lists what a lean Claude session runs without and what it added back.
func (d *Daemon) ListTools(ctx context.Context, req *connect.Request[v1.ListToolsRequest]) (*connect.Response[v1.ListToolsResponse], error) {
	actor := auth.ActorFrom(ctx)
	r := d.store.Read(ctx)
	a, err := d.resolveTargetAgent(r, actor, req.Msg.Agent)
	if err != nil {
		return nil, err
	}
	if err := canAccessTools(actor, a, false); err != nil {
		return nil, err
	}
	if a.Kind != claude.Kind {
		return nil, failedPrecondition("%s is a %s agent: only Claude sessions start lean", a.Name, a.Kind)
	}
	out, err := d.leanCut(ctx, r, a, d.agentEnv(a, ""))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

// AddTools adds cut tools, skills, plugins, MCP servers, or claude.ai connectors back to a Claude agent and
// schedules a reopen of its own conversation at its next IDLE.
func (d *Daemon) AddTools(ctx context.Context, req *connect.Request[v1.AddToolsRequest]) (*connect.Response[v1.AddToolsResponse], error) {
	m := req.Msg
	names := dedupe(func() []string {
		var xs []string
		for _, n := range m.Names {
			for _, p := range strings.Split(n, ",") {
				if p = strings.TrimSpace(p); p != "" {
					xs = append(xs, p)
				}
			}
		}
		return xs
	}())
	if len(names) == 0 {
		return nil, invalid("add what? `mesh tools` lists what this session runs without")
	}
	actor := auth.ActorFrom(ctx)
	targetRef := strings.TrimSpace(m.Agent)
	if targetRef == "" {
		if !IsAgent(actor.Class) {
			return nil, invalid("name an agent (--agent)")
		}
		targetRef = actor.Id
	}
	r0 := d.store.Read(ctx)
	if mid := m.Meta.GetMutationId(); mid != "" {
		if proc, resp, err := r0.Mutation(actor.Id, mid); err == nil && proc == agentmeshv1connect.MeshServiceAddToolsProcedure {
			out := &v1.AddToolsResponse{}
			if uerr := proto.Unmarshal(resp, out); uerr == nil {
				return connect.NewResponse(out), nil
			}
		}
	}
	// Learn session id before acquiring store write lock if needed.
	d.learnSession(ctx, actor, targetRef, func(a *v1.Agent) bool { return a.Kind == claude.Kind && !stopped(a) })

	if !d.leanOn(ctx) {
		return nil, failedPrecondition("claude.lean is off: this session runs with everything already")
	}

	a0, err := d.resolveTargetAgent(r0, actor, targetRef)
	if err != nil {
		return nil, err
	}
	if err := canAccessTools(actor, a0, true); err != nil {
		return nil, err
	}
	mu := d.agentLock(a0.Id)
	mu.Lock()
	defer mu.Unlock()

	out := &v1.AddToolsResponse{}
	_, err = d.write(ctx, m.Meta, agentmeshv1connect.MeshServiceAddToolsProcedure, out, func(tx *store.Tx) error {
		a, err := tx.GetAgent(a0.Id)
		if err != nil {
			return err
		}
		if err := canAccessTools(tx.Actor, a, true); err != nil {
			return err
		}
		if a.Kind != claude.Kind {
			return failedPrecondition("%s is a %s agent: only Claude sessions start lean", a.Name, a.Kind)
		}
		if stopped(a) {
			return failedPrecondition("%s has stopped: resume it first, or pass --mcp on resume", a.Name)
		}
		if a.SessionId == "" {
			return failedPrecondition("%s never reported a conversation id yet, so it cannot be reopened", a.Name)
		}
		cut, err := d.leanCut(ctx, tx.Reader, a, d.agentEnv(a, ""))
		if err != nil {
			return err
		}
		var addedTools, addedMCP, every []string
		toolsAdded, connectors := false, false
		for _, n := range names {
			kind, canon, rerr := resolveAddition(cut, n)
			if rerr != nil {
				return rerr
			}
			every = append(every, canon)
			switch kind {
			case "tool":
				toolsAdded = true
				addedTools = append(addedTools, canon)
			case "mcp":
				addedMCP = append(addedMCP, canon)
			case "connectors":
				connectors = true
			default:
				addedTools = append(addedTools, canon)
			}
		}
		every = dedupe(every)
		slices.Sort(every)

		// Exclusive MCP lease check (connectors enable the user's whole MCP config, including exclusive servers).
		leaseCheck := slices.Clone(addedMCP)
		if connectors {
			excl, err := d.settingStrings(tx.Reader, "claude.mcp_exclusive")
			if err != nil {
				return err
			}
			leaseCheck = append(leaseCheck, excl...)
		}
		if err := d.checkMCPLease(tx.Reader, leaseCheck, a.Id, tx.Actor); err != nil {
			return err
		}

		a.AddedTools = dedupe(append(a.AddedTools, addedTools...))
		a.Mcp = dedupe(append(a.Mcp, addedMCP...))
		if toolsAdded {
			a.ToolsAdded = true
		}
		if connectors {
			a.McpAll = true
		}
		cm, ce, cd := target(a)
		rv, err := requestReopen(tx, a, cm, ce, cd, toolsAddedWhy, strings.Join(every, ", "))
		if err != nil {
			return err
		}
		carry := "where you stopped"
		if nxt := strings.TrimSpace(m.Next); nxt != "" {
			carry = nxt
		}
		body := fmt.Sprintf("mesh reopened this conversation with %s (`mesh tools add`). Carry on: %s.", strings.Join(every, ", "), carry)
		if _, err := tellTx(tx, a, body); err != nil {
			return err
		}
		// The agent adding its own tools tells its parent it is waiting (PROTOCOL.md rule 10): a `wait`, which
		// rings nothing and owes no answer, and which the watch counts as a declared wait.
		if tx.Actor.GetId() == a.Id && a.ParentId != "" {
			if parent, err := tx.GetAgent(a.ParentId); err == nil {
				wait := &v1.Message{Id: ids.New("m", 10), RunId: a.RunId, From: tx.Actor, ToId: parent.Id, ToName: parent.Name,
					Type: v1.MessageType_MESSAGE_TYPE_WAIT, Body: fmt.Sprintf("%s: reopening on its own request with %s (`mesh tools add`); its report is still to come.", a.Name, strings.Join(every, ", ")),
					CreatedAt: tsOf(tx.Now()), DeliveredAt: tsOf(tx.Now())}
				if err := tx.PutMessage(wait); err != nil {
					return err
				}
				if _, err := tx.AppendEvent(&v1.Event{RunId: a.RunId, AgentId: parent.Id,
					Payload: &v1.Event_MessageSent{MessageSent: &v1.MessageSent{Message: wait}}}); err != nil {
					return err
				}
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		if err := tx.PutAgent(a); err != nil {
			return err
		}
		out.Agent = a
		out.Reopen = rv
		out.Added = every
		return nil
	})
	if err != nil {
		return nil, err
	}
	d.deliverSoon(out.Agent.Id)
	out.Agent = d.view(out.Agent)
	return connect.NewResponse(out), nil
}

// ListMCPServers lists the user's known MCP servers, whether each is always loaded (`claude.mcp_keep`), whether it is
// exclusive (`claude.mcp_exclusive`), and any live agent currently holding it.
func (d *Daemon) ListMCPServers(ctx context.Context, req *connect.Request[v1.ListMCPServersRequest]) (*connect.Response[v1.ListMCPServersResponse], error) {
	r := d.store.Read(ctx)
	viewer := viewerRun(auth.ActorFrom(ctx))
	repo := strings.TrimSpace(req.Msg.Repo)
	if repo != "" {
		if top, err := d.repoTop(ctx, repo); err == nil {
			repo = top
		} else if repoarg.IsBareName(repo) {
			return nil, err // a name that finds no repo is not a folder to read config from
		}
	}
	known := claude.KnownMCPServers(d.agentEnv(&v1.Agent{}, ""), repo)
	keep, err := d.settingStrings(r, "claude.mcp_keep")
	if err != nil {
		return nil, err
	}
	exclSet, err := d.exclusiveSet(r)
	if err != nil {
		return nil, err
	}
	aboutV, err := d.Setting(r, "claude.mcp_about")
	if err != nil {
		return nil, err
	}
	customAbout, _ := aboutV.(map[string]any)
	keepSet := map[string]bool{}
	for _, n := range keep {
		keepSet[n] = true
	}
	holders, err := d.exclusiveHolders(r, "")
	if err != nil {
		return nil, err
	}
	var names []string
	for n := range known {
		names = append(names, n)
	}
	sort.Strings(names)
	out := &v1.ListMCPServersResponse{}
	for _, n := range names {
		srv := known[n]
		heldBy := ""
		if exclSet[n] {
			if h, ok := holders[n]; ok {
				heldBy = holderLabelOrElse(h, viewer)
			}
		}
		out.Servers = append(out.Servers, &v1.MCPServerInfo{
			Name:      n,
			Scope:     srv.Scope,
			About:     claude.MCPAbout(n, srv, customAbout),
			Always:    keepSet[n],
			Exclusive: exclSet[n],
			HeldBy:    heldBy,
		})
	}
	return connect.NewResponse(out), nil
}
