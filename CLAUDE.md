# CLAUDE.md / AGENTS.md

This file provides guidance to Claude Code (claude.ai/code) and Antigravity (agy) when working with code in this repository.

agent-mesh lets an agent working in repo A hand repo B's part of a change to an agent running *inside* B (with B's `CLAUDE.md`/`AGENTS.md`, skills and hooks loaded), as a visible herdr pane with two-way messaging. `README.md` is the user-facing overview; `docs/design.md` records why each decision was made and the alternatives that lost. Read the relevant section there before changing behaviour.

## Task tracking

Open work is tracked in `TODO.md`. Tasks are deliberately short summaries; the agent doing a task is expected to think it through and work out the details itself.

Keep it current as part of the same change: remove a task (or tick it) when the work lands, reword it when scope changes, and add a line for any follow-up you discover but don't do now. Check it before starting something that might already be tracked.

## Peer projects

If `mesh peers` lists them, the `orchestrators` group is this repo's peers: agent orchestration tools similar to agent-mesh, cloned under `~/research/orchestrators`. Ask them before designing a mechanism they may have solved, and for any `Research:` task in `TODO.md`: all at once with `mesh ask --peers orchestrators --file q.md` (one `reports/peers-<stamp>.md`), or one with `mesh ask <name>-research --body "…"`. They're standing read-only askers that keep their context between asks; `mesh peers` shows each one's `about`.

- `agents-can-communicate` (asker `agents-can-communica-research`): cross-vendor messages between sessions the user opened, no lead agent. Claude's inbox socket, agy native delivery.
- `ccswarm`: headless one-shot agents driven by a YAML flow state machine, persona quorum votes as gates. Flow/stage design, quorum review, loop caps.
- `cyclops`: a daemon watching agents in tmux, fused state signals, durable mailboxes with one doorbell per message. Delivery into a human's draft.
- `firstmate`: a bash supervisor over herdr panes (watcher, turn-end hooks, leases, caps). Its failure modes: hung agents, stale herdr registrations, trust dialogs.
- `gastown`: patrol agents that respawn workers, a merge queue, convoys. Stop-hook guards, nudge queues, auto-restart, mostly as lessons in what broke.
- `mcp-agent-mail`: an MCP server with mailboxes, identities and file leases. Inbox hooks, leases, two-store drift.
- `orca`: tmux plus raw typing, roles from env vars. Guarding a user's draft, spawning panes on demand.
- `repowire`: a Go daemon meshing live sessions: asks with acks and reminders, Claude's inbox socket, identity across reconnects.
- `rove`: a daemon and PTY host owning every engine, flat dispatch with lineage. Delivery, matching reports to tasks.
- `tmux-orchestrator`: an archived orchestrator → manager → engineer tree over `send-keys`. Mostly a contrast.

## Commands

```bash
uvx pytest -q tests                              # full suite
uvx pytest -q tests -k approve                   # tests matching a name
uvx pytest -q tests/test_mesh.py::test_name      # one test
./install.sh            # symlink into place (--uninstall reverses it); safe to re-run
```

To try `bin/mesh` by hand (reviewing a change, say), point `MESH_HOME`, `MESH_WORKTREES` and `MESH_PEERS_DIR` at temp dirs and `MESH_HERDR` (and `MESH_CLAUDE`) at a missing binary, as `tests/conftest.py` does: the defaults are the machine's real run state and herdr's real panes, which every agent uses. A worker that forgets is refused every command but its own (`check_worker`).

There is no build or lint step: `bin/mesh` is one stdlib-only Python 3.10+ script with no dependencies. Keep it stdlib-only.

The installed `mesh`, `~/.config/agent-mesh` and the skills are **symlinks to this checkout**, so whatever branch is checked out is what every agent on the machine runs.

## Layout and how the pieces fit

- `bin/mesh`: the whole CLI (argparse in the `# ── CLI` section, one `cmd_*` per subcommand). Sections are marked with `# ── name ──` comments: paths, run state, identity, herdr topology, models, sessions, worktrees, messaging, commands, past workers.
- `protocol/`: Markdown the agents themselves follow, installed as `~/.config/agent-mesh`. `ORCHESTRATE.md` is the root/worker procedure (and the mode table), `PROTOCOL.md` the messaging rules every mesh agent reads, `PLAN_TEMPLATE.md` the plan format. The orchestration is done by agents following these files, not by code, so a behaviour change usually needs both a `bin/mesh` change and a protocol/doc change.
- `skills/delegate-agy`, `skills/delegate-claude`: the `/delegate-*` entry points. The user can type them, and agents invoke them on their own when a request fits the description (never a worker). They point the root at `ORCHESTRATE.md`.
- `skills/delegate-review`: a PR review the user asks for, by any mix of Claude, agy and Copilot (or `all`), run in parallel with one `mesh ask` each. It reads only ORCHESTRATE's "Questions and reviews".
- `skills/find-peers`: finds projects that share a repo's core mechanisms, ranks them by overlap, and on the user's yes registers them as peers (`mesh peers add`), unbriefed. Root only, any repo.
- `skills/harvest-peers`: a research round by the peers (first brief or follow-up from `HARVEST.md`, with bug-hunt targets), every finding checked in our code, survivors filed in the tracker on the user's yes. Never run by `find-peers`. Root only.
- `skills/check-plan`: a worker's self-check of its own draft plan (ORCHESTRATE Worker step 4): verify its claims in the code, attack it, fix the plan. Edits only the plan file, so it runs under the planning deny list; usable by hand in any session.
- `rules/global-snippet.md`: the block `install.sh` writes between markers into `~/.claude/CLAUDE.md` and `~/.gemini/AGENTS.md`.

Core mechanics that span several files. Each bullet is the gist and the names to start from; `docs/design.md` has the why (by section) and, in "Code map", the function-level map of each. Change a mechanism, update both.

- **Run state** lives in `$MESH_HOME/<run>/` (default `~/.local/state/agent-mesh`): `run.json` (agents, models history, worktrees), per-agent mailboxes, `log.jsonl` events, and the `RUN_FOLDERS` agents write into (`plans/`, `reports/`, `tasks/`). Mutate it only through `editing_run(run_id)` (locked, atomic write); `editing_run(run_id, folder)` edits an archived run.
- **Mailbox plus doorbell** (design §3): `mesh send` writes the message file first, then `deliver` types a one-line `📬 [mesh]` doorbell into the recipient's pane via `herdr agent prompt` (`ring_stored`), retrying until its fresh `(ref <token>)` shows above the input box, and never into a human's draft (`human_draft`, per-pane `ring_lock`). A lost doorbell loses nothing. `main` runs `check_worker` first: a worker of a live run runs only `WORKER_COMMANDS` and sends only to its parent (design §10).
- **Native transport** (design §11): `mesh` writes a Claude recipient's doorbell straight into its inbox socket (`write_inbox`), when `native_route` finds one that accepts it: a worker mesh launched accepting cross-session messages (`worker_settings` passes `crossSessionInbound: accept`), or a root that takes unattested frames (`inbound_setting`, `root_class`). `send_message` (a sender's `SendMessage`) remains only for a Claude sender reaching a bypass-mode root or a background worker launched before that flag; anything else rings the pane. A Claude pane worker that something else relaunched (a herdr restore, `relaunched_outside_mesh`) is reopened with its deny list before any doorbell (`reopen_relaunched`). The mailbox stays the record either way.
- **Views and sizes** (design §9, §11): a worker runs in a herdr pane or as a `claude --bg` session (`view`); a Copilot asker is `headless`. `SIZE_DEFAULTS` maps (kind, size) to model, effort and view; trivial and small tasks have no plan phase (`DIRECT_PREFIX`), `large` is the `auto` flow. A `--bg` session never sees the launch's environment, so `launch_bg` passes identity and config through `--settings` env (`worker_settings`), and every `claude` call runs on `claude_env()`.
- **Identity** (design §4) comes from `MESH_RUN` / `MESH_SELF` / `MESH_PARENT` / `MESH_ROLE`, injected at spawn. `inherited_identity` drops one that isn't this agent's: a Claude worker is its pane's only while its `CLAUDE_PID` leads the pane's process group (`pane_runs_this_claude`), and `pane_worker` finds one a herdr restore relaunched without `MESH_*`. Agents are addressed by herdr name (`<repo>-lead`, `<repo>-<slug>`), with pane id as fallback.
- **Topology:** a root keeps its own repo; one worker per other repo, in its own git worktree under `$MESH_WORKTREES` on branch `mesh/<run>/<slug>`. Workers never spawn agents (1:N, not 1:N:N; see design §7).
- **Models per phase** (design §9): `--model auto` plans on the first model in `PLAN_CHAIN` with quota, then `mesh approve` calls `swap_model`, which reopens **the same conversation** on `EXEC_MODEL` at `EXEC_EFFORT`. `mesh watch` exits 7 on a quota error; the root then runs `mesh switch`.
- **Questions and reviews** (`mesh ask`, design §10): an asker is an ordinary worker that got a read-only task (`ASK_PREFIX` / `REVIEW_PREFIX`), so follow-ups and resume need no special case; `cmd_ask` composes `init_run`, `reach_worker`, `deliver` and `watch_agent`. A review is of one commit (`review_heads`), and a Claude reviewer runs `/code-review high` on it (`CODE_REVIEW`, `head_diff`). A Claude worker that is `read_only` or `planning` runs under `claude_deny`'s `--disallowedTools`; `swap_model(..., lift=True)` (on approve) reopens it without.
- **Copilot askers** (design §10): `ask --kind copilot` goes to `ask_copilot`. `copilot_ask` runs `copilot` headless on one `--session-id`, holds the entry while it runs (`claim_copilot`, `live_holder`) and files the last message as its `done`; `file_orphan` files the answer a killed `ask` owed. `refuse_headless` keeps pane commands away from it; `open_peek` follows it in a pane beside the root (`beside_root`, `.panes.lock`).
- **Peers** (design §14): `$MESH_HOME/peers.json` (`load_peers`, `editing_peers`, `cmd_peers`) registers standing read-only askers, one per cloned project, in detached worktrees no run's `mesh stop` removes (`ensure_peer_worktree`). Every ask runs `prepare_peer`; `ask --peers` (`ask_peers`, `fan_out`) reaches them in threads, waits with `watch_each` and writes one `reports/peers-<stamp>.md`.
- **Resume onto a finished PR** (design §8): `leave_finished_pr` moves a change task's worktree to a new `mesh/<run>/<slug>` when no PR from its branch is open (`finished_pr`).
- **Compaction on resume** (design §12): `maybe_compact` runs `/compact <focus>` headless on the stopped conversation before `resume_worker` reopens it, when `should_compact` says so (`MESH_COMPACT_AT`, `MESH_AGY_COMPACT_AT`).
- **Root reset** (design §15): root commands print `root_hint` when the root's transcript is big and its cache cold (`root_context`, `MESH_ROOT_RESET_AT`). `mesh reset` hands off to a detached `reset_root`, which waits for a quiet input box, types `/clear` (or `/compact`), confirms it (`cleared_session`) and rings `bootstrap_text`; `maybe_autoreset` does it from the root's own watch. `mesh brief` renders `brief()`.
- **Worker hooks** (design, "The Stop hook"): every Claude worker launch passes `worker_hooks` in `--settings`. `Stop` runs `mesh hook stop` (`stop_hook`: blocks once per task with no report, `claim_nudge`, and on unread mail, `hook_mail`; always exit 0); `SessionStart` runs `mesh whoami --reminder <source>`. Both outputs go through `hook_text` (under `HOOK_OUTPUT_MAX`, no wall-clock text).
- **Derived status** (design §13): `mesh status` recomputes what each worker owes or waits on every call (`waiting_rows`), needing-the-root first; nothing of it is stored. `read_log` skips a torn line and `log_event` seals one before appending.
- **Watchdog:** `watch_agents` polls one `Watch` per worker (`mesh watch <name>`, or `--any` with a `Roster`). A worker idle with no report since its last task/answer (`since_last_task`) gets one `nudge`, then exit 3/4; 7 is a usage limit, quota or login (`limited`), 8 a hang (`progress_files` unchanged for `HANG_AFTER`), 0 a stopped run. A declared `wait` (`declared_wait`) holds idle counting off. `agent_status` checks herdr against the process table, and git reads run on `git_env()` (never the worker's index lock).
- **Conversation lookup** (`find_session`): Claude workers get a `--session-id` recorded at spawn; agy conversations are found in agy's `conversation_summaries.db`, then `history.jsonl`, by the first doorbell text (`You are <name> (worker) in agent-mesh run <run>`), and recorded on the worker's first `mesh send`. Changing that doorbell wording breaks resume.

## Conventions the design depends on

- Model changes happen only by reopening the same conversation (approve / switch / resume), never by briefing a fresh worker: the point is to keep the worker's research context. Reject designs that add delegation tiers or cold hand-offs.
- The mesh is process-agnostic: protocol files never name a tracker, branch convention or repo-specific process; workers follow their repo's own rules.
- Skills name model **families** (`argon`, `flash`, `opus`, `sonnet`), resolved to the newest id from `agy models`; don't hard-code model ids.
- `mesh resume` / `mesh open` refuse a worker that is still running (one process per conversation); `open` attaches to a running background worker instead. `mesh stop` stops background workers (never `claude rm`), removes only clean worktrees and keeps every branch that has a commit or an upstream (an untouched one is deleted and recreated from its recorded `base` on resume).
- Keep README, `ORCHESTRATE.md`, the skill descriptions and `docs/design.md` in sync when defaults (models, efforts, exit codes) change.
- Keep this file at most 24,000 bytes: agy truncates a rule file past that, and `AGENTS.md` is a symlink to it (`tests/test_docs.py` checks both). Detail goes in `docs/design.md` (its "Code map" for function names), with a gist here.

## Tests

`tests/conftest.py` loads `bin/mesh` as a module (`mesh` fixture), points `MESH_HOME`/`MESH_WORKTREES` at `tmp_path`, and replaces `mesh.herdr` with `FakeHerdr` (`fake` fixture): register answers with `fake.on("agent", "get", result=...)` / `error=...`, assert with `fake.called(...)`; `fake.screen(target, text, …)` is what `herdr agent read` prints (one text per read, the last repeated). The `run` fixture gives an initialised run `mtest` with root pane `w1:p1` and a worker `repo-fix-typo`. Git worktree tests use real temp repos. Tests must never run real `agy`, `claude` or `copilot`: the `mesh` fixture replaces `run_cli` with `FakeCli` (the `cli` fixture: `cli.on("--bg", stdout=…)`, `cli.called("stop", id)`; each call records `argv`, `cwd`, `env` and `input`), points `MESH_HERDR` at a missing binary and makes `subprocess.Popen` refuse `agy`, `claude`, `copilot` and `herdr` (`RefuseAgentClis`), so a path that bypasses the fakes fails the test, and points `MESH_CC_SOCKS` at an empty folder (`claude_root(mesh, run)` gives the root a socket; the `inbox` fixture binds real ones). For quotas use `with_quotas(mesh, monkeypatch)`, which seeds the `agy models` cache from the `AGY_MODELS` fixture (its ids, like `gemini-3.10-flash`, are made up to test version ordering) and stubs `agy_usage`.
