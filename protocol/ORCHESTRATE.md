# agent-mesh orchestration

The shared procedure behind `/delegate-claude-agy`, `/delegate-claude` and `/delegate-agy`. The *root* is the agent the user typed the command into. *Planners* run inside each affected repo. *Executors* run in their own worktrees. Read `PROTOCOL.md` as well: it applies to you too.

## Modes

| Mode | Planner | Executor | Executor escalation |
|---|---|---|---|
| `claude-agy` | `--kind claude --model opus --effort high` | `--kind agy --model flash`, effort per task | flash high → argon high → the planner does it |
| `claude` | `--kind claude --model opus --effort high` | `--kind claude --model opus --effort high` | one retry with a fresh executor → the planner does it |
| `agy` | `--kind agy --model argon --effort high` | `--kind agy --model flash`, effort per task | flash high → argon high → the planner does it |

- **Flash effort:** `low` for mechanical edits (renames, copying a pattern), `medium` by default, `high` for multi-step work or anything with new tests.
- **User overrides:** if the request names a model or effort ("use Argon to execute"), that wins.
- **Resolution:** `mesh` resolves the agy families `flash` / `argon` to the newest matching model. `mesh model --kind agy --model flash --effort high` shows the result.

## Root

1. **Start the run:** `mesh init --mode <mode> --request "<one-line summary>"`.
2. **Find the scope:** which repos the request touches (usually under `~/projects/`). Ask the user only if that's genuinely ambiguous.
3. **Decide who plans:**
   - **Plan it yourself** when the request covers a single repo, that repo is your current one, and you are the mode's planner kind. Continue with **Planner**, acting as both. You report to the user instead of a parent, and you finish with `mesh stop`.
   - **Otherwise**, write a task file per repo in `$(mesh dir)/tasks/` and spawn one planner each:
     ```bash
     mesh spawn --kind <planner-kind> --role planner --repo <path> --model <m> --effort <e> \
       --task-file "$(mesh dir)/tasks/<repo>.md"
     ```
     The task file contains:
     - that repo's slice of the request
     - the **mode table row** (the planner needs to know which executors to use and the escalation chain)
     - dependencies on other repos (what to wait for, and what the other planners will need from this one)
     - "report `done` with the PR link"
4. **Supervise:**
   - **Claude root:** start `mesh watch <planner> --timeout 7200` with `run_in_background` for each planner. A watch exiting with status 0 means the planner reported; any other status is an alert, so investigate (`mesh status`, `herdr agent read <name>`).
   - **agy root:** rely on the doorbell, and check `mesh status` each time you wake.
5. **Answer questions:**
   - pass cross-repo facts between planners (a field name, an API shape)
   - make the call yourself whenever the request or the code settles it
   - ask the user only for real product decisions, missing credentials, or a destructive choice
6. **Finish:** when every planner has reported, tell the user:
   - one line per repo with the PR link
   - what's left for them to do (review, test, deploy)
   - any escalations, and anything that failed

   Then run `mesh stop`. It closes the panes, removes clean worktrees (branches are kept) and archives the run log.

## Planner

1. **Follow the repo's rules.** You run inside the repo, so its `CLAUDE.md` / `AGENTS.md` are loaded. Follow them for everything repo-specific: task tracking, branch and commit conventions, contract checks, the PR process. **Never edit files or switch branches in the main checkout**: all changes happen in worktrees.
2. **Split the work** into executor tasks:
   - each one independent and verifiable on its own
   - at most 3 running in parallel
   - tasks that touch the same files run **one after another**, not in parallel
3. **Write one plan per task** from `~/.config/agent-mesh/PLAN_TEMPLATE.md` into `$(mesh dir)/plans/<repo>-<slug>.md`. Flash executes literally:
   - name the exact files and functions
   - spell out the invariants and the done-when command
   - list the stop-and-ask conditions
   - make the plan concrete enough that its questions will be few
4. **Spawn** each executor:
   ```bash
   mesh spawn --kind <exec-kind> --role executor --worktree <slug> --model <m> --effort <e> \
     --task-file "$(mesh dir)/plans/<repo>-<slug>.md"
   ```
   The slug names both the branch and the agent (`<repo>-<slug>` in herdr), so make it short and descriptive: `fix-reactions`, not `task1`. Then watch it the same way the root watches planners (`mesh watch <exec> --timeout 3600`).
5. **Answer questions** quickly, from the repo's context. Send `answer` messages. When an answer changes the plan, update the plan file too. Ask your own parent (`mesh send parent --type question`) only for cross-repo or product questions. While you wait for anyone, end your turn.
6. **Check every `done`** yourself; an executor's word is not proof:
   ```bash
   cd <worktree> && git log --oneline <base>..HEAD && git diff <base>...HEAD
   ```
   Run the plan's done-when command and the repo's lint, test and build. Then do one of three things:
   - **Accept** the work.
   - **Ask for fixes:** `mesh send <exec> --type task --body "<specific fixes>"`, then watch it again.
   - **Escalate** (next step).
7. **Escalate** after at most 2 fix rounds at one level. Close the executor (`mesh retire <exec>`) and start the next level of the chain **in the same worktree**:
   ```bash
   mesh spawn --kind <kind> --role executor --cwd <worktree> --model <stronger> --effort high \
     --task-file <plan + what failed and why>
   ```
   The new executor gets the old one's name plus `-2`. At the end of the chain, finish the work yourself in that worktree. Record every escalation in your report.
8. **Integrate:**
   - **One accepted branch:** it is the PR branch.
   - **Several:** run `mesh worktree --slug integration`, merge the accepted branches into it, resolve conflicts, and run the full checks again.
9. **Publish:** push the branch and open the PR with `gh pr create`, following the repo's own PR rules (title, body, attribution, base branch). The PR body includes:
   - what changed
   - what each executor did
   - the commands that passed
   - any escalations

   Then do whatever the repo's rules require after a PR.
10. **Report:** send a `done` to your parent (`--file` for anything long) with the PR link, a summary, the checks that passed, and any escalations. If you are also the root, report to the user and run `mesh stop`. Otherwise **don't** run `mesh stop`, because the root does that.

## Rules for everyone

- **Executors never push.** Planners push only their run's `mesh/<run>/…` branches and open PRs. Nobody merges a PR or deploys.
- **Quiet supervision:** a working agent needs no check-ins. Act on doorbells and watch alerts only.
- **Concise messages:** details go in `--file` reports and plans, not in chat.
