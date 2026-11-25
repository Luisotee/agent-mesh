# TODO

Open work, kept short on purpose: whoever picks a task up works out the details. The reasons behind past decisions are in `docs/design.md`; the cost study's baseline, method, scripts and savings ledger are in `docs/cost-research/README.md`. If you have the peer projects registered (`mesh peers`), ask them before designing a mechanism they may have solved (see `CLAUDE.md`, "Peer projects").

- [ ] Copilot workers, unverified live (the account had no Copilot subscription when it was built): a reopen on a named model, `write(<path>)` deny rules (to allow the report under `$(mesh dir)`), a `Stop` hook through `--plugin-dir`, and a draft in its `┃` input box (`gutter_typed` reads the box, checked only empty, so `reads_drafts` is off: a doorbell may type over a draft there until a live box with a draft is checked).
- [ ] agy's `⚠ RESOURCE_EXHAUSTED (code 429)` screen isn't read as a limit: the watch gave exit 3 (idle, nudged) three times on #96's agy reviewer, across Argon, Opus and Hydrogen, instead of exit 7. Seen again in run mebd0 (curupira-bot #64's agy reviewer): exit 3 twice, before and after `mesh switch` to `claude-opus-5-5-high`, still 429; `--reviewers auto` also picked agy while its quota was spent.
- [ ] `mesh cost`: count Copilot workers' calls (`session.usage_record` in their `events.jsonl`).
- [ ] `read_log` warns on every read about a log line a power cut left NUL-filled or torn (`m46c2/log.jsonl` line 24): seal or quarantine such lines once, so the warning comes once.
- [ ] `mesh cost`: agy workers whose entries have no `size` show as `agy worker ? pane` (1,064 calls over 2026-10-28 → 11-04): record or derive their size.
- [ ] OpenCode: a doorbell or a model swap through its HTTP API (the TUI's `--port`: `/api/session/<id>/prompt`, `/tui/*`) instead of typing and a reopen; a Stop-hook equivalent from its plugin events; its own `cost` in `mesh cost`; telling a model the server silently didn't serve (the TUI falls back to its default) from the one asked for.
- [ ] Copilot refused by policy isn't read as a failure: with the account's Copilot CLI policy denying models (`Failed to load models: access denied by Copilot policy`, then `✗ Execution failed: … 403 … unauthorized: not authorized to use this Copilot feature`), both reviewers of run mebd0 (curupira-bot #64, ai-boilerplate #60) idled and the watch gave exit 3 (nudged, task unread) instead of an alert naming the policy; `--reviewers` kept picking Copilot.
- [ ] OpenCode false `human_draft`: in run mebd0 (castanha-bot #47's reviewer) `mesh ring` refused with `"error": "human_draft"` and the watch said "someone has a draft typed in its pane", but the input box held only the placeholder (`Ask anything… "Fix a TODO in the codebase"`): the task was never delivered. Read the placeholder as empty.

## From the Orca / Paseo comparison (2026-11-24)

Ideas from comparing agent-mesh with stablyai/orca and getpaseo/paseo. Paseo was trialled and dropped (design, "Alternatives considered"): agent-mesh gets its own apps instead.

**Leaving herdr**

- [ ] A `Host` adapter for what `bin/mesh` asks herdr (about 170 calls: agent state, a prompt with delivery confirmation, pane process info, names, workspaces), like the `Cli` adapters (design §22); herdr becomes the first adapter, and tmux or a PTY daemon of mesh's own the next.
- [ ] Drive workers over structured channels instead of their TUI: agy through ACP (`agy acp`), Claude through its inbox socket (already) or stream-json, OpenCode through its HTTP API (see the OpenCode item above). State and limits then come from events, not screen reads, which drops most of the `human_draft`, startup-screen and limit-screen scraping.
- [ ] Workers on other machines over SSH (the Oracle ARM server): spawn, mail, watch and worktrees on a remote host, with the run state staying with the root.

**Desktop and mobile apps**

- [ ] `mesh serve`: a local HTTP/WebSocket API over the run state (runs, agents, mail, plans, reports, PRs, usage) with live events and actions (approve, answer, send, stop, resume), and auth for clients off the machine. Every UI below builds on it.
- [ ] Desktop app for Windows, Linux and macOS on `mesh serve` (Tauri or Electron): runs and workers live, plans to approve, questions to answer, reports, diffs, usage per vendor, a worker's terminal.
- [ ] Mobile app (iOS, Android), or a PWA first: notifications when a root is done or blocked (what `herdr/mesh-notify` does on the desktop), approve plans, answer questions, read reports; reached over Tailscale or an end-to-end encrypted relay.
- [ ] Interim mobile: send `mesh-notify` events and plan/question prompts to Telegram (Hermes on the Oracle server already has a bot), and take the reply as the answer.
- [ ] Diff review in the app: notes on lines of a worker's PR, sent back to that worker as one message (Orca, Paseo).

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
