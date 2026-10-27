# TODO

Open work, kept short on purpose: whoever picks a task up works out the details.

From the survey of similar projects (run `mac82`, 2026-10-25: firstmate, Gas Town, Rove, Cyclops, mcp_agent_mail, ccswarm, Orca, tmux-orchestrator, Claude Code agent teams). The full reports, with file:line references into each project, are in `~/.local/state/agent-mesh/archive/mac82/reports/`, the clones in `~/research/orchestrators/`, and the askers are the `orchestrators` peers: `mesh peers` lists them, `mesh ask <name>` or `mesh ask --peers orchestrators` asks them.

From the run `m6f06` research (2026-10-26): prompt-cache keepalive (don't build one: in-run waits stay warm, a blind ping over a month of this machine's sessions breaks even at best, and peers are retired between asks), memory tools (claude-mem and ai-memory, both skipped) and skills and plugins. The reports are `reports/agent-mesh-{cache-keepalive,memory-tools,skills}.md` in the run (archived under `~/.local/state/agent-mesh/archive/m6f06/` after `mesh stop`), and the askers are `agent-mesh-cache-keepalive`, `agent-mesh-memory-tools` and `agent-mesh-skills`.

From run `md64d` (2026-10-26): the root's own observations, PR #63's review, and the new peers' first answers (`reports/peers-20261026T083537929156Z.md` in the run, archived under `~/.local/state/agent-mesh/archive/md64d/` after `mesh stop`; "ACC" is its agents-can-communica-research section, "repowire" its repowire-research one).

- [ ] `mesh peers`' "newest answers" (`answers` in peers.json) points into the live run folder, so it dangles once `mesh stop` archives that run (seen: `md64d/reports/…`, now under `archive/md64d/`). It is also one global file, the newest `ask --peers` of any group, not each peer's last round. Record each peer's last answers file (and expose `run` in `mesh peers --json`), resolving archived runs. `harvest-peers` works around both by hand.
- [ ] The root starts no work until the user says go (unless told to run autonomously): for any request, not only task lists, it first proposes the work (groups, sizes, workers, routing) and waits.

From the WSL freezes of 2026-10-27 (03:43 and 03:50): two full runs filled WSL's 15.5 GB and its 4 GB swap and the VM hung (allocation failures, no OOM kill). The machine now has 32 GB swap, `vm.swappiness=100` and earlyoom. Idle, 22 sessions held 8.4 GB: `claude` ~195 MB each, its MCP servers (playwright ~83 MB, claude-code-explorer ~36 MB, shadcn ~76 MB) another 38% of the total, herdr and the pane shells ~0.3 GB. Fewer panes alone saves little: a `--bg` session is the same process with the same MCP servers.

- [ ] Auto-retire idle workers: once a worker has reported and stays idle ~15 min, the watch retires it, and `send`/`ask` to a retired worker resumes it instead of refusing. An idle process buys nothing over a resume (the transcript is on disk, the prompt cache keys on content). Design note first; touches `send`, `watch` and the protocol.
- [ ] Workers leave dev servers running (four `next dev`/`next start`, ~1.5 GB, before the freeze): a protocol rule to stop what you started before reporting, and `retire`/`stop` kill leftover processes whose cwd is inside the worker's worktree.
- [ ] One MCP process per server instead of one per session: run `playwright-mcp --port` once as a user service and point the user-scope entry at its URL; scope `claude-code-explorer` (and `shadcn`) to the projects that use them, or launch workers with `--strict-mcp-config` and only the servers their task needs. ~2.5–3 GB back at today's counts.
