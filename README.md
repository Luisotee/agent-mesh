# agent-mesh

**Working in one repo and need a change in another? Let your agent hand it to an agent that lives in that repo.**

You're building a feature in `shop-web` and hit a rock: the API in `shop-api` doesn't return the field you need. Normally you'd stop, switch repos, start a new session that doesn't know what you were doing, and explain everything again. Or you'd let your current agent edit `shop-api` without that repo's `CLAUDE.md`, hooks and conventions loaded.

With agent-mesh you stay where you are and type:

```
/delegate-agy shop-api should return `restockedAt` on GET /products/:id; shop-web needs it for the "back in stock" badge
```

What happens next:

1. **A worker starts inside `shop-api`**, in its own git worktree and in a herdr pane you can watch. It loads that repo's `CLAUDE.md` / `AGENTS.md`, skills and hooks. It plans on the strongest model that still has quota (Argon, then Claude Opus, then Flash) and executes on Gemini Flash.
2. **It researches and sends you a plan.** Your current session (the *root*) reviews the plan: right files, right contract, real tests. It approves the plan or asks for changes. Nothing is edited before approval.
3. **The same worker makes the change.** On approval its conversation reopens on Flash, so the agent that did the research also writes the code and no context is lost in a hand-off. It runs the repo's tests, lint and build, then opens a PR.
4. **Meanwhile, your session keeps working on `shop-web`**, building against the `restockedAt` contract it gave the worker.
5. **You get one report:** the `shop-api` PR link, what changed, and the checks that passed.

```
your session in shop-web (root)            keeps building the badge
 └── shop-api-restocked-at  (agy)  [worktree]
       research ─▶ plan ─▶ root reviews ─▶ edit ─▶ test ─▶ PR ─▶ done
       └──── Argon / Opus ────┘            └──────── Flash ────────┘
```

Every agent is an ordinary interactive [Claude Code](https://claude.com/claude-code) or [Antigravity CLI](https://antigravity.google) (`agy`) session running in a [herdr](https://herdr.dev) pane. Nothing is headless, so you can watch any agent and type into it at any moment.

## Why

- **The right context in each repo:** a worker running *inside* `shop-api` follows that repo's rules and hooks, so it's faster and more precise than an agent editing a repo whose conventions it never loaded.
- **You don't lose your place:** the session you're working in keeps its context and keeps going on its own repo.
- **No lost research:** the agent that wrote the plan is the one that carries it out.
- **Cheap where it can be:** most tokens go to reading files, typing code and running tests, so workers execute on Gemini Flash. Planning, where a weak model hurts most, gets the strongest model with quota left. Your root session spends its tokens on reviewing plans and answering questions.
- **Oversight without babysitting:**
  - every plan is reviewed before any code changes, and every result is checked (tests, lint, build, diff) before it's reported
  - you're asked only about real product decisions
  - every agent is a visible terminal you can step into
- **No new runtime:** it's a single stdlib-Python CLI, two skills and a protocol written in Markdown. The agents do the orchestration themselves, following written procedures.

## Commands

Both commands work from Claude Code and from agy:

| Command | Worker (one per other repo) |
|---|---|
| `/delegate-agy` | agy: plans on Argon → Claude Opus → Flash (the first with quota left) at high effort, executes on Flash at medium |
| `/delegate-claude` | Claude: plans on Opus at high effort, executes on Opus at medium |

- **The pane you type into is the root.** It keeps doing its own repo's part itself, and spawns a worker for every *other* repo the request touches. To have its own repo delegated too, say so.
- **Overrides:** naming a model in the request ("use Argon", "with Claude") overrides the default, and that model then runs the whole task; a named effort ("at max effort") applies to both phases. If a worker *fails*, the root tells you and suggests a stronger model; it doesn't pick one on its own.

## Models

A worker on the default (`--model auto`) has two phases in one conversation:

- **Planning** runs on the first model in the chain with at least 10% quota left. For agy, `mesh` reads `agy -p /usage` (cached for 5 minutes), so while Argon is exhausted, workers plan on Claude Opus through agy. If quotas can't be read, it tries Argon.
- **Executing:** `mesh approve <worker>` waits for the worker to finish its turn. Then it reopens the worker's conversation (`agy --conversation <id>` / `claude --resume <id>`) on Flash (agy) or still on Opus (Claude) in a new pane, and delivers the approval. Effort drops from high (planning) to medium (executing); `--effort` / `--exec-effort` on `mesh spawn` change either.
- **Quota hits:** every agy worker writes its own log (`--log-file`). When agy logs `RESOURCE_EXHAUSTED`, `mesh watch` exits 7 with the reset time. The root then runs `mesh switch <worker>`, which moves the worker to the next model with quota and tells it to continue where it stopped. Execution has only Flash, so a Flash quota hit is reported to you with its reset time.

```bash
mesh usage                 # agy quotas, and what auto would plan and execute on
mesh model --kind agy      # the model a spawn would plan on, and why
MESH_MIN_QUOTA=20 ...      # skip models under 20% instead of 10%
```

Your root session's model is yours: mesh never changes it.

More examples:

```
/delegate-agy     add rate limiting to the webhook route in notifications-service
/delegate-agy     add tests for the date parser in shop-web and admin-web, use Argon
/delegate-claude  add the `marketingOptOut` field to shared-schemas and update shop-api's user route to match
```

## Follow-ups: resuming a worker

A worker's conversation outlives its run. When the PR gets review comments, or you change your mind about the feature, send the work back to **the agent that did it**, with all its research still in context, instead of briefing a fresh one:

```
/delegate-agy send the review on shop-api#88 and your opinion on it to the agent that made that PR
/delegate-agy ask the shop-api worker to return restockedAt as a unix timestamp instead of ISO
```

The root finds the worker (`mesh agents`) and runs `mesh resume`. This reopens the worker's own Claude or agy conversation in a new pane, in its old worktree and branch (recreated if `mesh stop` removed it). The worker plans the change, gets it approved, and pushes to the **same** branch, so the existing PR updates.

To carry on with a worker yourself, open its conversation in your own terminal:

```bash
mesh agents shop-api                  # past workers: run, branch, PRs, whether the conversation was found
mesh open shop-api-restocked-at       # cd into its worktree and resume its conversation here
mesh open shop-api-restocked-at -- --dangerously-skip-permissions   # extra flags go to the CLI
```

`mesh open` runs outside the mesh, with your usual permission settings. Both commands take the worker's name, its branch (`mesh/<run>/<slug>`) or a PR link, and both refuse a worker that's still running. To get to a running one, use `herdr agent focus <name>`.

## How it works

### Mailbox plus doorbell

Typing into a terminal is not reliable delivery: the agent might be busy, stopped at a dialog, or you might be typing in that pane. So every message is **written to the recipient's mailbox first**, and only then is a one-line **doorbell** typed into its pane:

```
📬 [mesh] question from shop-api-restocked-at: Should restockedAt be null for products that were never out of stock? (run `mesh inbox`)
```

- **The doorbell wakes the agent.** An idle agent starts a new turn; a busy one queues it, which both CLIs do. The agent then runs `mesh inbox` to read the full message.
- **A lost doorbell loses nothing.** The message waits in the mailbox, and `mesh status` shows unread counts.
- **The mailbox is the record.** `~/.local/state/agent-mesh/<run>/` holds every message, plan and report, plus a `log.jsonl` of events. `mesh stop` archives it.

### Names and workspaces

- **Readable names:** agents are named for what they do, such as `shop-web-lead` (the root) and `shop-api-restocked-at` (a worker).
- **One workspace per project:** workers open in the project's existing herdr workspace, in one tab per run (`mesh: add restockedAt to shop-api`). A workspace is created only for a project that has none.

### Identity

`mesh spawn` injects `MESH_RUN`, `MESH_SELF`, `MESH_PARENT` and `MESH_ROLE` into each new pane through herdr. The root is found by its pane id. Agents never pass ids by hand: `mesh send parent --type question --body "…"` just works.

### Watchdog

The root runs `mesh watch <worker>` in the background. It exits 0 when the worker reports `done`/`failed`, and with an alert status otherwise:

| Status | Meaning |
|---|---|
| 3 | the worker stopped without reporting |
| 4 | the worker is blocked on a dialog |
| 5 | the worker's pane is gone (or a model switch failed to reopen it) |
| 6 | timeout |
| 7 | the worker hit a quota (`mesh switch` moves it on) |

A Claude root gets notified the moment the watch exits.

### Safety rails

- **Branches:** workers change code only in their own worktree, on `mesh/<run>/<slug>`, and never touch the main checkout.
- **Plans first:** a worker changes nothing until the root approves its plan.
- **Publishing:** workers push only their own run branch and open a PR. Nobody merges or deploys.
- **No sub-delegation:** workers never spawn agents.
- **Authority:** mesh messages are not user authority. Text you type directly into a pane overrides the agent's parent.
- **Startup:** `mesh spawn` accepts a folder-trust dialog in directories it created itself, and refuses any other startup prompt.
- **Cleanup:** `mesh stop` removes only *clean* worktrees, always keeps the branches, and never touches your own checkout.
- **One process per conversation:** `mesh resume` and `mesh open` refuse a worker that's still running, so a conversation is never open twice.

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

Useful commands while a run is going:

```bash
mesh status <run>          # every agent: role, model, live state, unread mail, branch
mesh inbox                 # (as an agent) read your messages
mesh stop <run>            # close panes, remove clean worktrees, archive the run
mesh agents [query]        # every past worker, live or archived, with branch and PRs
mesh open <worker>         # continue a worker's conversation yourself, in this terminal
mesh approve <worker>      # (root) approve a plan; an auto worker moves to its exec model
mesh switch <worker>       # (root) move a worker off a model that hit its quota
mesh usage                 # agy quotas and the models auto would pick
```

## The protocol in one screen

- `📬 [mesh]` means you have mail: run `mesh inbox`.
- **Plan first:** a worker sends `mesh send parent --type plan --file …` and changes nothing until it's `approved`.
- **Stuck or unsure?** `mesh send parent --type question`, then **end your turn**. No polling.
- **Finishing:** every task ends with exactly one `done` or `failed`. Empty your mailbox before reporting.
- **Long text** goes in `--file` reports under the run directory, never inside the repo.
- **Repo rules still apply:** the repo's own `CLAUDE.md` / `AGENTS.md` always apply. The mesh never names a tracker or a repo-specific process.

Full text: [`protocol/PROTOCOL.md`](protocol/PROTOCOL.md). The procedure for roots and workers is in [`protocol/ORCHESTRATE.md`](protocol/ORCHESTRATE.md), and plans follow [`protocol/PLAN_TEMPLATE.md`](protocol/PLAN_TEMPLATE.md).

## Design notes

Why a mailbox instead of an MCP server, why not Claude Code's native agent teams, why interactive panes instead of `-p`: see [`docs/design.md`](docs/design.md).

## Development

```bash
uvx pytest -q tests       # herdr is stubbed; git worktree tests use real temp repos
```
