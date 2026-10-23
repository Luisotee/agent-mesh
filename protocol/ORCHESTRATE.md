# agent-mesh orchestration

The shared procedure behind `/delegate-agy` and `/delegate-claude`. The *root* is the agent the user typed the command into, usually in the middle of its own work in its own repo. It keeps doing its own repo's part itself, and spawns one *worker* per **other** affected repo, so each one loads that repo's `CLAUDE.md` / `AGENTS.md`, skills and hooks. Each worker researches its repo, writes a plan, sends it to the root for review and, once it's approved, carries the plan out **itself**, with all the context its research built up. Workers never spawn agents of their own. Read `PROTOCOL.md` as well: it applies to you too.

## Modes

| Mode | Worker |
|---|---|
| `agy` | `--kind agy --model flash --effort high` (newest Gemini Flash; this is also what `mesh spawn` does with no flags) |
| `claude` | `--kind claude --model opus --effort high` |

- **User overrides:** if the request names a model, agent or effort ("use Argon", "do it with Claude"), that wins. Otherwise always use the mode's row: don't pick a stronger model on your own.
- **Resolution:** `mesh` resolves the agy families `flash` / `argon` to the newest matching model. `mesh model --kind agy --model flash --effort high` shows the result.

## Root

1. **Start the run:** `mesh init --mode <mode> --request "<one-line summary>"`.
2. **Find the scope:** which repos the request touches (usually under `~/projects/`). Ask the user only if that's genuinely ambiguous.
   - **Your own repo** (the one you're running in) stays **yours**: you do its part yourself, in your current checkout and branch, exactly as you would without a mesh. Its rules are already loaded in your session. Spawn a worker for it only when the user explicitly asks to delegate it too.
   - **Every other repo** gets a worker.
3. **Spawn one worker per other repo.** Write a task file per repo in `$(mesh dir)/tasks/` and start the worker in a fresh worktree:
   ```bash
   mesh spawn --kind <kind> --repo <path> --model <m> --effort <e> --worktree <slug> \
     --task-file "$(mesh dir)/tasks/<repo>.md"
   ```
   The slug names both the branch (`mesh/<run>/<slug>`) and the agent (`<repo>-<slug>` in herdr), so make it short and descriptive: `fix-reactions`, not `task1`. The task file contains:
   - that repo's slice of the request
   - what you know from your own work that it needs: the contract your repo expects (field names, API shapes, error cases), with file paths in your repo it can read for reference
   - dependencies on other repos (what to wait for, and what the other workers will need from this one)
   - "send your plan for review before changing anything; report `done` with the PR link"
4. **Supervise:**
   - **Claude root:** start `mesh watch <worker> --timeout 7200` with `run_in_background` for each worker. A watch exiting with status 0 means the worker reported `done`/`failed`; any other status is an alert, so investigate (`mesh status`, `herdr agent read <name>`). A `plan` or `question` arrives as a doorbell and doesn't end the watch.
   - **agy root:** rely on the doorbell, and check `mesh status` each time you wake.
   - **Keep working** on your own repo while workers run; their doorbells queue until your current turn ends. If your work depends on a worker's change, build against the contract you gave it, or wait for its `done` (its branch is `mesh/<run>/<slug>`) when you can't.
5. **Review every plan** (`plan` messages). Read the plan file and check it against the request, the repo and the other workers' plans:
   - Does it solve what was asked, and only that? Are the files and functions real (spot-check the code)?
   - Is the *done when* a real check (tests, lint, build), not "looks right"?
   - Do cross-repo contracts match (field names, API shapes)?

   Then reply with **one** `answer`: `approved`, or `changes: <specific list>`. The worker revises and sends the plan again. Ask the user only when a plan raises a real product decision; otherwise decide yourself.
6. **Answer questions:**
   - pass cross-repo facts between workers (a field name, an API shape)
   - make the call yourself whenever the request or the code settles it
   - ask the user only for real product decisions, missing credentials, or a destructive choice
7. **Check every `done`** before passing it on; a worker's word is not proof. Look at the PR diff (`gh pr diff <url>` or `git -C <worktree> diff <base>...HEAD`) against the approved plan. If something's wrong, `mesh send <worker> --type task --body "<specific fixes>"` and watch it again.
8. **On `failed`:** don't restart the worker on another model by yourself. Tell the user what blocked it and suggest a next step (a hint, or a stronger model such as `argon` or Claude). If the user agrees, retire the worker (`mesh retire <worker>`) and start the new one **in the same worktree** with `--cwd <worktree>`, giving it the plan and what failed; it gets the old name plus `-2`.
9. **Finish:** when every worker has reported and your own part is done, tell the user:
   - one line per repo with the PR link, plus what you changed in your own repo (it follows that repo's usual flow, not the mesh's)
   - what's left for them to do (review, test, deploy)
   - anything that failed or was restarted

   Then run `mesh stop`. It closes the panes, removes clean worktrees (branches are kept) and archives the run log. It never touches your own checkout.

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
10. **Report:** run `mesh inbox` once more, then send **one** `done` to your parent (`--file` under `$(mesh dir)/reports/`) with the PR link, a summary, the checks that passed, and any deviations from the plan. If you can't finish, send `failed` with what blocked you and what you tried. Don't run `mesh stop`: the root does that.

## Rules for everyone

- **Workers push only** their own `mesh/<run>/<slug>` branch and open PRs. Nobody merges a PR or deploys.
- **No sub-delegation:** workers never run `mesh spawn`. The run is the root (doing its own repo) plus one worker per other repo.
- **Quiet supervision:** a working agent needs no check-ins. Act on doorbells and watch alerts only.
- **Concise messages:** details go in `--file` reports and plans, not in chat.
