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

Every agent is an ordinary interactive [Claude Code](https://claude.com/claude-code) or [Antigravity CLI](https://antigravity.google) (`agy`) session. Large tasks run in [herdr](https://herdr.dev) panes, so you can watch them and type into them at any moment. Small Claude tasks run as Claude Code background sessions, which you can attach to with `mesh open`. Reviewers get a pane at any size, so you see the review happen.

## Why

- **The right context in each repo:** a worker running *inside* `shop-api` follows that repo's rules and hooks, so it's faster and more precise than an agent editing a repo whose conventions it never loaded.
- **You don't lose your place:** the session you're working in keeps its context and keeps going on its own repo.
- **No lost research:** the agent that wrote the plan is the one that carries it out.
- **Effort where it pays:**
  - Every task is sized. A translation or a small fix goes straight to the change with no plan (Opus at medium effort, or Flash), and a feature plans at high effort on the strongest model with quota left.
  - Most tokens go to reading files, typing code and running tests, so agy workers execute on Gemini Flash and Claude workers on Opus at medium effort. Reviews run at high effort, and a fix the review found gets high effort too.
  - Your root session spends its tokens on reviewing plans and answering questions, not on reading every worker's transcript.
- **Oversight without babysitting:**
  - every plan is reviewed before any code changes, and every result is checked (tests, lint, build, diff) before it's reported
  - you're asked only about real product decisions
  - every agent is a visible terminal you can step into
- **No new runtime:** it's a single stdlib-Python CLI, three skills and a protocol written in Markdown. The agents do the orchestration themselves, following written procedures.

## Commands

Both commands work from Claude Code and from agy:

| Command | Worker for a large task (one per other repo) |
|---|---|
| `/delegate-agy` | agy: plans on Argon → Claude Opus → Flash (the first with quota left) at high effort, executes on Flash at medium |
| `/delegate-claude` | Claude: plans on Opus at high effort, executes on Opus at medium |

- **You don't have to type the command.** Agents use these skills on their own when you ask for something in another repo ("have the shop-api agent add `restockedAt`", "ask agy how shop-api computes it"). Naming a CLI picks its mode; otherwise the agent uses its own. Typing the command still works and leaves no doubt.
- **The pane you type into is the root.** It keeps doing its own repo's part itself, and spawns a worker for every *other* repo the request touches. To have its own repo delegated too, say so.
- **Overrides:** naming a model in the request ("use Argon", "with Claude", "with Fable") overrides the default, and that model then runs the whole task; a named effort ("at max effort") applies to both phases. Claude Fable 5.1 is never a default, since it draws on a capped share of your Claude limits: name it for an ambiguous root-cause hunt or an architecture decision. If a worker *fails*, the root tells you and suggests a stronger model or more effort; it doesn't pick a model on its own.

### Sizes

The root sizes every task before it starts a worker:

| Size | For | Claude worker | agy worker | Plan reviewed? |
|---|---|---|---|---|
| trivial | translations, copy, config | Opus, medium effort, background | Flash, low | no |
| small | a bug fix, a UI tweak, a test | Opus, medium effort, background | Flash, high | no |
| large | a feature, a cross-repo contract | the table above, in a pane | the table above, in a pane | yes |

- **No plan:** a trivial or small worker goes straight to the change, verifies it and opens a PR. It asks first if the task turns out bigger than described.
- **Plan reviewed by you:** for a large Claude task you can approve the plan yourself, in the worker's pane. Say so in the request. The worker then plans with Opus in plan mode and, once you approve, executes on Opus in the same conversation, at high effort throughout (no switch to medium, since there's no `mesh approve`).
- **Routing:** the root may propose sending a large agy task to Claude when Argon and Opus quota run low, and sends reviews to the other vendor for a second opinion. It proposes; you confirm.
- **Reviews and fixes:** a fresh reviewer runs at high effort whatever the PR's size (a small one instead of its size's medium; a large one plans at high anyway). When a review finding is real, or a worker fails, the root retires the worker and resumes it with more effort for the fix (Claude at high, agy on Argon) instead of starting every task high.

### Background workers

A Claude worker for a trivial or small task runs as a Claude Code background session (`claude --bg`) instead of a pane. It's still a mesh worker: you can ask it again, resume it and review its PR. Where to see it:

```bash
mesh status                # every worker, with its size and view (pane or bg)
mesh open <worker>         # attach to a running background worker from any pane
claude agents              # Claude Code's own view of background sessions
```

Background workers report to a Claude root with `SendMessage`, so the root has to run in Claude Code; under an agy root they open in panes. A repo Claude Code has never been trusted in also gets a pane, and so does a fresh reviewer (`mesh ask --review`), whatever its size.

## Models

A worker on the default (`--model auto`) has two phases in one conversation:

- **Planning** runs on the first model in the chain with at least 10% quota left. For agy, `mesh` reads `agy -p /usage` (cached for 5 minutes), so while Argon is exhausted, workers plan on Claude Opus through agy. If quotas can't be read, it tries Argon.
- **Executing:** `mesh approve <worker>` waits for the worker to finish its turn. Then it reopens the worker's conversation (`agy --conversation <id>` / `claude --resume <id>`) on Flash (agy) or still on Opus (Claude) in a new pane, and delivers the approval. Effort drops from high (planning) to medium (executing); `--effort` / `--exec-effort` on `mesh spawn` change either. If the reopen can't start (the conversation isn't found, or the worker stays busy), approve sends nothing and suggests `mesh approve <worker> --keep-model`, which executes on the planning model.
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

Not every cross-repo need is a change. To ask another repo something, use the same commands. To get a PR reviewed, yours included, use `/delegate-review`, which takes the reviewers as arguments:

```
/delegate-agy     how does shop-api compute restockedAt, and is it ever null?
/delegate-agy     ask the agent that made shop-api#88 why it returns ISO dates
/delegate-review                  # the current branch's PR, reviewed by the other vendor (Claude ↔ agy)
/delegate-review  copilot 88      # PR #88, reviewed by Copilot
/delegate-review  copilot agy     # the current PR, by Copilot and agy at the same time
/delegate-review  all 88          # PR #88, by Claude, agy and Copilot
```

`/delegate-review` checks every finding against the code before it shows you, merges the ones several reviewers found (saying who found each), gives its verdict on each, and fixes nothing unless you ask.

The root runs `mesh ask`. It starts an agent in that repo (a reviewer gets the PR head checked out, and the diff in its task), or reaches a worker you already have, live or past. It gives the agent a read-only task, waits for its answer and prints it, then closes an agent it opened (`--keep` leaves it open; asking again reopens its conversation). The agent changes nothing and posts nothing on GitHub.

The agent is an ordinary worker, so it stays useful after it answers:

- **Ask it again:** a follow-up question goes to the same agent, with everything it already read.
- **Hand it the change:** when the answer leads to a fix, resume it with a task (`mesh resume <name> --task-file …`). It plans, the root approves, it executes and opens a PR, like any worker.
- **Ask the workers you already have:** the agent that wrote a PR is the one to ask why it's written that way, or to review a related PR.

A **Copilot** asker (`--kind copilot`) is the exception: it runs headless inside the `mesh ask` call and only answers. While it works, a pane beside yours follows it (`mesh peek <name> --follow`, which you can also run yourself) and closes once it has answered; past `MESH_SIDE_PANES` panes there's no room, and no peek pane. Asking it again resumes its session, but it can't take a change. On a plan that only offers `auto` (Copilot Student), Copilot picks the model.

```bash
mesh ask --repo ~/projects/shop-api --body "How is restockedAt computed?"   # a fresh agent
mesh ask --repo . --review 88                                               # a fresh reviewer at PR #88's head
mesh ask --repo . --review 88 --kind copilot                                # the same, by Copilot, headless
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
3. **A reviewer per PR:** each PR gets a read-only review, from the other vendor by default. The findings go back to the worker that wrote it, as claims to check: the root and the author each confirm a bug is real and pick the best fix before changing anything.
4. **Conflicts:** `mesh conflicts` checks the run's branches against the default branch and against each other, without touching any checkout. The root suggests a merge order, and when you merge a PR it asks the affected workers to rebase.
5. **Report:** the board as a table, with the PRs, worker names, statuses and the merge order.

## Follow-ups: resuming a worker

A worker's conversation outlives its run. When the PR gets review comments, or you change your mind about the feature, send the work back to **the agent that did it**, with all its research still in context, instead of briefing a fresh one:

```
/delegate-agy send the review on shop-api#88 and your opinion on it to the agent that made that PR
/delegate-agy ask the shop-api worker to return restockedAt as a unix timestamp instead of ISO
```

The root finds the worker (`mesh agents`) and runs `mesh resume`. This reopens the worker's own Claude or agy conversation in a new pane, in its old worktree and branch (recreated if `mesh stop` removed it). The worker plans the change on the planning model (the best with quota, as for a new worker, whatever it last ran on), gets it approved (which moves it to the execution model and effort, as for a new worker), and pushes to the **same** branch, so the existing PR updates.

To carry on with a worker yourself, open its conversation in your own terminal:

```bash
mesh agents shop-api                  # past workers: run, branch, PRs, whether the conversation was found
mesh open shop-api-restocked-at       # cd into its worktree and resume its conversation here
mesh open shop-api-restocked-at -- --dangerously-skip-permissions   # extra flags go to the CLI
```

`mesh open` runs outside the mesh, with your usual permission settings. Both commands take the worker's name, its branch (`mesh/<run>/<slug>`) or a PR link. `resume` refuses a worker that's still running. `open` refuses a running pane worker (use `herdr agent focus <name>`) and attaches to a running background worker.

### Compaction on the way back

An old conversation has lost its prompt cache, so every turn of a follow-up would pay for all of it again. Before reopening a worker, `resume` and `ask` compact its conversation headless when that pays off:

- a Claude conversation over 300k tokens (600k for a read-only question), or an agy one coming back from an earlier run;
- the default focus is what the new task needs; the root can give its own (`--focus "the restockedAt contract"`);
- `--compact yes|no` forces it either way, and `MESH_COMPACT_AT` moves the threshold.

## How it works

### Mailbox plus doorbell

Typing into a terminal is not reliable delivery: the agent might be busy, stopped at a dialog, or you might be typing in that pane. So every message is **written to the recipient's mailbox first**, and only then does the recipient get a one-line **doorbell**:

```
📬 [mesh] question from shop-api-restocked-at: Should restockedAt be null for products that were never out of stock? (run `mesh inbox`)
```

- **The doorbell wakes the agent.** An idle agent starts a new turn; a busy one queues it, which both CLIs do. The agent then runs `mesh inbox` to read the full message.
- **Between Claude sessions it goes by `SendMessage`.** A Claude worker reporting to a Claude root, or anyone writing to a background worker, gets a `send_message` back from `mesh send` and passes it on with Claude Code's own cross-session messaging. Nothing is typed into the pane you work in. The root is addressed by its session's socket, which `mesh init` records, because session names change. If you reopen the root, its next mesh command records the new socket.
- **A doorbell is typed once.** herdr can miss that a busy agent queued it and report the prompt as stalled, so before retrying, `mesh` reads the pane: a doorbell that showed up above the input box counts as delivered. A pane it couldn't read, or whose input box it can't find, never confirms one.
- **A lost doorbell loses nothing.** The message waits in the mailbox, and `mesh status` shows unread counts.
- **The mailbox is the record.** `~/.local/state/agent-mesh/<run>/` holds every message, plan and report, plus a `log.jsonl` of events. `mesh stop` archives it.

### Names and workspaces

- **Readable names:** agents are named for what they do, such as `shop-web-lead` (the root) and `shop-api-restocked-at` (a worker).
- **Beside you first:** a pane worker opens as a split in the root's own tab, so you watch it work from where you are. The first one takes the right half and later ones split the workers' side, three at most (`MESH_SIDE_PANES`, `0` turns it off). A swap to the exec model reopens the worker in the same spot.
- **Then one workspace per project:** workers past that open in the project's existing herdr workspace, in one tab per run (`mesh: add restockedAt to shop-api`). A workspace is created only for a project that has none.

### Identity

`mesh spawn` injects `MESH_RUN`, `MESH_SELF`, `MESH_PARENT` and `MESH_ROLE` into each new pane through herdr, and into a background session through `claude --bg --settings` (its own process environment comes from Claude Code's daemon, not from mesh). A Claude session that inherited another worker's variables is caught by its session id (or a run that no longer exists) and ignored. The root is found by its pane id, and only the Claude process running in that pane counts: a Claude started inside another one (a `claude -p` from an agent's tool) or a background session carrying the pane's id is never taken for the pane's agent. Agents never pass ids by hand: `mesh send parent --type question --body "…"` just works.

### Watchdog

The root runs `mesh watch <worker>` in the background, or one `mesh watch --any` for every worker of the run. It exits 0 when a worker reports `done`/`failed`, and with an alert status otherwise. A Claude root whose workers report by `SendMessage` runs `mesh watch --any --alerts`, which keeps watching through reports and exits on an alert, or with 0 once every watched worker has reported and ended:

| Status | Meaning |
|---|---|
| 3 | the worker stopped without reporting, and stayed silent after one nudge |
| 4 | the worker is blocked on a dialog |
| 5 | the worker's pane is gone (or a model switch failed to reopen it) |
| 6 | timeout |
| 7 | the worker hit a quota (`mesh switch` moves it on) |

A worker counts as stopped once it has read idle for `--grace` seconds (30) with no `plan`, `question`, `done` or `failed` since its last task or answer (an `info` isn't a report); a short idle reading mid-turn doesn't count. A worker the watch never saw working (a turn shorter than one poll, a task it never picked up, a watch started after the turn ended) counts as stopped the same way once that last task or answer is 4 × `--grace` old (2 minutes, never under 30 s), measured from the log, so a late watch still judges it; once a watch has alerted on that task, a restarted watch doesn't alert on it again until the worker works or gets a new task or answer. A pane worker then gets one nudge (an `info` asking it to report with `mesh send`), once per task even with several watches running, and the watch exits 3 only if it stays silent; the line says whether the nudge's doorbell rang, and whether its task (or answer) is still unread. A background worker can't be reached from the watch, so its exit-3 line says how to nudge it. The blocked (4) and stopped (3) alerts end with the pane's last lines, so you can tell a stray menu from a real question.

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
mesh agents [query] [--run <id>]  # every past worker, live or archived, with branch, PRs and reviews
mesh peek <asker> [--follow]  # what a Copilot asker is doing: its latest tool calls and messages
mesh open <worker>         # continue a worker's conversation yourself, in this terminal
mesh approve <worker>      # (root) approve a plan; an auto worker moves to its exec model
                           #   --keep-model: execute on the current model when that move can't start
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
