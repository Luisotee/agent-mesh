# agent-mesh

**Expensive models plan, cheap models type, and you can watch all of it.**

agent-mesh lets one coding agent hand a request to a team of agents spread across your repos:

- **a planner inside each affected repo**, so it works with that repo's own `CLAUDE.md` / `AGENTS.md`, skills and hooks
- **executors in their own git worktrees**, which carry out the plans
- **two-way messages** between them: executors ask questions, planners answer, and everyone reports back up

Every agent is an ordinary interactive [Claude Code](https://claude.com/claude-code) or [Antigravity CLI](https://antigravity.google) (`agy`) session running in a [herdr](https://herdr.dev) pane. Nothing is headless, so you can watch any agent in herdr's agents tab and type into it at any moment.

```
you ─▶ /delegate-claude-agy fix the reaction bug in curupira-bot and port it to castanha-bot

root (the session you typed into)
 ├── curupira-bot planner  (Claude Opus, high)  ──▶  executor e1 (agy, Gemini Flash)  [worktree]
 │                                               └─▶  executor e2 (agy, Gemini Flash)  [worktree]
 └── castanha-bot planner  (Claude Opus, high)  ──▶  executor e1 (agy, Gemini Flash)  [worktree]

questions flow up to the lowest agent that can answer them;
reports flow up as short summaries; each repo ends with a PR.
```

## Why

- **Cost:** most of the tokens in a coding task go to reading files, typing code and running tests, not to deciding what to do. agent-mesh gives that work to a cheap, fast model and keeps the expensive one for planning, answering questions and reviewing.
- **Per-repo context:** a planner sitting *inside* each repo loads that repo's rules, unlike one central agent juggling every repo's conventions. Each executor also loads the repo's `AGENTS.md`.
- **Oversight without babysitting:**
  - every result is checked by its planner (tests, lint, build, diff review) before it moves up
  - you're only asked about genuine product decisions
  - but you can step in anywhere, because every agent is a visible terminal you can type into
- **No new runtime:** it's a single stdlib-Python CLI, three skills and a protocol written in Markdown. The agents themselves do the orchestration, following written procedures.

## Modes

Each mode is a command, available in both Claude Code and agy:

| Command | Planner | Executor |
|---|---|---|
| `/delegate-claude-agy` | Claude, Opus, high effort | agy, newest Gemini Flash |
| `/delegate-claude` | Claude, Opus, high effort | Claude, Opus, high effort |
| `/delegate-agy` | agy, Gemini Argon, high effort | agy, newest Gemini Flash |

- **Flash effort per task:** `low` for mechanical edits, `medium` by default, `high` for multi-step work.
- **Escalation:** when an executor fails twice, it moves up a level: Flash high → Argon → the planner does the work itself.
- **Overrides:** naming a model in the request overrides the defaults.

The command decides who does the work. The pane you type it into decides who coordinates and gets the final report.

## How it works

### Mailbox plus doorbell

Typing into a terminal is not reliable delivery: the agent might be busy, stopped at a dialog, or you might be typing in that pane. So every message is **written to the recipient's mailbox first**, and only then is a one-line **doorbell** typed into its pane:

```
📬 [mesh] question from curupira-bot-fix-reactions: Should the reaction be removed when the reply fails? (run `mesh inbox`)
```

- **The doorbell wakes the agent.** An idle agent starts a new turn; a busy one queues it, which both CLIs do. The agent then runs `mesh inbox` to read the full message.
- **A lost doorbell loses nothing.** The message waits in the mailbox, and `mesh status` shows unread counts.
- **The mailbox is the record.** `~/.local/state/agent-mesh/<run>/` holds every message, plan and report, plus a `log.jsonl` of events. `mesh stop` archives it.

### Names and workspaces

- **Readable names:** agents are named for what they do, such as `ai-boilerplate-lead`, `curupira-bot-planner` and `curupira-bot-fix-reactions`.
- **One workspace per project:** agents open in the project's existing herdr workspace, in one tab per run (`mesh: fix the reaction bug`). A workspace is created only for a project that has none.

### Identity

`mesh spawn` injects `MESH_RUN`, `MESH_SELF`, `MESH_PARENT` and `MESH_ROLE` into each new pane through herdr. The root is found by its pane id. Agents never pass ids by hand: `mesh send parent --type question --body "…"` just works.

### Watchdog

Supervisors run `mesh watch <child>` in the background. It exits 0 when the child reports `done`/`failed`, and with an alert status otherwise:

| Status | Meaning |
|---|---|
| 3 | the child stopped without reporting |
| 4 | the child is blocked on a dialog |
| 5 | the child's pane is gone |
| 6 | timeout |

A Claude supervisor gets notified the moment the watch exits.

### Safety rails

- **Branches:** executors work only in their worktree, on `mesh/<run>/<slug>`, and never push.
- **Publishing:** planners push only their run's branches and open PRs. Nobody merges or deploys.
- **Authority:** mesh messages are not user authority. Text you type directly into a pane overrides the agent's parent.
- **Startup:** `mesh spawn` accepts a folder-trust dialog in directories it created itself, and refuses any other startup prompt.
- **Cleanup:** `mesh stop` removes only *clean* worktrees and always keeps the branches.

## Install

Requirements:
- [herdr](https://herdr.dev), with your agents running inside it
- `python3` 3.10 or newer
- `git`
- `gh` (for PRs)
- Claude Code and/or agy

```bash
git clone https://github.com/Luisotee/agent-mesh ~/projects/agent-mesh
~/projects/agent-mesh/install.sh        # --uninstall reverses it
```

`install.sh` symlinks:

| From | To |
|---|---|
| `bin/mesh` | `~/.local/bin/mesh` |
| `protocol/` | `~/.config/agent-mesh` |
| the skills | `~/.claude/skills/` and `~/.gemini/config/skills/` |

It also adds a short, marked block to `~/.claude/CLAUDE.md` and `~/.gemini/AGENTS.md`, so any agent that receives a doorbell knows the protocol. Re-running it is safe.

## Use

From any Claude Code or agy session inside herdr:

```
/delegate-claude-agy add rate limiting to the webhook route in whatsapp-cloud
/delegate-claude     add the opt-out field to fleetview's schemas and update ai-boilerplate's admin route to match
/delegate-agy        add tests for the whitelist parser in all three TS clients
```

Useful commands while a run is going:

```bash
mesh status <run>          # every agent: role, model, live state, unread mail, branch
mesh inbox                 # (as an agent) read your messages
mesh stop <run>            # close panes, remove clean worktrees, archive the run
```

## The protocol in one screen

- `📬 [mesh]` means you have mail: run `mesh inbox`.
- **Stuck or unsure?** `mesh send parent --type question`, then **end your turn**. No polling.
- **Finishing:** every task ends with exactly one `done` or `failed`. Empty your mailbox before reporting.
- **Long text** goes in `--file` reports under the run directory, never inside the repo.
- **Repo rules still apply:** the repo's own `CLAUDE.md` / `AGENTS.md` always apply. The mesh never names a tracker or a repo-specific process.

Full text: [`protocol/PROTOCOL.md`](protocol/PROTOCOL.md). The procedure for roots and planners is in [`protocol/ORCHESTRATE.md`](protocol/ORCHESTRATE.md), and plans follow [`protocol/PLAN_TEMPLATE.md`](protocol/PLAN_TEMPLATE.md).

## Design notes

Why a mailbox instead of an MCP server, why not Claude Code's native agent teams, why interactive panes instead of `-p`: see [`docs/design.md`](docs/design.md).

## Development

```bash
uvx pytest -q tests       # herdr is stubbed; git worktree tests use real temp repos
```
