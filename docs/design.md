# Design notes

The decisions behind agent-mesh, roughly in the order they were made, and the alternatives that lost.

## 1. Interactive panes, not headless `-p`

Both CLIs have a print mode (`claude -p`, `agy -p`): run one prompt, print the answer, exit. It's the obvious way for one agent to drive another, and it has three problems:

- **Invisible:** you can't watch the work or type into it.
- **One-shot:** the agent can't stop to ask a question and keep its working context. You have to resume the conversation with `-c`, and "most recent conversation in this workspace" is ambiguous once several agents share a repo.
- **No back channel:** the child has no way to reach its parent.

herdr runs every agent as a normal interactive session, and its CLI can start an agent in a pane (`agent start`), type into it (`agent prompt`), read it and wait for state changes (`agent wait`: `working` / `idle` / `done` / `blocked`). That covers visibility and delivery. The rest of agent-mesh is the conventions on top.

## 2. Planners live inside the repo

The first design had one central Claude writing plans for every repo. It was rejected: a planner outside the repo doesn't load that repo's `CLAUDE.md`, skills or hooks, and it would have to absorb every repo's conventions into one context window. In the final design the root spawns a **fresh** planner in each affected repo. It's fresh so that your existing sessions keep their own context and never receive mesh traffic.

The hierarchy also keeps the root's context small. Executors report to their planner, and the planner checks their work and sends the root only a short summary.

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
| root | `<repo>-lead` (`ai-boilerplate-lead`), unless you already named that session, in which case it keeps your name |
| planner | `<repo>-planner` (`curupira-bot-planner`) |
| executor | `<repo>-<worktree slug>` (`curupira-bot-fix-reactions`) |
| escalation in the same worktree | the same name plus `-2` (`curupira-bot-fix-reactions-2`) |

herdr requires names to match `[a-z][a-z0-9_-]{0,31}` and to be unique among live agents, so long parts are cut and a clash with another run gets `-2`, `-3`. The run id lives in the environment and in branch names (`mesh/<run>/<slug>`), not in agent names. `mesh stop` clears the name the root was given.

**One workspace per project.** Agents open in the project's existing herdr workspace, found by label (case-insensitive) or by a pane whose directory is inside the repo, so `walklore` is found for `city-tour-app`. Each run gets one tab per workspace, labelled from the request (`mesh: fix the reaction bug`). A workspace is created only when no workspace matches, and a lock stops two planners spawning at once from creating two.

## 5. Models are preferences, not code

Model ids go stale (Gemini 3.8 Flash, then 3.9…), so the skills name **families**:
- `flash`/`argon` for agy, which `mesh` resolves to the newest matching `gemini-<version>-<family>-<effort>` in `agy models`
- `opus` for Claude, which the CLI resolves itself

The modes are three commands rather than a flag, so the mode is never guessed from wording, and they're explicit-only (`disable-model-invocation`) so a casual "have agy do X" never starts a mesh.

## 6. The mesh is process-agnostic

Task trackers, branch conventions and PR checklists differ between repos. The mesh files never name any of them. A planner follows whatever its repo's `CLAUDE.md` / `AGENTS.md` says, because it runs inside that repo.

## Alternatives considered

| Option | Why not |
|---|---|
| [Claude Code agent teams](https://code.claude.com/docs/en/agent-teams) (lead, teammates, shared task list, mailbox, tmux panes) | One team per session and **no nested teams**, so it can't express root → planner → executors. Claude only, so agy can't join |
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

### Live test results (2026-10-23)

| Test | Result |
|---|---|
| Doorbell + mailbox round trip, a question mid-task, a message sent while the agent was busy | ✅ queued, not lost |
| `/delegate-claude-agy`, single repo (root plans, one Flash executor, forced question) | ✅ 2m44s, no human |
| `/delegate-agy` typed into an agy root | ✅ 3m51s, no human |
| `/delegate-claude` across two repos, where B needs A's commit hash through the root | ✅ 4m38s: root → 2 planners → 2 executors |
| Executor pane killed mid-task | ✅ `mesh watch` exits 5 with an alert |
| `mesh stop` while an executor is working | ✅ pane closed, dirty worktrees kept and flagged, branches kept |
