# CLAUDE.md / AGENTS.md

Guidance for Claude Code and Antigravity (agy) agents working in this repository.

agent-mesh lets an agent working in repo A hand repo B's part of a change to an agent running *inside* B (with B's `CLAUDE.md`/`AGENTS.md`, skills and hooks loaded), in a terminal the user can open, with two-way messaging, from the desktop and Android apps or the CLI. A Go daemon (`meshd`) owns the runs, agents, messages and terminals; agents and users act on it through one API (`mesh`, the generated CLI). `README.md` is the user-facing pitch. `docs/apps.md` is the plan and decision record for the daemon and apps; `docs/design.md` records the `bin/mesh` era that came before (its Python script and herdr are gone) and why each decision was made: read its section before changing behaviour it describes, and where `docs/apps.md` disagrees, the apps doc wins.

## Task tracking

Open work is tracked in `TODO.md` as short summaries: the agent doing a task works out the details. Check it before starting something possibly tracked, and keep it current in the same change: remove (or tick) a task when the work lands, reword it when scope changes, and add a line for any follow-up you find but don't do.

## Peer projects

If `mesh peers` lists them, the `orchestrators` group is this repo's peers: agent orchestration tools like agent-mesh, cloned under `~/research/orchestrators`. They are standing read-only askers that keep their context between asks (`mesh peers` shows each one's `about`). Ask them before designing a mechanism they may have solved, and for any `Research:` task in `TODO.md`: all at once with `mesh ask --peers orchestrators --tier mid --body @q.md` (one `reports/peers-<stamp>.md`), or one with `mesh ask --peers <name> --tier mid --body "…"`.

- `agents-can-communicate` (asker `agents-can-communica-research`): cross-vendor messages between sessions the user opened, no lead agent. Claude's inbox socket, agy native delivery.
- `aionui`: Electron desktop for 20+ agent CLIs (agent code in a separate Rust repo), remote control by web UI and chat bots, a dormant Expo app. Many-CLI support, permissions.
- `ccswarm`: headless one-shot agents driven by a YAML flow state machine, persona quorum votes as gates. Flow/stage design, quorum review, loop caps.
- `cyclops`: a daemon watching agents in tmux, fused state signals, durable mailboxes with one doorbell per message. Delivery into a human's draft.
- `emdash`: Electron desktop running CLIs in parallel worktrees, a workspace server with typed RPC. The app/daemon split.
- `firstmate`: a bash supervisor over herdr panes (watcher, turn-end hooks, leases, caps). Its failure modes: hung agents, stale herdr registrations, trust dialogs.
- `gastown`: patrol agents that respawn workers, a merge queue, convoys. Stop-hook guards, nudge queues, auto-restart, mostly as lessons in what broke.
- `happy`: native Kotlin Android and Swift iOS apps (moved off React Native, reason undocumented), relay and pairing history, hooks. Mobile and the RN lesson.
- `mcp-agent-mail`: an MCP server with mailboxes, identities and file leases. Inbox hooks, leases, two-store drift.
- `omnara`: a Go daemon for self-hosted managed agents (sandboxes, permissions, cron), plus its deleted CLI-wrapper history. Go supervision, stream resume, API evolution.
- `orca` (fmfsaisai/orca): tmux plus raw typing, roles from env vars. Guarding a user's draft, spawning panes on demand.
- `orca-ide` (stablyai/orca): an Electron agent IDE with mobile and remote servers; experimental runs, tasks and dispatches with a mailbox, coordinator wake, gates and permission escalation.
- `paradev` (ex-Superset): Electron desktop, React Native mobile, a hosted relay with per-stream dial-back, a separate PTY daemon. Terminal streaming and replay, PTY daemon lifecycle.
- `paseo`: a daemon per host with desktop, mobile, web and CLI clients; agents orchestrate over MCP or its CLI (`update_agent`, finish notifications); daemon plugins with lifecycle hooks. Dropped as a replacement (design, "Alternatives considered"); ask it about app, daemon and remote-host design.
- `repowire`: a Go daemon meshing live sessions: asks with acks and reminders, Claude's inbox socket, identity across reconnects.
- `rove`: a daemon and PTY host owning every engine, flat dispatch with lineage. Delivery, matching reports to tasks.
- `tmux-orchestrator`: an archived orchestrator → manager → engineer tree over `send-keys`. Mostly a contrast.
- `vibe-kanban` (sunsetting): Rust server, Tauri desktop, headless agents, kanban tasks across CLIs, SSH over the paired link. Event-stream resume, per-turn git state.

## Commands

```bash
daemon/scripts/check.sh                          # the daemon's gate: buf lint and breaking, generated code current, gofmt, vet, test -race, pytest
uvx pytest -q tests                              # the Python suite (install.sh, the plugin, rules files, the make-concise validator, the guard hook)
uvx pytest -q tests -k install                   # tests matching a name
./install.sh            # build and install the daemon, link mesh, the protocol and the skills (--uninstall reverses it); safe to re-run
```

The repo runs no hosted CI: run the part's check script before you report a change (`daemon/scripts/check.sh`, `desktop/scripts/check.sh`, `android/scripts/check.sh`). The installed `mesh`, meshd, the protocol and the skills are **copies** under `~/.local/lib/agent-mesh` (links point into it): a branch switch in this checkout changes no agent until `./install.sh` is re-run.

## Layout

- `daemon/`: the Go daemon, terminal host, CLI and relay. `daemon/share/` is what daemon agents read: `protocol/` (`ORCHESTRATE.md` the root's procedure, `PROTOCOL.md` the messaging rules every agent follows, `ROOT_CARD.md`, `PLAN_TEMPLATE.md`) and `skills/` (the `/delegate`, `/delegate-review`, `/find-peers`, `/harvest-peers`, `/self-review`, `/check-plan` and `/make-concise` entry points). The folder is also a Claude Code plugin named `agent-mesh`: meshd loads it per launch (`--plugin-dir`), and `.claude-plugin/marketplace.json` offers it as a one-plugin marketplace (`source: "./daemon/share"`, no `version`: users track commits). `tests/test_plugin.py` checks it ships exactly `SKILLS` of `install.sh`.
- `proto/`: the API as protobuf, the source of the three generated clients.
- `desktop/`: the Electron app. `android/`: the Kotlin app.
- `install.sh`: the daemon install: builds into `~/.local/lib/agent-mesh`, installs and starts the systemd user units, links `mesh`, `~/.config/agent-mesh` (the protocol) and the skills, writes the block of `rules/global-snippet.md` between markers into `~/.claude/CLAUDE.md` and `~/.gemini/AGENTS.md`, and cleans up an install of the old `bin/mesh`. It skips the `~/.claude/skills` links when the Claude plugin is installed, or with `--plugin`. `tests/test_install.py` runs it in a temp HOME with `go`, `systemctl` and `herdr` stubbed.
- `daemon/share/skills/make-concise`: rewrites a repo's rules files concisely without dropping a rule, run by that repo's own agent; `validate.py` (stdlib, tested in `tests/test_make_concise.py`) fails on a lost fence, marked block, code span, URL, path or `@` import, or a file over 24,000 bytes.
- `.claude/settings.json`: a `PreToolUse` tripwire (`.claude/hooks/guard_generated.py`, tested in `tests/test_project_claude.py`) that stops every Claude agent here from editing generated code with its editing tools; a shell write passes it, and the check scripts are the backstop.
- Each part has a rules file (commands, layout, conventions); read it before working there:
  - `daemon/CLAUDE.md`: the Go daemon and relay.
  - `proto/CLAUDE.md`: the API, and the clients a schema change regenerates.
  - `desktop/CLAUDE.md`: the Electron app.
  - `android/CLAUDE.md`: the Kotlin app.

## Conventions the design depends on

- Model changes happen only by reopening the same conversation (approve / switch / resume), never by briefing a fresh worker, so the worker keeps its research context. Reject designs that add delegation tiers or cold hand-offs.
- The mesh is process-agnostic: protocol files never name a tracker, branch convention or repo-specific process; workers follow their repo's own rules.
- Skills name model **families** (`argon`, `hydrogen`, `flash`, `opus`, `sonnet`), resolved to the newest id from `agy models`; don't hard-code model ids.
- Topology: a root supervises one worker per repo, its own included, each in a git worktree on branch `mesh/<run>/<slug>`. Workers never spawn agents, except read-only askers and reviewers (`mesh spawn --role asker|reviewer`).
- Re-run `./install.sh` once a change that needs it is merged and pulled: a change to `rules/global-snippet.md` (it is copied into `~/.claude/CLAUDE.md` and `~/.gemini/AGENTS.md`, not linked), to `daemon/` or `daemon/share/` (they are installed copies), a skill added, removed or renamed (update `SKILLS` / `RETIRED_SKILLS` in `install.sh` and the plugin's description in `.claude-plugin/marketplace.json`), or `install.sh` itself.
- Keep README, `daemon/README.md`, `ORCHESTRATE.md`, the skill descriptions and `docs/apps.md` in sync when defaults (models, efforts, exit codes) change.
- Keep each rules file (this one and the four parts') at most 24,000 bytes, because agy truncates a rule file past that, each with an `AGENTS.md` symlink to it beside it (`tests/test_docs.py` checks both). Put detail in the part's README or `docs/` and a gist here.
- A change that alters a part's commands, layout or conventions updates that part's rules file (this one for the repo) in the same PR.
- A change to the API regenerates all three clients in the same PR (`proto/CLAUDE.md`).

## Tests

The Go suites run the fakes (`daemon/internal/testutil/fakeclaude`, `fakeagy`) in an isolated HOME, never a real CLI or the live state dirs (`daemon/CLAUDE.md`). The Python suite in `tests/` uses temp HOMEs and stubs for `go`, `systemctl` and `herdr`; nothing in it reaches the machine's own install.
