# TODO

Open work, kept short on purpose: whoever picks a task up works out the details. The reasons behind past decisions are in `docs/design.md`; the cost study's baseline, method, scripts and savings ledger are in `docs/cost-research/README.md`. If you have the peer projects registered (`mesh peers`), ask them before designing a mechanism they may have solved (see `CLAUDE.md`, "Peer projects").

- [ ] Copilot workers, unverified live (the account had no Copilot subscription when it was built): a reopen on a named model, `write(<path>)` deny rules (to allow the report under `$(mesh dir)`), a `Stop` hook through `--plugin-dir`, and a draft in its `┃` input box (`gutter_typed` reads the box, checked only empty, so `reads_drafts` is off: a doorbell may type over a draft there until a live box with a draft is checked).
- [ ] agy's `⚠ RESOURCE_EXHAUSTED (code 429)` screen isn't read as a limit: the watch gave exit 3 (idle, nudged) three times on #96's agy reviewer, across Argon, Opus and Hydrogen, instead of exit 7. Seen again in run mebd0 (curupira-bot #64's agy reviewer): exit 3 twice, before and after `mesh switch` to `claude-opus-5-5-high`, still 429; `--reviewers auto` also picked agy while its quota was spent.
- [ ] `mesh cost`: count Copilot workers' calls (`session.usage_record` in their `events.jsonl`).
- [ ] `read_log` warns on every read about a log line a power cut left NUL-filled or torn (`m46c2/log.jsonl` line 24): seal or quarantine such lines once, so the warning comes once.
- [ ] `mesh cost`: agy workers whose entries have no `size` show as `agy worker ? pane` (1,064 calls over 2026-10-28 → 11-04): record or derive their size.
- [ ] OpenCode: a doorbell or a model swap through its HTTP API (the TUI's `--port`: `/api/session/<id>/prompt`, `/tui/*`) instead of typing and a reopen; a Stop-hook equivalent from its plugin events; its own `cost` in `mesh cost`; telling a model the server silently didn't serve (the TUI falls back to its default) from the one asked for.
- [ ] Copilot refused by policy isn't read as a failure: with the account's Copilot CLI policy denying models (`Failed to load models: access denied by Copilot policy`, then `✗ Execution failed: … 403 … unauthorized: not authorized to use this Copilot feature`), both reviewers of run mebd0 (curupira-bot #64, ai-boilerplate #60) idled and the watch gave exit 3 (nudged, task unread) instead of an alert naming the policy; `--reviewers` kept picking Copilot.
- [ ] Research: agy's new families `neon` and `carbon` (`agy models` lists `gemini-4-neon-*` low to xhigh and `gemini-4-carbon-*`; agy's default orchestrator is "Orchestrator (Carbon)"), and where each belongs in the agy tiers: Neon in place of or beside Argon in `strong`, Carbon in `mid` or `strong`. Read Google's release notes and benchmarks live, not from memory; propose `tiers.agy.*` changes with sources, then `mesh models --ack` what's decided.
- [ ] Research: live sessions grow past 300k context (up to 786k in the week of 2026-11-18) with nothing compacting them (`compact.claude` acts only on resume), and calls above 300k were 34% of that week's spend. Measure what a cap on live workers and roots would save against the re-reads after compaction ("Settled" in `docs/cost-research/`: compacting at 250k didn't pay at the old sizes).
- [ ] `mesh cost`: join the root's verdicts and review-fix rounds to the PR author's execution tier, so a cheaper executor's quality shows per tier (today verdicts sum per reviewer only).
- [ ] Drop design §7's rule that workers start no agents, for read-only ones only. A worker may start (1) its own PR's review round once its self-review is done: the answers go to the worker, which judges and fixes them, then sends one `done` with its verdicts, and the root checks only what the worker disagreed with (one root wake per PR, warm fixes); and (2) askers on other repos and the peers (`mesh ask`), whose answers come back to it. It never starts a change worker or asks another repo for changes: that goes to the root, which assigns it. Update `check_worker` / `WORKER_COMMANDS`, ORCHESTRATE and PROTOCOL, design §7, `CLAUDE.md` and `rules/global-snippet.md` ("a mesh worker never spawns agents") together, then re-run `./install.sh`.
- [ ] Three Copilot hold tests use 2026-11-19 timestamps and fail since that day passed (`test_a_copilot_at_its_monthly_quota_is_continued_only_once_the_hold_ends`, `test_copilot_limits_read_from_its_pane_and_its_events`, `test_a_copilot_quota_error_counts_only_until_a_later_turn`): date them relative to now.
- [ ] An option in the apps' settings (`docs/apps.md`), per repo, for whether its code may go to free models whose providers keep prompts or train on them (some of OpenCode's do, per its Zen docs); today every repo may, by the user's choice (2026-11-25).

## From the Orca / Paseo comparison (2026-11-24)

Ideas from comparing agent-mesh with stablyai/orca and getpaseo/paseo. Paseo was trialled and dropped (design, "Alternatives considered"): agent-mesh gets its own apps instead.

**Apps (plan in `docs/apps.md`)**

herdr and the terminal workflow are replaced by a daemon per host plus desktop, Android and web apps; nothing of herdr is kept. Steps, in order (`docs/apps.md`, "Order of work"):

- [ ] Daemon core (Go), the rest of step 1. The action registry with actor classes, meshd with SQLite and resumable events are in (`daemon/`, `proto/`). Left: agents in daemon-owned PTYs (the terminal host, `SpawnAgent`/`StopAgent`/`ReportHook`/`OpenTerminal`), state from hooks and a reducer (Claude, then agy), the generated CLI, resume and model switches, worker caps, settings as host data, and the switchover (the CLI becomes `mesh`). Agents then run without herdr.
- [ ] Daemon events: a run is one trust domain for now, so every agent of a run sees its `MessageSent` bodies (`SubscribeEvents`). Decide whether askers and reviewers should see only their own messages.
- [ ] Port the skills and protocol to the daemon: keep the `mesh` command names agents already use where they still fit, rewrite `ORCHESTRATE.md` and `PROTOCOL.md` for daemon events instead of doorbells, panes and `mesh watch` exit codes, drop the herdr fallbacks in the skills (`harvest-peers` "No herdr", `delegate-*` panes), and have the daemon install the skills into each CLI.
- [ ] Desktop app (Electron + React), first version: hosts, workspaces and the lead-agent view, terminals, plan approval, questions, reports, settings, native notifications, tray.
- [ ] Connections: `mesh relay` on the user's VPS (outbound from the daemon, TLS end to end, QR pairing), then built-in Tailscale (`tsnet`).
- [ ] Android app (Kotlin + Compose): status, notifications with actions through UnifiedPush, plans, questions, reports, terminal.
- [ ] Workers on other hosts: a daemon on the Oracle server, reached through the same routes.
- [ ] The rest of `docs/apps.md`'s feature list: git, PRs and CI, the structured chat view with diff review notes, schedules, usage, plugins.

**Orchestration ideas from them**

- [ ] Task and dispatch ids on every task and report, so a stale or retried worker's report can't complete the wrong task (Orca); optional dependencies between tasks.
- [ ] Permission prompts to the root: a worker stopped at one alerts the root with the prompt's text; the root denies it with a reason, or approves it when the user allowed that (Orca).
- [ ] CI failures and PR review comments go back to the worker that owns the branch, without the root relaying them (Orca, Orchestrator.inc).
- [ ] Agent profiles with "when to use" notes, which the root reads to pick kind, model and effort beyond size (Paseo).
- [ ] Schedules: start a worker on a cron or an event (Paseo schedules, Orca automations).
- [ ] Plan in Claude's real Plan mode instead of the planning deny list (design §9, line "Planning is enforced"). It lost because leaving Plan mode was a dialog in the pane only a human could answer, and the plan went to Claude Code's own file. Paseo shows it works when an agent can answer: its root approves `ExitPlanMode` with `respond_to_permission`, or denies it with a message to question the plan. For mesh: a `PermissionRequest` hook on `ExitPlanMode` (or `--permission-prompt-tool` for a headless worker) copies the plan to `$(mesh dir)/plans/`, sends it to the root, and waits; `mesh approve` answers allow, and a `changes` reply answers deny with its text, so the worker keeps planning in the same conversation. The model and effort swap stays with `mesh approve`.
- [ ] Research: compact a conversation before it's closed (retire, stop), while its cache is still warm, instead of only on resume (`maybe_compact`, design §12). A compaction on a warm cache reads the context at the cache-read price; on resume after the cache expired, the compaction or the reopen pays for it all again. Find out from our session data (`mesh cost`, `docs/cost-research/`) how often closed conversations are resumed and how long after, at what size, for which kinds (workers, peers, reviewers, roots), and what each policy would have cost; ask the peers how they handle it.
- [ ] Branch names, commit messages and PR text from a small model (Haiku class) instead of the worker's (Paseo's metadata generation).

**Not being the only developer**

- [ ] Make it contributable: split `bin/mesh` (12.8k lines) into a package along its sections, a CONTRIBUTING guide, tagged releases, an install that isn't a symlinked checkout (`uv tool install`), open work as GitHub issues.
- [ ] Extension points (hooks or plugins) so cost and review policies, and new CLIs, can ship outside the core.
