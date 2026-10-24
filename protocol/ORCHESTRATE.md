# agent-mesh orchestration

The shared procedure behind `/delegate-agy` and `/delegate-claude`. The *root* is the agent the user typed the command into, usually in the middle of its own work in its own repo. It keeps doing its own repo's part itself, and spawns one *worker* per **other** affected repo, so each one loads that repo's `CLAUDE.md` / `AGENTS.md`, skills and hooks. Each worker researches its repo, writes a plan, sends it to the root for review and, once it's approved, carries the plan out **itself**, with all the context its research built up. Workers never spawn agents of their own. Read `PROTOCOL.md` as well: it applies to you too.

## Modes

| Mode | Worker | Plans on | Executes on |
|---|---|---|---|
| `agy` | `--kind agy` (also what `mesh spawn` does with no flags) | the first of Argon → Claude Opus → Flash with quota left, high effort | Flash, medium effort |
| `claude` | `--kind claude` | Opus, high effort | Sonnet, medium effort |

- **Models per phase:** with no `--model` (that is, `--model auto`), a worker plans on a strong model and moves to the cheaper one when you approve its plan with `mesh approve`. It stays in the **same conversation**, so it keeps its research. Effort follows the phase: high while planning, medium while executing (`--effort` / `--exec-effort`). For agy, `mesh` reads `agy -p /usage` and skips any model with under 10% left (`MESH_MIN_QUOTA`). `mesh usage` shows the quotas and what `auto` would pick.
- **User overrides:** if the request names a model, agent or effort ("use Argon", "do it with Claude"), pass it as `--model`; that one model then runs the whole task, with no switch (default effort high). A named effort goes in `--effort` and covers planning and execution alike, with or without `--model`. Otherwise don't pass `--model`, and don't pick a model on your own.
- **Resolution:** `mesh` resolves the agy families `argon` / `flash` / `opus` / `sonnet` to the newest matching model. `mesh model --kind agy` shows what a spawn would use, and why.

## Root

1. **Start the run:** `mesh init --mode <mode> --request "<one-line summary>"`.
2. **Find the scope:** which repos the request touches (usually under `~/projects/`). Ask the user only if that's genuinely ambiguous.
   - **Your own repo** (the one you're running in) stays **yours**: you do its part yourself, in your current checkout and branch, exactly as you would without a mesh. Its rules are already loaded in your session. Spawn a worker for it only when the user explicitly asks to delegate it too.
   - **Every other repo** gets a worker.
   - **Work an earlier worker did** (its PR got review comments, "make it Y instead of X"): resume that worker instead of spawning a fresh one. See "Follow-ups" below.
3. **Spawn one worker per other repo.** Write a task file per repo in `$(mesh dir)/tasks/` and start the worker in a fresh worktree:
   ```bash
   mesh spawn --kind <kind> --repo <path> --worktree <slug> \
     --task-file "$(mesh dir)/tasks/<repo>.md"      # plus --model/--effort only if the user named them
   ```
   The slug names both the branch (`mesh/<run>/<slug>`) and the agent (`<repo>-<slug>` in herdr), so make it short and descriptive: `fix-reactions`, not `task1`. The task file contains:
   - that repo's slice of the request
   - what you know from your own work that it needs: the contract your repo expects (field names, API shapes, error cases), with file paths in your repo it can read for reference
   - dependencies on other repos (what to wait for, and what the other workers will need from this one)
   - "send your plan for review before changing anything; report `done` with the PR link"

   Tell the user which model each worker plans on. When the spawn output has `notes` (for example "gemini-4-argon-high has 0% left"), add that in a few words.
4. **Supervise:**
   - **Claude root:** start `mesh watch <worker> --timeout 7200` with `run_in_background` for each worker. A watch exiting with status 0 means the worker reported `done`/`failed`; any other status is an alert, so investigate (`mesh status`, `herdr agent read <name>`). A `plan` or `question` arrives as a doorbell and doesn't end the watch. A watch carries on through `mesh approve` reopening the worker on its execution model.
   - **Quota hit** (watch exit 7, `ALERT <worker> hit its quota on <model>`): run `mesh switch <worker>` right away, without asking. It reopens the worker's conversation on the next model with quota left, and tells it to continue. Then start its watch again, and tell the user in one line, for example "Argon ran out; <worker> continues on Opus". If switch says no model is left (a Flash executor with Flash exhausted), tell the user the reset time and wait. A quota hit is not a `failed`. If `mesh approve` or `mesh switch` itself fails to reopen the worker, the worker is marked `swap_failed`, the watch alerts (exit 5), and the same command can be run again.
   - **agy root:** rely on the doorbell, and check `mesh status` each time you wake.
   - **Keep working** on your own repo while workers run; their doorbells queue until your current turn ends. If your work depends on a worker's change, build against the contract you gave it, or wait for its `done` (its branch is `mesh/<run>/<slug>`) when you can't.
5. **Review every plan** (`plan` messages). Read the plan file and check it against the request, the repo and the other workers' plans:
   - Does it solve what was asked, and only that? Are the files and functions real (spot-check the code)?
   - Is the *done when* a real check (tests, lint, build), not "looks right"?
   - Do cross-repo contracts match (field names, API shapes)?

   Then reply **once**:
   - **approved:** `mesh approve <worker>`, with `--body "<notes>"` if you have any. This also moves a worker on `auto` to its execution model.
   - **changes:** `mesh send <worker> --type answer --body "changes: <specific list>"`. The worker revises and sends the plan again, still on its planning model.

   Ask the user only when a plan raises a real product decision; otherwise decide yourself.
6. **Answer questions:**
   - pass cross-repo facts between workers (a field name, an API shape)
   - make the call yourself whenever the request or the code settles it
   - ask the user only for real product decisions, missing credentials, or a destructive choice
7. **Check every `done`** before passing it on; a worker's word is not proof. Look at the PR diff (`gh pr diff <url>` or `git -C <worktree> diff <base>...HEAD`) against the approved plan. If something's wrong, `mesh send <worker> --type task --body "<specific fixes>"` and watch it again.
8. **On `failed`:** don't restart the worker on another model by yourself. Tell the user what blocked it and suggest a next step (a hint, or a stronger model such as `argon` or Claude). If the user agrees, retire the worker (`mesh retire <worker>`) and start the new one **in the same worktree** with `--cwd <worktree>`, giving it the plan and what failed; it gets the old name plus `-2`.
9. **Finish:** when every worker has reported and your own part is done, tell the user:
   - one line per repo with the PR link and the worker's name (so it can be resumed later), plus what you changed in your own repo (it follows that repo's usual flow, not the mesh's)
   - what's left for them to do (review, test, deploy)
   - anything that failed or was restarted

   Then run `mesh stop`. It closes the panes, removes clean worktrees (branches are kept) and archives the run log. It never touches your own checkout. Workers can still be resumed after it.

## Follow-ups

When the request is about something a worker already built, in this run or an earlier one, resume **that worker**: it still has its research, plan and reasoning, so a fresh one would only rediscover them.

1. **Find it:** `mesh agents <PR link | repo | branch>` lists past workers with their run, branch, PRs and whether their conversation was found. With several matches, the newest is usually the one that last touched the PR.
2. **If it's still running in this run**, just `mesh send <worker> --type task --file …`.
3. **Otherwise resume it:** `mesh init` first if you have no run, then
   ```bash
   mesh resume <name | branch | PR link> --task-file "$(mesh dir)/tasks/<repo>.md"
   ```
   It reopens the worker's own conversation, on the model it ran on, in its old worktree and branch (recreated if `mesh stop` removed it), as your worker. The task file contains:
   - what to change: for a review, the comments themselves (`gh pr view <url> --comments`, `gh api repos/<owner>/<repo>/pulls/<n>/comments`) and **your opinion on each one**: agree, disagree and why, or what to do instead
   - "plan first; push to the same branch so the PR updates; report `done`"
4. Supervise and review it exactly like a new worker: watch it, review its plan, check its `done`.

A conversation can't move to another CLI. If the user wants a different agent (agy → Claude), spawn a fresh one with `--cwd <worktree>` and give it the old plan and report, as on `failed`. `mesh resume --model` changes the model within the same CLI. A resumed worker keeps one model throughout: approving its plan doesn't switch it. On a quota hit, `mesh switch` still works.

## Worker

You start inside your own worktree, on branch `mesh/<run>/<slug>`. It holds the same code as the main checkout, so research there.

1. **Follow the repo's rules.** The repo's `CLAUDE.md` / `AGENTS.md` apply to everything repo-specific: task tracking, branch and commit conventions, contract checks, the PR process. **Never edit files or switch branches in the main checkout**, and never touch another agent's worktree.
2. **Set up:** a fresh worktree has no installed dependencies and no untracked files such as `.env`. Install what the repo needs. Don't copy secrets in unless the task says to.
3. **Research** until you know exactly what to change: read the code, run the existing tests, reproduce the bug if there is one. **Change nothing yet.**
4. **Plan:** write `$(mesh dir)/plans/<your-name>.md` from `~/.config/agent-mesh/PLAN_TEMPLATE.md`. It is written for a reviewer who hasn't read the code: say what you found and where, name the exact files and functions, the invariants, the steps, the done-when command, and your open questions.
5. **Ask for review:** `mesh send parent --type plan --file "$(mesh dir)/plans/<your-name>.md"`, then **end your turn**. Don't start implementing while you wait.
6. **On the answer:**
   - `approved` → go on.
   - `changes: …` → update the plan file, send it again as a `plan`, end your turn.
7. **Execute** the approved plan yourself, in your worktree. Commit on your branch with clear messages. If the code turns out different from the plan in a way that changes its scope, stop, send a `question` and end your turn. Small corrections you can just note in the report.
8. **Verify:** run the plan's done-when command and the repo's lint, test and build. Fix what fails. After two honest attempts at the same failure, send a `question` instead of guessing.
9. **Publish:** push your branch and open the PR with `gh pr create`, following the repo's own PR rules (title, body, attribution, base branch). The PR body includes what changed, the commands that passed, and anything the reviewer should look at. Then do whatever the repo's rules require after a PR.
   - **A follow-up** to work you already published goes on the **same branch**, so the existing PR updates. Open a new PR only if the task asks for one.
10. **Report:** run `mesh inbox` once more, then send **one** `done` to your parent (`--file` under `$(mesh dir)/reports/`) with the PR link, a summary, the checks that passed, and any deviations from the plan. If you can't finish, send `failed` with what blocked you and what you tried. Don't run `mesh stop`: the root does that.

## Rules for everyone

- **Workers push only** their own `mesh/<run>/<slug>` branch and open PRs. Nobody merges a PR or deploys.
- **No sub-delegation:** workers never run `mesh spawn`. The run is the root (doing its own repo) plus one worker per other repo.
- **Quiet supervision:** a working agent needs no check-ins. Act on doorbells and watch alerts only.
- **Concise messages:** details go in `--file` reports and plans, not in chat.
