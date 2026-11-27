package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	v1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter"
	"github.com/Luisotee/agent-mesh/daemon/internal/probe"
)

// The commands Claude Code accepts at its prompt, for the composer's `/` completion. Two sources, merged:
//
//   - Claude's own answer to the SDK's `initialize` control request (`probeCommands`): every command and skill it
//     loads for this worktree (the repo's and the user's `.claude/commands` and skills, enabled plugins, the
//     share plugin's skills), with descriptions and argument hints. It starts no model call and no session. It
//     leaves out the commands that only make sense in the terminal UI (dialogs): those are `tuiCommands`.
//   - `tuiCommands`: the terminal UI's own, read from the popup of Claude Code 2.1.332 (a command a newer Claude
//     has and this lacks is simply not offered; typing it still works).
//
// `dialogs` says which commands of the first source open a dialog or picker in the terminal when typed (the
// headless answer doesn't know).

// CommandsVersion is the Claude Code version the tables below were read from.
const CommandsVersion = "2.1.332"

// commandsTTL is how long one worktree's probed list is kept: a skill the user adds shows up within it.
const commandsTTL = 30 * time.Second

// tuiCommands are the terminal-only built-ins (dialogs mostly).
var tuiCommands = []adapter.Cmd{
	{Name: "add-dir", Desc: "Add a working directory", Hint: "<path>", Dialog: adapter.DialogBare},
	{Name: "branch", Desc: "Create a branch of the current conversation at this point", Hint: "[name]"},
	{Name: "btw", Desc: "Ask a side question without interrupting Claude", Hint: "[question]", Dialog: adapter.DialogBare},
	{Name: "bug", Desc: "Report a bug or share your session", Hint: "[report]", Dialog: adapter.DialogBare},
	{Name: "cd", Desc: "Move this session to a new working directory", Hint: "<path>", Dialog: adapter.DialogBare},
	{Name: "copy", Desc: "Copy the last response to clipboard (/copy 2 for the one before)", Dialog: adapter.DialogBare},
	{Name: "diff", Desc: "View uncommitted changes and per-turn diffs", Dialog: adapter.DialogAlways},
	{Name: "exit", Aliases: []string{"quit"}, Desc: "Exit the CLI", Danger: true},
	{Name: "export", Desc: "Export this session to a file or clipboard", Hint: "[filename]", Dialog: adapter.DialogBare},
	{Name: "feedback", Desc: "Send feedback to Anthropic or report a bug", Hint: "[report]", Dialog: adapter.DialogBare},
	{Name: "fork", Desc: "Copy this conversation into a new background session and keep working here", Hint: "[prompt]"},
	{Name: "help", Desc: "Show help and available commands", Hint: "[command]", Dialog: adapter.DialogAlways},
	{Name: "hooks", Desc: "View hook configurations for tool events", Dialog: adapter.DialogAlways},
	{Name: "ide", Desc: "Manage IDE integrations and show status", Hint: "[open]", Dialog: adapter.DialogBare},
	{Name: "install-github-app", Desc: "Set up Claude GitHub Actions for a repository", Dialog: adapter.DialogAlways},
	{Name: "keybindings", Desc: "Open your keyboard shortcuts file"},
	{Name: "login", Desc: "Sign in with your Anthropic account", Dialog: adapter.DialogAlways},
	{Name: "logout", Desc: "Sign out from your Anthropic account", Dialog: adapter.DialogAlways, Danger: true},
	{Name: "memory", Desc: "Edit CLAUDE.md files and memory settings", Dialog: adapter.DialogAlways},
	{Name: "mobile", Desc: "Show QR code to download the Claude mobile app", Dialog: adapter.DialogAlways},
	{Name: "permissions", Aliases: []string{"allowed-tools"}, Desc: "Manage allow and deny tool permission rules", Dialog: adapter.DialogAlways},
	{Name: "plan", Desc: "Turn on plan mode or view the plan", Hint: "[open|<description>]"},
	{Name: "plugin", Aliases: []string{"plugins", "marketplace"}, Desc: "Manage Claude Code plugins", Dialog: adapter.DialogAlways},
	{Name: "privacy-settings", Desc: "View and update your privacy settings", Dialog: adapter.DialogAlways},
	{Name: "release-notes", Desc: "View release notes"},
	{Name: "remote-control", Desc: "Control this session from your phone or claude.ai/code", Dialog: adapter.DialogAlways},
	{Name: "resume", Desc: "Resume a previous session", Hint: "[session]", Dialog: adapter.DialogAlways},
	{Name: "rewind", Desc: "Restore earlier code, conversation, or both", Dialog: adapter.DialogAlways},
	{Name: "sandbox", Desc: "Configure the sandbox", Dialog: adapter.DialogBare},
	{Name: "skills", Desc: "List available skills", Dialog: adapter.DialogAlways},
	{Name: "status", Desc: "Show version, account, model, MCP servers, and setup warnings", Dialog: adapter.DialogAlways},
	{Name: "statusline", Desc: "Set up the status line"},
	{Name: "subtask", Desc: "Send a subagent off with your full context; its result comes back here", Hint: "<task>"},
	{Name: "tasks", Aliases: []string{"bashes"}, Desc: "View and manage this session's background work", Dialog: adapter.DialogAlways},
	{Name: "terminal-setup", Desc: "Set up Shift+Enter for newlines", Dialog: adapter.DialogAlways},
	{Name: "theme", Desc: "Change the theme", Dialog: adapter.DialogAlways},
	{Name: "tui", Desc: "Set the terminal UI renderer (classic | fullscreen)", Hint: "[classic|fullscreen]", Dialog: adapter.DialogBare},
}

// dialogs: the commands the headless list has that open a dialog in the terminal. `config`'s headless form sets
// a key; the terminal's opens the settings panel.
var dialogs = map[string]adapter.Cmd{
	"config":       {Desc: "Open settings", Hint: "[key=value]", Dialog: adapter.DialogBare},
	"model":        {Dialog: adapter.DialogBare},
	"effort":       {Dialog: adapter.DialogBare},
	"mcp":          {Dialog: adapter.DialogBare},
	"output-style": {Dialog: adapter.DialogBare},
	"fast":         {Dialog: adapter.DialogBare},
	"advisor":      {Dialog: adapter.DialogBare},
	"autocompact":  {Dialog: adapter.DialogBare},
	"import":       {Dialog: adapter.DialogBare},
	"usage":        {Aliases: []string{"cost", "stats"}, Dialog: adapter.DialogAlways},
	"doctor":       {Dialog: adapter.DialogAlways},
}

// builtinNames are the headless list's built-ins (what isn't a skill, a plugin's or the user's own).
var builtinNames = map[string]bool{
	"compact": true, "clear": true, "config": true, "context": true, "effort": true, "fast": true, "init": true,
	"mcp": true, "model": true, "output-style": true, "usage": true, "rename": true, "color": true, "focus": true,
	"import": true, "reload-plugins": true, "reload-skills": true, "advisor": true, "autocompact": true,
	"doctor": true, "insights": true, "recap": true, "goal": true, "ultrareview": true, "security-review": true,
	"skill-doctor": true, "heapdump": true, "extra-usage": true, "usage-credits": true,
	"list-agents": true, "auto-mode-setup": true, "team-onboarding": true,
}

// ListCommands lists what the agent's Claude Code accepts at its prompt.
func (a *Adapter) ListCommands(ctx context.Context, req *connect.Request[adapterv1.ListCommandsRequest]) (*connect.Response[adapterv1.ListCommandsResponse], error) {
	m := req.Msg
	// The cached list is shared by every caller: the overrides go on copies.
	probed := append([]*v1.AgentCommand(nil), a.probedCommands(ctx, m)...)
	for i, c := range probed {
		c = proto.CloneOf(c)
		probed[i] = c
		if o, ok := dialogs[c.Name]; ok && c.Source == "builtin" {
			c.Dialog = o.Dialog
			if o.Desc != "" {
				c.Description = o.Desc
			}
			if o.Hint != "" {
				c.ArgumentHint = o.Hint
			}
			c.Aliases = append(c.Aliases, o.Aliases...)
		}
	}
	return connect.NewResponse(&adapterv1.ListCommandsResponse{Commands: adapter.MergeCommands(probed, adapter.Commands(tuiCommands))}), nil
}

type cmdEntry struct {
	at    time.Time
	cmds  []*v1.AgentCommand
	ready chan struct{}
}

// probedCommands is the headless list for a worktree, kept for commandsTTL, one probe at a time per worktree. A
// probe that fails gives nothing (the table's commands are still offered) and is not kept long.
func (a *Adapter) probedCommands(ctx context.Context, m *adapterv1.ListCommandsRequest) []*v1.AgentCommand {
	settings := probeSettings(m)
	key := m.Cwd + "\x00" + settings
	if m.ReadOnly {
		key += "\x00ro"
	}
	ttl := a.CommandsTTL
	if ttl == 0 {
		ttl = commandsTTL
	}
	for {
		a.cmdMu.Lock()
		if a.cmds == nil {
			a.cmds = map[string]*cmdEntry{}
		}
		e := a.cmds[key]
		switch {
		case e == nil || (e.ready == nil && time.Since(e.at) > ttl):
			// Worktrees that went away leave their entries: dropped when they are long stale.
			for k, old := range a.cmds {
				if old.ready == nil && time.Since(old.at) > 10*time.Minute {
					delete(a.cmds, k)
				}
			}
			e = &cmdEntry{ready: make(chan struct{})}
			a.cmds[key] = e
			a.cmdMu.Unlock()
			// A caller that leaves while the probe runs doesn't kill it (it is everyone's, `runCommandsProbe`); one that
			// leaves while it waits for a probe slot gives its place up, and the entry goes so the next caller probes.
			cmds, err := a.runCommandsProbe(ctx, m, settings)
			a.cmdMu.Lock()
			if err != nil && ctx.Err() != nil {
				delete(a.cmds, key)
				close(e.ready)
				a.cmdMu.Unlock()
				return nil
			}
			e.cmds, e.at = cmds, time.Now()
			if err != nil {
				e.at = time.Now().Add(-ttl + 5*time.Second) // tried again after 5 s, not at every keystroke
			}
			close(e.ready)
			e.ready = nil
			a.cmdMu.Unlock()
			return cmds
		case e.ready != nil:
			ready := e.ready
			a.cmdMu.Unlock()
			select {
			case <-ready:
			case <-ctx.Done():
				return nil
			}
		default:
			cmds := e.cmds
			a.cmdMu.Unlock()
			return cmds
		}
	}
}

// commandsArgv is the probe's command line. It leaves no trace: no session file (`--no-session-persistence`), no
// hook of the user's, the repo's or the mesh's (`disableAllHooks`), no MCP server started (`--strict-mcp-config`
// with no config: a repo's `.mcp.json` can't run a program from a probe), and for a read-only agent the user's
// settings only, as its launch.
func commandsArgv(bin, share string, readOnly bool, settings string) []string {
	argv := []string{bin, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--no-session-persistence", "--strict-mcp-config", "--settings", settings}
	if readOnly {
		argv = append(argv, "--setting-sources", "user")
	}
	if share != "" {
		argv = append(argv, "--plugin-dir", share)
	}
	return argv
}

// probeSettings is the probe's `--settings`: hooks off, and what the agent's launch turns off (its plugins, the
// claude.ai skills) and the share plugin's own copies in the user's plugins (`shadowedPlugins`), so the list is the
// one the launched CLI has.
func probeSettings(m *adapterv1.ListCommandsRequest) string {
	out := map[string]any{"disableAllHooks": true}
	if m.Lean != nil {
		for k, v := range leanSettings(m.Env, &adapterv1.LeanLaunch{OffPlugins: m.Lean.OffPlugins, HideClaudeaiSkills: m.Lean.HideClaudeaiSkills}) {
			out[k] = v
		}
	}
	if share := shareOf(m.Env); share != "" {
		if off := shadowedPlugins(m.Env, share); len(off) > 0 {
			merged := map[string]any{}
			if cur, ok := out["enabledPlugins"].(map[string]bool); ok {
				for k, v := range cur {
					merged[k] = v
				}
			}
			for k, v := range off {
				merged[k] = v
			}
			out["enabledPlugins"] = merged
		}
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// shareOf is the share folder of an environment when it is the Claude plugin (`.claude-plugin/plugin.json`).
func shareOf(env map[string]string) string {
	if s := env["AGENT_MESH_SHARE"]; s != "" {
		if _, err := os.Stat(filepath.Join(s, ".claude-plugin", "plugin.json")); err == nil {
			return s
		}
	}
	return ""
}

const initializeRequest = `{"type":"control_request","request_id":"mesh-commands","request":{"subtype":"initialize"}}` + "\n"

// runCommandsProbe runs the probe. It belongs to everyone waiting on it, not to the caller that started it: once it
// has a slot it runs on to its end whether that caller leaves or not (a leaving caller must not kill it and have its
// cancellation cached as a failure); ctx is only the wait for the slot.
func (a *Adapter) runCommandsProbe(ctx context.Context, m *adapterv1.ListCommandsRequest, settings string) ([]*v1.AgentCommand, error) {
	bin := m.Binary
	if bin == "" {
		bin = "claude"
	}
	argv := commandsArgv(bin, shareOf(m.Env), m.ReadOnly, settings)
	var env []string
	for k, v := range m.Env {
		if strings.HasPrefix(k, "AGENT_MESH_") && k != "AGENT_MESH_SHARE" {
			continue // the probe is nobody's agent
		}
		env = append(env, k+"="+v)
	}
	for _, k := range SessionVars {
		env = dropVar(env, k)
	}
	timeout := a.ProbeTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	// Two probes at most at once (each is a whole Claude process): the others wait, or give up with their caller.
	slots := a.slots()
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	out, err := probe.Run(context.WithoutCancel(ctx), probe.Spec{Bin: argv[0], Args: argv[1:], Cwd: m.Cwd, Stdin: []byte(initializeRequest),
		Env: env, Dir: a.ProbeDir, Timeout: timeout})
	cmds := parseInitialize(out)
	if len(cmds) == 0 && err == nil {
		err = os.ErrNotExist
	}
	return cmds, err
}

// slots is the probes' semaphore, made on first use (an Adapter built as a literal has none).
func (a *Adapter) slots() chan struct{} {
	a.cmdMu.Lock()
	defer a.cmdMu.Unlock()
	if a.probeSlots == nil {
		a.probeSlots = make(chan struct{}, 2)
	}
	return a.probeSlots
}

func dropVar(env []string, k string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, k+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// parseInitialize reads the `commands` of Claude's answer to the `initialize` control request from its
// stream-json output.
func parseInitialize(out []byte) []*v1.AgentCommand {
	var cmds []*v1.AgentCommand
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var l struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string `json:"subtype"`
				RequestID string `json:"request_id"`
				Response  struct {
					Commands []struct {
						Name         string `json:"name"`
						Description  string `json:"description"`
						ArgumentHint string `json:"argumentHint"`
					} `json:"commands"`
				} `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.Type != "control_response" || l.Response.RequestID != "mesh-commands" || l.Response.Subtype != "success" {
			continue
		}
		for _, c := range l.Response.Response.Commands {
			if c.Name == "" || strings.ContainsAny(c.Name, " \t") || strings.HasPrefix(c.Name, "__") || strings.HasPrefix(c.Description, "(removed)") {
				continue // an MCP prompt's display name, an internal command, a command Claude kept only to say it is gone
			}
			desc, src := describeSource(c.Name, c.Description)
			cmds = append(cmds, &v1.AgentCommand{Name: c.Name, Description: desc, ArgumentHint: c.ArgumentHint, Source: src})
		}
		break
	}
	return cmds
}

// describeSource says where a listed command comes from and takes the marker off its description: the headless
// list ends a user's or a project's with "(user)" or "(project)" and starts a plugin's with "(plugin-name) ".
func describeSource(name, desc string) (string, string) {
	for _, s := range []string{"user", "project"} {
		if t, ok := strings.CutSuffix(desc, " ("+s+")"); ok {
			return t, s
		}
	}
	if strings.Contains(name, ":") {
		if strings.HasPrefix(desc, "(") {
			if i := strings.Index(desc, ") "); i > 0 {
				desc = desc[i+2:]
			}
		}
		return desc, "plugin"
	}
	switch {
	case builtinNames[name]:
		return desc, "builtin"
	}
	return desc, "skill"
}
