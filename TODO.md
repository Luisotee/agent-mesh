# TODO

Open work, kept short on purpose: whoever picks a task up works out the details.

From the survey of similar projects (run `mac82`, 2026-10-25: firstmate, Gas Town, Rove, Cyclops, mcp_agent_mail, ccswarm, Orca, tmux-orchestrator, Claude Code agent teams). The full reports, with file:line references into each project, are in `~/.local/state/agent-mesh/archive/mac82/reports/`, the clones in `~/research/orchestrators/`, and each `<project>-research` worker can be asked again (`mesh ask <name>`). Items marked *live check* depend on behaviour nobody has measured yet.

## Bugs (confirmed in the code)

- [ ] A report is credited to the wrong task: `since_last_task` counts a `done` for task 1, sent after task 2 arrived, as task 2's report, so a worker that then ends task 2 silently is never nudged or alerted. Make `cmd_send` refuse `plan`/`done`/`failed` while the sender has an unread `task`/`answer` (with an escape flag), which also turns PROTOCOL rule 2 into code (Rove, mcp_agent_mail)
- [ ] `mesh approve` checks neither that the caller is the worker's parent (or `--as`) nor that the worker's newest report since its last task is a `plan`: a worker can approve itself, or one that sent a `question` gets "approved" and swapped to execution. Same caller check for `switch`, `retire`, `stop` (mcp_agent_mail, ccswarm)
- [ ] One torn line in `log.jsonl` makes `read_log` raise for every reader (watch, status, ask); skip it and warn once (Cyclops, ccswarm)
- [ ] Doorbell confirmation can be wrong: the 25-character needle is the same for every task from one sender, so an earlier doorbell being submitted confirms a new one still sitting in the box. Put a per-message token in the doorbell, stop retrying once the message has moved to `read/`, and log successful rings with how they were confirmed (Cyclops)
- [ ] `ring()` never checks for a human's draft (the composer-empty check credited to firstmate was never taken: `ring()` reads the pane only to avoid duplicates), so a doorbell can be submitted fused with the user's half-typed text. *Live check* first: what `herdr agent prompt` does with a draft, and whether `herdr agent read --ansi` keeps Claude's NBSP/dim markers. Then hold only on a positively seen draft and type anyway when the screen is ambiguous (Rove deleted a broad composer gate after an 11 h stall, Cyclops dropped fail-closed after 16 h); keep the bounded retry and exit 2. Add a per-pane lock so concurrent rings don't interleave (Orca, Cyclops, Gas Town)
- [ ] A startup dialog in a fresh pane (Claude Code's "Settings Warning" for a repo's malformed `.claude/settings.json` rule, seen on ccswarm) makes `mesh ask`/`spawn` die with `herdr timeout: timed out waiting for agent status`, leaving the task stored but the doorbell unrung. Classify the startup screen (trust, a known blocking dialog, unknown), report the last two like watch's exit 4 with the pane tail, and ring once it clears. While there: re-read the trust dialog before the final Enter, since "No, exit" is the default (firstmate, Cyclops)

## Watchdog

- [ ] A Claude worker whose login expired (`Login expired · Please run /login`, recorded as `error: "authentication_failed"` in its session file) reads like a usage limit's 3/4/8: give it the same once-per-task alert, telling the root to ask the user to log in
- [ ] Check the usage-limit readings against a real limit when one happens: the transcript's `error` code and text (only the text is matched now) and the herdr state of Claude's limit screen; a `StopFailure` hook in `worker_hooks` would give the code directly
- [ ] Let a worker declare a deliberate wait (`--type wait --until <time>`) so a long test run, which herdr reads as idle for agy, isn't nudged and alerted; warn from root-side commands when a run has live workers and no running watch (firstmate)

## Reports and reviews

- [ ] Read-only Claude askers and reviewers are held only by the prompt: pass `COPILOT_DENY`'s list as `--disallowedTools` (an asker turned into a changer is reopened without it). *Live check*: deny rules win under `--dangerously-skip-permissions`. agy has no deny list; say so in design §10 (ccswarm)
- [ ] Tie a review to the head commit it read (`Reviewing <pr> at <sha>` in the task, `head` in `reviews`), cap review→fix rounds at two in ORCHESTRATE, and have `REVIEW_PREFIX` mark each finding blocking (with its failing input) or advisory (ccswarm)
- [ ] `mesh resume` onto a merged or closed PR pushes to a dead branch: check `gh pr view --json state` and start a new branch from the default one (Gas Town)
- [ ] Find a PR's author by its head branch (`gh pr view --json headRefName` against recorded branches) before the message-link regex, which needed the `reviews` exclusion (Rove)
- [ ] A killed `mesh ask` can orphan its Copilot child, and `live_holder` then lets a second process open the same session: start it in its own session, record the child pid, and identify pids by pid plus start time (ccswarm, Cyclops)

## Status and UX

- [ ] Derive what each worker owes or waits on in `mesh status` (plan unanswered for 12m, question, task unread, done plus PR state, or "no commits, no PR" so the root sees a done that changed nothing, which is legitimate when the bug turned out not to be one), needing-the-root first, so `board.md` keeps only the planned columns; add the evidence to exit 3/5 alerts (commits ahead, pushed, PR open, worktree dirty); a `mesh log [--follow]` timeline of `log.jsonl` (Rove, Gas Town, firstmate, ccswarm)
- [ ] `mesh conflicts` skips a branch whose tip is its base, so uncommitted overlap stays invisible until the first commit: intersect each live worktree's changed files too (mcp_agent_mail)
- [ ] Maybe: `mesh slot <name> -- <cmd>` (an `flock`, released when the process dies) for test suites that share a port or database, and the four-worker cap enforced in `spawn` (mcp_agent_mail, firstmate)

## Protocol wording and tests

- [ ] PROTOCOL: never type into another agent's pane (`herdr agent prompt`, `send-keys`); never `git stash` (the stash is shared across worktrees); send the report before any memory notes or tidying; say what goes wrong when a worker `cd`s into the main checkout. ORCHESTRATE task files: quote the user's words where they matter, mark which claims about the repo are assumptions, list what's out of scope. Strip quotes, backticks and `$` from the doorbell preview (Cyclops, Rove, tmux-orchestrator, Gas Town, Orca, firstmate)
- [ ] Real captured panes (`herdr agent read --ansi`: Claude and agy idle, busy, with a draft, a queued doorbell, a trust dialog, the usage-limit screen) as test fixtures for `above_input_box`, `trust_keys` and the needle, re-captured when a CLI changes; point the `mesh` fixture's herdr at a missing binary so no test can type into a real pane; maybe an opt-in live suite in a separate herdr session (Orca, Cyclops, Gas Town, firstmate, Rove)
- [ ] Record in `docs/design.md` what the survey argues against, with the projects' incidents: a queue that holds doorbells until the composer is clear, file leases, auto-restarting workers, pane-hash staleness checks, merging by the orchestrator, a second store next to the mailbox

## Other

- [ ] Live-test `--plan-review user` on plain `opus` (was `opusplan`): the plan dialog still shows `blocked` in herdr, and after approval the worker executes on Opus in the same pane
- [ ] herdr's agy manifest counts working only on the `esc to cancel` footer or a spinner, so a busy agy can read idle; report it upstream with a pane capture (mesh now works around it in `ring()` and the watch's grace)
