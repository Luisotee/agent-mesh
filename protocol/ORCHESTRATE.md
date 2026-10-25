# agent-mesh orchestration

The shared procedure behind `/delegate-agy` and `/delegate-claude`, for changes, questions, PR reviews and whole task lists.

- **The root** is the agent running the command (typed by the user, or picked by the agent for the user's request), usually in the middle of its own work in its own repo. It keeps doing its own repo's part itself, and spawns one *worker* per **other** affected repo.
- **Each worker** loads its repo's `CLAUDE.md` / `AGENTS.md`, skills and hooks. Depending on the task's size, it either does the task directly, or researches it, sends the root a plan to review and, once the plan is approved, carries it out **itself**, with all the context its research built up.
- **Workers never spawn agents** of their own.

Read `PROTOCOL.md` as well: it applies to you too.

## Modes

| Mode | Worker | Plans on | Executes on |
|---|---|---|---|
| `agy` | `--kind agy` (also what `mesh spawn` does with no flags) | the first of Argon → Claude Opus → Flash with quota left, high effort | Flash, medium effort |
| `claude` | `--kind claude` | Opus, high effort | Opus, medium effort |

The mode sets the CLI for new workers, and the table is what a **large** task gets.

- **Models per phase:** with no `--model` (that is, `--model auto`), a large task's worker plans on a strong model. It moves to the execution phase when you approve its plan with `mesh approve`: agy moves to Flash, while Claude stays on Opus and only lowers its effort. It stays in the **same conversation**, so it keeps its research.
  - Effort follows the phase: high while planning, medium while executing (`--effort` / `--exec-effort`).
  - For agy, `mesh` reads `agy -p /usage` and skips any model with under 10% left (`MESH_MIN_QUOTA`). `mesh usage` shows the quotas and what `auto` would pick.
- **User overrides:** if the request names a model, agent or effort ("use Argon", "do it with Claude"), pass it as `--model`. That one model then runs the whole task, with no switch (default effort high). A named effort goes in `--effort` and covers planning and execution alike, with or without `--model`. Otherwise don't pass `--model`.
- **Resolution:** `mesh` resolves the agy families `argon` / `flash` / `opus` / `sonnet` to the newest matching model. `mesh model --kind agy` shows what a spawn would use, and why.

### Sizes

Size every task, and pass `--size` on every `mesh spawn` and `mesh ask`. Most tokens go to reading files and typing code, so small work runs on a cheap model, and only large work pays for a plan.

| Size | For | Claude worker | agy worker | Plan? |
|---|---|---|---|---|
| `trivial` | translations, copy, config, a version bump | Sonnet, low effort, in the background | Flash, medium | no |
| `small` | a bug fix, a UI tweak, a test, a contained refactor | Sonnet, medium effort, in the background | Flash, high | no |
| `large` (the default) | a feature, a cross-repo contract, anything you'd want to review before it's built | the mode table, in a pane | the mode table, in a pane | yes |

- **No plan** means the task goes out as a `Direct task (no plan)`: the worker makes the change, verifies it and opens the PR. If the task turns out bigger or less clear than described, it sends a `question` before changing anything.
- **When unsure, go up a size.** A wrong small task costs a redo, and a wrong large one only costs a plan review.
- **Questions:** `small`, unless answering needs research across the whole repo (then `large`, which plans on the strong model).
- **Reviews:** sized by the PR: `small` for a PR of up to a few hundred lines in a few files, `large` for anything bigger or riskier (auth, money, data migrations).
- **Plan reviewed by the user:** for a large Claude task whose plan the user wants to approve themselves, pass `--plan-review user`. The worker plans in plan mode on Opus (`opusplan`), in a pane, and the user approves it in that pane; Claude Code then executes it on Sonnet. You don't review that plan, and you don't run `mesh approve`.

### Where workers run

- **Panes:** large tasks run in herdr panes, where the user can watch them and type into them.
- **Background:** Claude workers for trivial and small tasks run as background sessions (`claude --bg`). They don't take up the screen, but they're still recorded workers, which you can ask again, resume and review like any other.
  - `mesh status` shows them, and so does Claude Code's own `claude agents` view.
  - `mesh open <name>` attaches to one from any pane.
  - `--view pane|bg` overrides the default.
- **When background falls back to a pane:** it needs a Claude root (it reports to you by `SendMessage`) and a repo Claude Code already trusts. When either is missing, the worker opens in a pane instead, and the spawn output says why in `notes`.

### Routing

The command sets the CLI. You may *propose* routing a task to the other CLI, in the triage table (see "Task lists") or in your one-line summary, when it saves the user something:

- **Under `/delegate-claude`:** a trivial or small task can go to agy Flash (`--kind agy`) when `mesh usage` shows Flash quota, keeping the user's Claude limits for the large tasks.
- **Under `/delegate-agy`:** a large task can go to Claude when `mesh usage` shows Argon and Claude Opus both under the threshold.
- **Reviews:** they go to the other vendor by default (`mesh ask --review --kind <the other one>`), since a second model catches what the first one missed.

A routing proposal is only a proposal: the user confirms the table, or you mention it in your one-line summary for a single task. A request that names an agent or model is never re-routed.

## Root

1. **Start the run:** `mesh init --mode <mode> --request "<one-line summary>"`.
   - Under Claude Code, `init` records your session's address (`address` in its output). Claude workers then report to you with `SendMessage`, so nothing is typed into the pane the user works in, and background workers become possible. If your session is reopened, your next mesh command records its new address.
   - Cross-session messages reach you without the user's approval only when your session and the worker's run in the same permission mode. Workers bypass permissions, so in any other mode the user has to approve each report. If `address` is null (an agy root), every worker uses doorbells.
2. **Find the scope:** which repos the request touches (usually under `~/projects/`). Ask the user only if that's genuinely ambiguous.
   - **Your own repo** (the one you're running in) stays **yours**: you do its part yourself, in your current checkout and branch, exactly as you would without a mesh. Its rules are already loaded in your session. Spawn a worker for it only when the user explicitly asks to delegate it too.
   - **Every other repo** gets a worker.
   - **Work an earlier worker did** (its PR got review comments, "make it Y instead of X"): resume that worker instead of spawning a fresh one. See "Follow-ups" below.
   - **A question or a PR review** ("how does shop-api do X?", "review the PR we just made") changes nothing: use `mesh ask` instead of steps 3 to 9. See "Questions and reviews" below.
   - **A list of tasks** ("work through these", a TODO list, issues): see "Task lists" below.
3. **Size and route each task** (see "Sizes" and "Routing").
4. **Spawn one worker per other repo.** Write a task file per repo in `$(mesh dir)/tasks/` and start the worker in a fresh worktree:
   ```bash
   mesh spawn --kind <kind> --size <size> --repo <path> --worktree <slug> \
     --task-file "$(mesh dir)/tasks/<repo>.md"      # plus --model/--effort only if the user named them
   ```
   The slug names both the branch (`mesh/<run>/<slug>`) and the agent (`<repo>-<slug>`), so make it short and descriptive: `fix-reactions`, not `task1`. The task file contains:
   - that repo's slice of the request
   - what you know from your own work that it needs: the contract your repo expects (field names, API shapes, error cases), with file paths in your repo it can read for reference
   - dependencies on other repos (what to wait for, and what the other workers will need from this one)
   - for a large task: "send your plan for review before changing anything"; for every task: "report `done` with the PR link"

   Tell the user, in one line per worker, its size, model and where it runs (pane or background). When the spawn output has `notes` (for example "gemini-4-argon-high has 0% left"), add that in a few words.
5. **Supervise:**
   - **How reports reach you:**
     - Claude workers under a Claude root report with `SendMessage`. Their message arrives as a cross-session message from the worker and wakes you if you're idle.
     - agy workers ring a doorbell in your pane.
     - Either way, the full text is in your mailbox: run `mesh inbox`.
   - **Claude root:** start one `mesh watch --any --alerts --timeout 7200` with `run_in_background` after spawning. It ignores reports, which reach you anyway, and exits with 0 once every worker it watches has reported and ended (nothing to do), or on an alert:
     - 3: a worker stopped without reporting;
     - 4: it's blocked on a dialog;
     - 5: it's gone;
     - 6: timeout;
     - 7: quota.

     Investigate with `mesh status`, then `herdr agent read <name>` for a pane or `claude logs <bg id>` for a background worker. Start the watch again after handling the alert, and whenever you spawn or resume a worker.
   - **Quota hit** (exit 7, `ALERT <worker> hit its quota on <model>`): run `mesh switch <worker>` right away, without asking. It reopens the worker's conversation on the next model with quota left, and tells it to continue. Then tell the user in one line, for example "Argon ran out; <worker> continues on Opus".
     - If switch says no model is left (a Flash executor with Flash exhausted), tell the user the reset time and wait.
     - A quota hit is not a `failed`.
     - If `mesh approve` or `mesh switch` itself fails to reopen the worker, the worker is marked `swap_failed`, the watch alerts (exit 5), and the same command can be run again.
   - **agy root:** rely on the doorbell, and check `mesh status` each time you wake.
   - **Keep working** on your own repo while workers run; their reports queue until your current turn ends. If your work depends on a worker's change, build against the contract you gave it, or wait for its `done` (its branch is `mesh/<run>/<slug>`) when you can't.
   - **Keep your own context lean:** you live longest, so your context costs the most. Read plans and reports, not workers' transcripts. Look at diffs only where the plan says the risk is.
6. **Review every plan** (`plan` messages; large tasks only). Read the plan file and check it against the request, the repo and the other workers' plans:
   - Does it solve what was asked, and only that? Are the files and functions real (spot-check the code)?
   - Is the *done when* a real check (tests, lint, build), not "looks right"?
   - Do cross-repo contracts match (field names, API shapes)?

   Then reply **once**:
   - **approved:** `mesh approve <worker>`, with `--body "<notes>"` if you have any. This also moves a worker on `auto` to its execution model.
   - **changes:** `mesh send <worker> --type answer --body "changes: <specific list>"`. The worker revises and sends the plan again, still on its planning model.

   Ask the user only when a plan raises a real product decision; otherwise decide yourself.
7. **Answer questions** with `mesh send <worker> --type answer`:
   - pass cross-repo facts between workers (a field name, an API shape)
   - make the call yourself whenever the request or the code settles it
   - ask the user only for real product decisions, missing credentials, or a destructive choice
8. **Check every `done`** before passing it on; a worker's word is not proof.
   - Compare the PR diff (`gh pr diff <url> --name-only` first, then the risky files) against the approved plan or the task.
   - For trivial and small tasks, a reviewer (see "Questions and reviews") checks the diff instead of you.
   - If something's wrong, `mesh send <worker> --type task --body "<specific fixes>"`.
9. **On `failed`:** don't restart the worker on another model by yourself.
   - Tell the user what blocked it and suggest a next step (a hint, a larger size, or a stronger model such as `argon` or Claude).
   - If the user agrees, retire the worker (`mesh retire <worker>`) and start the new one **in the same worktree** with `--cwd <worktree>`, giving it the plan and what failed. It gets the old name plus `-2`.
10. **Finish:** when every worker has reported and your own part is done, tell the user:
    - one line per repo with the PR link and the worker's name (so it can be resumed later), plus what you changed in your own repo (it follows that repo's usual flow, not the mesh's)
    - what's left for them to do (review, test, deploy)
    - anything that failed or was restarted

    Then run `mesh stop`. It closes the panes, stops background workers, removes clean worktrees (branches with commits are kept) and archives the run log. It never touches your own checkout. Workers can still be resumed after it.

**Messages to a worker:** always send them with `mesh send` (or `mesh approve`). If the output has a `send_message` (a background worker has no pane to ring), pass it on with the `SendMessage` tool, with `to` and `message` exactly as printed.

## Task lists

When the request is a list of tasks ("work through TODO.md", "fix these five issues"), you supervise and **write no code yourself**, in your own repo too. Each task gets its own worker in a worktree.

1. **Triage:**
   - Read each task, and just enough of the code to size it.
   - Write the board to `$(mesh dir)/board.md`, with one row per task: task, repo, size, CLI and model, pane or background, depends on, overlaps with, and later the worker, PR and status.
   - Show it to the user and get **one** confirmation, including any routing you propose. Don't start before that.
2. **Order:** tasks that touch the same files in one repo don't run side by side.
   - **Stack** the later one on the earlier one's branch (`--base mesh/<run>/<earlier slug>`; its PR then targets that branch).
   - **Or** start it after the earlier one's `done`.
   - A dependency between repos follows step 4's contract rules.
3. **Cap:** keep at most **4** workers working at once. Start the next one when one reports.
4. **Supervise** as in the Root steps. After each `done`:
   - start a reviewer for that PR (`mesh ask --repo <repo> --review <PR> --size <by PR size>`, the other vendor by default);
   - run `mesh conflicts`.
5. **Reviews:** send the findings you agree with to the author (see "Follow-ups": `mesh ask <author> …` or a `task`), not to the reviewer. A PR is done when its review has nothing left that you agree with.
6. **Conflicts:** `mesh conflicts` lists the run's branches that conflict with their default branch or with each other.
   - Nobody merges, so report a merge order that avoids them.
   - When the user says a PR was merged, send each worker whose branch now conflicts a task to rebase onto the new default branch and push.
7. **Keep the board current:** update `board.md` on every event. After your context is compacted, rebuild your picture from `board.md`, `mesh status` and `mesh inbox --all`, not from memory.
8. **Report:** the board as a table, with each task's PR, worker name, size, status and review outcome. Then the suggested merge order, and what's left for the user.

## Questions and reviews

A question about another repo, or a review of a PR, goes to a worker as well, so it can be asked again later and can take the change on itself. `mesh ask` starts or reaches the worker, gives it a read-only task and, unless the answer comes by `SendMessage`, waits for its `done` and prints it.

```bash
mesh ask --repo <path> --size small --body "<question>"       # a fresh agent in that repo
mesh ask --repo <path> --size small --review <PR link | number>  # a fresh reviewer, with the PR head checked out
mesh ask <name | branch | PR link> --body "<question>"        # a worker you already have, live or past
mesh ask <name | branch | PR link> --review <PR link>         # it reviews a PR without leaving its branch
```

- **No run needed:** without one, `ask` starts it (mode `--kind`, default agy).
- **Your own repo too:** a review of your own PR goes to a fresh agent in your repo (`--repo .`). It's a second pair of eyes that changes nothing, so it doesn't conflict with doing your repo's part yourself.
- **A fresh agent** follows the mode table and `--size`, like a spawn. Pass `--kind` for the CLI, and `--model` / `--effort` only if the user named them.
- **Asking an existing worker** reuses all its context: the agent that wrote a PR knows why it's written that way.
  - A PR link names the worker that opened the PR; a reviewer is found by its name.
  - A worker busy on another task answers once that's done: `ask` then returns at once, and the answer comes later.
  - A past worker is resumed, and its conversation is compacted first when it's large (see "Follow-ups").
- **The question:** give it what you know, as you would in a task file (use `--file` for anything long): what you're working on, what you need back, and in what form.
- **Waiting:**
  - **A Claude worker under a Claude root:** `ask` returns once the question is delivered (pass `--wait` to block anyway), and the answer arrives as the worker's `SendMessage`. If the output has a `send_message`, pass it on with `SendMessage` first.
  - **An agy worker, Claude root:** run `ask` with `run_in_background`, and the answer arrives as the notification.
  - **agy root:** run it in the foreground, or pass `--no-wait` and wait for the doorbell.
  - Exit codes are those of `mesh watch`; on 7, `mesh switch` the worker and `mesh watch` it.

Then:

1. **Relay the answer** to the user with the worker's name, so they can ask it again. Spot-check claims that matter against the code, as you would a `done`.
2. **A review:** decide on each finding (agree, or disagree and why). Fix your own repo's PR yourself. Send findings on another worker's PR, with your opinion on each, to the worker that made it (see "Follow-ups"), not to the reviewer.
3. **More questions:** `mesh ask <name> --body "…"` again. The worker still has everything it read.
4. **Turning the answer into a change** in that repo: send the same worker a normal task (`mesh send <name> --type task --file …`, or `mesh resume <name> --task-file …` once it's gone). From there it's an ordinary worker.
   - A large asker plans, you `mesh approve`, and it executes on its branch `mesh/<run>/<slug>` and opens a PR. Supervise it as in steps 5 to 10.
   - A small asker has no plan phase: make the task a `Direct task (no plan)` by starting it with that line.

`mesh stop` deletes a branch that never got a commit or a push, such as an asker's. Resuming that worker starts the branch again at the same commit.

## Follow-ups

When the request is about something a worker already built, in this run or an earlier one, resume **that worker**: it still has its research, plan and reasoning, so a fresh one would only rediscover them.

1. **Find it:** `mesh agents <PR link | repo | branch>` lists past workers with their run, branch, PRs and whether their conversation was found. With several matches, the newest is usually the one that last touched the PR.
2. **If it's still running in this run**, just `mesh send <worker> --type task --file …`.
3. **Otherwise resume it:** `mesh init` first if you have no run, then
   ```bash
   mesh resume <name | branch | PR link> --task-file "$(mesh dir)/tasks/<repo>.md"
   ```
   It reopens the worker's own conversation, on the model it ran on, in its old worktree and branch (recreated if `mesh stop` removed it), as your worker. A background worker comes back in the background. The task file contains:
   - what to change: for a review, the comments themselves (`gh pr view <url> --comments`, `gh api repos/<owner>/<repo>/pulls/<n>/comments`) and **your opinion on each one**: agree, disagree and why, or what to do instead
   - "plan first; push to the same branch so the PR updates; report `done`" (for a small follow-up: start it with `Direct task (no plan)`)
4. **Compaction:** an old conversation has lost its prompt cache, so every turn of the follow-up would pay for all of its context again. `resume` (and `ask`) compact it first, headless, keeping what the new task needs:
   - with `--compact auto` (the default): a Claude conversation over 100k tokens (200k for a read-only question; `MESH_COMPACT_AT`), or an agy one coming back from an earlier run;
   - `--compact yes|no` forces it either way;
   - `--focus "<what matters>"` replaces the default focus when you know better ("the restockedAt contract and why we chose ISO dates");
   - the output's `compaction` shows the context size before and after.
5. Supervise and review it exactly like a new worker.

A conversation can't move to another CLI. If the user wants a different agent (agy → Claude), spawn a fresh one with `--cwd <worktree>` and give it the old plan and report, as on `failed`. `mesh resume --model` changes the model within the same CLI. A resumed worker keeps one model throughout: approving its plan doesn't switch it. On a quota hit, `mesh switch` still works.

## Worker

You start inside your own worktree, on branch `mesh/<run>/<slug>`. It holds the same code as the main checkout (for a reviewer, the PR's head), so research there.

**Reporting.** Every message to your parent goes through `mesh send parent …`, which stores it. If its output has a `send_message`, your parent is a Claude session that you reach with the `SendMessage` tool (load it with ToolSearch if it isn't loaded). Send it with `to` and `message` exactly as printed, then end your turn as usual.

**Read-only tasks.** A task that starts with `Question (read-only)` or `Review (read-only)` comes from `mesh ask`. Skip the plan (steps 4 to 9): research, then answer. Change no files, commit nothing, push nothing, switch no branches, and post nothing on GitHub. Install dependencies only if you need them to answer (to run a test, for example). Report one `done`:
- **A question:** the answer, with the file paths and functions that back it, and what you're unsure of.
- **A review:** findings ranked by severity, each with file:line, why it matters and a suggested fix, then what you checked (the tests you ran, say).

Then end your turn. A later task may ask more, or ask you to make a change, which follows the steps below as usual.

**Direct tasks.** A task that starts with `Direct task (no plan)` skips steps 4 to 6: research (step 3), then execute, verify, publish and report (steps 7 to 10). If the research shows the task is bigger or less clear than it says, send a `question` with what you found and end your turn before changing anything.

**Plan reviewed by the user.** A task that starts with `Plan review by the user` runs in plan mode: write your plan as Claude Code's plan mode asks, and the user approves it in your pane. Don't send a `plan` message. Once approved, continue from step 7.

1. **Follow the repo's rules.** The repo's `CLAUDE.md` / `AGENTS.md` apply to everything repo-specific: task tracking, branch and commit conventions, contract checks, the PR process. **Never edit files or switch branches in the main checkout**, and never touch another agent's worktree.
2. **Set up:** a fresh worktree has no installed dependencies and no untracked files such as `.env`. Install what the repo needs. Don't copy secrets in unless the task says to.
3. **Research** until you know exactly what to change: read the code, run the existing tests, reproduce the bug if there is one. **Change nothing yet.**
4. **Plan:** write `$(mesh dir)/plans/<your-name>.md` from `~/.config/agent-mesh/PLAN_TEMPLATE.md`. It is written for a reviewer who hasn't read the code: say what you found and where, name the exact files and functions, the invariants, the steps, the done-when command, and your open questions.
5. **Ask for review:** `mesh send parent --type plan --file "$(mesh dir)/plans/<your-name>.md"`, then **end your turn**. Don't start implementing while you wait.
6. **On the answer:**
   - `approved` → go on.
   - `changes: …` → update the plan file, send it again as a `plan`, end your turn.
7. **Execute** the approved plan (or the direct task) yourself, in your worktree. Commit on your branch with clear messages. If the code turns out different from the plan in a way that changes its scope, stop, send a `question` and end your turn. Small corrections you can just note in the report.
8. **Verify:** run the plan's done-when command and the repo's lint, test and build. Fix what fails. After two honest attempts at the same failure, send a `question` instead of guessing.
9. **Publish:** push your branch and open the PR with `gh pr create`, following the repo's own PR rules (title, body, attribution, base branch). The PR body includes what changed, the commands that passed, and anything the reviewer should look at. Then do whatever the repo's rules require after a PR.
   - **A follow-up** to work you already published goes on the **same branch**, so the existing PR updates. Open a new PR only if the task asks for one.
   - **A stacked task** (your worktree started from another worker's branch) opens its PR against that branch.
10. **Report:** run `mesh inbox` once more, then send **one** `done` to your parent (`--file` under `$(mesh dir)/reports/`) with the PR link, a summary, the checks that passed, and any deviations from the plan. If you can't finish, send `failed` with what blocked you and what you tried. Don't run `mesh stop`: the root does that.

## Rules for everyone

- **Workers push only** their own `mesh/<run>/<slug>` branch and open PRs. Nobody merges a PR or deploys.
- **No sub-delegation:** workers never run `mesh spawn`, start `claude --bg` sessions, or spawn agents in any other way. The run is the root plus its workers.
- **Quiet supervision:** a working agent needs no check-ins. Act on reports, doorbells and watch alerts only.
- **Concise messages:** details go in `--file` reports and plans, not in chat.
