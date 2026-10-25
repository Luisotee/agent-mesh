# Design notes

The decisions behind agent-mesh, roughly in the order they were made, and the alternatives that lost.

## 1. Interactive panes, not headless `-p`

Both CLIs have a print mode (`claude -p`, `agy -p`): run one prompt, print the answer, exit. It's the obvious way for one agent to drive another, and it has three problems:

- **Invisible:** you can't watch the work or type into it.
- **One-shot:** the agent can't stop to ask a question and keep its working context. You have to resume the conversation with `-c`, and "most recent conversation in this workspace" is ambiguous once several agents share a repo.
- **No back channel:** the child has no way to reach its parent.

herdr runs every agent as a normal interactive session, and its CLI can start an agent in a pane (`agent start`), type into it (`agent prompt`), read it and wait for state changes (`agent wait`: `working` / `idle` / `done` / `blocked`). That covers visibility and delivery. The rest of agent-mesh is the conventions on top.

Since 0.7 small Claude tasks may run as Claude Code background sessions (`claude --bg`, §11). These aren't `-p` runs: each one is a full interactive session without a terminal, which keeps its conversation, takes messages, and can be attached to (`mesh open`). So all three objections still hold for headless runs, and none of them applies to background sessions.

## 2. Agents live inside the repo

The first design had one central Claude writing plans for every repo. It was rejected: a planner outside the repo doesn't load that repo's `CLAUDE.md`, skills or hooks, and it would have to absorb every repo's conventions into one context window. Instead the root spawns a **fresh** agent in each affected repo. It's fresh so that your existing sessions keep their own context and never receive mesh traffic.

It also keeps the root's context small: each repo agent sends up a plan and a short report, not its research.

## 3. Mailbox plus doorbell, not raw typing

Typing a message straight into a terminal is unreliable. Other projects hit the same problem: [initech](https://pkg.go.dev/github.com/nmelo/initech) exists because tmux `send-keys` has no delivery guarantee, and one lost "done" message stalls the whole chain. The failure modes are real:

- the recipient is stopped at a dialog (herdr rejects the prompt with `agent_blocked`)
- you're typing in that pane
- the TUI mangles a long paste

So the message goes to a file first, and the terminal only gets a one-line doorbell:

- `mesh send` retries the doorbell with backoff for about a minute
- if the doorbell still fails, `mesh send` exits 2, but the message is safe and `mesh ring` can retry later
- one-line doorbells make typing collisions much less likely
- the mailbox doubles as an audit log of who said what

A live test showed that a doorbell typed while agy was busy wasn't lost: agy queued it, as Claude Code does. The agent had already sent `done` by then, though, so it reported on instructions that were out of date. Hence the protocol rule: **empty your mailbox right before reporting**.

## 4. Addressing by herdr name

herdr shows Claude Code's session id for Claude panes but nothing for agy panes, so session ids can't address every agent. Every mesh agent gets a herdr name, and its pane id is kept as a fallback.

Names are meant to be read in herdr's agents tab, so they say what the agent is, not which run it belongs to:

| Agent | Name |
|---|---|
| root | `<repo>-lead` (`shop-web-lead`), unless you already named that session, in which case it keeps your name |
| worker | `<repo>-<worktree slug>` (`shop-api-restocked-at`) |
| restart in the same worktree (e.g. on a stronger model) | the same name plus `-2` (`shop-api-restocked-at-2`) |

herdr requires names to match `[a-z][a-z0-9_-]{0,31}` and to be unique among live agents, so long parts are cut and a clash with another run gets `-2`, `-3`. The run id lives in the environment and in branch names (`mesh/<run>/<slug>`), not in agent names. `mesh stop` clears the name the root was given.

**Beside the root first.** A pane worker opens as a split in the root's own tab: the first splits the root's pane, later ones split the biggest worker pane, so the root keeps its half and the user sees the workers from the space they're working in. The run's own tab in the project's workspace used to be the only placement, and it hid the work from the user. Past `MESH_SIDE_PANES` workers (3) the panes get too small to read, so the rest go to the run's tab below; a swap (approve, switch) splits the side worker's own pane, so it reopens where it was whatever the count; if the root's tab is gone, it goes to its project's workspace. Only panes still open count (one the user closed frees its place), and background workers have none. Placement holds a per-run lock (`.panes.lock`) from the count through the split to recording the pane on its agent: `/delegate-review all` starts three `mesh ask` calls in one turn (two reviewer panes and a Copilot peek pane), which would otherwise all count the same panes and split the root three times.

**Then one workspace per project.** Past that, agents open in the project's existing herdr workspace, found by label (case-insensitive) or by a pane whose directory is inside the repo, so a workspace labelled `shop` is found for `shop-api`. Each run gets one tab per workspace, labelled from the request (`mesh: add restockedAt to shop-api`). A workspace is created only when no workspace matches, and a lock stops two workers spawning at once from creating two.

## 5. Models are preferences, not code

Model ids go stale (Gemini 3.8 Flash, then 3.9…), so the skills name **families**:
- `argon` / `flash` / `opus` / `sonnet` for agy, which `mesh` resolves to the newest matching `gemini-<version>-<family>-<effort>` or `claude-<family>-<version>-<effort>` in `agy models`
- `opus` / `sonnet` for Claude, which the CLI resolves itself

The modes are two skills rather than a flag. They used to be explicit-only (`disable-model-invocation`), so a casual "have agy do X" never started a mesh. That made the user remember a command for the thing the mesh exists for, and an agent asked "have the shop-api agent fix it" would edit shop-api itself or refuse. Now agents invoke them like any skill when a request fits the description: work, a question or a review for another repo. The mode comes from the CLI the user names, or the agent's own CLI when none is named. Two limits stay: a worker never invokes them (1:N, §7), and outside herdr, where no mesh can start, a read-only ask is one headless `agy -p` from B's directory. Within a mesh, the root picks the model *size* per task (§11), but never a model family the user didn't ask for.

## 6. The mesh is process-agnostic

Task trackers, branch conventions and PR checklists differ between repos. The mesh files never name any of them. A worker follows whatever its repo's `CLAUDE.md` / `AGENTS.md` says, because it runs inside that repo.

## 7. From 1:N:N to 1:N

Up to 0.2 a run was three levels deep: root → a planner per repo → executors in worktrees. In practice the planner did the expensive part, reading the repo until it understood the change, and then handed a written plan to an executor that started cold and had to rediscover the same code. Plans can't carry everything research learns, so executors asked questions the planner already knew the answers to, and the hand-off cost a second round of reading.

Since 0.3 a run is the root plus **one worker per other repo**. The worker researches, writes the plan, and sends it to the root as a `plan` message. It changes nothing until the root answers `approved`; then **the same agent** executes, verifies and opens the PR. Review moved up a level, from planner-reviews-executor to root-reviews-plan, and it now happens *before* any code is written.

The typical run is two repos: you're working with an agent in repo A and find that the change also needs repo B. The root therefore keeps its own repo, working in your checkout as before, and spawns a worker only for each *other* repo. A worker for the root's own repo is started only when you ask for one.

Until 0.4 the default worker was the newest Gemini Flash at **high** effort, for the whole task. Since 0.5 a worker plans on a stronger model and executes on Flash (§9). A model named by the user still runs the whole task, and after a `failed` the root only suggests a stronger model.

## 8. Resuming workers

Since 0.4 a worker's conversation outlives its run. The usual follow-ups (a PR review to address, "make it Y instead of X") are about work a worker already researched, and briefing a fresh worker repeats the cold hand-off that 0.3 removed. So `mesh resume` reopens **the same conversation** as a worker of the current run, and `mesh open` reopens it in the user's own terminal.

- **Finding the conversation:**
  - **Claude Code** takes `--session-id`, so `mesh spawn` picks a UUID, passes it, and records it in `run.json`. `--resume <id>` reopens it.
  - **agy** can't be given an id, but its `history.jsonl` logs every submitted prompt with its `conversationId` and directory. Mesh's first doorbell names the agent and the run (`You are <name> (worker) in agent-mesh run <run>`), so it pins the conversation even when a restart shares the worktree. `--conversation <id>` reopens it.
  - **Older runs:** Claude workers spawned before 0.4 are found the same way, through the doorbell in their session file.
  - **Main checkouts:** for a worker that ran in a main checkout, only the doorbell counts. A "last prompt in this directory" fallback there could pick up the user's own session.
- **Same path:** Claude stores sessions per working directory, so a worktree that `mesh stop` removed is recreated **at its old path**, on its kept branch. It joins the new run's worktree list, so that run's `mesh stop` cleans it up again.
- **Finding the worker:** the root and the user think in PRs, not agent names. `mesh agents`, `resume` and `open` accept a name, a branch or a PR link, and PR links are read from the worker's own messages (its `done` report). When several runs match, the newest wins.
- **One process per conversation:** both commands refuse while a live agent sits in the worker's worktree. Two CLIs on one conversation would fork it.
- **`mesh open` is the user's session:** it strips the `MESH_*` environment, and it doesn't add `--dangerously-skip-permissions`, because the user is at the keyboard and their normal settings apply. The protocol tells an agent that finds itself outside a run to work with the user directly.
- **Untouched branches:** since 0.6 a worktree records the commit it started from. `mesh stop` deletes a branch only if it still points at that commit and has no upstream, which is what an asker (§10) leaves behind. Deleting it loses nothing, and resume starts it again at the same commit, at the same path.
- **The CLI is fixed:** a conversation can't move between Claude and agy, so changing agent means a fresh worker in the same worktree, as after a `failed`. The model can change within the same CLI.

## 9. Models per phase

Flash is cheap and good at typing code and running tests, but weak at the research and planning that decide whether the change is right. Since 0.5 a worker on `--model auto` (the default) plans on a strong model at high effort and executes at medium effort, in **one conversation**. agy workers also move to the cheap Flash; Claude workers stay on Opus:

- **The switch happens at approval:** the plan is the boundary between deciding and doing, and the root is already there to approve it. `mesh approve` waits for the worker to finish its turn, opens a pane next to it, closes the old one, and reopens the conversation with `agy --conversation <id> --model <flash>` / `claude --resume <id> --model opus --effort medium`. It reuses the session lookup from §8, so the executor keeps the research. This is not the 0.2 cold hand-off. For Claude the model doesn't change, so the reopen exists only to pass `--effort medium`: don't skip it when the model matches. Approve skips it only when model and effort both match (a named `--effort` covering both phases).
- **Quotas, not a fixed model:** the user's strongest model (Argon) runs out for days at a time. For agy, the planning model is the first of Argon → Claude Opus (through agy's Claude pool) → Flash with at least `MESH_MIN_QUOTA`% (default 10) left in every limit that covers it.
  - The limits come from `agy -p /usage`, whose rows are a group, a label, the percent left and the reset time. A group ending in "models" covers ids by vendor prefix ("Claude and GPT models" → `claude-`, `gpt-`); any other group covers the model of that name ("Gemini 4 Argon").
  - Unreadable quotas never block a spawn: the chain's first model is used.
- **Detecting a quota hit:** agy keeps the session alive and logs `RESOURCE_EXHAUSTED … Resets in 138h17m29s`. Each agy worker gets its own `--log-file`, so `mesh watch` can scan only that worker's new log lines and exit 7. The root then runs `mesh switch` without asking: a quota hit says nothing about the task, so moving on can't hide a real failure. `switch` re-reads the quotas, skips every model that already hit its quota, and tells the worker to continue.
- **No fallback for execution:** the user chose Flash only for execution. A Flash quota hit stops the worker and the root reports the reset time, rather than spending the Claude pool on typing code.
- **Effort follows the phase too:** high while planning, where thinking pays, and medium (agy's own default) while executing an approved plan. agy bakes the effort into the model id (`gemini-3.8-flash-medium`); Claude takes `--effort medium` on the resume.
- **A resumed worker** gets the same two phases when its follow-up task has a plan: it plans where a spawn on `auto` would (the plan chain with quota, at high effort), not on the model it last ran on, and `mesh approve` moves it to the execution model and effort. An asker that answered on Flash and then gets a feature would otherwise plan it on Flash, which is where the plan matters most; a worker coming back from an earlier run is compacted anyway, so the model change costs little. It used to keep one model throughout, so a resumed agy worker executed a whole review's fixes at high effort, which is the cost the phases exist to avoid. A direct task, a question, or a named `--model` / `--effort` gets no phase, as in a spawn.
- **Claude workers** plan and execute on Opus; only the effort changes. Prompt caches belong to one model, so a switch to Sonnet would re-read the whole planning context on the first execution turn, and most of a worker's tokens are cache reads ($0.20/M on Opus 5.5, $0.10/M on Sonnet 5.5), leaving Sonnet a small saving for a weaker executor. A resume that changes only `--effort` keeps the cache: measured on 2026-10-24, the medium-effort resume read the full previous prompt from cache and wrote 65 tokens. Claude Code's usage isn't visible to mesh, so there is no quota choice or detection for them; `mesh switch --model` still works by hand.
- **Opus at every size, medium by default (2026-10):** small Claude tasks used to run on Sonnet at low or medium effort to save tokens. Once quality mattered more than tokens, the measured numbers decided it (Anthropic's sweep on a SWE-bench Pro subset):
  - Opus 5.5 solves 87.4% at low ($0.11 a task), 92.8% at medium ($0.21), 95.3% at high ($0.29) and 96.7% at xhigh ($0.74). Sonnet 5.5 at medium scores about 7 points below its own high, and Sonnet at high (87.6%) only matches Opus at low. So every Claude size runs Opus, at medium.
  - Raising effort mostly buys verification and edge cases, not a better approach. Anthropic measured that running at medium and re-running only the failures at high (about 97%) beats running everything at high, and the mesh already has the failure signal: plan reviews and PR reviews. So effort goes up where checking is the job (planning, reviews), and for the fix after a real finding or a `failed`, not for every task. That fix is a `mesh resume`: Claude with `--effort high`, agy with `--model argon`, since a named effort alone would keep an agy worker on Flash.
  - xhigh and max aren't defaults: Anthropic recommends them only where evals show a gain, and max is prone to overthinking. Claude Fable 5.1 isn't one either: Max plans cap it at half the weekly limit, and other models can't read its thinking, so a Fable planner handing over to an Opus executor would lose its reasoning. It runs only when the user names it, for a whole task.
- **The root's model is the user's:** mesh never changes the root session's model.

## 10. Questions and reviews are workers

Not every cross-repo need is a change. Often the root needs to *know* something about another repo ("how is restockedAt computed?"), or wants a second pair of eyes on a PR it just opened. The global rule already allowed a headless `agy -p` for that (§5), but a headless answer is a dead end. You can't watch it, it can't ask back, and its context is lost for the next question or for the change the answer leads to. Picking that conversation up later would need a second lookup path next to §8's.

Since 0.6 `mesh ask` gives the question to an **ordinary worker** instead. It's spawned exactly like one (same role, same first doorbell, same `auto` plan phase), and its first task starts with `Question (read-only)` or `Review (read-only)`. The protocol says that such a task gets no plan and no edits, only one `done` with the answer. Everything else comes for free:

- **Follow-ups** are more `mesh ask` calls to the same agent, which still has everything it read. Once it's closed (below) or after `mesh stop`, `ask` resumes it (§8).
- **Turning it into a change** is a plain task. The asker is still in its plan phase, so it plans, `mesh approve` moves it to its execution model, and it opens a PR from the branch it already has. That's the same "no cold hand-off" argument as §7.
- **Existing workers answer too:** the agent that wrote a PR is the best one to ask why. An existing worker reviewing a PR mustn't leave its branch, so it reads the PR from `gh pr diff` / `FETCH_HEAD`. A fresh reviewer gets a worktree at the PR head, so it can run the tests. Every review task carries the PR's diff (up to 100,000 characters; a longer one is left for the reviewer to read), so the reviewer starts from the whole change instead of piecing it together over its first turns.
- **Reviews run at high effort:** a fresh trivial or small reviewer gets `REVIEW_EFFORT` (high) instead of its size's effort, and a large one plans at `DEFAULT_EFFORT` (high) anyway. Checking is all a reviewer does, and its findings decide whether a fix round happens (§9). REVIEW_EFFORT isn't passed to a large reviewer: it would read as a named effort and carry into the execution phase of a fix the reviewer is later given. The size still picks its plan phase and view, and a named `--effort` wins. Copilot keeps its own per-size effort.
- **One command:** the cost of a question was the ceremony (init, task file, spawn, watch, inbox), not the model time. `ask` starts a run if there's none, delivers, waits like `mesh watch` (same exit codes) and prints the answer. A Claude root runs it in the background and gets the answer as the notification.

Two details keep this from leaving a mess:

- **Reviewer vs author:** a reviewer's `done` quotes the PR link, which used to make `mesh resume <PR>` pick the newest match, the reviewer. Workers now record the PRs they reviewed, and PR matching skips those.
- **Empty branches:** every question creates a branch with no commits. `mesh stop` deletes those (§8).
- **Idle agents:** an asker used to stay open after answering, in case of a follow-up, so panes piled up for the rest of the run. Since resuming is cheap and keeps the whole conversation, `ask` now closes an agent it opened (started or resumed) once it has answered, and the next `ask` reopens it under the same name. A worker that was already open, mid-change, is left alone, and `--keep` overrides. When the answer comes by `SendMessage` or `--no-wait`, `ask` has returned before it, so the protocol tells the root to retire the agent itself, as it does a change worker it has nothing more for.

**Findings are claims.** Reviewers are often wrong: they flag code paths that can't happen, or suggest a fix that treats a symptom or fights the repo's design. An agent that applies every finding turns a review into a source of bugs. So the protocol (PROTOCOL.md rule 9) makes every agent that receives findings, the root deciding on them and the author fixing them, check that each bug is real and that the fix is the best one, and say which it fixes, how, and which it declines and why. The root's own opinion on a finding is held to the same test. The reviewer is asked for a concrete failure per finding and to mark what it couldn't confirm, so each claim can be checked.

Reviews post nothing on GitHub. That's outward-facing, so it needs the user's own word (PROTOCOL.md, "Authority").

**One command for reviews.** A review the user asks for used to go through `/delegate-claude` or `/delegate-agy`. That named the wrong thing ("delegate to Claude" when Claude only reviews), and it loaded the whole orchestration procedure for one `mesh ask`. Worse, three skill descriptions all claimed "review a PR", so which one fired was a toss-up. Now `/delegate-review` is the only skill whose description covers a review the user asks for. It takes the reviewers as arguments (any of `claude`, `agy`, `copilot`, or `all`), so there's one skill and not one per CLI, and it reads only "Questions and reviews". Several reviewers run at once, one `mesh ask` each, in one run the skill makes sure exists first, and each gets its own `--name` (`review-<n>-<cli>`): two `ask`s for the same PR would otherwise race for the same name and worktree. The skill then merges what they found; a finding several reviewers agree on is likelier, but still checked. The delegate skills keep the reviews their runs give their own workers' PRs, since those are part of supervising, not something the user asks for.

**Copilot reviewers are headless.** Copilot is a third reviewer, used when the user names it (on a student plan it's a fallback once Claude and agy limits are spent, so mesh puts no credit cap on it). It breaks two rules above on purpose, and only for read-only tasks:

- **No pane (§1):** herdr could start Copilot in a pane (`--kind copilot`), but a pane worker has to follow the mesh protocol and report with `mesh send`, and the global rules that teach it that are installed only for Claude Code and agy. Headless, Copilot needs none of it: its JSON output carries the answer and the model, so mesh files the answer itself. The cost is that nobody can step in, which a read-only review can do without. Watching is `mesh peek <name> [--follow]`, which prints its tool calls and messages from the events file Copilot writes per session (`~/.copilot/session-state/<id>/events.jsonl`). The user wants to see a review work, so `mesh ask` opens a pane beside the root running `mesh peek --follow` for as long as Copilot answers, and closes it after (`mesh stop`, or the next `ask` of that reviewer, closes one a killed `ask` left). It is a view only, never a second agent, and only beside the root: when that tab is full it is skipped rather than sent to the run's tab, where nobody is looking. `mesh ask` runs `copilot --output-format json` in the reviewer's worktree, with the prompt on stdin, and files the last thing it said as its `done` (`failed` on a non-zero exit or no answer). While it runs, `mesh status` and `mesh watch` see it as `working`, and `mesh stop` refuses to remove its worktree and archive the run from under it. The reviewer still gets the PR head checked out, the diff in its task and the repo's `AGENTS.md` / `CLAUDE.md`, which Copilot loads on its own.
- **Not a dead end anyway (§10's objection):** mesh picks the session id at spawn and passes the same `--session-id` on every ask: Copilot starts the session the first time and resumes it after that, so follow-ups keep what it read. `mesh open` reopens it interactively (`--resume`). While an ask runs, the entry carries the `mesh ask` process's pid, so a second `ask` or an `open` on the same conversation is refused; a `running` whose pid is gone (a killed ask) counts as stale. `send`, `ring`, `approve` and `switch` refuse a Copilot asker outright, since nothing reads its mailbox and it has no pane to swap.
- **The prompt goes on stdin,** not as `-p <task>`: a review's task carries up to 100,000 characters of diff, and Linux caps a single argument at 128 KiB. A line at the end tells Copilot that there is no `mesh send` and its final message is the answer, since it never reads PROTOCOL.md. What it can't do is take the change: `mesh resume` refuses it, and the change goes to the PR's author or a fresh worker.
- **Read-only** is the review prompt plus `--deny-tool`: `write`, and the shell commands that would publish or merge (`git push`, `gh pr review` / `comment` / `merge` / `edit` / `close`, `gh issue comment`). A shell rule needs the `:*` form (`shell(gh pr comment:*)`): Copilot ignores a bare three-word `shell(gh pr comment)`, checked live. Shell stays allowed otherwise so it can run tests, and a shell command can still write (`>`, `sed -i`, `git commit`), which is the same trust the other reviewers get. Unlike theirs, a slip happens out of sight, but only in its own throwaway worktree, which `mesh stop` keeps when it's dirty.
- **Models:** Copilot Student offers only `--model auto`, which refuses an effort but takes a routing profile (`--auto-tier intelligence`). A named model (`--model`, `MESH_COPILOT_MODEL`) gets `--reasoning-effort` from `--size`. mesh records the model Copilot reports it used.

## 11. Native transport, sizes and task lists

By 0.6 mesh had its own copies of things Claude Code now does natively: typing doorbells into panes, polling `mesh watch`, and one fixed model per mode. 0.7 uses the native pieces where they're better, and keeps mesh where they don't reach (agy, mixed runs, quotas, resume across runs).

- **The mailbox stays the record, `SendMessage` becomes the transport.**
  - Between two Claude sessions, the doorbell goes by Claude Code's cross-session messaging instead of being typed into a pane. A worker's report no longer lands in the middle of what the user is typing in the root's pane, and a background worker, which has no pane, can be reached at all.
  - Every message is still written to the mailbox first. PR lookup (§8), `ask`'s reply, the watchdog and the audit trail all keep reading the mailbox, and an agy agent, which can't use `SendMessage`, talks to a Claude one exactly as before.
  - mesh itself can't call a tool, so `mesh send` hands the sender a `send_message` to pass on.
- **Address by socket, not by name.** Claude Code retitles a session as its conversation moves on, and a send to the old name fails. Every session also listens on `cc-socks/<pid>.sock`, and `SendMessage` accepts that address.
  - `mesh init` finds the root's socket by walking up from its own process to the Claude Code process.
  - A background worker's socket comes from the pid in `claude agents --json`.
  - A socket that's gone (the root restarted) falls back to the doorbell.
  - The socket belongs to the process, not the conversation, so a root reopened with `--resume` has a new one. Every mesh command the root runs re-records its current socket (`refresh_root_address`), so a reopened root heals on its next `mesh inbox` or `watch` with no extra step.
- **Panes for large work, background for small.** The user wants to oversee the work that matters in herdr, but doesn't need a pane for a translation fix. Large tasks keep panes. Trivial and small Claude tasks run as `claude --bg` sessions: recorded workers like any other (resume, ask, PR lookup), visible in `mesh status` and `claude agents`, and `mesh open` attaches to one. agy has no background sessions, so agy workers always get panes. A fresh reviewer gets a pane at any size too: a review is the work the user wants to see, and a pane costs the same tokens as a background session (same session, model and effort), only screen space.
- **Background quirks** (from the smoke test below), and what mesh does about them:
  - `--bg` picks its own session id, so mesh reads it back from `claude agents --json`.
  - `--bg --resume <id>` with no flags continues the session with the options it was first started with. With flags it starts a copy of the conversation under a new id. So every background relaunch passes the full flags, and mesh records the copy's id. The copy has the whole conversation, so this is not a cold hand-off.
  - **The launch's environment never reaches the session.** `claude --bg` hands the session to Claude Code's daemon, which runs it in a pre-forked spare process. The first `claude --bg` with no daemon running starts one with *its* environment, and every spare after it inherits that until the daemon's last session exits. So one worker's `MESH_*` identity ended up in every later background session: other workers, and once the root's own conversation reopened in the background, which then took itself for a reviewer. (The smoke test missed it because it started a cold daemon.) mesh now runs every `claude` CLI call (`--bg`, `agents`, `stop`, the compaction turn) without the caller's identity, herdr pane ids or Claude session variables (`claude_env`), so a daemon any of them starts is clean; `--bg` also loses all `MESH_*`. It gives the session its environment through `--settings '{"env": …}'`, which reaches the spare and overrides its values: the identity, and every `MESH_*` config key (`MESH_CONFIG`), empty when the launch has none, since settings can only set a variable and an empty one counts as unset. The herdr pane ids are set empty for the same reason. Everything else (PATH, a virtualenv, direnv or repo-specific variables, tokens) still comes from whoever started the daemon.
  - **Safety net:** `identity()` ignores a `MESH_RUN` / `MESH_SELF` from the environment when it names a Claude worker whose recorded session isn't this one (`CLAUDE_CODE_SESSION_ID`, which Claude Code sets for its tools), and falls back to the pane map. A `MESH_RUN` that names no live run is ignored too: a daemon outlives the run whose worker started it. A pane worker's session can change legitimately (`/clear`, plan mode's "clear context"), so in its own pane it is trusted when herdr reports this session running there (`agent_session`), and the entry is re-recorded; when herdr can't say, the pane is trusted. An entry still `starting` is skipped for five minutes, since its new session isn't recorded until the launch returns. A name passed with `--as` is never second-guessed. When the pane map then resolves, the command acts as that pane's agent, run and name together, says so on stderr and logs `identity_ignored`. Not covered yet: a daemon started from the root's pane by the user's own `claude --bg` carries that pane's id, which maps to the root, whose session isn't recorded.
  - `--bg` refuses a folder Claude Code was never trusted in. A worktree of a trusted repo counts as trusted; otherwise the worker opens in a pane, where the trust dialog is handled as before.
- **No plan gate for small tasks.** A plan review costs a round trip and a strong model's research, which is worth it for a feature and not for a typo. Trivial and small tasks are `Direct task (no plan)`. They still run on their own branch, verify, and open a PR that the root (or a reviewer) checks. The worker must ask first if the task is bigger than described, and a wrong size costs one redo.
- **Sizes are a table, not a judgement call in code.** `SIZE_DEFAULTS` maps (CLI, size) to model, effort and view. The root picks the size; the user's named model or effort still wins.
- **User-reviewed plans.** `--model opus --permission-mode plan` does natively what `mesh approve` does for Claude: Opus plans, and Claude Code leaves plan mode after approval and executes on Opus, in the same conversation. It used `opusplan` (Sonnet executes) until quality outranked tokens (§9). With no `mesh approve` there's no execution phase, so the task runs at high effort throughout: the exception to "execute at medium", accepted because the user chose to supervise this one. herdr shows the waiting dialog as `blocked`, so the watchdog excuses that one worker. It's opt-in, for when the user wants to approve a plan themselves. By default the root reviews plans and Claude stays on Opus (§9).
- **Task lists:** a list of tasks makes the root a supervisor.
  - It triages into a board the user confirms once, writes no code itself, runs at most four workers at a time, and gives each PR a reviewer.
  - It checks conflicts with `git merge-tree --write-tree` (`mesh conflicts`), which needs no checkout, and skips stacked pairs.
  - The board lives in the run directory, so a root whose context was compacted can rebuild its picture from it instead of from memory.
- **What we don't use:**
  - Claude Code's `-w` worktrees: mesh's worktrees record their base, name their branch after the run, and are recreated at the same path on resume, which Claude needs to find the session (§8).
  - `/batch` and agent teams: a worker never spawns agents (§7), teams can't include agy, and neither can be resumed across runs.
  - `notify_when_idle` for stalls: it works, but each subscription is one-shot and its notice waits until the root's turn ends, so a busy root would have to re-subscribe to every worker after every turn. `mesh watch --any --alerts` covers every worker with one background command, reads `claude agents` for background workers, and also catches agy quota hits.

## 12. Compaction on resume

Resuming a worker is the point of §8, but an old conversation is expensive to resume. Its prompt cache expired long ago, so the first turn re-reads everything at full price, and every later turn re-reads it from cache. A 150k-token conversation resumed for a 30-turn fix reads about 4.5M cached tokens before it does anything new.

- **Compact first, headless:** before reopening, mesh runs `/compact <focus>` on the stopped conversation in print mode, in the worker's own directory, without its `MESH_*` identity so the compaction turn can't message anyone: `claude -p --resume <id>` / `agy --conversation <id> -p`. Both CLIs expand slash commands in print mode. The worker then reopens on the compacted conversation, in a pane or in the background as before.
- **Only when it pays:**
  - **Claude:** mesh reads the context size from the session file (the last reply's `usage` since the last compact boundary), and compacts above `MESH_COMPACT_AT` (300k). For a read-only question the threshold doubles, since a question or two doesn't repay a compaction. It was 100k until every model had a native 1M window and cache reads fell to $0.20/M on Opus 5.5: compaction throws away the detail of the worker's research, which is what resuming is for, so it now waits until the re-reads are large.
  - **agy:** it shows no context size, so a conversation is compacted when it comes back from an earlier run, and a read-only question skips it.
  - **Within a run:** approve and switch never compact; the context is warm and the task is the same.
- **Focused on the next task:** the default focus keeps the repo facts, the decisions and their reasons, the branch and the PR, and names the new task. The root can replace it with `--focus` when it knows what matters.
- **Never a failure:** a compaction that fails is a note, and the worker resumes as it was.

## Alternatives considered

| Option | Why not |
|---|---|
| [Claude Code agent teams](https://code.claude.com/docs/en/agent-teams) (lead, teammates, shared task list, mailbox, tmux panes) | One team per session and **no nested teams**, Claude only (agy can't join), no resume, and teammates' plans are approved automatically |
| Only Claude Code's own background agents (`claude --bg`, agent view, `SendMessage`) | Used since 0.7 for small Claude tasks (§11), but they can't include agy, pick quota-based models, or find a worker by its PR across runs |
| MCP bridges that start other CLIs as tools ([agent-link-mcp](https://socket.dev/npm/package/agent-link-mcp), [weft](https://github.com/cipher982/weft)) | Workers run as headless subprocesses, so you can't watch them or type into them |
| [Tmux-Orchestrator](https://www.sourcepulse.org/projects/11036576), [codex-orchestrator](https://www.sourcepulse.org/projects/26815350) | The same idea (an orchestrator → manager → engineer tree; Claude planning while Codex executes). They validate the shape, but they're built on raw tmux with no agent-state detection and no mailbox |
| A message broker or daemon | One more process to run. The filesystem plus herdr is enough at this scale |

## Lessons from the live tests

- **Trust dialogs:** a fresh worktree triggers a folder-trust dialog in both CLIs. agy highlights "Yes" by default, while Claude Code highlights **"No, exit"**. Blindly pressing Enter would quit Claude, so `mesh` finds the cursor and moves it to "Yes" before confirming. It does this only for trust dialogs, in directories mesh chose.
- **Late messages:** doorbells that arrive while an agent is working are queued and handled after the current turn. They aren't lost.
- **Dirty worktrees:** a worktree with untracked files counts as dirty, and `mesh stop` keeps it. That's the safe default, and repos that ignore their caches don't hit it.
- **Typing can be lost silently.** A prompt typed while Claude Code was still redrawing after its trust dialog disappeared, and herdr reported success. Every doorbell now uses `agent prompt --wait --until working`: an idle agent that doesn't start working within 5 seconds didn't receive it, so mesh retries. A busy agent counts as delivered, because it queues the doorbell.
- **`agy models` hits the network and can hang**, and it ignores SIGTERM. One failed TLS handshake to Google stalled a spawn. The list is now cached for 6 hours, a stale cache is used when a refresh fails, and an exact model id skips the lookup.
- **Side effects come last.** A spawn that failed while resolving the model had already created its worktree. Everything that can fail without side effects now runs first.

### Smoke test of the native pieces (2026-10-24, Claude Code 2.1.310, agy 1.4.7)

| Check | Result |
|---|---|
| `claude --bg` started from a Bash tool call, in a worktree of a trusted repo | ✅ runs in that directory and loads its `CLAUDE.md`. It ignores `--session-id`, and refuses a folder that was never trusted |
| Environment of a background session | ✅ it inherits the launching process's env (`MESH_*` work). Only with a cold daemon: a running one hands out spares with its own env (see Background quirks), so mesh now uses `--settings` env |
| Background session → root with `SendMessage` | ✅ by its current name or by `uds:…/cc-socks/<pid>.sock`. ❌ by a name the root had before Claude Code retitled it |
| Root → background session with `SendMessage` | ✅ wakes an idle session |
| `notify_when_idle` | ✅ one notice per subscription, but delivered only once the subscriber's turn ended (messages arrive mid-turn) |
| States in `claude agents --json` | `status` is busy / idle / waiting while the process runs and is absent once it exits; `state` is a looser label |
| `claude stop`, then `--bg --resume <id>` | without flags it continues the same id with the saved options *and environment*; with flags it starts a copy with the full conversation and new model (a new environment only with a cold daemon; mesh now uses `--settings` env) |
| `/compact <focus>` in print mode | ✅ Claude writes a compact boundary; agy logs `expanded slash command "compact"` |
| `opusplan` in plan mode in a herdr pane | ✅ herdr shows `blocked` at the plan dialog; Opus planned, Sonnet executed after approval (mesh now uses `opus`, not yet tested live) |

### Live test results (2026-10-23, the 0.2 planner → executor design)

| Test | Result |
|---|---|
| Doorbell + mailbox round trip, a question mid-task, a message sent while the agent was busy | ✅ queued, not lost |
| `/delegate-claude-agy`, single repo (root plans, one Flash executor, forced question) | ✅ 2m44s, no human |
| `/delegate-agy` typed into an agy root | ✅ 3m51s, no human |
| `/delegate-claude` across two repos, where B needs A's commit hash through the root | ✅ 4m38s: root → 2 planners → 2 executors |
| Executor pane killed mid-task | ✅ `mesh watch` exits 5 with an alert |
| `mesh stop` while an executor is working | ✅ pane closed, dirty worktrees kept and flagged, branches kept |
