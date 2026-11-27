package agy

import (
	"context"
	"path/filepath"

	"connectrpc.com/connect"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter"
)

// CommandsVersion is the agy version the built-in table below was read from (the `/` popup of 1.5.18). agy
// lists no commands without a terminal, so they are a table, plus the skills it reads from disk; a command a newer
// agy has and this lacks is simply not offered (typing it still works). Which commands open a dialog was judged
// from the popup's descriptions: the ones not run live are `Bare`, whose only cost is the terminal tab opening.
const CommandsVersion = "1.5.18"

var builtinCommands = []adapter.Cmd{
	{Name: "add-dir", Desc: "Add a directory to the workspace", Dialog: adapter.DialogBare},
	{Name: "agents", Desc: "List available custom agents", Dialog: adapter.DialogAlways},
	{Name: "artifact", Desc: "View and review artifacts", Dialog: adapter.DialogAlways},
	{Name: "btw", Desc: "Ask a side question without interrupting the current task", Dialog: adapter.DialogBare},
	{Name: "changelog", Desc: "Show release notes and changes", Dialog: adapter.DialogAlways},
	{Name: "clear", Aliases: []string{"new"}, Desc: "Clear conversation and start a new one"},
	{Name: "codesearch", Aliases: []string{"cs"}, Desc: "Search code in the workspace", Hint: "<query>"},
	{Name: "compact", Desc: "Compact the conversation now (summarize history to free up the context window)"},
	{Name: "config", Aliases: []string{"settings"}, Desc: "Open settings panel", Dialog: adapter.DialogAlways},
	{Name: "context", Desc: "Visualize current context usage", Dialog: adapter.DialogAlways},
	{Name: "copy", Desc: "Copy the last planner response to the clipboard, the n-th with '/copy n'", Hint: "[n]"},
	{Name: "credits", Desc: "Show remaining G1 credits and purchase link", Dialog: adapter.DialogAlways},
	{Name: "diff", Desc: "View uncommitted changes and per-turn diffs", Dialog: adapter.DialogAlways},
	{Name: "effort", Desc: "Set the reasoning effort", Dialog: adapter.DialogBare},
	{Name: "exit", Aliases: []string{"quit"}, Desc: "Exit the CLI", Danger: true},
	{Name: "feedback", Desc: "Submit qualitative feedback to improve the agent", Dialog: adapter.DialogBare},
	{Name: "fork", Aliases: []string{"branch"}, Desc: "Create a branch of the current conversation at this point"},
	{Name: "goal", Desc: "Run until the specified goal is completely finished", Hint: "<goal>"},
	{Name: "help", Desc: "Show available commands and keybindings", Dialog: adapter.DialogAlways},
	{Name: "hooks", Desc: "Manage hook configurations for tool events", Dialog: adapter.DialogAlways},
	{Name: "keybindings", Desc: "Set custom keybindings", Dialog: adapter.DialogAlways},
	{Name: "logout", Desc: "Log out", Dialog: adapter.DialogBare, Danger: true},
	{Name: "mcp", Desc: "Manage MCP servers", Dialog: adapter.DialogAlways},
	{Name: "model", Desc: "Set a model, or run a single prompt on another model", Dialog: adapter.DialogBare},
	{Name: "open", Desc: "Open a file or view opened/edited files", Dialog: adapter.DialogBare},
	{Name: "permissions", Desc: "Manage tool permissions", Dialog: adapter.DialogAlways},
	{Name: "plan", Desc: "Plan carefully before executing a task"},
	{Name: "plugin", Desc: "Manage, discover, install, uninstall, enable, and disable plugins", Dialog: adapter.DialogAlways},
	{Name: "projects", Desc: "Create and edit projects", Dialog: adapter.DialogAlways},
	{Name: "record", Desc: "Dictate a prompt using your microphone", Dialog: adapter.DialogAlways},
	{Name: "remote-control", Desc: "Turn on remote control for this session (off to disable)", Dialog: adapter.DialogBare},
	{Name: "rename", Desc: "Rename the current conversation", Hint: "[name]", Dialog: adapter.DialogBare},
	{Name: "resume", Aliases: []string{"switch"}, Desc: "Browse and resume past conversations", Dialog: adapter.DialogAlways},
	{Name: "rewind", Aliases: []string{"undo"}, Desc: "Rewind conversation to a previous message", Dialog: adapter.DialogAlways},
	{Name: "schedule", Desc: "Run an instruction on a recurring schedule or as a one-time timer"},
	{Name: "skills", Desc: "List available skills", Dialog: adapter.DialogAlways},
	{Name: "statusline", Desc: "Toggle the statusline"},
	{Name: "tasks", Desc: "View background tasks", Dialog: adapter.DialogAlways},
	{Name: "thinking", Desc: "Toggle streaming the model's full thinking in the transcript"},
	{Name: "title", Desc: "Toggle custom terminal window title"},
	{Name: "usage", Aliases: []string{"quota"}, Desc: "View model quota usage", Dialog: adapter.DialogAlways},
	{Name: "voice", Desc: "Dictate a prompt or talk with the agent by voice", Dialog: adapter.DialogAlways},
	{Name: "boost", Desc: "Invoke the Boost multi-agent orchestrator for complex tasks"},
	{Name: "browser", Desc: "Invoke a browser agent for web tasks"},
	{Name: "grill-me", Desc: "Interview me to align on a plan"},
	{Name: "learn", Desc: "Reflect on recent successes or corrections to capture reusable skills or rules"},
	{Name: "swarm", Desc: "Run a swarm: several agents working in parallel on one task"},
	{Name: "teamwork", Desc: "Invoke a team of agents to autonomously tackle large projects"},
}

// ListCommands lists the built-ins and the skills agy reads from the user's config (`~/.gemini/config/skills`) and
// its plugins (`~/.gemini/config/plugins/<plugin>/skills/<skill>`, offered as `/plugin:skill`).
func (a *Adapter) ListCommands(_ context.Context, req *connect.Request[adapterv1.ListCommandsRequest]) (*connect.Response[adapterv1.ListCommandsResponse], error) {
	cmds := adapter.Commands(builtinCommands)
	if home := req.Msg.Env["HOME"]; home != "" {
		cfg := filepath.Join(home, ".gemini", "config")
		user := adapter.SkillCmds(filepath.Join(cfg, "skills"), "", "skill")
		var plugin []adapter.Cmd
		if dirs, err := filepath.Glob(filepath.Join(cfg, "plugins", "*", "skills")); err == nil {
			for _, d := range dirs {
				plugin = append(plugin, adapter.SkillCmds(d, filepath.Base(filepath.Dir(d)), "plugin")...)
			}
		}
		cmds = adapter.MergeCommands(cmds, adapter.Commands(user), adapter.Commands(plugin))
	}
	return connect.NewResponse(&adapterv1.ListCommandsResponse{Commands: cmds}), nil
}
