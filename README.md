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
- **No new runtime:** it's a single stdlib-Python CLI, five skills and a protocol written in Markdown. The agents do the orchestration themselves, following written procedures.

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
- **Routing:** the root may propose sending a large agy task to Claude when Argon and Opus quota run low; it proposes, you confirm. Every PR a worker opens gets reviewers without your asking (except a tiny one, a single trivial or small task of up to 150 lines touching nothing risky, which the root reviews itself): Claude and agy, or all three (Claude, agy and Copilot) for a large or risky PR. A small PR gets one review round; a large one's re-reviews after a fix go to Claude and agy only. A large task's diff is checked against its plan by the root first.
- **Reviews and fixes:** a fresh reviewer runs at high effort whatever the PR's size (a small one instead of its size's medium; a large one plans at high anyway). A real review finding, or a worker stuck on its task, goes back to the same worker; the root adds effort (retire, then resume Claude at high or agy on Argon) only when it judges the fix needs it. A stuck worker asks the root before giving up, and `failed` is only for what the run can't fix (credentials, access, the user's decision).

### Background workers

A Claude worker for a trivial or small task runs as a Claude Code background session (`claude --bg`) instead of a pane. It's still a mesh worker: you can ask it again, resume it and review its PR. Where to see it:

```bash
mesh status                # every worker, with its size and view (pane or bg)
mesh open <worker>         # attach to a running background worker from any pane
claude agents              # Claude Code's own view of background sessions
```

Background workers report to a Claude root as cross-session messages, so the root has to run in Claude Code; under an agy root they open in panes. A repo Claude Code has never been trusted in also gets a pane, and so does a fresh reviewer (`mesh ask --review`), whatever its size.

## Models

A worker on the default (`--model auto`) has two phases in one conversation:

- **Planning** runs on the first model in the chain with at least 10% quota left. For agy, `mesh` reads `agy -p /usage` (cached for 5 minutes), so while Argon is exhausted, workers plan on Claude Opus through agy. If quotas can't be read, it tries Argon.
- **Executing:** `mesh approve <worker>` waits for the worker to finish its turn. Then it reopens the worker's conversation (`agy --conversation <id>` / `claude --resume <id>`) on Flash (agy) or still on Opus (Claude) in a new pane, and delivers the approval. Effort drops from high (planning) to medium (executing); `--effort` / `--exec-effort` on `mesh spawn` change either. If the reopen can't start (the conversation isn't found, or the worker stays busy), approve sends nothing and suggests `mesh approve <worker> --keep-model`, which executes on the planning model. A Claude worker plans with file edits and `git push` denied (its shell still runs set-up and tests), and the reopen is what lifts that, so approve reopens it even with `--keep-model`; agy is held by the prompt.
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
/delegate-review                  # the current branch's PR: Claude + agy, or all three if large
/delegate-review  copilot 88      # PR #88, reviewed by Copilot
/delegate-review  copilot agy     # the current PR, by Copilot and agy at the same time
/delegate-review  all 88          # PR #88, by Claude, agy and Copilot
```

`/delegate-review` checks every finding against the code before it shows you, merges the ones several reviewers found (saying who found each), gives its verdict on each, and fixes nothing unless you ask. Reviewers mark each finding blocking (with the input that makes it fail) or advisory, and every review names the commit it read, so a reviewer asked again after a fix reviews what changed since. A Claude reviewer runs Claude Code's `/code-review` skill as the core of its review and checks each of its findings before reporting it. A run's own PRs get one review round when small, whose fix the root checks itself, and at most two review→fix rounds when large; what's still open after that comes to you.

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

### Standing peer askers

Some repos are worth asking again and again: projects similar to yours that you cloned to learn from, say. `mesh peers` keeps a machine-wide list of them, each with one research asker whose conversation outlives every run:

```bash
mesh peers add https://github.com/acme/swarm --group orchestrators   # clone into ~/research/orchestrators/swarm
mesh ask swarm-research --size large --file brief.md                 # its first research brief
mesh peers                                                           # what each is, what it last read, its size
mesh peers edit swarm-research --about "A Rust daemon…; ask about delivery"   # whom to ask about what
mesh ask swarm-research --body "How do they merge?"                  # one of them
mesh ask --peers orchestrators --file question.md                    # all of a group (or all, or a,b,c) at once
mesh peers refresh                                                   # what changed upstream, without asking
```

- **Current:** before each question mesh fetches the project, moves the asker's checkout to the new upstream head and tells it what changed since it last looked (the commits, capped), so its answer accounts for them.
- **One result:** `mesh ask --peers` asks them all at once and blocks until every one has answered. It writes their answers into one file (`$(mesh dir)/reports/peers-<time>.md`) and prints a line per peer and the file's path, which `mesh peers` keeps showing as the newest answers. Run it in the background. A peer stuck at a startup screen mesh doesn't answer is given up on after 5 minutes (`MESH_HELD_WAIT`, seconds), with its line saying how to clear it; its answer then comes to your inbox.
- **Read-only for good:** a peer asker only answers. A change to that project goes to an ordinary worker there. Its checkout stays in place between runs, and its conversation is compacted over 300k tokens to what it learned about the project.
- **Someone else's settings:** a peer's repo isn't yours to fix, so when its `.claude/settings.json` has a rule Claude Code rejects, mesh takes the warning's default "Continue", which skips just those rules. Your own repos still stop there for you.
- **What each one is:** every peer and group has an `about` (at most 400 characters), which `mesh peers` shows and the answers file repeats. Write what the project is and what it's worth asking about. A new peer starts from its GitHub description; `mesh peers edit <peer | group> --about …` replaces it.
- **Finding them:** `/find-peers` (or just ask for projects like yours to learn from) characterizes your repo's core mechanisms, searches GitHub, awesome lists, plugin directories and the web, ranks what it finds by shared mechanisms rather than stars, and shows you a shortlist. On your yes it runs `mesh peers add` for each, with an `about`, and stops there. The search and ranking work in any repo.
- **Harvesting them:** `/harvest-peers` (or ask for ideas or likely bugs from the peers) runs a research round: a first brief for peers that never researched anything, else a follow-up with what changed here since and 3 to 5 of your riskiest mechanisms for them to compare against theirs. It checks every finding against your code before showing it (real bug or gap, already tracked, already rejected, wrong), ranks them as bugs, improvements and features, and on your yes adds them to your tracker. You decide when to run it; `/find-peers` never does.
- `mesh peers add <path>` registers a clone you already have. It adopts the asker that already researched it when one has the default name (`<repo>-research`, or `--name`). `mesh peers remove <name>` drops a peer and keeps its clone.

## Task lists

Hand the root a whole list, and it supervises instead of coding:

```
/delegate-claude work through TODO.md: the translation fix, the settings UI tweak, and the two export features
```

1. **Triage:** the root sorts the tasks into a few themed groups, writes a board (`$(mesh dir)/board.md`), and asks you to confirm it once.
2. **Workers:** one per group, in its own worktree, with at most four working at a time. A group is tasks that share a theme, or where one task's context helps another; small unrelated ones join a group rather than get a worker. There's no size cap: workers run as strong a model as the root, so one takes on a whole batch. Its worker sends **one plan** for all its tasks, then does them in order, a commit each, and opens **one PR**. New work it proposes, and small follow-ups, go back to it rather than to a fresh agent. The root writes no code itself, in its own repo too.
3. **Reviewers per PR:** each PR gets two or three read-only reviews by default, as every PR a run makes does: Claude and agy, or all three (Claude, agy and Copilot) for a large or risky PR. A tiny PR, a single trivial or small task's PR of up to 150 changed lines touching nothing risky, gets the root's own review instead. A small PR gets no re-review: the root checks its fix itself. A large one's re-review after a fix goes to Claude and agy only. The findings go back to the worker that wrote it, as claims to check: the root and the author each confirm a bug is real and pick the best fix before changing anything.
4. **Conflicts:** `mesh conflicts` checks the run's branches against the default branch and against each other, without touching any checkout. It also notes when two workers change the same files, which needs nothing by itself: a conflict is rebased when the PRs merge. The root suggests a merge order, and when you merge a PR it asks the affected workers to rebase.
5. **Report:** the board as a table, with the PRs, worker names, statuses and the merge order.

## Follow-ups: resuming a worker

A worker's conversation outlives its run. When the PR gets review comments, or you change your mind about the feature, send the work back to **the agent that did it**, with all its research still in context, instead of briefing a fresh one:

```
/delegate-agy send the review on shop-api#88 and your opinion on it to the agent that made that PR
/delegate-agy ask the shop-api worker to return restockedAt as a unix timestamp instead of ISO
```

The root finds the worker (`mesh agents`) and runs `mesh resume`. This reopens the worker's own Claude or agy conversation in a new pane, in its old worktree and branch (recreated if `mesh stop` removed it). The worker plans the change on the planning model (the best with quota, as for a new worker, whatever it last ran on), gets it approved (which moves it to the execution model and effort, as for a new worker), and pushes to the **same** branch, so the existing PR updates. If that PR was merged or closed meanwhile (`gh pr list --head <branch>`), a change task starts a new branch, `mesh/<run>/<slug>`, in the same worktree, and opens a new PR; the resume output and the task say so. After a merge the branch starts from the freshly fetched default branch (a worktree with uncommitted changes stays put and the task tells the worker instead); after a close, from the old branch's tip (or the PR's head, fetched from GitHub, when the branch lost it), so its commits come along. When `gh` can't answer, it resumes on the old branch with a note.

To carry on with a worker yourself, open its conversation in your own terminal:

```bash
mesh agents shop-api                  # past workers: run, branch, PRs, whether the conversation was found
mesh open shop-api-restocked-at       # cd into its worktree and resume its conversation here
mesh open shop-api-restocked-at -- --dangerously-skip-permissions   # extra flags go to the CLI
```

`mesh open` runs outside the mesh, with your usual permission settings. Both commands take the worker's name, its branch (`mesh/<run>/<slug>`) or a PR link. A PR link names the worker whose branch is the PR's head (asked of `gh`), or else the one whose reports quote the link. `resume` refuses a worker that's still running. `open` refuses a running pane worker (use `herdr agent focus <name>`) and attaches to a running background worker.

### Compaction on the way back

An old conversation has lost its prompt cache, so every turn of a follow-up would pay for all of it again. Before reopening a worker, `resume` and `ask` compact its conversation headless when that pays off:

- a Claude conversation over 300k tokens, or an agy one over 100k (agy compacts itself at about 250k); twice that for a read-only question. An agy conversation whose size can't be read is compacted when it comes back from an earlier run;
- the default focus is what the new task needs; the root can give its own (`--focus "the restockedAt contract"`);
- `--compact yes|no` forces it either way, and `MESH_COMPACT_AT` / `MESH_AGY_COMPACT_AT` move the thresholds.

### The root's own context

The root lives longest, and every turn re-reads its whole context: a twentieth of the input price while the prompt cache is warm, the cache-write price (2× the input price on the 1 h cache, 1.25× on the 5-minute one) for all but the system prompt's ~25k (cached across sessions) once the cache has expired (an hour on a Claude subscription within its plan usage, five minutes on usage credits or an API key; mesh reads which from the root's session file). So mesh measures the root too, and the root resets itself, without asking anyone:

- root commands print a `mesh: root context 452k, cache cold since 14:02 …` line when a reset pays: over 300k tokens (`MESH_ROOT_RESET_AT`) with a cold cache, or when the root is about to wait on workers; over twice that while warm. `mesh status` shows the size and cache state in its header;
- the root writes what the files can't hold to `$(mesh dir)/notes.md` (the user's standing instructions, decisions and why, what's next), runs `mesh reset --next "<what next>"` and ends its turn. A detached process waits until the turn has ended and the input box is empty (never over a draft), types `/clear` (or `/compact <focus>` with `--compact`) into the root's own pane, then a prompt that sends the fresh session to `mesh brief`;
- `mesh brief` is one bounded view of the run for a fresh root: request, notes, board, each worker's state, what waits on it, PRs, unread mail headers and the old transcript's path, each part capped with a command for the rest;
- with `MESH_ROOT_AUTORESET=1`, the root's own `mesh watch` resets an idle root over the threshold once its cache is cold.

## How it works

### Mailbox plus doorbell

Typing into a terminal is not reliable delivery: the agent might be busy, stopped at a dialog, or you might be typing in that pane. So every message is **written to the recipient's mailbox first**, and only then does the recipient get a one-line **doorbell**:

```
📬 [mesh] question from shop-api-restocked-at (ref 3fa9c1): Should restockedAt be null for products that were never out of stock? (run `mesh inbox`)
```

- **The doorbell wakes the agent.** An idle agent starts a new turn; a busy one queues it, which both CLIs do. The agent then runs `mesh inbox` to read the full message.
- **A Claude recipient's doorbell goes straight into its session.** `mesh` writes it into the session's cross-session inbox (its socket), from any sender, agy included, so nothing is typed into its pane. Workers are started to accept these messages. A Claude root takes them under a `crossSessionInbound: accept` setting or in a mode that asks for permissions; in bypass mode it takes only a message from a session in its own mode, so a Claude worker gets a `send_message` back from `mesh send` and passes it on with `SendMessage`, and an agy worker's doorbell is typed. `mesh init` and `mesh status` warn when your settings hold such messages. The root is addressed by its session's socket, which `mesh init` records, because session names change. If you reopen the root, its next mesh command records the new socket.
- **A doorbell is typed once.** herdr can miss that a busy agent queued it and report the prompt as stalled, so before retrying, `mesh` reads the pane: a doorbell whose `ref` showed up above the input box counts as delivered. The `ref` is new for every doorbell, so an older one never confirms it. A pane it couldn't read, or whose input box it can't find, never confirms one. Retrying also stops once the recipient has read the message, and every doorbell that got through is logged (`rung`, with how it was confirmed).
- **Your draft is never sent for you.** herdr submits a doorbell fused to whatever is in the input box, so `mesh` doesn't type while you have a half-written prompt in an agent's pane: it waits through its retries (about a minute) and then gives up with exit 2 (`human_draft`), the message still in the mailbox. When the box can't be read, it types anyway. One doorbell at a time goes into a pane, since herdr merges prompts typed at once.
- **The preview is plain text.** Backticks, `$` and quotes are dropped from the message preview, so it never reads as a shell command.
- **A lost doorbell loses nothing.** The message waits in the mailbox, and `mesh status` shows unread counts and a task whose doorbell failed.
- **The mailbox is the record.** `~/.local/state/agent-mesh/<run>/` holds every message, plan and report, plus a `log.jsonl` of events. `mesh stop` archives it.

### Names and workspaces

- **Readable names:** agents are named for what they do, such as `shop-web-lead` (the root) and `shop-api-restocked-at` (a worker).
- **Beside you first:** a pane worker opens as a split in the root's own tab, so you watch it work from where you are. The first one takes the right half and later ones split the workers' side, three at most (`MESH_SIDE_PANES`, `0` turns it off). A swap to the exec model reopens the worker in the same spot.
- **Then one workspace per project:** workers past that open in the project's existing herdr workspace, in one tab per run (`mesh: add restockedAt to shop-api`). A workspace is created only for a project that has none.

### Identity

`mesh spawn` injects `MESH_RUN`, `MESH_SELF`, `MESH_PARENT` and `MESH_ROLE` into each new pane through herdr, and into a background session through `claude --bg --settings` (its own process environment comes from Claude Code's daemon, not from mesh). Every Claude worker, pane or background, also gets two hooks through `--settings`, added to your own and the repo's: a `Stop` hook (`mesh hook stop`, below) and a `SessionStart` hook that, after a `/compact`, a `/clear` or a resume, restates its name, parent, phase and current task (`mesh whoami --reminder`); after a clear or a compaction, its next `mesh inbox` shows its current task again. A Claude session that inherited another worker's variables is caught by its session id (or a run that no longer exists) and ignored. The root is found by its pane id, and only the Claude process running in that pane counts: a Claude started inside another one (a `claude -p` from an agent's tool) or a background session carrying the pane's id is never taken for the pane's agent. "Running in that pane" means leading the pane's foreground process group: `claude`, `time claude`, `env … claude` and `direnv exec . claude` all do, but a launcher script that starts `claude` without `exec`, or tmux/zellij inside the pane, puts another process there, and the root is refused. Use `exec claude "$@"` in the script; inside a multiplexer, pass `--run <run> --as <name>`. A Claude pane worker that herdr restored without its variables (after a power outage, say) is found the same way: by its pane, its recorded session and its process leading the pane. Such a restore also drops mesh's flags (its hooks, a planner's or asker's deny list, its model and effort), so the next message mesh sends it first reopens it in the same conversation with them, once its turn ends; until then the message is stored, not rung. Agents never pass ids by hand: `mesh send parent --type question --body "…"` just works.

### Watchdog

The root runs `mesh watch <worker>` in the background, or one `mesh watch --any` for every worker of the run. It exits 0 when a worker reports `done`/`failed`, and with an alert status otherwise; a `plan` or `question` reaches you by its doorbell or `SendMessage`, and the watch keeps going. A Claude root whose workers report by `SendMessage` runs `mesh watch --any --alerts`, which keeps watching through reports and exits on an alert, or with 0 once every watched worker has reported and ended. An `--any` watch also takes on workers spawned or resumed after it started, so it needn't be restarted for them. A worker you `mesh retire` while a watch runs is let go, report or not, rather than reported gone. The watch also retires a worker itself once it has been idle 15 minutes after its `done` or `failed` (`MESH_IDLE_RETIRE`, seconds; 0 = never), since an idle session holds memory and buys nothing over a resume; `mesh send` or `mesh ask` to a retired worker resumes it in the same conversation, then delivers. And and `mesh stop` ends a running watch with 0 (`run <run> stopped`) from the moment it starts closing panes; a `mesh ask` whose run is stopped before the answer exits 1. A report from a watched worker that's still unread in your mailbox (sent while no watch ran, its doorbell lost) makes the watch exit 0 at once:

| Status | Meaning |
|---|---|
| 3 | the worker stopped without reporting, and stayed silent after one nudge |
| 4 | the worker is blocked on a dialog |
| 5 | the worker's pane is gone, its CLI has exited (or a model switch failed to reopen it) |
| 6 | timeout |
| 7 | the worker hit a quota (`mesh switch` moves it on), or a Claude worker hit its usage limit (wait for the reset) or its login expired (the user runs `/login`) |
| 8 | the worker may be hung: it reads working, but its transcript hasn't changed for 40 minutes |

A worker counts as stopped once it has read idle for `--grace` seconds (30) with no `plan`, `question`, `done` or `failed` since its last task or answer (an `info` isn't a report); a short idle reading mid-turn doesn't count. A worker the watch never saw working (a turn shorter than one poll, a task it never picked up, a watch started after the turn ended) counts as stopped the same way once that last task or answer is 4 × `--grace` old (2 minutes, never under 30 s), measured from the log, so a late watch still judges it; once a watch has alerted on that task, a restarted watch doesn't alert on it again until the worker works or gets a new task or answer. A pane worker then gets one nudge (an `info` asking it to report with `mesh send`), once per task even with several watches running, and the watch exits 3 only if it stays silent; the line says whether the nudge's doorbell rang, and whether its task (or answer) is still unread. A Claude worker usually gets that nudge sooner, from its own `Stop` hook: when its turn is about to end with unread mail, or with no report since its last task or answer, the hook keeps it going once with the reminder, and that counts as the task's nudge, so the watch doesn't nudge it again. That covers background workers too, which the watch can't reach: when a background worker's hook didn't nudge it, its exit-3 line says how to nudge it yourself. The blocked (4), stopped (3) and hung (8) alerts end with the pane's last lines, so you can tell a stray menu from a real question, and the stopped (3) and gone (5) ones say what the worker's branch shows: commits past its base, pushed or not, an open PR, a dirty worktree.

A worker that ends its turn to wait on a long command it started (both CLIs move a long test run to the background and end the turn) declares it with `mesh send parent --type wait --until 30m --body "<why>"`. It's stored for the root without a doorbell, and until then the watch neither nudges it nor raises 3 or 8, and its Stop hook lets the turn end. A wait covers that one turn end: a worker that wakes early and stops again without reporting is reminded once, then judged as usual. After the deadline the usual rules apply. Each running watch holds a lock per worker it watches, so `spawn`, `resume`, `approve`, `switch` and `ask` warn when a worker that owes a report has no watch.

A Claude worker at its usage limit is told by the pane's last 20 lines or by its session file's last reply (a background worker has only that one), and gets exit 7 with the reset time instead of a nudge, a 3 or a 4; there's no model to move it to, so the root waits for the reset and sends it a task to continue. An expired login (`Login expired · Please run /login`) is read the same two ways (a readable session file decides the login, and the limit once it records one, so a line left on the screen after a new login or the reset doesn't count) and gets its own exit 7: the root asks the user to run `/login` there, then sends the task to continue. A worker that reads working, with nothing reported since its last task or answer, while its transcript (Claude's session file; agy's conversation file and log) stays unchanged for `--hang` seconds (`MESH_HANG_AFTER`, 2400; 0 turns it off) gets exit 8: a hung test, a server in watch mode, or a registration herdr kept after the agent died (then it's exit 5, once the pane shows only a shell). The watch never stops it. Exits 7 and 8, like 3, come once per task (a limit and an expired login once each). herdr can also lose a live agent (seen after a Claude Code auto-update): a worker herdr doesn't list counts as gone only when its pane no longer runs its `claude`/`agy`; otherwise it reads `unknown`.

A Claude root gets notified the moment the watch exits.

### Many agents on one machine

Every agent is a whole CLI process with its own MCP servers. On 2026-10-27 two full runs filled a WSL VM's 15.5 GB of RAM and its 4 GB of swap and it hung, with no OOM kill. Measured idle afterwards, 22 sessions held 8.4 GB: `claude` about 195 MB each, the MCP servers each session starts (playwright ~83 MB, claude-code-explorer ~36 MB, shadcn ~76 MB) another 38 %, herdr and the pane shells ~0.3 GB; workers had also left four `next dev`/`next start` servers running (~1.5 GB). A background worker is the same process with the same servers, so fewer panes alone saves little.

- **Swap and an OOM killer.** Give the machine enough swap to absorb a spike, and run [earlyoom](https://github.com/rfjakob/earlyoom) (or systemd-oomd) so a full machine kills something instead of hanging. Tell it what to sacrifice and what to spare, e.g. `--prefer '(^|/)(next-server|node|pnpm)' --avoid '(^|/)(herdr|claude|agy)$'`.
- **One MCP server per machine, not per session.** A stdio MCP server is started by every session that has it configured, so its memory multiplies by the agents running. Run a server that can be shared once, as a user service listening on a port (Playwright's MCP server takes `--port`), and point the user-scope entry at its URL; scope the others to the projects that use them (a project `.mcp.json`, or `claude mcp add --scope project` / `--scope local`) instead of the user scope. mesh launches workers with their repo's own MCP configuration and doesn't trim it (see [design notes](docs/design.md), §17).
- **Retire what has reported.** An idle worker holds its whole process; its conversation stays on disk, and `mesh resume` (or `mesh ask`) reopens it. `mesh status` shows who's done.
- **What mesh does itself:** `spawn`, `resume` and `ask` refuse to start an agent under a memory floor, and `retire` and `stop` stop what a worker left running in its worktree (both under Safety rails below). Workers are asked to stop their own servers before reporting.

### Safety rails

- **Branches:** workers change code only in their own worktree, on `mesh/<run>/<slug>`, and never touch the main checkout.
- **Plans first:** a worker changes nothing until the root approves its plan.
- **Publishing:** workers push only their own run branch and open a PR. Nobody merges or deploys.
- **No sub-delegation:** workers never spawn agents, background or headless.
- **Authority:** mesh messages are not user authority. Text you type directly into a pane overrides the agent's parent.
- **Startup:** `mesh spawn` accepts a folder-trust dialog in directories it created itself (moving to "Yes" and reading the screen again before Enter, since Claude Code's default is "No, exit"), and answers no other startup screen. A worker that waits at one (Claude Code's Settings Warning for a malformed `.claude/settings.json` rule, its external CLAUDE.md imports question, or a screen mesh doesn't know) gets its task stored, not rung: `spawn`, `ask`, `resume`, `approve` and `switch` exit 4 with an `ALERT` and the pane's last lines, and once the user clears the screen a running `mesh watch` (or `mesh ring <worker>`) rings its doorbell. Messages sent to it meanwhile are stored, not typed. A CLI that exits during startup is an error, with the pane's last lines.
- **Cleanup:** `mesh stop` stops background workers (their conversations are kept), removes only *clean* worktrees and never touches your own checkout. `mesh retire` and `mesh stop` also stop what a worker left running in its worktree (a dev server, a watcher: a process there whose parent has died), TERM then KILL, logged as `killed`; a live pane's shell, a live agent and its MCP servers, and your own terminal or tmux there (anything holding a terminal) are left alone, and so is a worktree another agent still works in. It keeps every branch that has a commit or was pushed; an asker's branch that got neither is deleted, and resuming that worker starts it again.
- **Memory floor:** `spawn`, `resume` and `ask` (each peer of `ask --peers` too) start no agent while the machine's available memory plus free swap is under a floor: by default 10 % of RAM plus 10 % of swap (where earlyoom's defaults start killing) plus 1 GB, at least 2 GB; `MESH_MIN_FREE_MB` sets it, 0 turns it off. An agent started in the last two minutes, by any `mesh`, counts as 400 MB used, since it hasn't grown yet. They refuse before the agent's worktree or entry is made, naming the worker, and say to retire workers that have reported. `mesh open`, typed by you, isn't guarded. See [Many agents on one machine](#many-agents-on-one-machine).
- **One process per conversation:** `mesh resume` and `mesh open` refuse a worker that's still running, so a conversation is never open twice. "Running" means herdr lists an agent in its worktree, or, should herdr have lost it, a `claude`/`agy` process works there (in a shared main checkout, one naming its session).

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
mesh status <run>          # every agent: model, size, view, live state, unread mail, branch, and what it
                           #   owes or waits on (plan unanswered 12m, task unread, done + PR state), needing-you first
mesh log [<run>] [--follow]  # the run's events as a timeline: sends, nudges, alerts, swaps
mesh inbox                 # (as an agent) read your messages
mesh stop <run>            # close panes, remove clean worktrees and empty branches, archive the run
mesh ask <worker> --body …  # ask a worker (or --repo <path> for a fresh one) and wait for the answer
mesh agents [query] [--run <id>]  # every past worker, live or archived, with branch, PRs and reviews
mesh peek <asker> [--follow]  # what a Copilot asker is doing: its latest tool calls and messages
mesh open <worker>         # continue a worker's conversation yourself, in this terminal
mesh approve <worker>      # (root) approve the plan it sent; an auto worker moves to its exec model
                           #   --keep-model: execute on the current model when that move can't start
mesh switch <worker>       # (root) move a worker off a model that hit its quota
                           #   approve, switch, retire and stop refuse a worker (the user: --run <id> --as <root>)
mesh usage                 # agy quotas and the models auto would pick
mesh conflicts             # which run branches conflict with the default branch or each other,
                           #   and which workers change the same files (uncommitted too)
mesh watch --any [--alerts]  # one watchdog for every worker of the run
mesh brief                 # (root) one bounded view of the run: what a fresh session needs
mesh reset [--next …] [--compact [focus]]  # (root) once your turn ends, clear your own pane and resume from mesh brief
```

## The protocol in one screen

- `📬 [mesh]` means you have mail: run `mesh inbox`.
- **Plan first:** a worker on a large task sends `mesh send parent --type plan --file …` and changes nothing until it's `approved` (a Claude worker can't: the asker's deny list holds it until `mesh approve`). Before sending it, the worker runs the `check-plan` skill, which checks the plan's claims in the code, attacks it and fixes it. A `Direct task (no plan)` goes straight to the change.
- **`send_message`:** when `mesh send` prints one (a report to a root in bypass mode, or a message to a background worker launched before workers accepted cross-session messages, until it's retired and resumed), pass it on with `SendMessage`, exactly as printed.
- **Read-only tasks:** a `Question (read-only)` or `Review (read-only)` task gets no plan and no changes, only one `done` with the answer. A Claude asker also runs with a deny list (no edits to the repo, no push, no PR comments or reviews), lifted by reopening its conversation when it's given a direct change (a change it plans keeps the list until `mesh approve`); agy has no deny list, so its askers are held by the prompt. Any worker is refused every `mesh` command but its own (`send` to its parent, `inbox`, `whoami`, `dir`, the readings).
- **Stuck or unsure?** `mesh send parent --type question`, then **end your turn**. No polling.
- **Finishing:** every task ends with exactly one `done` or `failed`. Empty your mailbox before reporting: `mesh send` refuses a report while a task or answer is unread.
- **Long text** goes in `--file` reports under the run directory, never inside the repo.
- **Repo rules still apply:** the repo's own `CLAUDE.md` / `AGENTS.md` always apply. The mesh never names a tracker or a repo-specific process.

Full text: [`protocol/PROTOCOL.md`](protocol/PROTOCOL.md). The procedure for roots and workers is in [`protocol/ORCHESTRATE.md`](protocol/ORCHESTRATE.md), and plans follow [`protocol/PLAN_TEMPLATE.md`](protocol/PLAN_TEMPLATE.md).

## Design notes

Why a mailbox instead of an MCP server, why not Claude Code's native agent teams, why interactive panes instead of `-p`, which native Claude Code pieces mesh uses since 0.7 and why it compacts on resume: see [`docs/design.md`](docs/design.md).

## Development

```bash
uvx pytest -q tests       # herdr is stubbed; git worktree tests use real temp repos
```
