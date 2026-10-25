# TODO

Open work, kept short on purpose: whoever picks a task up works out the details.

From the survey of similar projects (run `mac82`, 2026-10-25: firstmate, Gas Town, Rove, Cyclops, mcp_agent_mail, ccswarm, Orca, tmux-orchestrator, Claude Code agent teams). The full reports, with file:line references into each project, are in `~/.local/state/agent-mesh/archive/mac82/reports/`, the clones in `~/research/orchestrators/`, and each `<project>-research` worker can be asked again (`mesh ask <name>`). Items marked *live check* depend on behaviour nobody has measured yet.

## Bugs (confirmed in the code)

- [ ] A startup dialog in a fresh pane (Claude Code's "Settings Warning" for a repo's malformed `.claude/settings.json` rule, seen on ccswarm) makes `mesh ask`/`spawn` die with `herdr timeout: timed out waiting for agent status`, leaving the task stored but the doorbell unrung. Classify the startup screen (trust, a known blocking dialog, unknown), report the last two like watch's exit 4 with the pane tail, and ring once it clears. While there: re-read the trust dialog before the final Enter, since "No, exit" is the default (firstmate, Cyclops)

## Watchdog

- [ ] A Claude worker whose login expired (`Login expired · Please run /login`, recorded as `error: "authentication_failed"` in its session file) reads like a usage limit's 3/4/8: give it the same once-per-task alert, telling the root to ask the user to log in
- [ ] Check the usage-limit readings against a real limit when one happens: the transcript's `error` code and text (only the text is matched now) and the herdr state of Claude's limit screen; a `StopFailure` hook in `worker_hooks` would give the code directly
- [ ] Once #21 (send-guards) and the declared wait both land: fold `send_wait`'s unread task/answer check into `refuse_stale_report` (`wait` alongside `REPORT_TYPES`)
- [ ] Maybe: let the Stop hook pass only one turn end per declared wait (then nudge as usual), so a worker that wakes early and stops silently is caught before the deadline; the watch would need the same rule
- [ ] Once #26 (status-log) and the declared wait both land: `waiting_rows` shows an active wait (`declared_wait`) as `on task 12m, waiting until 14:30`, and the duplicate `work_evidence` goes
- [ ] A nudge held by a draft in the worker's pane ends in exit 3 as "its nudge was not delivered": say why (the `doorbell_failed` event has `error: human_draft`), so the root knows the user is typing there

## Reports and reviews

- [ ] A review task's embedded diff comes from `gh pr diff` at ask time, which can be newer than the head the task names if the author pushes in between: build it with `git diff <merge-base>..<head>` from the fetched head instead
- [ ] A fresh reviewer asked again after a fix still has the old head checked out (it reads the new one through `git diff`/`git show`): move its worktree to the new head when it's clean, so it can run the fixed tests
- [ ] `mesh resume` onto a merged or closed PR pushes to a dead branch: check `gh pr view --json state` and start a new branch from the default one (Gas Town)
- [ ] Find a PR's author by its head branch (`gh pr view --json headRefName` against recorded branches) before the message-link regex, which needed the `reviews` exclusion (Rove)
- [ ] A killed `mesh ask` can orphan its Copilot child, and `live_holder` then lets a second process open the same session: start it in its own session, record the child pid, and identify pids by pid plus start time (ccswarm, Cyclops)

## Status and UX

- [ ] `mesh conflicts` skips a branch whose tip is its base, so uncommitted overlap stays invisible until the first commit: intersect each live worktree's changed files too (mcp_agent_mail)
- [ ] Maybe: `mesh slot <name> -- <cmd>` (an `flock`, released when the process dies) for test suites that share a port or database, and the four-worker cap enforced in `spawn` (mcp_agent_mail, firstmate)

## Protocol wording and tests

- [ ] Real captured panes (`herdr agent read --ansi`: Claude and agy idle, busy, with a draft, a queued doorbell, a trust dialog, the usage-limit screen) as test fixtures for `above_input_box`, `trust_keys` and the needle, re-captured when a CLI changes; point the `mesh` fixture's herdr at a missing binary so no test can type into a real pane; maybe an opt-in live suite in a separate herdr session (Orca, Cyclops, Gas Town, firstmate, Rove)

## Other

- [ ] Live-test `--plan-review user` on plain `opus` (was `opusplan`): the plan dialog still shows `blocked` in herdr, and after approval the worker executes on Opus in the same pane
- [ ] herdr's agy manifest counts working only on the `esc to cancel` footer or a spinner, so a busy agy can read idle; report it upstream with a pane capture (mesh now works around it in `ring()` and the watch's grace)
