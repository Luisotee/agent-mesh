# agent-mesh orchestration

The shared procedure behind `/delegate-agy` and `/delegate-claude`, for changes, questions, PR reviews and whole task lists. `/delegate-review` uses only its "Questions and reviews" section.

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
  - **Fable** (`--model fable`, Claude only) runs the whole task on Claude Fable 5.1, Anthropic's strongest model, which pays off on ambiguous root-cause hunts and architecture decisions. Use it only when the user names it: it draws on a capped share of the user's Claude limits.
- **Resolution:** `mesh` resolves the agy families `argon` / `flash` / `opus` / `sonnet` to the newest matching model. `mesh model --kind agy` shows what a spawn would use, and why.

### Sizes

Size every task, and pass `--size` on every `mesh spawn` and `mesh ask`. Only large work pays for a plan; the rest goes out as a direct task. Claude workers run Opus at every size, and agy saves its quota by running small work on Flash.

| Size | For | Claude worker | agy worker | Plan? |
|---|---|---|---|---|
| `trivial` | translations, copy, config, a version bump | Opus, medium effort, in the background | Flash, low | no |
| `small` | a bug fix, a UI tweak, a test, a contained refactor | Opus, medium effort, in the background | Flash, high | no |
| `large` (the default) | a feature, a cross-repo contract, anything you'd want to review before it's built | the mode table, in a pane | the mode table, in a pane | yes |

- **No plan** means the task goes out as a `Direct task (no plan)`: the worker makes the change, verifies it and opens the PR. If the task turns out bigger or less clear than described, it sends a `question` before changing anything.
- **When unsure, go up a size.** A wrong small task costs a redo, and a wrong large one only costs a plan review.
- **Questions:** `small`, unless answering needs research across the whole repo (then `large`, which plans on the strong model).
- **Reviews:** sized by the PR: `small` for a PR of up to a few hundred lines in a few files, `large` for anything bigger or riskier (auth, money, data migrations). A fresh reviewer runs at high effort whatever its size: checking is its whole job.
- **Escalate, don't start high:** when a review finding turns out real, or a worker reports `failed`, give the fix more effort rather than starting every task high: resume the worker for the fix with more effort. A live worker keeps the effort it has (`mesh send` changes neither model nor effort), so close it first with `mesh retire <name>` (not `mesh stop`, which ends the whole run), then resume it:
  - **Claude:** `mesh resume <name> --effort high …`, which runs the whole fix on Opus at high.
  - **agy:** `mesh resume <name> --model argon …` (or `opus` when Argon is out of quota). A named `--effort` alone keeps it on the family it last ran, which after execution is Flash.
  - A bug whose cause is still unknown is `large`.
- **Plan reviewed by the user:** for a large Claude task whose plan the user wants to approve themselves, pass `--plan-review user`. The worker plans in plan mode on Opus, in a pane, and the user approves it in that pane; Claude Code then executes it on Opus, in the same conversation. This task has no execution phase, so it runs at high effort throughout, unlike the medium a root-approved plan executes at. You don't review that plan, and you don't run `mesh approve`.

### Where workers run

- **Panes:** large tasks run in herdr panes, where the user can watch them and type into them.
- **Background:** Claude workers for trivial and small tasks run as background sessions (`claude --bg`). They don't take up the screen, but they're still recorded workers, which you can ask again, resume and review like any other.
  - `mesh status` shows them, and so does Claude Code's own `claude agents` view.
  - `mesh open <name>` attaches to one from any pane.
  - `--view pane|bg` overrides the default.
  - A fresh reviewer (`mesh ask --review`) gets a pane at any size, so the user sees the review.
- **When background falls back to a pane:** it needs a Claude root (it reports to you by `SendMessage`) and a repo Claude Code already trusts. When either is missing, the worker opens in a pane instead, and the spawn output says why in `notes`.

### Routing

The command sets the CLI. You may *propose* routing a task to the other CLI, in the triage table (see "Task lists") or in your one-line summary, when it saves the user something:

- **Under `/delegate-agy`:** a large task can go to Claude when `mesh usage` shows Argon and Claude Opus both under the threshold.
- **Reviews:** each PR gets two reviewers by default, run side by side, since a second model catches what the first one missed. Which two follows the review's size ("Sizes"): a `small` review gets Claude and agy (`--kind claude`, `--kind agy`), and a `large` one (a big PR, or a risky one: auth, money, data migrations) gets Claude and Copilot (`--kind claude`, `--kind copilot`), since Copilot's credits are kept for the PRs that matter. Give each its own name (`--name review-<n>-<kind>`) so their worktrees don't collide. A request that names reviewers gets those instead. The pair is a default, not a proposal: it needs no confirmation.

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
   - that repo's slice of the request, with the user's own words quoted where the wording matters
   - what you know from your own work that it needs: the contract your repo expects (field names, API shapes, error cases), with file paths in your repo it can read for reference
   - dependencies on other repos (what to wait for, and what the other workers will need from this one)
   - what you know about that repo and what you only assume, with the assumptions marked so the worker checks them
   - what is out of scope: files, behaviour or other repos it should leave alone
   - for a large task: "send your plan for review before changing anything"; for every task: "report `done` with the PR link"

   **Exit 4** from `spawn` (or `ask`, `resume`, `approve`, `switch`): the worker waits at a startup screen mesh doesn't answer (the `ALERT` names it and ends with the pane's last lines, e.g. a Settings Warning for a malformed `.claude/settings.json` rule). Its message is stored. Tell the user to clear it in that pane; don't answer it yourself. Start the watch as usual: it rings the doorbell once the screen is gone, and doesn't alert on that screen again (an agy root runs `mesh ring <worker>` once the user says it's cleared). After `ask`, `mesh watch <worker>` rings it and waits for the answer.

   Tell the user, in one line per worker, its size, model and where it runs (pane or background). When the spawn output has `notes` (for example "gemini-4-argon-high has 0% left"), add that in a few words.
5. **Supervise:**
   - **How reports reach you:**
     - Claude workers under a Claude root report with `SendMessage`. Their message arrives as a cross-session message from the worker and wakes you if you're idle.
     - agy workers ring a doorbell in your pane.
     - Either way, the full text is in your mailbox: run `mesh inbox`.
   - **Claude root:** start one `mesh watch --any --alerts --timeout 7200` with `run_in_background` after spawning. It ignores reports, which reach you anyway, and exits with 0 once every worker it watches has reported and ended or been retired (nothing to do), with 0 at once when a watched worker's `plan`, `question`, `done` or `failed` is still unread in your mailbox (its doorbell was lost: read it with `mesh inbox`), or on an alert:
     - 3: a worker stopped without reporting (a `plan`, `question`, `done` or `failed`), and stayed silent after the one nudge the watch gave it ("its nudge was not delivered" if that doorbell didn't ring, "its task is still unread" or "its answer is still unread" if it never picked it up; a worker that was never seen working is judged 4 × `--grace` after its last task or answer) (a Claude worker's own Stop hook usually gave that nudge, the line says so; a background worker whose hook didn't gets a line with the `mesh send … --type info` to nudge it yourself, then pass its `send_message` on);
     - 4: it's blocked on a dialog (the alert ends with the pane's last lines: a stray menu, or a real question to answer);
     - 5: it's gone (herdr lists no agent and its pane runs no `claude`/`agy`, or herdr still reads it working over a bare shell);
     - 6: timeout;
     - 7: quota, or a Claude worker at its usage limit;
     - 8: it may be hung: it reads working, but its transcript hasn't changed for 40 minutes (`--hang`, `MESH_HANG_AFTER`; a test or a server in watch mode that never returns). Look at its pane; tell the user, and leave it running unless it is plainly stuck. The watch never stops it, and raises this once per task.

     A worker that ended its turn to wait on a long command it started (a test suite) may declare it: a `wait` message in your mailbox (no doorbell), `Waiting until <time>: <why>`. Until that time the watch doesn't nudge it or raise 3 or 8 for that task (a usage limit is still 7); after it, the usual rules apply, and the exit-3 line says the wait ended. A new task or answer, or any report, ends a wait. A `wait` is not a report: don't answer it. Exits 3 and 5 also say what the worker's branch shows (`its work: 2 commit(s) past its base, pushed, PR #31 open, worktree dirty`), so you can tell a worker that finished and didn't say so from one that never started. `spawn`, `resume`, `approve`, `switch` and `ask` warn on stderr when a worker that owes a report has no `mesh watch` covering it (a running `--any` watch doesn't pick up workers spawned after it): start one. A worker that looks gone gets its own line (`mesh watch <name>`, which exits 5, or resume or retire it), and none once a watch has raised it as gone for that task.

     Investigate with `mesh status` (and `mesh log --agent <name>` for what happened in order), then `herdr agent read <name>` for a pane or `claude logs <bg id>` for a background worker. Start the watch again after handling the alert, and whenever you spawn or resume a worker. A restarted watch doesn't raise exit 3 again for a task already alerted on: only once the worker works again, or gets a new task or answer. Exits 7 (usage limit) and 8 are raised once per task too, until a new task or answer. A worker herdr has lost track of while its pane still runs its CLI (seen after a Claude Code auto-update) reads `unknown` in `mesh status`, and isn't called gone.
   - **Quota hit** (exit 7, `ALERT <worker> hit its quota on <model>`): run `mesh switch <worker>` right away, without asking. It reopens the worker's conversation on the next model with quota left, and tells it to continue. Then tell the user in one line, for example "Argon ran out; <worker> continues on Opus".
     - If switch says no model is left (a Flash executor with Flash exhausted), tell the user the reset time and wait.
     - A Claude worker at its usage limit (`ALERT <worker> hit Claude's usage limit (resets …)`) has nowhere to move: the limit is the account's. Tell the user the reset time, wait for it, then send the task the line gives (`continue where you stopped`); that task re-arms the alert.
     - A quota hit is not a `failed`.
     - If `mesh approve` or `mesh switch` itself fails to reopen the worker, the worker is marked `swap_failed`, the watch alerts (exit 5), and the same command can be run again (approve's check for a waiting plan lets the retry of a failed approval through, not an approve after a failed switch).
     - If `mesh approve` can't start the move (its conversation isn't found, or it stays busy), nothing is sent, and the error suggests `mesh approve <worker> --keep-model`: it then executes on its planning model, and a later `mesh switch` moves it to the execution model. Use that rather than `mesh send … --type answer`, which leaves it recorded as planning, and tell the user it executes on its planning model.
   - **agy root:** rely on the doorbell, and check `mesh status` each time you wake. While the user has a draft typed in your pane, doorbells wait for it for about a minute, then give up (the sender's `mesh send` exits 2); those reports are still in your mailbox, and `mesh status` and the watch still show them.
   - **What's waiting on whom:** `mesh status` derives it for every worker, rows that need you first: a plan or question unanswered (and for how long), a task still unread ("doorbell failed": `mesh ring` it), a `done` with its PR's state and checks, or "no commits, no PR" (legitimate when the bug turned out not to be one, so read the report), "in review" while its reviewers work. `mesh log [--follow]` prints the run's events as a timeline.
   - **Keep working** on your own repo while workers run; their reports queue until your current turn ends. If your work depends on a worker's change, build against the contract you gave it, or wait for its `done` (its branch is `mesh/<run>/<slug>`) when you can't.
   - **Keep your own context lean:** you live longest, so your context costs the most. Read plans and reports, not workers' transcripts. Look at diffs only where the plan says the risk is.
   - **Blocked on a dialog** (exit 4): a stray menu (a picker, a `/` command the worker opened) you may dismiss yourself with `herdr agent send-keys <name> esc`, then read the pane again. A real question, a permission prompt or a startup screen goes to the user.
6. **Review every plan** (`plan` messages; large tasks only). Read the plan file and check it against the request, the repo and the other workers' plans:
   - Does it solve what was asked, and only that? Are the files and functions real (spot-check the code)?
   - Is the *done when* a real check (tests, lint, build), not "looks right"?
   - Do cross-repo contracts match (field names, API shapes)?

   Then reply **once**:
   - **approved:** `mesh approve <worker>`, with `--body "<notes>"` if you have any. This also moves a worker on `auto` to its execution model. It refuses unless the worker's newest report since its last task or answer is a `plan`: after a `question` or a `changes:` answer, the worker must send its plan again, changed or not, before you can approve it.
   - **changes:** `mesh send <worker> --type answer --body "changes: <specific list>"`. The worker revises and sends the plan again, still on its planning model.

   Ask the user only when a plan raises a real product decision; otherwise decide yourself.
7. **Answer questions** with `mesh send <worker> --type answer`:
   - pass cross-repo facts between workers (a field name, an API shape)
   - make the call yourself whenever the request or the code settles it
   - ask the user only for real product decisions, missing credentials, or a destructive choice
8. **Check every `done`** before passing it on; a worker's word is not proof.
   - **A large task:** compare the PR diff (`gh pr diff <url> --name-only` first, then the risky files) against the plan you approved. A trivial or small task had no plan to check it against, so its reviewers do that instead of you.
   - **Then start its two reviewers**, so every PR the run makes is reviewed without the user asking: side by side, picked by the review's size (see "Routing"), one `mesh ask --repo <repo> --review <PR> --kind <kind> --size <by PR size> --name review-<n>-<kind>` each. Handle their findings as "Questions and reviews" step 2 says. A PR is done when its reviews have nothing left that you agree with, or after two review→fix rounds (step 2 there).
   - If something's wrong, `mesh send <worker> --type task --body "<specific fixes>"` (start a small fix with `Direct task (no plan)`).
9. **Close what you're done with:** once a worker's `done` checks out and you have nothing more for it, `mesh retire <worker>`. Its pane closes, and its conversation stays resumable (`mesh resume <worker>` brings it back with everything it knew), so an idle agent never sits open just in case.
10. **On `failed`:** unless what blocked it is something only the user can give (credentials, access, a product decision), retry it once yourself, as "Escalate" in "Sizes" says: the same worker, resumed with more effort (Claude) or a stronger model (agy), with a hint from what blocked it.
   - If it fails again, or the next step is a different CLI, tell the user what blocked it and suggest one (a hint, a larger size, Claude instead of agy).
   - If the user agrees to a different CLI, retire the worker (`mesh retire <worker>`) and start the new one **in the same worktree** with `--cwd <worktree>`, giving it the plan and what failed. It gets the old name plus `-2`.
11. **Finish:** when every worker has reported, its PR's reviews are settled and your own part is done, tell the user:
    - one line per repo with the PR link and the worker's name (so it can be resumed later), plus what you changed in your own repo (it follows that repo's usual flow, not the mesh's)
    - what's left for them to do (review, test, deploy)
    - anything that failed or was restarted
    - the follow-ups you added to a repo's list of open work, or did in the run

    Then run `mesh stop`. It closes the panes, stops background workers, removes clean worktrees (branches with commits are kept) and archives the run log. It never touches your own checkout. Workers can still be resumed after it.

**Root-only commands:** `mesh approve`, `switch` and `retire` act only for the worker's parent (you), and `mesh stop` refuses any worker. They find you by your pane, as every command does; the user acting for you from another terminal passes `--run <run> --as <your name>`.

**Messages to a worker:** always send them with `mesh send` (or `mesh approve`). If the output has a `send_message` (a background worker has no pane to ring), pass it on with the `SendMessage` tool, with `to` and `message` exactly as printed.

## Task lists

When the request is a list of tasks ("work through TODO.md", "fix these five issues"), you supervise and **write no code yourself**, in your own repo too. Each task gets its own worker in a worktree.

1. **Triage:**
   - Read each task, and just enough of the code to size it.
   - Write the board to `$(mesh dir)/board.md`, with one row per task: task, repo, size, CLI and model, pane or background, depends on, overlaps with, and later the worker's name. Its PR and status aren't kept there: `mesh status` derives them.
   - Show it to the user and get **one** confirmation, including any routing you propose. Don't start before that.
2. **Order:** tasks that touch the same files in one repo don't run side by side.
   - **Stack** the later one on the earlier one's branch (`--base mesh/<run>/<earlier slug>`; its PR then targets that branch).
   - **Or** start it after the earlier one's `done`.
   - A dependency between repos follows step 4's contract rules.
3. **Cap:** keep at most **4** workers working at once. Start the next one when one reports.
4. **Supervise** as in the Root steps, which start each `done`'s two reviewers (Root step 8). After each `done`, also run `mesh conflicts`.
5. **Reviews:** check each finding before you act on it (see "Questions and reviews", step 2), then send the findings you agree with to the author (see "Follow-ups": `mesh ask <author> …` or a `task`), not to the reviewer. A PR is done when its reviews have nothing left that you agree with, or after two review→fix rounds.
6. **Conflicts:** `mesh conflicts` lists the run's branches that conflict with their default branch or with each other.
   - Below them it warns about workers that change the same files, committed or not (in `--json`, rows with `"kind": "overlap"`; real conflicts are `"kind": "conflict"`). Overlap isn't a conflict yet: stack or sequence the later worker, or tell the two which files they share.
   - Nobody merges, so report a merge order that avoids them.
   - When the user says a PR was merged, send each worker whose branch now conflicts a task to rebase onto the new default branch and push.
7. **The board is the plan, `mesh status` the state:** change `board.md` only when the plan changes (a task re-sized, split, re-ordered, or given a worker). After your context is compacted, rebuild your picture from `board.md`, `mesh status --json` and `mesh inbox --all`, not from memory.
   - Read `mesh status`, `mesh agents` and `mesh inbox` with `--json` rather than parsing their tables.
   - `mesh agents --run <run> --json` lists the run's workers and reviewers with their PRs, resumed ones included: gather PRs there, not by looping `gh pr list` over the repos.
8. **Report:** the board as a table, with each task's PR, worker name, size, status (from `mesh status`) and review outcome. Then the suggested merge order, and what's left for the user.

## Questions and reviews

A question about another repo, or a review of a PR, goes to a worker as well, so it can be asked again later and can take the change on itself. `mesh ask` starts or reaches the worker, gives it a read-only task and, unless the answer comes by `SendMessage`, waits for its `done` and prints it.

```bash
mesh ask --repo <path> --size small --body "<question>"       # a fresh agent in that repo
mesh ask --repo <path> --size small --review <PR link | number>  # a fresh reviewer, with the PR head checked out
mesh ask <name | branch | PR link> --body "<question>"        # a worker you already have, live or past
mesh ask <name | branch | PR link> --review <PR link>         # it reviews a PR without leaving its branch
```

- **No run needed:** without one, `ask` starts it (mode `--kind`, default agy; for `--kind copilot`, your own CLI).
- **Your own repo too:** a review of your own PR goes to a fresh agent in your repo (`--repo .`). It's a second pair of eyes that changes nothing, so it doesn't conflict with doing your repo's part yourself.
- **A fresh agent** follows the mode table and `--size`, like a spawn, except that a fresh reviewer opens in a pane at any size and runs at high effort when it is trivial or small ("Sizes"). Pass `--kind` for the CLI, and `--model` / `--effort` only if the user named them.
- **Copilot** (`--kind copilot`: a default reviewer of large PRs, otherwise only when the user names it) answers questions and reviews, nothing else. It runs headless inside the `ask` call, with no agent pane of its own: `--size` sets only its effort, and on a plan that offers only `auto` (Copilot Student), Copilot picks the model. `ask` waits for it and prints its answer, so a Claude root still runs it with `run_in_background`; while it works, a pane beside yours follows it (`mesh peek <name> --follow`) and closes once it has answered, and `mesh stop` waits until it has answered. Ask it again with `mesh ask <name>`, which resumes its session; it can't be resumed into a change.
- **Asking an existing worker** reuses all its context: the agent that wrote a PR knows why it's written that way.
  - A PR link names the worker that opened the PR (the one on its head branch); a reviewer is found by its name.
  - A worker busy on another task answers once that's done: `ask` then returns at once, and the answer comes later.
  - A past worker is resumed, and its conversation is compacted first when it's large (see "Follow-ups").
- **The question:** give it what you know, as you would in a task file (use `--file` for anything long): what you're working on, what you need back, and in what form.
- **Waiting:**
  - **A Claude worker under a Claude root:** `ask` returns once the question is delivered (pass `--wait` to block anyway), and the answer arrives as the worker's `SendMessage`. If the output has a `send_message`, pass it on with `SendMessage` first.
  - **An agy worker, Claude root:** run `ask` with `run_in_background`, and the answer arrives as the notification.
  - **agy root:** run it in the foreground, or pass `--no-wait` and wait for the doorbell.
  - Exit codes are those of `mesh watch`; on 7 (an agy quota), `mesh switch` the worker and `mesh watch` it; on a Claude usage limit, wait for the reset as in "Supervise".
  - Exit 4 with `ALERT <name> is waiting at a startup screen` comes before any waiting: the agent's pane stopped at a screen mesh doesn't answer (a Settings Warning for a malformed `.claude/settings.json` rule, say), and the question is stored. Tell the user to clear it in that pane, and don't answer it yourself. Then `mesh watch <name>` rings it once the screen is gone and waits for the answer; read it with `mesh inbox`, then `mesh retire <name>` once you have no follow-up for it.

Then:

1. **Relay the answer** to the user with the worker's name, so they can ask it again. `mesh ask` closes an agent it opened once it has answered (`--keep` leaves it open); asking again reopens the same conversation. When the answer came by `SendMessage` or after `--no-wait`, `mesh retire <name>` it yourself once you have no follow-up for it. Spot-check claims that matter against the code, as you would a `done`.
2. **A review:** a finding is a claim to check, not an order (`PROTOCOL.md` rule 9). Reviewers mark each one **blocking** (a defect with the input that makes it fail) or **advisory**. For each one, check against the code that the bug really happens (trace it, or run the failing case) and whether the suggested fix is the best one, then decide: agree, agree with a different fix, or disagree and why. Fix your own repo's PR yourself. Send findings on another worker's PR, with your opinion on each, to the worker that made it (see "Follow-ups"), not to the reviewer.
   - **Every review is of one commit:** the task names the PR's head (`Review (read-only) of <PR> at <sha>`), and the reviewer records it. After a fix, ask the **same** reviewers again (`mesh ask <reviewer> --review <PR>`): the task names the new head and the one it read before, so it checks the fix commits and whether its earlier findings are fixed, instead of the whole PR.
   - **At most two review→fix rounds** per PR. If findings you agree with are still open after the second fix's re-review, stop: give them to the user with your opinion on each, rather than starting a third round.
3. **More questions:** `mesh ask <name> --body "…"` again. The worker still has everything it read.
4. **Turning the answer into a change** in that repo: send the same worker a normal task (`mesh send <name> --type task --file …`, or `mesh resume <name> --task-file …` once it's gone). From there it's an ordinary worker.
   - A Claude asker runs with a deny list (no file edits in its worktree or your checkout, no `git push`, no `gh pr review` / `comment` / `merge` / `edit` / `close`, no `gh issue comment`). A change task is what lifts it: `mesh send` waits for its turn to end, reopens the same conversation on the same model without the list (the output says `read_only_lifted`), then delivers the task. A question or review sent the same way leaves it in place. An asker `mesh ask` already retired (its usual state) is given the change with `mesh resume <name> --task-file …` instead, which compacts it when large and also drops the list; `mesh send` refuses it. If a background asker's reopen fails, its task is already stored: `mesh resume <name>` with no task brings it back to read it. An agy asker has no deny list: only its prompt holds it.
   - A large asker plans, you `mesh approve`, and it executes on its branch `mesh/<run>/<slug>` and opens a PR. Supervise it as in steps 5 to 11.
   - A small asker has no plan phase: make the task a `Direct task (no plan)` by starting it with that line.

**Standing peer askers** (`mesh peers`): askers registered for good, one per cloned peer project, outside any run. The registry is machine-wide; groups (`orchestrators`) pick a subset. Ask one by name like any worker (`mesh ask <peer> --body …`), or several with one command:

```bash
mesh peers                                       # names, groups, what each last read, the newest answers file
mesh ask --peers all|<group>|a,b --file q.md     # one question to all of them
```

- Each ask fetches the peer's upstream, moves its checkout to the new head and tells it what changed since it last looked. `--no-refresh` skips the fetch.
- `ask --peers` blocks until every peer has answered or alerted, so a Claude root runs it with `run_in_background`. Their answers don't come to your mailbox one by one: read the **one file** whose path it prints (also shown by `mesh peers`), never N inboxes. Its stdout is one line per peer. A peer that didn't answer (held at a screen, asked back, stopped) stays open with its line saying what to do, then `mesh retire` it. Those that answered are closed.
- A peer only answers; a change to that project goes to an ordinary worker there. Add one with `mesh peers add <git-url | path> --group <g>`, then give it its first research brief: `mesh ask <name> --size large --file brief.md`.

`mesh stop` deletes a branch that never got a commit or a push, such as an asker's. Resuming that worker starts the branch again at the same commit.

## Follow-ups

When the request is about something a worker already built, in this run or an earlier one, resume **that worker**: it still has its research, plan and reasoning, so a fresh one would only rediscover them.

1. **Find it:** `mesh agents <PR link | repo | branch>` lists past workers with their run, branch, PRs and whether their conversation was found. With several matches, the newest is usually the one that last touched the PR.
2. **If it's still running in this run**, just `mesh send <worker> --type task --file …`. If it sends a plan, it plans on the model it's on, and `mesh approve` moves it to the execution model (a worker spawned with a named `--model` / `--effort` stays on it); to plan on the strong model instead, `mesh retire` it and resume it (step 3).
3. **Otherwise resume it:** `mesh init` first if you have no run, then
   ```bash
   mesh resume <name | branch | PR link> --task-file "$(mesh dir)/tasks/<repo>.md"
   ```
   It reopens the worker's own conversation (on the planning model when the task has a plan phase, see below), in its old worktree and branch (recreated if `mesh stop` removed it), as your worker. A background worker comes back in the background. If the worker's PR was merged or closed, a change task gets a new branch instead (from the default one after a merge, from the old branch's tip after a close), and a note at its end saying so (see `notes` in the output). The task file contains:
   - what to change: for a review, the comments themselves (`gh pr view <url> --comments`, `gh api repos/<owner>/<repo>/pulls/<n>/comments`) and **your opinion on each one**: agree, disagree and why, or what to do instead. Ask it to check each finding itself before fixing it (`PROTOCOL.md` rule 9) and to say in its plan which it fixes, how, and which it declines and why
   - "plan first; push to the same branch so the PR updates; report `done`" (for a small follow-up: start it with `Direct task (no plan)`)
4. **Compaction:** an old conversation has lost its prompt cache, so every turn of the follow-up would pay for all of its context again. `resume` (and `ask`) compact it first, headless, keeping what the new task needs:
   - with `--compact auto` (the default): a Claude conversation over 300k tokens (`MESH_COMPACT_AT`) or an agy one over 100k (`MESH_AGY_COMPACT_AT`), twice that for a read-only question; an agy one whose size can't be read is compacted when it comes back from an earlier run;
   - `--compact yes|no` forces it either way;
   - `--focus "<what matters>"` replaces the default focus when you know better ("the restockedAt contract and why we chose ISO dates");
   - the output's `compaction` shows the context size before and after.
5. Supervise and review it exactly like a new worker.

A conversation can't move to another CLI. If the user wants a different agent (agy → Claude), spawn a fresh one with `--cwd <worktree>` and give it the old plan and report, as on `failed`. `mesh resume --model` changes the model within the same CLI. A resumed worker keeps the model it ran on, except for a task that has a plan phase (not a `Direct task (no plan)` or a question, and no `--model` / `--effort`): that one plans where a spawn would (the first of Argon → Opus → Flash with quota for agy, Opus for Claude, at high effort), and `mesh approve` then moves it to the execution model and effort: agy to Flash at medium, Claude to medium effort. On a quota hit, `mesh switch` still works.

## Worker

You start inside your own worktree, on branch `mesh/<run>/<slug>`. It holds the same code as the main checkout (for a reviewer, the PR's head), so research there.

**Reporting.** Every message to your parent goes through `mesh send parent …`, which stores it. If its output has a `send_message`, your parent is a Claude session that you reach with the `SendMessage` tool (load it with ToolSearch if it isn't loaded). Send it with `to` and `message` exactly as printed, then end your turn as usual.

**Read-only tasks.** A task that starts with `Question (read-only)` or `Review (read-only)` comes from `mesh ask`. Skip the plan (steps 4 to 9): research, then answer. Change no files, commit nothing, push nothing, switch no branches, and post nothing on GitHub. Install dependencies only if you need them to answer (to run a test, for example). Report one `done`:
- **A question:** the answer, with the file paths and functions that back it, and what you're unsure of.
- **A review:** findings ranked by severity, each with file:line, a concrete failure (the input or state, and what goes wrong), why it matters and a suggested fix, and marked **blocking** (a defect you reproduced or traced, with the input that makes it fail) or **advisory** (everything else), then what you checked (the tests you ran, say). Report only what you've confirmed in the code, mark anything you couldn't confirm as unverified, and leave out style preferences the repo's rules don't ask for. A review task names the commit it is of; when it also names the one you reviewed before, review what changed since and say which of your earlier findings are fixed.

Then end your turn. A later task may ask more, or ask you to make a change, which follows the steps below as usual.

**Direct tasks.** A task that starts with `Direct task (no plan)` skips steps 4 to 6: research (step 3), then execute, verify, publish and report (steps 7 to 10). If the research shows the task is bigger or less clear than it says, send a `question` with what you found and end your turn before changing anything.

**Plan reviewed by the user.** A task that starts with `Plan review by the user` runs in plan mode: write your plan as Claude Code's plan mode asks, and the user approves it in your pane. Don't send a `plan` message. Once approved, continue from step 7.

1. **Follow the repo's rules.** The repo's `CLAUDE.md` / `AGENTS.md` apply to everything repo-specific: task tracking, branch and commit conventions, contract checks, the PR process. **Never edit files or switch branches in the main checkout**, and never touch another agent's worktree.
2. **Set up:** a fresh worktree has no installed dependencies and no untracked files such as `.env`. Install what the repo needs. Don't copy secrets in unless the task says to.
3. **Research** until you know exactly what to change: read the code, run the existing tests, reproduce the bug if there is one. Check every assumption the task marks before you rely on it. **Change nothing yet.**
4. **Plan:** write `$(mesh dir)/plans/<your-name>.md` from `~/.config/agent-mesh/PLAN_TEMPLATE.md`. It is written for a reviewer who hasn't read the code: say what you found and where, name the exact files and functions, the invariants, the steps, the done-when command, and your open questions.
5. **Ask for review:** `mesh send parent --type plan --file "$(mesh dir)/plans/<your-name>.md"`, then **end your turn**. Don't start implementing while you wait.
6. **On the answer:**
   - `approved` → go on.
   - `changes: …` → update the plan file, send it again as a `plan`, end your turn.
   - Any other answer while you plan (to a `question`, say) → send the plan again as a `plan`, even unchanged, and end your turn: only a plan sent since the last answer can be approved.
7. **Execute** the approved plan (or the direct task) yourself, in your worktree. Commit on your branch with clear messages. If the code turns out different from the plan in a way that changes its scope, stop, send a `question` and end your turn. Small corrections you can just note in the report.
8. **Verify:** run the plan's done-when command and the repo's lint, test and build. Fix what fails. After two honest attempts at the same failure, send a `question` instead of guessing.
9. **Publish:** push your branch and open the PR with `gh pr create`, following the repo's own PR rules (title, body, attribution, base branch). The PR body includes what changed, the commands that passed, and anything the reviewer should look at. Then do whatever the repo's rules require after a PR.
   - **A follow-up** to work you already published goes on the **same branch**, so the existing PR updates. Open a new PR only if the task asks for one, or a note from mesh at its end says your PR was merged or closed.
   - **A stacked task** (your worktree started from another worker's branch) opens its PR against that branch.
10. **Report:** run `mesh inbox` once more, then send **one** `done` to your parent (`--file` under `$(mesh dir)/reports/`) with the PR link, a summary, the checks that passed, and any deviations from the plan. If you can't finish, send `failed` with what blocked you and what you tried. Don't run `mesh stop`: the root does that.

## Rules for everyone

- **Workers push only** their own `mesh/<run>/<slug>` branch and open PRs. Nobody merges a PR or deploys.
- **No sub-delegation:** workers never run `mesh spawn`, start `claude --bg` sessions, or spawn agents in any other way. The run is the root plus its workers.
- **Quiet supervision:** a working agent needs no check-ins. Act on reports, doorbells and watch alerts only.
- **Concise messages:** details go in `--file` reports and plans, not in chat.
- **Decide, don't ask:** the root makes every call that the request, the code or a sensible default settles: sizes, reviewers, effort, which review findings to fix and how, retries, wording, merge order. Ask the user only when their answer would change the outcome in a way you can't judge yourself: a product decision, a trade-off only they can weigh, missing credentials, or anything destructive or outward-facing. Report what you decided, briefly, so they can overrule it. A task list's one board confirmation stays.
- **Follow-ups land somewhere:** work that you or a worker discover but the request didn't ask for (a worker's report notes it, see `PROTOCOL.md` rule 3) goes on that repo's own list of open work, wherever its rules track it. Whoever works in that repo adds it: you in your own repo (outside a task list), otherwise a worker of the run that made a PR in that repo (not a reviewer), as a task (it adds the line in its PR). If it's small and close to that worker's task, it can do the work itself instead. A follow-up for a repo with no worker in the run goes in your Finish report, marked for the user to file. Never leave it only in chat.
