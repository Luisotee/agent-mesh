# TODO

Open work, kept short on purpose: whoever picks a task up works out the details.

From the survey of similar projects (run `mac82`, 2026-10-25: firstmate, Gas Town, Rove, Cyclops, mcp_agent_mail, ccswarm, Orca, tmux-orchestrator, Claude Code agent teams). The full reports, with file:line references into each project, are in `~/.local/state/agent-mesh/archive/mac82/reports/`, the clones in `~/research/orchestrators/`, and the askers are the `orchestrators` peers: `mesh peers` lists them, `mesh ask <name>` or `mesh ask --peers orchestrators` asks them.

- [ ] Research: keeping an idle agent's prompt cache warm by sending it a small message before the cache TTL runs out (`MESH_CACHE_TTL`), so its next real turn reads the cache instead of rewriting it. Is it cost effective (a keepalive turn's cost vs. a cold re-read of a large context), and which agents would it suit (a root or worker waiting on others, peers between asks)?
