# TODO

Open work, kept short on purpose: whoever picks a task up works out the details.

From the survey of similar projects (run `mac82`, 2026-10-25: firstmate, Gas Town, Rove, Cyclops, mcp_agent_mail, ccswarm, Orca, tmux-orchestrator, Claude Code agent teams). The full reports, with file:line references into each project, are in `~/.local/state/agent-mesh/archive/mac82/reports/`, the clones in `~/research/orchestrators/`, and the askers are the `orchestrators` peers: `mesh peers` lists them, `mesh ask <name>` or `mesh ask --peers orchestrators` asks them.

From the run `m6f06` research (2026-10-26): prompt-cache keepalive (don't build one: in-run waits stay warm, a blind ping over a month of this machine's sessions breaks even at best, and peers are retired between asks), memory tools (claude-mem and ai-memory, both skipped) and skills and plugins. The reports are `reports/agent-mesh-{cache-keepalive,memory-tools,skills}.md` in the run (archived under `~/.local/state/agent-mesh/archive/m6f06/` after `mesh stop`), and the askers are `agent-mesh-cache-keepalive`, `agent-mesh-memory-tools` and `agent-mesh-skills`.

From run `md64d` (2026-10-26): the root's own observations, PR #63's review, and the new peers' first answers (`reports/peers-20261026T083537929156Z.md` in the run, archived under `~/.local/state/agent-mesh/archive/md64d/` after `mesh stop`; "ACC" is its agents-can-communica-research section, "repowire" its repowire-research one).

- [ ] `mesh peers`' "newest answers" (`answers` in peers.json) points into the live run folder, so it dangles once `mesh stop` archives that run (seen: `md64d/reports/…`, now under `archive/md64d/`). It is also one global file, the newest `ask --peers` of any group, not each peer's last round. Record each peer's last answers file (and expose `run` in `mesh peers --json`), resolving archived runs. `harvest-peers` works around both by hand.
- [ ] The root starts no work until the user says go (unless told to run autonomously): for any request, not only task lists, it first proposes the work (groups, sizes, workers, routing) and waits.
