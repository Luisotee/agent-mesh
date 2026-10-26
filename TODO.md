# TODO

Open work, kept short on purpose: whoever picks a task up works out the details.

From the survey of similar projects (run `mac82`, 2026-10-25: firstmate, Gas Town, Rove, Cyclops, mcp_agent_mail, ccswarm, Orca, tmux-orchestrator, Claude Code agent teams). The full reports, with file:line references into each project, are in `~/.local/state/agent-mesh/archive/mac82/reports/`, the clones in `~/research/orchestrators/`, and the askers are the `orchestrators` peers: `mesh peers` lists them, `mesh ask <name>` or `mesh ask --peers orchestrators` asks them.

From the run `m6f06` research (2026-10-26): prompt-cache keepalive (don't build one: in-run waits stay warm, a blind ping over a month of this machine's sessions breaks even at best, and peers are retired between asks), memory tools (claude-mem and ai-memory, both skipped) and skills and plugins. The reports are `reports/agent-mesh-{cache-keepalive,memory-tools,skills}.md` in the run (archived under `~/.local/state/agent-mesh/archive/m6f06/` after `mesh stop`), and the askers are `agent-mesh-cache-keepalive`, `agent-mesh-memory-tools` and `agent-mesh-skills`.

- [ ] Count the static prefix (~25k of system prompt and tools stays cached across sessions) when `root_context` / design §15 weigh a cold turn: its real cost is (N − 25k) at the write price.
- [ ] Maybe: a PreToolUse gate that turns a whole-file Read of a big code file (`bin/mesh` is ~460 KB) into a targeted read. claude-mem's own eval cut cost about a third on a 49 KB file; only worth it with an outline step behind it.
