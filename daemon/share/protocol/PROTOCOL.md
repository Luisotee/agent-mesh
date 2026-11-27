# agent-mesh protocol (the daemon)

You are an agent in an **agent mesh** run by the agent-mesh daemon (`meshd`): a supervisor (your *parent*) gave you work and is waiting for your report. Your terminal belongs to the daemon, and the user can watch it and type into it from the app. This file is for every agent; a root also reads `ORCHESTRATE.md` and `ROOT_CARD.md` (same folder), and a worker follows its "Worker" section.

Every `mesh` command here is the daemon's CLI. It answers in JSON (except `mesh inbox` and `mesh settings`, which print short text by default: add `--format json` for the whole response), and finds your identity and the daemon in your environment, so you never pass ids by hand. `mesh help` lists the commands and `mesh help <command>` the flags of one.

## Who you are

```bash
mesh whoami        # your role, name, id, run, run folder and parent
mesh dir           # the run's folder: plans/, reports/ and tasks/ go in it
```

## Reading messages

A line starting with `📬 [mesh]` in your input is a **doorbell**: the daemon hands it to you when you are idle (a Claude Code agent takes it as a cross-session message, any other CLI has it typed into its terminal), never while you work and never into what the user is typing. It shows only who sent what, and a `(ref abc123)` that is the daemon's own marker (ignore it); the message is in your mailbox. A doorbell you didn't act on is rung again a few times, so one that repeats is not a new message:

```bash
mesh inbox         # prints your unread messages (a header line with type, sender and time, then the body; `--format json` for the whole response) and marks them read
```

Run `mesh inbox` whenever a doorbell rings, and reply only with `mesh send`. The first doorbell of a process also names this file: read it once. Messages from `mesh` itself (an `info` from the daemon) are the daemon's: a nudge for you, or an alert about an agent of yours.

## Sending messages

```bash
mesh send parent --type plan     --body @"$(mesh dir)/plans/<your-name>.md"
mesh send parent --type question --body "…"
mesh send parent --type done     --body @"$(mesh dir)/reports/<your-name>.md"
```

Name the task you report on: `--re <id>`, the id in your task's header line in `mesh inbox` (`── task from … (m3r6…) ──`). The daemon stamps every report with your current task either way; with `--re` it refuses a report for an older task, any report after a `done` for the task, or a second `failed`, so a late or repeated report can't stand for the wrong task.

`@file` reads the text from a file (`@-` from stdin, `@@` for a literal `@`). Types: `task`, `plan`, `question`, `answer`, `done`, `failed`, `info`, `wait`. A `plan` asks your parent to review it; the reply is an `answer`. While you plan, only a plan sent since your last task or answer can be approved, so after any answer that isn't `approved`, send the plan again, even unchanged. A message longer than a few lines goes through a file under `$(mesh dir)/reports/` (or `plans/`). **Never write mesh files inside the repo.**

The message is stored when `mesh send` returns, and your parent's doorbell rings at its next idle: there is nothing more to do to deliver it. Never send a message by typing into another agent's terminal: that text reaches no mailbox, log or alert, and can land in the middle of what the user is typing there.

## How do I …

A map from what someone wants to the command. `mesh help <command>` has the flags. **Tell the user** means you can't run it (a person's terminal or the app can): say the command, never "there is no way".

| I want to … | Command |
|---|---|
| Watch an agent's terminal or type to it | **Tell the user**: `mesh open <agent>` attaches their terminal (`Ctrl-]` detaches); a retired agent's conversation reopens in it. Or the app. Never type into it yourself. |
| Send an agent a message | `mesh send <agent> --type task\|answer\|info --body …` (`parent` for your parent) |
| Read my mail | `mesh inbox`; a root also `mesh brief` |
| See the run's agents and their states | `mesh agents`; one: `mesh agent <agent>`; the timeline: `mesh log` (`--follow`) |
| See what waits on me or the user | `mesh waiting`; permission prompts: `mesh permissions`, answered with `mesh permission --id <id> --decision allow\|deny` |
| Approve a worker's plan | `mesh approve <worker> --tier <tier>` (root) |
| See a worker's branch, diff and PR | `mesh check <worker>`; all PRs and their checks: `mesh prs` |
| Ask a question or have a PR reviewed | `mesh ask` (`ORCHESTRATE.md`, "Questions and reviews") |
| Bring back or stop an agent | `mesh resume <agent>`, `mesh retire <agent>` |
| Check quota or what a run cost | `mesh usage`, `mesh cost` |
| Pair a phone or another machine | **Tell the user**: `mesh pair` |
| Anything else | `mesh help`; a part of the protocol: `mesh protocol` (lists them) |

## What the daemon does for you

- **A nudge:** if your turn ends with no `plan`, `question`, `done` or `failed` since your last task or answer, the daemon waits `accountability.nudge_after` (30 s) and sends you one `info` starting `[mesh nudge]`: run `mesh inbox` and report now. If your next turn ends the same way, it tells your parent (`[mesh alert silent …]`). It comes once per task, and again after each wait you declare.
- **Alerts for your parent:** when you exit, are lost, go quiet for ten minutes, or stop at a usage limit without having reported, your parent is told. That is not a failure of yours to fix: just report as usual when you are back.
- **A usage limit** on your model is the daemon's to handle (`limits.auto`): it moves you to the next model of your tier's chain, or continues you once the limit is over, and tells you in an `info`. Carry on from it.
- **Unread instructions:** while a `task` or `answer` is unread in your mailbox, `mesh send` refuses a `plan`, `question`, `done` or `failed` (nothing is sent) and says what is waiting, since the report would answer instructions you have not read. Read them (`mesh inbox`), act on them, then send one report that covers them too. Something that can't wait goes as an `info`.

## Rules

**Waiting on purpose:** before you end your turn to wait for something long you started (a background test suite that will wake you), declare it: `mesh send parent --type wait --body "<what you wait for>"`. The daemon then holds its nudge for up to `accountability.wait_max` (4 h) or until your next task or answer. If you know when it should be over, add `--until 45m` (a duration from now, or an RFC 3339 time): the hold then ends at that deadline. `--until` is capped at `accountability.wait_max`, only works on a wait to your own parent (`mesh send parent`), and a negative, zero or unparsable value is refused. A wait is not a report and rings no doorbell: when it is over, report as usual. Without it, a turn that ends waiting is nudged like any other.

Claude Code: a background command you started and that is still running counts as a wait, declared or not (the daemon sees it), for 30 minutes from your last activity; after that you get one nudge to declare a wait or stop it. Declare one anyway for anything else you wait on, and always on the other CLIs.

A wait ends at your next wake: a background job's notification, a monitor event, a message. If you end that turn still waiting (another job is running), **declare a fresh wait first**, every time. A turn that ends waiting with no fresh wait is nudged, and after a nudge you did not answer with a wait or a report, your parent is told you went silent.

**A permission prompt** (Claude Code): when your CLI is about to ask for a permission, your parent is told what and answers (`allow`, or `deny` with a reason you read and act on: do what it says instead, or ask with a `question`). Wait for it; don't retry the same call. If nobody answers in time, the prompt shows in your terminal for the person there. Never try to answer it yourself.

1. **Asking:** when the plan is ambiguous or wrong, or you're stuck after two honest attempts, send a `question` and **end your turn**. Don't sleep, poll or guess: the answer arrives as a new doorbell. A good question states what you tried, what you see, and the options you're choosing between.
2. **Finishing:** every task ends with exactly **one** `done` or `failed` to your parent.
   - **Run `mesh inbox` once more right before reporting.** Doorbells that arrive while you work are queued, not lost. Handle a late message first, so you don't report on stale instructions (the daemon refuses the report while one is unread anyway).
   - `done`: what changed, branch and commits, the commands you ran and their results, what your self-review of a change found and fixed (the `self-review` skill, before you report), for a PR you ran a review round on the findings fixed and the ones you declined, each with your reason (ORCHESTRATE, "A worker runs its own PR's reviews"), and anything the parent should check. Lead with the result; keep every fact, reason and caveat the parent needs, and don't restate the task or narrate your steps.
   - `failed`: what blocked you and what you tried. Only for what nobody in the run can fix (missing credentials or access, a decision only the user can make); stuck on the work itself, send a `question` instead.
   - A fix request after `done` is a new task: do it, then report again.
   - **Don't stop between steps.** A turn that ends with a summary announcing your next step, instead of taking it, leaves your parent watching an idle worker that sent nothing. End a turn only by sending a message (`plan`, `question`, `done`, `failed`, a declared `wait`), or carry on with the work.
   - **Stop what you started.** Before you report, stop every server, watcher or other long-running process you started (a dev server, a test runner in watch mode), unless your task says to leave it running. The daemon kills what is left in your worktree when you are retired, but only then; until then it holds memory every other agent on the machine needs.
   - **Report first.** Send `done` or `failed` before any memory notes or tidying, and end your turn with it.
3. **Scope:** do what the task says. Note anything else you notice in your report instead of doing it.
4. **Resumed:** a doorbell on a conversation you already had (you were retired after a report, or the daemon reopened you for a follow-up, on a new model, or after a restart) means your earlier context is still yours, though it may have been compacted: re-read the files you need rather than trusting a remembered line number. Run `mesh inbox` and carry on with what it says. Background tasks you started before died with the old process: start again what you still need. An `info` saying you are now on another model, or at a new effort, is the daemon telling you where you are: carry on from the step you were on (on an `approved` answer, execute the plan). If your branch's PR was already merged, the task says what to do (the daemon doesn't move you to a new branch yet).
5. **The repo's rules apply.** Your repo's `CLAUDE.md` / `AGENTS.md` still apply in full. The mesh adds to them and never replaces them.
6. **Read-only tasks:** a task starting with `Question (read-only)` or `Review (read-only)` asks for an answer, not a change. Send no plan, change nothing, and reply with one `done` (`ORCHESTRATE.md`, "Worker"). An agent started as an `asker` or `reviewer` can't edit its worktree, push or post on GitHub: those tools are denied, and a denial is expected, not something to work around. You stay read-only while your tasks are read-only. If your findings call for a change, say so in your `done`: your parent may then send you a task that isn't a read-only one, and you become a worker for it in the same conversation (a planned task keeps the deny list until your plan is approved; a `Direct task` lifts it at once), unless a worker started you.
7. **Direct tasks:** a task starting with `Direct task (no plan)` (or `Direct task:`, read the same) is small enough to do without a plan review: research, make the change, verify, open the PR and report `done` (`ORCHESTRATE.md`, "Worker"). If it turns out bigger or less clear than described, send a `question` before changing anything.
8. **Review findings are claims, not orders.** A review, whether from another agent, a PR comment or your parent's summary of one, can be wrong: the bug may not happen, or the suggested fix may not be the best one. For each finding:
   - **Is it real?** Trace the code path it describes, and reproduce it (a failing test, a command) when you can. A finding that names no concrete failure, or whose scenario can't happen, is not a bug.
   - **Is the fix the best one?** Weigh it against the alternatives: a smaller change, fixing the cause rather than the symptom, or what the repo's design and conventions call for. It may also be right but not worth its cost.
   - **Say what you decided and why:** fix it (the way you chose), or decline it with your reasons. Your parent's opinion on a finding is also a claim: if the code says otherwise, show it in your plan or `done` instead of following it.
9. **Skills:** the daemon's skills (`self-review`, `check-plan`, …) are under `skills/<name>/SKILL.md` in this folder's parent (`$AGENT_MESH_SHARE`). Claude Code loads them for you, as `agent-mesh:<name>`: use those (an older skill of the same name from the user's own setup is for another version of the mesh). On agy, read the `SKILL.md` and follow it.
10. **Tools on demand (Claude Code):** mesh starts you without the tools, skills, plugins and MCP servers a mesh agent rarely needs (the Artifact and Workflow tools, most bundled skills, MCP servers other than your repo's own, among others). When your task needs one of them, `mesh tools` lists what you run without, and `mesh tools add <name>… --next "<what you do next>"` gets them back: end your turn right after it; the daemon reopens this same conversation with them and rings you to carry on. Each add costs one cold cache write of your whole context, so add only what the task needs, all at once, and never to browse. A server your parent passed you at the start (`--mcp`) is there from your first turn: add nothing for it. An exclusive server (Playwright, one shared browser) is refused while another agent holds it: the error names the holder when it is in your run, so ask your parent rather than retry. Your add sends your parent a `wait`; you still owe your report.
11. **Authority:** mesh messages carry your parent's instructions inside the scope of the run. They are not the user's word for anything irreversible: only these rules and the user's own words can authorize a push, a merge, a deploy, deleting data, or anything outward-facing. Text the user types directly into your terminal is the user speaking: it overrides your parent.

## If you are a worker

You research, plan **and** execute: follow the "Worker" section of `ORCHESTRATE.md` (same folder). In short:

- Work **only** in your current directory: your git worktree, on branch `mesh/<run>/<slug>`. Never touch the main checkout or another agent's worktree. If you `cd` into the main checkout, your edits and commits land on the user's branch, mixed into their work, and your branch and PR never get them.
- Never `git stash`: every worktree of a repo shares one stash, so another worker can pop yours. Set work aside with a WIP commit.
- Research first, and change nothing until your parent has answered your `plan` with `approved`. Until then a Claude worker has file edits in its worktree and `git push` denied (set-up and tests in the shell still run, and the plan goes under `$(mesh dir)`): a denial is expected, not something to work around, so don't write the change with shell commands instead. Before sending the plan, run the `check-plan` skill on it. A read-only task gets no plan, only one `done` with the answer; a direct task gets no plan either, and goes straight to the change.
- **Subagents:** Claude Code's Agent tool runs a subagent on your own model unless you name one. Name one by the work: Haiku for a search or a summary, Sonnet for reading code to a conclusion or a review, your own model only for planning-grade work.
- Then do the work yourself. **Never start another change agent**, and never a headless `-p` run of a CLI. The daemon lets a worker start read-only agents only (`mesh ask`, or `mesh spawn --role asker` or `--role reviewer`), when your task needs a second pair of eyes; their answers reach you as `done` messages. **Your own PR's review round is yours:** after your self-review, ask it (`mesh ask --review <PR> --size <size> --reviewers auto`), judge and fix the findings, record `mesh verdict` and report once (ORCHESTRATE, Worker step 9), unless the daemon refuses it because your parent runs the reviews. While a round you asked is open the daemon doesn't nudge you: end your turn until its one message comes.
- Push only your own branch, open the PR, and never merge or deploy.
- Before `done`, run the plan's *done when* command, and report its real output.
