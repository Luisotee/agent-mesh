# TODO

Open work, kept short on purpose: whoever picks a task up works out the details.

From the survey of similar projects (run `mac82`, 2026-10-25: firstmate, Gas Town, Rove, Cyclops, mcp_agent_mail, ccswarm, Orca, tmux-orchestrator, Claude Code agent teams). The full reports, with file:line references into each project, are in `~/.local/state/agent-mesh/archive/mac82/reports/`, the clones in `~/research/orchestrators/`, and the askers are the `orchestrators` peers: `mesh peers` lists them, `mesh ask <name>` or `mesh ask --peers orchestrators` asks them. Items marked *live check* depend on behaviour nobody has measured yet.

## Watchdog

- [ ] A Claude worker whose login expired (`Login expired · Please run /login`, recorded as `error: "authentication_failed"` in its session file) reads like a usage limit's 3/4/8: give it the same once-per-task alert, telling the root to ask the user to log in
- [ ] Check the usage-limit readings against a real limit when one happens: the transcript's `error` code and text (only the text is matched now) and the herdr state of Claude's limit screen; a `StopFailure` hook in `worker_hooks` would give the code directly
- [ ] Once #21 (send-guards) and the declared wait both land: fold `send_wait`'s unread task/answer check into `refuse_stale_report` (`wait` alongside `REPORT_TYPES`)
- [ ] Maybe: let the Stop hook pass only one turn end per declared wait (then nudge as usual), so a worker that wakes early and stops silently is caught before the deadline; the watch would need the same rule
- [ ] Once #26 (status-log) and the declared wait both land: `waiting_rows` shows an active wait (`declared_wait`) as `on task 12m, waiting until 14:30`, and the duplicate `work_evidence` goes
- [ ] A nudge held by a draft in the worker's pane ends in exit 3 as "its nudge was not delivered": say why (the `doorbell_failed` event has `error: human_draft`), so the root knows the user is typing there
- [ ] `LIMIT_RE`'s leading `[^A-Za-z'\n]*` lets a `"` through, so a usage-limit message quoted in a pane (`"You've hit your limit…`) still reads as a real limit: exclude `"` there too

## Reports and reviews

- [ ] A review task's embedded diff comes from `gh pr diff` at ask time, which can be newer than the head the task names if the author pushes in between: build it with `git diff <merge-base>..<head>` from the fetched head instead
- [ ] A fresh reviewer asked again after a fix still has the old head checked out (it reads the new one through `git diff`/`git show`): move its worktree to the new head when it's clean, so it can run the fixed tests
- [ ] A Copilot left answering by a SIGKILLed `mesh ask` keeps its asker busy, but its answer is lost: nobody files it. Maybe have the next `ask` (or `mesh watch`) adopt it, or give Copilot's output a file to be read back from
- [ ] A reviewer asked a follow-up without `--review` isn't recorded as reviewing, so `mesh status` doesn't show the author's PR as "in review"

## Status and UX

- [ ] Maybe: `mesh watch --any --alerts` raises one `overlap` alert per new pair of workers `mesh conflicts` finds touching the same files, logged under the run lock like `record_alert` (mcp_agent_mail)
- [ ] `mesh status` doesn't show a worker `held` at a startup screen (its task stored, its doorbell waiting for the watch); show the screen and since when
- [ ] Maybe: `mesh slot <name> -- <cmd>` (an `flock`, released when the process dies) for test suites that share a port or database, and the four-worker cap enforced in `spawn` (mcp_agent_mail, firstmate)
- [ ] `mesh open` skips the running-worker check when herdr's server isn't running (`live_hint` treats the `MeshError` as outside herdr): fall back to the process table (`agent_processes`) so a live worker's conversation isn't opened twice

## Protocol wording and tests

- [ ] Real captured panes (`herdr agent read --ansi`: Claude and agy idle, busy, with a draft, a queued doorbell, a trust dialog, the usage-limit screen) as test fixtures for `above_input_box`, `trust_keys` and the needle, re-captured when a CLI changes; maybe an opt-in live suite in a separate herdr session (Orca, Cyclops, Gas Town, firstmate, Rove)

## Peers

- [ ] Live-check `mesh ask --peers orchestrators`: eight panes at once, ccswarm past its Settings Warning on its own, one answers file
- [ ] A peer held at a startup screen other than the Settings Warning keeps the fan-out waiting until `--timeout`: maybe give a held peer its own shorter deadline
- [ ] Maybe: `mesh peers` shows each peer's newest answer time from the runs' logs, and `peers refresh --all` runs on a schedule

## Other

- [ ] Live-test `--plan-review user` on plain `opus` (was `opusplan`): the plan dialog still shows `blocked` in herdr, and after approval the worker executes on Opus in the same pane
- [ ] Maybe: pre-register trust for worktrees mesh created (Claude's `~/.claude.json` project entry, agy's `trustedWorkspaces`, both the logical and resolved path), as an explicit opt-in since it writes the user's settings, so no trust dialog shows at all (firstmate)
- [ ] herdr's agy manifest counts working only on the `esc to cancel` footer or a spinner, so a busy agy can read idle; report it upstream with a pane capture (mesh now works around it in `ring()` and the watch's grace)
