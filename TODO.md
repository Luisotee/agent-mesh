# TODO

Open work, kept short on purpose: whoever picks a task up works out the details.

From the survey of similar projects (run `mac82`, 2026-10-25: firstmate, Gas Town, Rove, Cyclops, mcp_agent_mail, ccswarm, Orca, tmux-orchestrator, Claude Code agent teams). The full reports, with file:line references into each project, are in `~/.local/state/agent-mesh/archive/mac82/reports/`, the clones in `~/research/orchestrators/`, and the askers are the `orchestrators` peers: `mesh peers` lists them, `mesh ask <name>` or `mesh ask --peers orchestrators` asks them.

From the run `m6f06` research (2026-10-26): prompt-cache keepalive (don't build one: in-run waits stay warm, a blind ping over a month of this machine's sessions breaks even at best, and peers are retired between asks), memory tools (claude-mem and ai-memory, both skipped) and skills and plugins. The reports are `reports/agent-mesh-{cache-keepalive,memory-tools,skills}.md` in the run (archived under `~/.local/state/agent-mesh/archive/m6f06/` after `mesh stop`), and the askers are `agent-mesh-cache-keepalive`, `agent-mesh-memory-tools` and `agent-mesh-skills`.

From run `md64d` (2026-10-26): the root's own observations, PR #63's review, and the new peers' first answers (`reports/peers-20261026T083537929156Z.md` in the run, archived under `~/.local/state/agent-mesh/archive/md64d/` after `mesh stop`; "ACC" is its agents-can-communica-research section, "repowire" its repowire-research one).

- [ ] `CLAUDE.md` is ~27.5 KB, over agy's 24,000-byte rule-file limit ("AGENTS.md truncated by 5714 bytes"), so agy workers here never read its last sections. Trim it under 24,000 (move detail to design.md) and keep it there, once PR #63 has merged.
- [ ] Claude Code holds cross-session messages across permission classes (ACC §1): a root in a prompting mode gets every bypass worker's report behind a dialog, and root → `--bg` worker doorbells expire unseen. Pass `"crossSessionInbound": "accept"` in `worker_settings` (first check `--settings` may loosen it), and warn at `mesh init` / `status` on a mismatch.
- [ ] Find and trust a Claude socket as Claude does (ACC §5): `CLAUDE_CODE_MESSAGING_SOCKET`, or `messagingSocketPath` in `sessions/<pid>.json`, not a rebuilt path (wrong without `XDG_RUNTIME_DIR`); and check the root's pid start time in `native_route` (pid reuse).
- [ ] Write the doorbell straight into a Claude recipient's inbox socket from `mesh` itself (repowire §1, ACC §2): any sender, no `SendMessage` step, no pane typing for Claude recipients. Medium; after the two above.
- [ ] Maybe: agy delivery without typing, through `agy agentapi send-message` (ACC §3). It needs agy's session CSRF token recorded on disk: the user decides before it's built.
- [ ] Design §11: one line that `CLAUDE_CODE_HARBOR_KITE=0` turns Claude's inbox off (ACC, "Lessons").
