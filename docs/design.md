# Design notes

The decisions behind agent-mesh, roughly in the order they were made, and the alternatives that lost.

## 1. Interactive panes, not headless `-p`

Both CLIs have a print mode (`claude -p`, `agy -p`): run one prompt, print the answer, exit. It's the obvious way for one agent to drive another, and it has three problems:

- **Invisible:** you can't watch the work or type into it.
- **One-shot:** the agent can't stop to ask a question and keep its working context. You have to resume the conversation with `-c`, and "most recent conversation in this workspace" is ambiguous once several agents share a repo.
- **No back channel:** the child has no way to reach its parent.

herdr runs every agent as a normal interactive session, and its CLI can start an agent in a pane (`agent start`), type into it (`agent prompt`), read it and wait for state changes (`agent wait`: `working` / `idle` / `done` / `blocked`). That covers visibility and delivery. The rest of agent-mesh is the conventions on top.

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

**One workspace per project.** Agents open in the project's existing herdr workspace, found by label (case-insensitive) or by a pane whose directory is inside the repo, so a workspace labelled `shop` is found for `shop-api`. Each run gets one tab per workspace, labelled from the request (`mesh: add restockedAt to shop-api`). A workspace is created only when no workspace matches, and a lock stops two workers spawning at once from creating two.

## 5. Models are preferences, not code

Model ids go stale (Gemini 3.8 Flash, then 3.9…), so the skills name **families**:
- `argon` / `flash` / `opus` / `sonnet` for agy, which `mesh` resolves to the newest matching `gemini-<version>-<family>-<effort>` or `claude-<family>-<version>-<effort>` in `agy models`
- `opus` / `sonnet` for Claude, which the CLI resolves itself

The modes are commands rather than a flag, so the mode is never guessed from wording, and they're explicit-only (`disable-model-invocation`) so a casual "have agy do X" never starts a mesh. That limit is on meshes, not on the CLIs: a read-only ask ("have agy summarise repo B") is one headless `agy -p` from B's directory, with no panes, worktree or PR, and the global rule says so, so an agent doesn't refuse it.

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
- **The CLI is fixed:** a conversation can't move between Claude and agy, so changing agent means a fresh worker in the same worktree, as after a `failed`. The model can change within the same CLI.

## 9. Models per phase

Flash is cheap and good at typing code and running tests, but weak at the research and planning that decide whether the change is right. Since 0.5 a worker on `--model auto` (the default) plans on a strong model and executes on a cheap one, in **one conversation**:

- **The switch happens at approval:** the plan is the boundary between deciding and doing, and the root is already there to approve it. `mesh approve` waits for the worker to finish its turn, opens a pane next to it, closes the old one, and reopens the conversation with `agy --conversation <id> --model <flash>` / `claude --resume <id> --model sonnet`. It reuses the session lookup from §8, so the executor keeps the research. This is not the 0.2 cold hand-off.
- **Quotas, not a fixed model:** the user's strongest model (Argon) runs out for days at a time. For agy, the planning model is the first of Argon → Claude Opus (through agy's Claude pool) → Flash with at least `MESH_MIN_QUOTA`% (default 10) left in every limit that covers it.
  - The limits come from `agy -p /usage`, whose rows are a group, a label, the percent left and the reset time. A group ending in "models" covers ids by vendor prefix ("Claude and GPT models" → `claude-`, `gpt-`); any other group covers the model of that name ("Gemini 4 Argon").
  - Unreadable quotas never block a spawn: the chain's first model is used.
- **Detecting a quota hit:** agy keeps the session alive and logs `RESOURCE_EXHAUSTED … Resets in 138h17m29s`. Each agy worker gets its own `--log-file`, so `mesh watch` can scan only that worker's new log lines and exit 7. The root then runs `mesh switch` without asking: a quota hit says nothing about the task, so moving on can't hide a real failure. `switch` re-reads the quotas, skips every model that already hit its quota, and tells the worker to continue.
- **No fallback for execution:** the user chose Flash only for execution. A Flash quota hit stops the worker and the root reports the reset time, rather than spending the Claude pool on typing code.
- **Claude workers** plan on Opus and execute on Sonnet. Claude Code's usage isn't visible to mesh, so there is no quota choice or detection for them; `mesh switch --model` still works by hand.
- **The root's model is the user's:** mesh never changes the root session's model.

## Alternatives considered

| Option | Why not |
|---|---|
| [Claude Code agent teams](https://code.claude.com/docs/en/agent-teams) (lead, teammates, shared task list, mailbox, tmux panes) | One team per session and **no nested teams**, and Claude only, so agy can't join |
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

### Live test results (2026-10-23, the 0.2 planner → executor design)

| Test | Result |
|---|---|
| Doorbell + mailbox round trip, a question mid-task, a message sent while the agent was busy | ✅ queued, not lost |
| `/delegate-claude-agy`, single repo (root plans, one Flash executor, forced question) | ✅ 2m44s, no human |
| `/delegate-agy` typed into an agy root | ✅ 3m51s, no human |
| `/delegate-claude` across two repos, where B needs A's commit hash through the root | ✅ 4m38s: root → 2 planners → 2 executors |
| Executor pane killed mid-task | ✅ `mesh watch` exits 5 with an alert |
| `mesh stop` while an executor is working | ✅ pane closed, dirty worktrees kept and flagged, branches kept |
