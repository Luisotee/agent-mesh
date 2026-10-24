# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

agent-mesh lets an agent working in repo A hand repo B's part of a change to an agent running *inside* B (with B's `CLAUDE.md`/`AGENTS.md`, skills and hooks loaded), as a visible herdr pane with two-way messaging. `README.md` is the user-facing overview; `docs/design.md` records why each decision was made and the alternatives that lost. Read the relevant section there before changing behaviour.

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
- `skills/delegate-agy`, `skills/delegate-claude`: the `/delegate-*` entry points (`disable-model-invocation: true`, user-typed only). They point the root at `ORCHESTRATE.md`.
- `rules/global-snippet.md`: the block `install.sh` writes between markers into `~/.claude/CLAUDE.md` and `~/.gemini/AGENTS.md`.

Core mechanics that span several files:

- **Run state** lives in `$MESH_HOME/<run>/` (default `~/.local/state/agent-mesh`): `run.json` (agents, models history, worktrees), per-agent mailboxes, `log.jsonl` events. Mutate it only through `editing_run(run_id)` (locked, atomic write).
- **Mailbox plus doorbell:** `mesh send` writes the message file first, then types a one-line `📬 [mesh]` doorbell into the recipient's pane via `herdr agent prompt`, with retries. A lost doorbell loses nothing.
- **Identity** comes from `MESH_RUN` / `MESH_SELF` / `MESH_PARENT` / `MESH_ROLE`, injected into each pane at spawn. Agents are addressed by herdr name (`<repo>-lead`, `<repo>-<slug>`), with pane id as fallback.
- **Topology:** a root keeps its own repo; one worker per other repo, in its own git worktree under `$MESH_WORKTREES` on branch `mesh/<run>/<slug>`. Workers never spawn agents (1:N, not 1:N:N; see design §7).
- **Models per phase** (design §9): `--model auto` plans on the first model in `PLAN_CHAIN` with quota (agy quotas parsed from `agy -p /usage`, threshold `MESH_MIN_QUOTA`) at `DEFAULT_EFFORT`, then `mesh approve` calls `swap_model`, which closes the pane and reopens **the same conversation** (`agy --conversation <id>` / `claude --resume <id>`) on `EXEC_MODEL` at `EXEC_EFFORT`. agy encodes effort in the model id (`gemini-<ver>-<family>-<effort>`). `mesh watch` exits 7 on a `RESOURCE_EXHAUSTED` quota line in the worker's own `--log-file`; the root then runs `mesh switch`.
- **Conversation lookup** (`find_session`): Claude workers get a `--session-id` UUID recorded at spawn; agy conversations are found in agy's `history.jsonl` by the first doorbell text (`You are <name> (worker) in agent-mesh run <run>`). Changing that doorbell wording breaks resume.

## Conventions the design depends on

- Model changes happen only by reopening the same conversation (approve / switch / resume), never by briefing a fresh worker: the point is to keep the worker's research context. Reject designs that add delegation tiers or cold hand-offs.
- The mesh is process-agnostic: protocol files never name a tracker, branch convention or repo-specific process; workers follow their repo's own rules.
- Skills name model **families** (`argon`, `flash`, `opus`, `sonnet`), resolved to the newest id from `agy models`; don't hard-code model ids.
- `mesh resume` / `mesh open` refuse a worker that is still running (one process per conversation). `mesh stop` removes only clean worktrees and always keeps branches.
- Keep README, `ORCHESTRATE.md`, the skill descriptions and `docs/design.md` in sync when defaults (models, efforts, exit codes) change.

## Tests

`tests/conftest.py` loads `bin/mesh` as a module (`mesh` fixture), points `MESH_HOME`/`MESH_WORKTREES` at `tmp_path`, and replaces `mesh.herdr` with `FakeHerdr` (`fake` fixture): register answers with `fake.on("agent", "get", result=...)` / `error=...`, assert with `fake.called(...)`. The `run` fixture gives an initialised run `mtest` with root pane `w1:p1` and a worker `repo-fix-typo`. Git worktree tests use real temp repos. Tests must never run real `agy`: use `with_quotas(mesh, monkeypatch)`, which seeds the `agy models` cache from the `AGY_MODELS` fixture (its ids, like `gemini-3.10-flash`, are made up to test version ordering) and stubs `agy_usage`.
