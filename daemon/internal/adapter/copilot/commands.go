package copilot

import (
	"context"
	"path/filepath"

	"connectrpc.com/connect"

	adapterv1 "github.com/Luisotee/agent-mesh/daemon/gen/agentmesh/adapter/v1"
	"github.com/Luisotee/agent-mesh/daemon/internal/adapter"
)

// CommandsVersion is the Copilot CLI version the table below was read from (the `/` popup of 1.0.116). The CLI
// lists no commands without a terminal, so they are a table, plus the skills it reads from disk; a command a newer
// Copilot has and this lacks is simply not offered (typing it still works). The dialog column was judged from the
// descriptions, not run live (this account's Copilot is policy-blocked): the uncertain ones are `Bare`.
const CommandsVersion = "1.0.116"

var builtinCommands = []adapter.Cmd{
	{Name: "add-dir", Desc: "Allow file access to a directory and load its .github skills and agents as trusted configuration", Hint: "<path>", Dialog: adapter.DialogBare},
	{Name: "after", Desc: "Schedule a one-shot prompt, skill, or schedulable slash command for this session", Hint: "<delay> <prompt>"},
	{Name: "agent", Desc: "Browse and select agents", Hint: "[name]", Dialog: adapter.DialogBare},
	{Name: "ahp", Desc: "Agent Host Protocol: manage the hosts your sessions run on, and the sessions on them", Dialog: adapter.DialogAlways},
	{Name: "allow-all", Desc: "Enable all permissions (tools, paths, and URLs)"},
	{Name: "app", Desc: "Try out the GitHub Copilot desktop app"},
	{Name: "ask", Desc: "Ask a side question or reopen a recently cached answer without adding to history", Hint: "[question]", Dialog: adapter.DialogBare},
	{Name: "autopilot", Desc: "Toggle autopilot mode, or set an autopilot objective"},
	{Name: "changelog", Desc: "Display changelog for CLI versions. Add 'summarize' to get an AI summary", Hint: "[summarize]"},
	{Name: "chronicle", Desc: "Session history tools and insights", Dialog: adapter.DialogBare},
	{Name: "clear", Desc: "Abandon this session and start fresh"},
	{Name: "collect-debug-logs", Desc: "Collect debug logs to a local .tgz file, share with GitHub after consent", Dialog: adapter.DialogBare},
	{Name: "compact", Desc: "Summarize conversation history to reduce context window usage", Hint: "[focus instructions]"},
	{Name: "computer", Desc: "Show or toggle Computer Use"},
	{Name: "config", Desc: "Open the configuration screen, or one area of it (e.g. '/config mcp')", Hint: "[area]", Dialog: adapter.DialogBare},
	{Name: "context", Desc: "Show context window token usage for the current model"},
	{Name: "copy", Desc: "Copy the last response to the clipboard as markdown"},
	{Name: "cwd", Desc: "Change working directory or show current directory", Hint: "[path]"},
	{Name: "delegate", Desc: "Send this session to GitHub and Copilot will create a PR", Hint: "[--base <branch>]"},
	{Name: "diagnose", Desc: "Analyze session logs, including why a permission request needed approval"},
	{Name: "diff", Desc: "Review the changes made in the current directory", Dialog: adapter.DialogAlways},
	{Name: "env", Desc: "Show loaded environment details (instructions, MCP servers, skills, agents, hooks, plugins, LSPs)", Dialog: adapter.DialogAlways},
	{Name: "environments", Desc: "Open the Environments screen to manage incoming and outgoing connections", Dialog: adapter.DialogAlways},
	{Name: "every", Desc: "Schedule a recurring prompt, skill, or schedulable slash command for this session", Hint: "<interval> <prompt>"},
	{Name: "exit", Desc: "Exit the CLI; use 'print' to print the session after exiting alt screen", Danger: true},
	{Name: "experimental", Desc: "Show available experimental features", Dialog: adapter.DialogBare},
	{Name: "feedback", Desc: "Report a bug or feature request, or send private feedback", Dialog: adapter.DialogBare},
	{Name: "fleet", Desc: "Enable fleet mode for parallel subagent execution"},
	{Name: "fork", Desc: "Fork the current session into a new session, optionally with a name, and switch to it", Hint: "[name]"},
	{Name: "help", Desc: "Show help for interactive commands", Dialog: adapter.DialogAlways},
	{Name: "ide", Desc: "Connect to an IDE workspace", Dialog: adapter.DialogBare},
	{Name: "index", Desc: "Inspect and manage this worktree's shared search index (experimental)", Dialog: adapter.DialogBare},
	{Name: "init", Desc: "Initialize instructions; add guidance after /init", Hint: "[guidance]"},
	{Name: "instructions", Desc: "View and toggle custom instruction files", Dialog: adapter.DialogAlways},
	{Name: "keep-alive", Desc: "Prevent sleep this run: on/off/busy or a duration. No args: status", Hint: "[on|off|busy|30m]"},
	{Name: "limits", Desc: "View or edit session limits; the AI Credit limit is a soft cap", Dialog: adapter.DialogBare},
	{Name: "list-dirs", Desc: "Display allowed directories and exact session path grants"},
	{Name: "login", Desc: "Log in to Copilot", Dialog: adapter.DialogAlways},
	{Name: "logout", Desc: "Sign out of an account you added with /login", Dialog: adapter.DialogBare, Danger: true},
	{Name: "lsp", Desc: "Manage language server configuration", Dialog: adapter.DialogBare},
	{Name: "mcp", Desc: "Manage MCP server configuration", Dialog: adapter.DialogBare},
	{Name: "memory", Desc: "Show memory status, or enable/disable memory across sessions", Dialog: adapter.DialogBare},
	{Name: "model", Desc: "Select the AI model for this session (use 'auto' to let Copilot pick automatically)", Hint: "[model]", Dialog: adapter.DialogBare},
	{Name: "move", Desc: "Move your uncommitted changes into a new git worktree and switch into it", Hint: "[branch]", Dialog: adapter.DialogBare},
	{Name: "new", Desc: "Start a new conversation"},
	{Name: "permissions", Desc: "Switch between permission modes", Dialog: adapter.DialogAlways},
	{Name: "plan", Desc: "Create an implementation plan before coding", Hint: "[task]"},
	{Name: "plugin", Desc: "Manage plugins and plugin marketplaces", Dialog: adapter.DialogBare},
	{Name: "pr", Desc: "Operate on pull requests for the current branch", Dialog: adapter.DialogBare},
	{Name: "refine", Desc: "Rewrite a rough prompt into a clear one for review", Hint: "<prompt>"},
	{Name: "remote", Desc: "Show remote status or toggle remote control from GitHub web and mobile"},
	{Name: "rename", Desc: "Rename the current session, or auto-generate a name from conversation", Hint: "[name]"},
	{Name: "research", Desc: "Run deep research investigation using GitHub search and web sources", Hint: "<topic>"},
	{Name: "reset-allowed-tools", Desc: "Reset session tool and exact-path approvals, turn off allow-all"},
	{Name: "restart", Desc: "Restart the CLI, restoring supported live sessions in this process"},
	{Name: "resume", Desc: "Switch sessions by local or cloud history ID, task ID, or name", Hint: "[session]", Dialog: adapter.DialogBare},
	{Name: "review", Desc: "Run code review agent to analyze changes"},
	{Name: "sandbox", Desc: "Manage sandbox settings and permissions", Dialog: adapter.DialogBare},
	{Name: "search", Desc: "Search the conversation timeline (experimental)", Dialog: adapter.DialogBare},
	{Name: "security-review", Desc: "Scan uncommitted changes for security vulnerabilities"},
	{Name: "session", Desc: "View and manage sessions. Use subcommands for details", Dialog: adapter.DialogBare},
	{Name: "settings", Desc: "Open the settings UI, show, set, or unset a single value", Dialog: adapter.DialogBare},
	{Name: "share", Desc: "Open sharing actions for this session, or export with file, html, gist, research, or link", Dialog: adapter.DialogBare},
	{Name: "skills", Desc: "Manage skills for enhanced capabilities", Dialog: adapter.DialogBare},
	{Name: "statusline", Desc: "Configure status line items", Dialog: adapter.DialogBare},
	{Name: "subagents", Desc: "Configure each subagent's model and settings, or turn it on or off", Dialog: adapter.DialogBare},
	{Name: "tasks", Desc: "View subagents, background shells, and workflow runs", Dialog: adapter.DialogAlways},
	{Name: "terminal-setup", Desc: "Configure terminal for multiline input support (shift+enter)"},
	{Name: "theme", Desc: "Open the color mode picker", Dialog: adapter.DialogBare},
	{Name: "undo", Desc: "Pick an earlier turn to rewind to: restore the conversation only, or also the files", Dialog: adapter.DialogAlways},
	{Name: "update", Desc: "Update the CLI to the latest version"},
	{Name: "usage", Desc: "Display session usage metrics and statistics"},
	{Name: "user", Desc: "Manage signed-in accounts", Dialog: adapter.DialogBare},
	{Name: "version", Desc: "Display version information and check for updates"},
	{Name: "vim", Desc: "Toggle Vim mode for the input"},
	{Name: "voice", Desc: "Manage voice mode (dictation transcription via Foundry Local)", Dialog: adapter.DialogBare},
	{Name: "worktree", Desc: "Create a new git worktree from your configured base ref and switch into it", Hint: "[branch]"},
}

// ListCommands lists the built-ins and the skills Copilot reads from `~/.copilot/skills` and the worktree's
// `.github/skills`, `.claude/skills` and `.agents/skills`.
func (a *Adapter) ListCommands(_ context.Context, req *connect.Request[adapterv1.ListCommandsRequest]) (*connect.Response[adapterv1.ListCommandsResponse], error) {
	var skills []adapter.Cmd
	if home := req.Msg.Env["HOME"]; home != "" {
		skills = append(skills, adapter.SkillCmds(filepath.Join(home, ".copilot", "skills"), "", "user")...)
	}
	if cwd := req.Msg.Cwd; cwd != "" {
		for _, d := range []string{".github/skills", ".claude/skills", ".agents/skills"} {
			skills = append(skills, adapter.SkillCmds(filepath.Join(cwd, d), "", "project")...)
		}
	}
	return connect.NewResponse(&adapterv1.ListCommandsResponse{Commands: adapter.MergeCommands(adapter.Commands(builtinCommands), adapter.Commands(skills))}), nil
}
