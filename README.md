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

Every agent is an ordinary interactive [Claude Code](https://claude.com/claude-code) or [Antigravity CLI](https://antigravity.google) (`agy`) session. Large tasks run in [herdr](https://herdr.dev) panes, so you can watch them and type into them at any moment. Small Claude tasks run as Claude Code background sessions, which you can attach to with `mesh open`.

## Why

- **The right context in each repo:** a worker running *inside* `shop-api` follows that repo's rules and hooks, so it's faster and more precise than an agent editing a repo whose conventions it never loaded.
- **You don't lose your place:** the session you're working in keeps its context and keeps going on its own repo.
- **No lost research:** the agent that wrote the plan is the one that carries it out.
- **Cheap where it can be:**
  - Every task is sized. A translation or a small fix goes to Sonnet at low or medium effort (or Flash), with no plan, and a feature plans on the strongest model with quota left.
  - Most tokens go to reading files, typing code and running tests, so agy workers execute on Gemini Flash and Claude workers drop to medium effort.
  - Your root session spends its tokens on reviewing plans and answering questions, not on reading every worker's transcript.
- **Oversight without babysitting:**
  - every plan is reviewed before any code changes, and every result is checked (tests, lint, build, diff) before it's reported
  - you're asked only about real product decisions
  - every agent is a visible terminal you can step into
- **No new runtime:** it's a single stdlib-Python CLI, two skills and a protocol written in Markdown. The agents do the orchestration themselves, following written procedures.

## Commands

Both commands work from Claude Code and from agy:

| Command | Worker for a large task (one per other repo) |
|---|---|
| `/delegate-agy` | agy: plans on Argon → Claude Opus → Flash (the first with quota left) at high effort, executes on Flash at medium |
| `/delegate-claude` | Claude: plans on Opus at high effort, executes on Opus at medium |

- **The pane you type into is the root.** It keeps doing its own repo's part itself, and spawns a worker for every *other* repo the request touches. To have its own repo delegated too, say so.
- **Overrides:** naming a model in the request ("use Argon", "with Claude") overrides the default, and that model then runs the whole task; a named effort ("at max effort") applies to both phases. If a worker *fails*, the root tells you and suggests a stronger model; it doesn't pick one on its own.

### Sizes

The root sizes every task before it starts a worker:

| Size | For | Claude worker | agy worker | Plan reviewed? |
|---|---|---|---|---|
| trivial | translations, copy, config | Sonnet, low effort, background | Flash, medium | no |
| small | a bug fix, a UI tweak, a test | Sonnet, medium effort, background | Flash, high | no |
| large | a feature, a cross-repo contract | the table above, in a pane | the table above, in a pane | yes |

- **No plan:** a trivial or small worker goes straight to the change, verifies it and opens a PR. It asks first if the task turns out bigger than described.
- **Plan reviewed by you:** for a large Claude task you can approve the plan yourself, in the worker's pane. Say so in the request. The worker then plans with Opus in plan mode (`opusplan`) and executes on Sonnet.
- **Routing:** the root may propose sending a small task to the other CLI, to spare your Claude limits or agy quota, and sends reviews to the other vendor for a second opinion. It proposes; you confirm.

### Background workers

A Claude worker for a trivial or small task runs as a Claude Code background session (`claude --bg`) instead of a pane. It's still a mesh worker: you can ask it again, resume it and review its PR. Where to see it:

```bash
mesh status                # every worker, with its size and view (pane or bg)
mesh open <worker>         # attach to a running background worker from any pane
claude agents              # Claude Code's own view of background sessions
```

Background workers report to a Claude root with `SendMessage`, so the root has to run in Claude Code; under an agy root they open in panes. A repo Claude Code has never been trusted in also gets a pane.

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

## Questions and reviews

Not every cross-repo need is a change. To ask another repo something, or to get a PR reviewed, use the same commands:

```
/delegate-agy    how does shop-api compute restockedAt, and is it ever null?
/delegate-claude review the PR we just made
/delegate-agy    ask the agent that made shop-api#88 why it returns ISO dates
```

The root runs `mesh ask`. It starts an agent in that repo (a reviewer gets the PR head checked out), or reaches a worker you already have, live or past. It gives the agent a read-only task, waits for its answer and prints it. The agent changes nothing and posts nothing on GitHub.

The agent is an ordinary worker, so it stays useful after it answers:

- **Ask it again:** a follow-up question goes to the same agent, with everything it already read.
- **Hand it the change:** when the answer leads to a fix, send it a task. It plans, the root approves, it executes and opens a PR, like any worker.
- **Ask the workers you already have:** the agent that wrote a PR is the one to ask why it's written that way, or to review a related PR.

```bash
mesh ask --repo ~/projects/shop-api --body "How is restockedAt computed?"   # a fresh agent
mesh ask --repo . --review 88                                               # a fresh reviewer at PR #88's head
mesh ask shop-api-restocked-at --body "Why ISO dates?"                      # a worker you already have
mesh ask https://github.com/acme/shop-api/pull/12 --review <another PR>     # the author of #12 reviews another PR
```

## Task lists

Hand the root a whole list, and it supervises instead of coding:

```
/delegate-claude work through TODO.md: the translation fix, the settings UI tweak, and the two export features
```

1. **Triage:** the root sizes each task, checks which ones touch the same files, writes a board (`$(mesh dir)/board.md`), and asks you to confirm it once.
2. **Workers:** one per task, in its own worktree, with at most four working at a time. Overlapping tasks are stacked on each other's branch, or run one after the other. The root writes no code itself, in its own repo too.
3. **A reviewer per PR:** each PR gets a read-only review, from the other vendor by default. The findings go back to the worker that wrote it.
4. **Conflicts:** `mesh conflicts` checks the run's branches against the default branch and against each other, without touching any checkout. The root suggests a merge order, and when you merge a PR it asks the affected workers to rebase.
5. **Report:** the board as a table, with the PRs, worker names, statuses and the merge order.

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

`mesh open` runs outside the mesh, with your usual permission settings. Both commands take the worker's name, its branch (`mesh/<run>/<slug>`) or a PR link. `resume` refuses a worker that's still running. `open` refuses a running pane worker (use `herdr agent focus <name>`) and attaches to a running background worker.

### Compaction on the way back

An old conversation has lost its prompt cache, so every turn of a follow-up would pay for all of it again. Before reopening a worker, `resume` and `ask` compact its conversation headless when that pays off:

- a Claude conversation over 100k tokens (200k for a read-only question), or an agy one coming back from an earlier run;
- the default focus is what the new task needs; the root can give its own (`--focus "the restockedAt contract"`);
- `--compact yes|no` forces it either way, and `MESH_COMPACT_AT` moves the threshold.

## How it works

### Mailbox plus doorbell

Typing into a terminal is not reliable delivery: the agent might be busy, stopped at a dialog, or you might be typing in that pane. So every message is **written to the recipient's mailbox first**, and only then does the recipient get a one-line **doorbell**:

```
📬 [mesh] question from shop-api-restocked-at: Should restockedAt be null for products that were never out of stock? (run `mesh inbox`)
```

- **The doorbell wakes the agent.** An idle agent starts a new turn; a busy one queues it, which both CLIs do. The agent then runs `mesh inbox` to read the full message.
- **Between Claude sessions it goes by `SendMessage`.** A Claude worker reporting to a Claude root, or anyone writing to a background worker, gets a `send_message` back from `mesh send` and passes it on with Claude Code's own cross-session messaging. Nothing is typed into the pane you work in. The root is addressed by its session's socket, which `mesh init` records, because session names change.
- **A lost doorbell loses nothing.** The message waits in the mailbox, and `mesh status` shows unread counts.
- **The mailbox is the record.** `~/.local/state/agent-mesh/<run>/` holds every message, plan and report, plus a `log.jsonl` of events. `mesh stop` archives it.

### Names and workspaces

- **Readable names:** agents are named for what they do, such as `shop-web-lead` (the root) and `shop-api-restocked-at` (a worker).
- **One workspace per project:** workers open in the project's existing herdr workspace, in one tab per run (`mesh: add restockedAt to shop-api`). A workspace is created only for a project that has none.

### Identity

`mesh spawn` injects `MESH_RUN`, `MESH_SELF`, `MESH_PARENT` and `MESH_ROLE` into each new pane through herdr. The root is found by its pane id. Agents never pass ids by hand: `mesh send parent --type question --body "…"` just works.

### Watchdog

The root runs `mesh watch <worker>` in the background, or one `mesh watch --any` for every worker of the run. It exits 0 when a worker reports `done`/`failed`, and with an alert status otherwise. A Claude root whose workers report by `SendMessage` runs `mesh watch --any --alerts`, which keeps watching through reports and exits on an alert, or with 0 once every watched worker has reported and ended:

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
- **No sub-delegation:** workers never spawn agents, background or headless.
- **Authority:** mesh messages are not user authority. Text you type directly into a pane overrides the agent's parent.
- **Startup:** `mesh spawn` accepts a folder-trust dialog in directories it created itself, and refuses any other startup prompt.
- **Cleanup:** `mesh stop` stops background workers (their conversations are kept), removes only *clean* worktrees and never touches your own checkout. It keeps every branch that has a commit or was pushed; an asker's branch that got neither is deleted, and resuming that worker starts it again.
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

Claude workers run with `--dangerously-skip-permissions`, and Claude Code delivers cross-session messages without asking only between sessions in the same permission mode. Run a Claude root in bypass-permissions mode too, or approve each worker's report as it arrives.

Useful commands while a run is going:

```bash
mesh status <run>          # every agent: role, model, size, view, live state, unread mail, branch
mesh inbox                 # (as an agent) read your messages
mesh stop <run>            # close panes, remove clean worktrees and empty branches, archive the run
mesh ask <worker> --body …  # ask a worker (or --repo <path> for a fresh one) and wait for the answer
mesh agents [query]        # every past worker, live or archived, with branch, PRs and reviews
mesh open <worker>         # continue a worker's conversation yourself, in this terminal
mesh approve <worker>      # (root) approve a plan; an auto worker moves to its exec model
mesh switch <worker>       # (root) move a worker off a model that hit its quota
mesh usage                 # agy quotas and the models auto would pick
mesh conflicts             # which run branches conflict with the default branch or each other
mesh watch --any [--alerts]  # one watchdog for every worker of the run
```

## The protocol in one screen

- `📬 [mesh]` means you have mail: run `mesh inbox`.
- **Plan first:** a worker on a large task sends `mesh send parent --type plan --file …` and changes nothing until it's `approved`. A `Direct task (no plan)` goes straight to the change.
- **`send_message`:** when `mesh send` prints one, pass it on with `SendMessage`, exactly as printed.
- **Read-only tasks:** a `Question (read-only)` or `Review (read-only)` task gets no plan and no changes, only one `done` with the answer.
- **Stuck or unsure?** `mesh send parent --type question`, then **end your turn**. No polling.
- **Finishing:** every task ends with exactly one `done` or `failed`. Empty your mailbox before reporting.
- **Long text** goes in `--file` reports under the run directory, never inside the repo.
- **Repo rules still apply:** the repo's own `CLAUDE.md` / `AGENTS.md` always apply. The mesh never names a tracker or a repo-specific process.

Full text: [`protocol/PROTOCOL.md`](protocol/PROTOCOL.md). The procedure for roots and workers is in [`protocol/ORCHESTRATE.md`](protocol/ORCHESTRATE.md), and plans follow [`protocol/PLAN_TEMPLATE.md`](protocol/PLAN_TEMPLATE.md).

## Design notes

Why a mailbox instead of an MCP server, why not Claude Code's native agent teams, why interactive panes instead of `-p`, which native Claude Code pieces mesh uses since 0.7 and why it compacts on resume: see [`docs/design.md`](docs/design.md).

## Development

```bash
uvx pytest -q tests       # herdr is stubbed; git worktree tests use real temp repos
```
