# CLAUDE.md / AGENTS.md

This file provides guidance to Claude Code (claude.ai/code) and Antigravity (agy) when working with code in this repository.

agent-mesh lets an agent working in repo A hand repo B's part of a change to an agent running *inside* B (with B's `CLAUDE.md`/`AGENTS.md`, skills and hooks loaded), as a visible herdr pane with two-way messaging. `README.md` is the user-facing overview; `docs/design.md` records why each decision was made and the alternatives that lost. Read the relevant section there before changing behaviour.

## Task tracking

Open work is tracked in `TODO.md`. Tasks are deliberately short summaries; the agent doing a task is expected to think it through and work out the details itself.

Keep it current as part of the same change: remove a task (or tick it) when the work lands, reword it when scope changes, and add a line for any follow-up you discover but don't do now. Check it before starting something that might already be tracked.

## Commands

```bash
uvx pytest -q tests                              # full suite
uvx pytest -q tests -k approve                   # tests matching a name
uvx pytest -q tests/test_mesh.py::test_name      # one test
./install.sh            # symlink into place (--uninstall reverses it); safe to re-run
```

There is no build or lint step: `bin/mesh` is one stdlib-only Python 3.10+ script with no dependencies. Keep it stdlib-only.

The installed `mesh`, `~/.config/agent-mesh` and the skills are **symlinks to this checkout**, so whatever branch is checked out is what every agent on the machine runs.

## Layout and how the pieces fit

- `bin/mesh`: the whole CLI (argparse in the `# ── CLI` section, one `cmd_*` per subcommand). Sections are marked with `# ── name ──` comments: paths, run state, identity, herdr topology, models, sessions, worktrees, messaging, commands, past workers.
- `protocol/`: Markdown the agents themselves follow, installed as `~/.config/agent-mesh`. `ORCHESTRATE.md` is the root/worker procedure (and the mode table), `PROTOCOL.md` the messaging rules every mesh agent reads, `PLAN_TEMPLATE.md` the plan format. The orchestration is done by agents following these files, not by code, so a behaviour change usually needs both a `bin/mesh` change and a protocol/doc change.
- `skills/delegate-agy`, `skills/delegate-claude`: the `/delegate-*` entry points. The user can type them, and agents invoke them on their own when a request fits the description (never a worker). They point the root at `ORCHESTRATE.md`.
- `skills/delegate-review`: a PR review the user asks for, by any mix of Claude, agy and Copilot (or `all`), run in parallel with one `mesh ask` each. It reads only ORCHESTRATE's "Questions and reviews".
- `rules/global-snippet.md`: the block `install.sh` writes between markers into `~/.claude/CLAUDE.md` and `~/.gemini/AGENTS.md`.

Core mechanics that span several files:

- **Run state** lives in `$MESH_HOME/<run>/` (default `~/.local/state/agent-mesh`): `run.json` (agents, models history, worktrees), per-agent mailboxes, `log.jsonl` events. Mutate it only through `editing_run(run_id)` (locked, atomic write).
- **Mailbox plus doorbell:** `mesh send` writes the message file first, then types a one-line `📬 [mesh]` doorbell into the recipient's pane via `herdr agent prompt`, with retries; before each retry `ring` reads the pane (`pane_lines`, `above_input_box`) so a doorbell a busy agent queued isn't typed again. A lost doorbell loses nothing.
- **Native transport** (design §11): between Claude sessions the doorbell goes by `SendMessage` instead. `native_route` decides: a Claude sender reaches a Claude root at its socket (`address`, recorded by `init_run` via `claude_address`), or a running background worker at its own socket. `deliver` then returns a `send_message` for the sender to pass on, instead of ringing. The mailbox stays the record either way.
- **Views and sizes:** a worker runs in a herdr pane or as a `claude --bg` session (`view`); a Copilot asker is `headless` (below). `SIZE_DEFAULTS` maps (kind, size) to model, effort and view for trivial / small tasks, which have no plan phase and get `DIRECT_PREFIX` (Claude runs Opus at every size; design §9). `large` is the `auto` flow below. A fresh reviewer gets `REVIEW_EFFORT` at trivial and small sizes, and a pane at any size. Background helpers (`launch_bg`, `bg_status`, `stop_bg`, `swap_bg`) read `claude agents --json --all`. A `--bg` session runs in a daemon's spare process, which never sees the launch's environment, so `launch_bg` passes the identity and every `MESH_CONFIG` key (empty when unset) through `--settings` env (`bg_settings`), and every `claude` call runs on `claude_env()` (no identity, pane ids or session variables). Every background relaunch passes full flags, which forks a copy, and mesh records the copy's session id.
- **Identity** comes from `MESH_RUN` / `MESH_SELF` / `MESH_PARENT` / `MESH_ROLE`, injected into each pane at spawn (a background worker gets them through `--settings` env). `inherited_identity` drops an environment identity naming no live run, an agy or Copilot agent (inside Claude Code), or a Claude worker whose recorded session isn't `CLAUDE_CODE_SESSION_ID`. In its own pane a Claude worker is rebound only when its `CLAUDE_PID` leads the pane's foreground process group (`pane_runs_this_claude`, from `herdr pane process-info`; herdr's `agent_session` is spoofable), and the `by-pane` map, which names only roots, is checked the same way (`foreign_to_pane`), so a nested `claude -p` or a background session carrying the pane's ids is never its agent, unless named with `--as`. Such a refusal (`PaneRefused`) never makes `mesh ask` start a run. Agents are addressed by herdr name (`<repo>-lead`, `<repo>-<slug>`), with pane id as fallback.
- **Topology:** a root keeps its own repo; one worker per other repo, in its own git worktree under `$MESH_WORKTREES` on branch `mesh/<run>/<slug>`. Workers never spawn agents (1:N, not 1:N:N; see design §7).
- **Models per phase** (design §9): `--model auto` plans on the first model in `PLAN_CHAIN` with quota (agy quotas parsed from `agy -p /usage`, threshold `MESH_MIN_QUOTA`) at `DEFAULT_EFFORT`, then `mesh approve` calls `swap_model`, which closes the pane and reopens **the same conversation** (`agy --conversation <id>` / `claude --resume <id>`) on `EXEC_MODEL` at `EXEC_EFFORT`. agy encodes effort in the model id (`gemini-<ver>-<family>-<effort>`). `mesh watch` exits 7 on a `RESOURCE_EXHAUSTED` quota line in the worker's own `--log-file`; the root then runs `mesh switch`.
- **Questions and reviews** (`mesh ask`, design §10): an asker is an ordinary worker that got a read-only task (`ASK_PREFIX` / `REVIEW_PREFIX`), so follow-ups, `resume` and turning it into a change need no special case. `ask` composes `init_run`, `spawn_worker` / `resume_worker`, `deliver` and `watch_agent`. A reviewer records the PR in `reviews`, which `worker_prs` skips so a PR link still finds its author.
- **Copilot askers** (design §10): `ask --kind copilot` (`ASK_KINDS`; `spawn` / `init` still take only `KINDS`) goes to `ask_copilot` instead. `copilot_ask` runs `copilot --output-format json` in the asker's worktree with the prompt on stdin, the same `--session-id` every time (it starts the session, then resumes it), and files the last message (`copilot_answer`) as its `done`. While it runs, the entry holds the `mesh ask` pid (`claim_copilot`, checked and set under one run lock), so `live_holder` reports it busy, `agent_status` says `working` and `cmd_stop` refuses. `refuse_headless` keeps `deliver`, `swap_model`, `ring`, `approve` and `switch` away from it. `COPILOT_DENY` lists the denied tools (`shell(<cmd>:*)` form). `mesh peek` reads Copilot's own `events.jsonl` for the session; while it answers, `open_peek` runs `mesh peek --follow` in a pane beside the root (only there; it first closes one a killed `ask` left), and `close_peek` closes it after. `beside_root` counts, splits and records the pane on its agent under `.panes.lock`, so parallel `mesh` calls don't split the same pane.
- **Compaction on resume** (design §12): `maybe_compact` runs `/compact <focus>` headless on the stopped conversation before `resume_worker` reopens it, when `should_compact` says so (`context_tokens` from the Claude session file over `MESH_COMPACT_AT`; agy across runs).
- **Watchdog:** `watch_agents` polls one `Watch` per worker (`mesh watch <name>` or `--any`); `--alerts` keeps going through reports. A worker is stopped after `--grace` seconds idle with no report (`REPORT_TYPES`) since its last task/answer (`since_last_task`); a pane worker first gets one `nudge` through `deliver` (`claim_nudge` logs a `nudge` event per task under the run lock), and alerts 3/4 carry `pane_tail`. The log is read once per poll and passed to each `Watch.tick`. `mesh conflicts` uses `git merge-tree --write-tree`.
- **Conversation lookup** (`find_session`): Claude workers get a `--session-id` UUID recorded at spawn; agy conversations are found in agy's `conversation_summaries.db` (its `preview` column), then `history.jsonl`, by the first doorbell text (`You are <name> (worker) in agent-mesh run <run>`), and recorded as `session` on the worker's first `mesh send` (`record_session`). Changing that doorbell wording breaks resume.

## Conventions the design depends on

- Model changes happen only by reopening the same conversation (approve / switch / resume), never by briefing a fresh worker: the point is to keep the worker's research context. Reject designs that add delegation tiers or cold hand-offs.
- The mesh is process-agnostic: protocol files never name a tracker, branch convention or repo-specific process; workers follow their repo's own rules.
- Skills name model **families** (`argon`, `flash`, `opus`, `sonnet`), resolved to the newest id from `agy models`; don't hard-code model ids.
- `mesh resume` / `mesh open` refuse a worker that is still running (one process per conversation); `open` attaches to a running background worker instead. `mesh stop` stops background workers (never `claude rm`), removes only clean worktrees and keeps every branch that has a commit or an upstream (an untouched one is deleted and recreated from its recorded `base` on resume).
- Keep README, `ORCHESTRATE.md`, the skill descriptions and `docs/design.md` in sync when defaults (models, efforts, exit codes) change.

## Tests

`tests/conftest.py` loads `bin/mesh` as a module (`mesh` fixture), points `MESH_HOME`/`MESH_WORKTREES` at `tmp_path`, and replaces `mesh.herdr` with `FakeHerdr` (`fake` fixture): register answers with `fake.on("agent", "get", result=...)` / `error=...`, assert with `fake.called(...)`; `fake.screen(target, text, …)` is what `herdr agent read` prints (one text per read, the last repeated). The `run` fixture gives an initialised run `mtest` with root pane `w1:p1` and a worker `repo-fix-typo`. Git worktree tests use real temp repos. Tests must never run real `agy`, `claude` or `copilot`: the `mesh` fixture replaces `run_cli` with `FakeCli` (the `cli` fixture: `cli.on("--bg", stdout=…)`, `cli.called("stop", id)`; each call records `argv`, `cwd`, `env` and `input`) and points `MESH_CC_SOCKS` at an empty folder (`claude_root(mesh, run)` gives the root a socket). For quotas use `with_quotas(mesh, monkeypatch)`, which seeds the `agy models` cache from the `AGY_MODELS` fixture (its ids, like `gemini-3.10-flash`, are made up to test version ordering) and stubs `agy_usage`.
