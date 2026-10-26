# TODO

Open work, kept short on purpose: whoever picks a task up works out the details.

From the survey of similar projects (run `mac82`, 2026-10-25: firstmate, Gas Town, Rove, Cyclops, mcp_agent_mail, ccswarm, Orca, tmux-orchestrator, Claude Code agent teams). The full reports, with file:line references into each project, are in `~/.local/state/agent-mesh/archive/mac82/reports/`, the clones in `~/research/orchestrators/`, and the askers are the `orchestrators` peers: `mesh peers` lists them, `mesh ask <name>` or `mesh ask --peers orchestrators` asks them.

From the run `m6f06` research (2026-10-26): prompt-cache keepalive (don't build one: in-run waits stay warm, a blind ping over a month of this machine's sessions breaks even at best, and peers are retired between asks), memory tools (claude-mem and ai-memory, both skipped) and skills and plugins. The reports are `reports/agent-mesh-{cache-keepalive,memory-tools,skills}.md` in the run (archived under `~/.local/state/agent-mesh/archive/m6f06/` after `mesh stop`), and the askers are `agent-mesh-cache-keepalive`, `agent-mesh-memory-tools` and `agent-mesh-skills`.

- [ ] Read the prompt-cache TTL from the root's session file (the last request's `usage.cache_creation`: `ephemeral_1h_input_tokens` vs `ephemeral_5m_input_tokens`) instead of assuming `MESH_CACHE_TTL`'s 3600: on usage credits Claude Code drops to 5 minutes, and `root_context` then calls a cold root warm for 55 minutes.
- [ ] Count the static prefix (~25k of system prompt and tools stays cached across sessions) when `root_context` / design §15 weigh a cold turn: its real cost is (N − 25k) at the write price.
- [ ] Keep every hook's output (`whoami --reminder`, the Stop hook's block reason) under Claude Code's 10,000-char hook-output cap, past which it swaps the whole text for a stub, with a test that asserts it; and keep wall-clock text out of anything injected, so two identical renders match and don't break the prompt cache.
- [ ] A `find-peers` skill of our own (root only, next to `delegate-*`, for Claude and agy): characterize a repo's core mechanisms, search GitHub / awesome lists / plugin directories / the web for projects that share them, rank by mechanism overlap rather than stars, show the shortlist, and on the user's yes `mesh peers add` each with an `about` and a first brief. No such skill exists; the skills report sketches the steps.
- [ ] Maybe: add `prassanna-ravishankar/repowire` and `automatis-tools/agents-can-communicate` (cross-vendor, cross-project agent messaging) to the `orchestrators` peers.
- [ ] Maybe: a PreToolUse gate that turns a whole-file Read of a big code file (`bin/mesh` is ~460 KB) into a targeted read. claude-mem's own eval cut cost about a third on a 49 KB file; only worth it with an outline step behind it.
- [ ] `mesh watch --any` exits 1 ("unknown run …") when `mesh stop` archives its run under it; it should end cleanly (exit 0, "run stopped") once the run is archived.
