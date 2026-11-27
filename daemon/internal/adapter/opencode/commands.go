package opencode

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"connectrpc.com/connect"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter"
	"github.com/Luisotee/agent-mesh/daemon/internal/probe"
)

// CommandsVersion is the OpenCode version the table below was read from (the `/` popup of 1.19.6, with and
// without a session). The TUI lists its built-ins only in a terminal, so they are a table; the commands a repo or
// the user defines are read from `opencode debug config` (its `command` map, which includes the `command`
// folders' markdown files). A command a newer OpenCode has and this lacks is simply not offered.
const CommandsVersion = "1.19.6"

var builtinCommands = []adapter.Cmd{
	{Name: "agents", Desc: "Switch agent", Dialog: adapter.DialogAlways},
	{Name: "compact", Aliases: []string{"summarize"}, Desc: "Compact session"},
	{Name: "connect", Desc: "Connect provider", Dialog: adapter.DialogAlways},
	{Name: "copy", Desc: "Copy session transcript"},
	{Name: "debug", Desc: "View debug info", Dialog: adapter.DialogAlways},
	{Name: "diff", Desc: "Open diff viewer", Dialog: adapter.DialogAlways},
	{Name: "editor", Desc: "Open editor", Dialog: adapter.DialogAlways},
	{Name: "exit", Desc: "Exit the app", Danger: true},
	{Name: "export", Desc: "Export session transcript", Dialog: adapter.DialogBare},
	{Name: "fork", Desc: "Fork session"},
	{Name: "help", Desc: "Help", Dialog: adapter.DialogAlways},
	{Name: "init", Desc: "Guided AGENTS.md setup", Dialog: adapter.DialogBare},
	{Name: "mcps", Desc: "Toggle MCPs", Dialog: adapter.DialogAlways},
	{Name: "models", Desc: "Switch model", Dialog: adapter.DialogAlways},
	{Name: "move", Desc: "Move to another project dir", Dialog: adapter.DialogAlways},
	{Name: "new", Desc: "New session"},
	{Name: "redo", Desc: "Redo"},
	{Name: "rename", Desc: "Rename session", Dialog: adapter.DialogBare},
	{Name: "review", Desc: "Review changes [commit|branch|pr], defaults to uncommitted", Hint: "[commit|branch|pr]"},
	{Name: "sessions", Desc: "Switch session", Dialog: adapter.DialogAlways},
	{Name: "skills", Desc: "Skills", Dialog: adapter.DialogAlways},
	{Name: "status", Desc: "View status", Dialog: adapter.DialogAlways},
	{Name: "themes", Desc: "Switch theme", Dialog: adapter.DialogAlways},
	{Name: "thinking", Aliases: []string{"toggle-thinking"}, Desc: "Toggle thinking visibility"},
	{Name: "timeline", Desc: "Jump to message", Dialog: adapter.DialogAlways},
	{Name: "timestamps", Aliases: []string{"toggle-timestamps"}, Desc: "Toggle timestamps"},
	{Name: "undo", Desc: "Undo previous message"},
}

// ListCommands lists the built-ins and the custom commands of the agent's worktree and the user's config.
func (a *Adapter) ListCommands(ctx context.Context, req *connect.Request[adapterv1.ListCommandsRequest]) (*connect.Response[adapterv1.ListCommandsResponse], error) {
	m := req.Msg
	cmds := adapter.Commands(builtinCommands)
	bin := m.Binary
	if bin == "" {
		bin = "opencode"
	}
	out, err := probe.Run(ctx, probe.Spec{Bin: bin, Args: []string{"debug", "config"}, Cwd: m.Cwd, Env: mapToEnv(m.Env),
		Dir: a.ProbeDir, Timeout: 10 * time.Second})
	if err == nil {
		cmds = adapter.MergeCommands(cmds, adapter.Commands(customCommands(out)))
	}
	return connect.NewResponse(&adapterv1.ListCommandsResponse{Commands: cmds}), nil
}

// customCommands reads the `command` map of `opencode debug config`.
func customCommands(out []byte) []adapter.Cmd {
	var cfg struct {
		Command map[string]struct {
			Description string `json:"description"`
			Template    string `json:"template"`
		} `json:"command"`
	}
	// The command prints its JSON after any log lines: from the first brace.
	if i := strings.Index(string(out), "{"); i >= 0 {
		out = out[i:]
	}
	if json.Unmarshal(out, &cfg) != nil {
		return nil
	}
	var cs []adapter.Cmd
	for name, c := range cfg.Command {
		desc := c.Description
		if desc == "" {
			desc = oneLine(c.Template)
		}
		cs = append(cs, adapter.Cmd{Name: name, Desc: desc, Source: "custom"})
	}
	return cs
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > 120 { // by characters: a cut inside one would be invalid UTF-8, which a proto string refuses
		s = string(r[:120]) + "…"
	}
	return s
}
