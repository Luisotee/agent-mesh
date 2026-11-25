# Apps: desktop, mobile and web

Status: planning (2026-11-25). Nothing is built yet. The technology picks below are recommendations, chosen for quality over simplicity, to be revisited before each part is built.

This is a refactor of agent-mesh, not an add-on: where this plan and `docs/design.md` or `CLAUDE.md` disagree, this plan wins. design.md stays as the record of what today's `bin/mesh` learned, and its lessons apply unless this plan overrules them.

agent-mesh moves from herdr panes and a terminal CLI to its own apps. Windows, Linux and Android are required; macOS, iOS (once someone pays for the Apple developer account) and a browser client would be nice to have. herdr and the terminal workflow are replaced, not kept alongside: there is one user so far, so nothing legacy has to survive.

## Principles

- **Self-hosted and free.** Everything runs on the user's own machines and VPS, or on free tiers. Nothing depends on a service run by us: the relay and the push server are ones the user hosts.
- **Agents can do everything the UI can.** Every UI action is an API action, settings included, and so is opening views in the user's app. The UI has no abilities of its own.
- **Every action records its actor.** Each action is logged with who did it, a device or an agent by name, so it's always clear who approved or changed what.
- **Our own product.** Paseo and stablyai/orca (both registered peers) are sources of ideas, not templates. We don't build a smaller copy of them, and we don't cap the scope at theirs.

## Architecture

```
 desktop app (Electron + React) ─┐                       ┌─ daemon at home (WSL), dials out
 Android app (Kotlin + Compose)  ┼─ ConnectRPC over TLS ─┤   ├─ action registry (protobuf)
 agents' `mesh` CLI (Go)  ───────┘   direct, via the      │   ├─ agents in PTYs (real CLIs)
                                     user's relay, or     │   ├─ SQLite: runs, mail, events
                                     Tailscale / a VPN    │   └─ push via UnifiedPush (ntfy)
                                                          └─ daemon on the VPS, ...
```

- **One daemon per host** owns the agent processes, the run state and the API. It replaces herdr and the parts of `bin/mesh` that read and type into panes. A host is the user's machine (WSL included) or a VPS. An app can connect to several hosts at once.
- **Agents run as their real CLIs in PTYs the daemon owns** (Claude Code, agy, Codex, OpenCode, Copilot). Every CLI keeps all its own features (rewind, slash commands, its UI), and a new CLI works on day one.
  - Terminals live in a separate process from the daemon, so they survive a daemon restart (both peers ended up there).
  - State comes from the CLIs' hooks, session files and sockets, never from reading the screen: Orca and our own `human_draft` and limit-screen bugs show how brittle screen reading is.
  - Where a CLI offers a structured channel (Claude's stream-json and inbox socket, agy's ACP, OpenCode's HTTP API, Codex's JSON mode), the daemon uses it as well, which enables a structured chat view.
  - Each agent can toggle between that chat view and its terminal (Orca's Chat | CLI switch). One process writes a conversation at a time: the chat view of a PTY agent renders from its transcript and hooks, and its composer delivers into that same running process (Claude's inbox socket where it exists, else the PTY). Never a second process on the same conversation; moving an agent between a PTY and a headless structured channel is a handover (stop one, `--resume` in the other).
- **One action registry, defined as protobuf schemas.** Each action has a name, a request and response schema, an actor permission and a handler. ConnectRPC generates the Go server, the desktop's TypeScript client and the Android app's Kotlin client, and the agents' `mesh` CLI is generated from the same schemas, with no per-command code. Agents call it from their shell, which needs no setup per CLI and costs context only when it's used. An MCP server can be generated from the schemas later if a client needs one.
  - A test fails when an app calls something outside the registry.
  - UI-control actions are actions too: open a view, focus an agent, show a diff, notify.
  - Protobuf's rules for evolving a schema (new fields optional, numbers never reused) plus capabilities the host advertises are the answer to the version drift both peers fought.
- **Live events** are a server stream on the same API, resumable ("Protocols and lifecycle"). The event log is the run's log, as `log.jsonl` is today.
- **Terminals get their own binary stream** per terminal (a WebSocket, message types defined in the protobuf schemas), not ConnectRPC calls: Connect has no client or bidirectional streaming in the browser or Electron's renderer, and terminals sharing one HTTP/2 connection stall behind a busy one (emdash #3426: 9 s typing echo with 8 busy agents). Every peer that streams terminals does this.
- **One store: SQLite.** Runs, agents, mail, the event log and full-text session search live in one database file per host. Plans and reports stay Markdown files that agents write, referenced by path. design.md's lesson is against *two* stores that drift, not against a database.
- **Agents spawn agents.** Any agent may start and supervise its own children, not only the root (this overturns design §7's 1:N rule). Nesting is capped by limits a child can't raise: depth, children per agent and agents per run, from host settings, plus message-rate windows against storms (omnara's caps, AionUi's storm windows). Status, usage and cost roll up the tree, and the lead-agent view shows it.
- **Settings are data on the host,** the same from every device. Today's `POLICY` leaves become the host settings schema, and the settings screen is generated from it. App preferences (theme, layout) are stored on the host too, so an agent can change them.

## Protocols and lifecycle

From the peers' research round (2026-11-25, run m93a9: `reports/peers-20261125T111923945777Z.md`). Each is where at least one peer lost weeks.

- **Event streams resume.** Durable events carry a monotonic sequence number and a generation; a client subscribes `since` a cursor, and the first message is a snapshot or "resumed from". Ephemeral frames (text deltas, ticks) are never stored or replayed. Clients resync on every reconnect; replay is linear in size (vibe-kanban #3455); streams carry per-key events, not patched lists (emdash #3426: 271 resyncs an hour). Only an explicit end message ends a stream; any other close resumes (vibe-kanban #3227).
- **Terminal protocol.** Each session has an epoch and a byte offset over a ring buffer; a client attaches exact (from its offset), tail, or reanchored (repaint). A resize is a sync boundary, and the size is the smallest visible client's; repaint by shrinking the width first (Ink TUIs only redraw on a shrink). Replayed output never produces replies (the client's input is off while replaying, Paradev #9288), and the host answers terminal queries (DA1/DA2) itself while no client is attached (emdash #2720). A mode preamble is replayed on attach.
- **Backpressure.** Never pause a PTY on a remote client's acknowledgement (Paradev #5031 removed that: the PTY stayed blocked forever). Always drain; a bounded local pause at most; a slow client is dropped and resumes by offset. Input and the focused terminal go first; background terminals send only "has output" marks, with byte budgets per subscription (emdash #3442: 1.6 TB in 5 days).
- **Connections.** A connection is ready when the handshake completes (carrying the daemon's id and start time), not when the socket opens. "Online" comes from heartbeats; backoff resets only after the first heartbeat is acknowledged; foreground and network events are hints. Generation fences stop late callbacks from an old connection. A failed auth is its own close code and stops retrying (AionUi #4124: 28k 401s a day).
- **Tokens per actor.** Every agent gets a token at spawn and every device one at pairing; the actor is taken from the token, never from the request (AionUi #977), which is what makes "every action records its actor" true. Actor classes (root, agent, device, read-only device) each have a fixed set of actions, pinned by a test; terminal input is a permissioned action; a device can't manage other devices. The local API answers 401 to anything without a token and refuses browser origins (happy's control port was open until 2026-11-24).
- **Idempotent writes.** Every write carries a client id and an expiry, so a retried or delayed phone tap neither runs twice nor runs late; the resulting events echo the mutation id. Long actions are jobs with their own state.
- **Daemon and terminal-host lifecycle.** The terminal host runs in its own systemd unit or scope (with the default `KillMode=control-group`, a daemon restart kills detached children: happy #1751). Liveness is a held lock; the instance has an id and a token as a fence; adopting surviving terminals never kills them (Paradev #5721), and the daemon reconciles them at boot. Never signal a recorded pid without checking its start time. One instance per host, bounded shutdown waits (AionUi #884).
- **Hooks.** Claude keeps per-launch `--settings` hooks, as `bin/mesh` does. CLIs whose hooks are global (agy) get an env guard that exits 0 outside our agents, a version marker and a lock, and unparseable config is left alone. Events from another harness (Claude running `codex exec`) are dropped; subagents' events go to their own roster; SessionStart means idle; a parse failure is never read as Stop. Each account's config dir gets its own hooks. The hook resolves the daemon's address at call time and spools while the daemon restarts.
- **One state reducer per agent.** Adapters turn hooks, streams and process events into neutral events, and one pure reducer derives the state. Needs-input is a counted sub-condition, and every state has an event that clears it; "working" with no event or output for a while shows as unknown; a cancel kills the process group.
- **Permission prompts.** Claude's `PermissionRequest` hook (long timeout) for PTY agents, `can_use_tool` for headless ones. The first answer wins across the root, desktop and phone, and a different later answer gets a conflict; a deny carries its reason verbatim; a timeout is its own outcome; pending prompts are re-sent after a restart, and the user's input waits while one is pending (happy #1735).
- **API compatibility.** `buf breaking` in CI against `main` and the last release, with an approved-breaks file; a protocol major version whose error says which side to upgrade; frozen old-client fixtures; hello and pairing are the most stable endpoints. Database migrations are tested against released versions and run with a backup first; a missing secret that already encrypted data is never regenerated (AionUi #722).
- **Agents' CLI versions.** Each adapter has a table of verified CLI versions and shows a banner outside it; decoding is tolerant; captured session fixtures per CLI version, and one live test suite per CLI.
- **Finding CLIs under a service manager.** PATH comes from the login shell (bounded, about 3 s) plus fallback dirs; `git`, `gh` and `ps` by absolute path; an env allowlist for agents, no secrets in argv, and Claude's session variables stripped as `bin/mesh` does (`CLAUDE_SESSION_VARS`).
- **Platform scope.** The first daemon runs on Linux, which on Windows means WSL. A native Windows daemon later needs ConPTY, Job Objects, named pipes and windowless spawns in one helper.

## Stack (recommended)

| Part | Pick | Why |
|---|---|---|
| Daemon, `mesh` CLI and relay | Go, one static binary | A long-running supervisor of processes, terminals and connections is Go's home ground (Docker, Tailscale, Syncthing). One binary cross-compiled for x86 and ARM, with no runtime or native modules to install. Hooks call `mesh` on every turn, and a Go binary starts in milliseconds. Rust would be as solid, but its strengths don't matter here and its slower iteration does; TypeScript's argument (shared types) is moot with generated clients. |
| API | Protobuf schemas with ConnectRPC; a binary WebSocket per terminal | Schema-first, with generated server and clients in every language we use, server streams for events, and well-understood schema evolution. Hand-maintained types drifted at every peer (Paradev's tRPC shape change crashed desktop 1.30, #7726). Terminals need a full-duplex stream of their own ("Architecture"). |
| Storage | SQLite (pure-Go driver, so builds stay static) | One atomic, queryable store with full-text search; check FTS5 and WAL behaviour on the chosen driver. |
| Desktop | Electron + React + TypeScript, styled with Tailwind and shadcn/ui (Windows, Linux; macOS later) | For a terminal- and diff-heavy developer tool, the web has the best parts (xterm.js, diff viewers, Markdown and diagrams), and Electron gives the same Chromium on every OS (VS Code is the proof). Tauri uses WebKitGTK on Linux; Flutter, Avalonia, Qt and Compose Desktop have weaker terminals and rich content. No hand-written CSS: Tailwind for styling, shadcn/ui components (copied into the repo so we own and restyle them), as the desktops of emdash, Orca and Paradev do, built on Base UI rather than Radix (shadcn supports both; Base UI is the actively developed successor from Radix's authors, and emdash uses it). Colors, spacing, type and a compact density (a terminal and diff tool, not shadcn's roomy default) are design tokens in one file, which the Tailwind theme and the Android app's Compose theme are both generated from, so the two apps look like one product. |
| Android | Kotlin + Jetpack Compose | Long, streaming, Markdown-heavy chats are the phone's main screen, and that is where the peers' React Native apps struggled ("Lessons from the peers' stacks"): happy rewrote its phone apps natively, Paseo's Android app still runs out of memory in React Native's UI pipeline. Native also removes the workarounds React Native needed: OkHttp pins the daemon's certificate per connection, UnifiedPush's official Android library and native notification actions (inline replies to approve a plan or answer a question) need no wrapper, and Termux's terminal view (MIT) is a plain native view. The API client is generated from the same protobuf schemas (connect-kotlin), so a third language adds no hand-kept wire types (happy's cost: hand-written wire, crypto and sync per platform). Builds with Gradle, released as an APK on GitHub Releases and F-Droid; no over-the-air updates. It starts as a focused companion and grows. |
| iOS (later) | Swift, with a client generated from the same schemas (connect-swift) and SwiftTerm for terminals | Once someone pays for the Apple account. Sharing the Android app's non-UI code through Kotlin Multiplatform is the alternative to weigh then. |
| Browser (nice to have) | The desktop's React app, served by the daemon | Comes almost free. |
| Push | UnifiedPush, with ntfy (on the user's VPS, or the free ntfy.sh) as the distributor | The user's daemon sends to the user's own ntfy. Firebase would let only the app's publisher send pushes, which forces a central server of ours. The phone needs the ntfy app installed once. |
| Auth | A token per device (paired by a QR code the daemon shows, revocable) and per agent (minted at spawn) | No accounts; every action is attributed ("Protocols and lifecycle"). |
| Distribution | GitHub Releases (desktop builds, APK, daemon binaries) and F-Droid; a Docker image for the daemon and relay; Google Play optional later | Free. Releases are signed (minisign or cosign, pinned key), not checked against a hash from the same server; the daemon updates into immutable version dirs with an atomic switch, under a supervisor with backoff; an update never kills terminals; the Docker image doesn't self-update; app updates over the air only from release builds. |

## Reaching a host

An app can hold several routes per host and uses whichever works, so the same machine is reached directly at home and through the relay elsewhere.

1. **Direct:** a daemon already reachable, such as one on the VPS behind the user's reverse proxy, or on the LAN. From Windows to WSL it's `localhost` (mirrored networking).
2. **The user's own relay** (`mesh relay`, part of the same binary; the default for anything behind NAT):
   - The daemon dials out to the relay on the user's VPS and keeps a control connection open, so nothing at home needs an open port.
   - Apps connect to the relay's address and authenticate to it. For each app connection the relay asks the daemon, over the control connection, to dial back with a one-time ticket, then splices the two raw byte streams (Paradev's protocol). No multiplexing, so one busy stream can't stall another, and each stream has its own end-to-end TLS handshake.
   - A reconnecting daemon replaces its old control connection (with a `replaced` close code); closes are typed; an offline daemon fails fast; `TCP_NODELAY` everywhere (Paradev #5013: 60–200 ms per keystroke without it); binary end to end; the daemon re-registers on every reconnect.
   - TLS runs end to end: the daemon holds its own certificate and the app pins its fingerprint from the pairing QR code. The relay can't read or change the traffic, and there is no custom crypto.
   - The pairing QR code carries the relay URL, the host's id, the certificate fingerprint and a one-time token. One relay serves many daemons.
3. **Tailscale built in** (`tsnet`: the daemon joins the tailnet itself, nothing to install on the host), or Headscale for a fully self-hosted tailnet.
4. **Any VPN or tunnel** (WireGuard, Cloudflare Tunnel): works with nothing of ours.

ntfy can run on the same VPS as the relay, so one small server covers both connections and notifications.

## What carries over from agent-mesh

The code mostly doesn't, since most of `bin/mesh` is herdr and screen handling. The model and the lessons do:

- the root and workers in their own worktrees;
- plans approved, then the same conversation swapped to the execution model;
- mailboxes and reports;
- review rounds sized by the PR;
- the quota reserve and switching models on a limit;
- peers;
- cost per run, agent and phase.

`docs/design.md` (each section, and "Alternatives considered" and the live-test lessons) is the record of why, and applies to the daemon as much as it did to `bin/mesh`.

## Features

From the peers' inventories (2026-11-25, run m93a9; ask `paseo-research` or `orca-ide-research` for details), plus our own. P = Paseo has it, O = Orca has it. ★ = in the first version.

### Ours (neither peer has these)

- ★ A lead-agent view: the run's root and its workers across repos as a tree, each with its state, what it owes and what it waits on (today's `mesh status`). Paseo has no cross-workspace tree; Orca's orchestration has no graphical view.
- ★ Plan approval: read the plan, approve it or send changes, and the worker continues in the same conversation on the execution model.
- ★ Review rounds: reviewers picked by PR size, one merged findings file, findings checked and sent to the PR's author.
- ★ The quota reserve and model switching on a limit, per vendor, across accounts. Orca has no account failover (#20512).
- ★ Cost per run, agent, kind and phase (`mesh cost`).
- Peers: standing askers on other projects, and research rounds.
- Actions logged with their actor, human or agent.

### Hosts and connection

- ★ Several hosts in one app, with host badges and filters (P, O).
- ★ Pairing by QR code or link, with per-device revocation (P, O).
- ★ A connection log and troubleshooting screen; reconnect on returning to the foreground (P, O).
- Remote hosts over SSH (P, O).
- SSH, port forwarding and a remote IDE over the paired connection (`mesh ssh-proxy` as an SSH ProxyCommand that pins the daemon's certificate) (vibe-kanban).
- A host reaching another host's agents through it (P).
- Version gates that ask you to update the host (P, O).
- Keep the machine awake while agents work (O).
- A ports panel with forwarding (O).

### Projects and workspaces

- ★ New workspace: project, host, isolation (project folder, new worktree or existing one), start from a branch or a PR (P, O).
- ★ Worktree lifecycle: setup and teardown scripts with a live log, `.worktreeinclude`, archive with a warning about unpushed work, undo, restore (P, O).
- ★ Status buckets per workspace: needs input, failed, ready to review, working, done; the sidebar groups and filters by them (P).
- Create a workspace from an issue, PR, Linear or Jira item (O).
- Sidebar: pin, labels, sort, choose the fields shown (P, O).
- Auto-archive once the PR merges (P).
- Scripts and services, with ports, health checks and URLs (P).
- Disk, CPU and memory per workspace (O).
- Restore the layout after a restart (O).

### Agents

- ★ Provider, model, mode and effort per agent, switchable mid-session (P, O).
- ★ Agent profiles with "when to use" notes that orchestrators read (P).
- ★ Session history across hosts with full-text search; resume any past session (O, P).
- ★ Several accounts per CLI, swapped by the quota reserve (O).
- Provider catalog: detect, install, update and diagnose CLIs; custom providers (P, O).
- Import a session started in a plain terminal (P).
- Restart recovery after a crash; release idle agents; hibernation (P, O). "Idle" counts the agent's background processes and subagents too (omnara).
- A self-test per CLI in the provider screen: start it and check its handshake and version (vibe-kanban, AionUi).
- A system prompt appended to every agent on a host (P).
- Saved prompts and quick commands (O).

### Conversation

- ★ Each turn records the git HEAD before and after, which gives rewind and per-turn diffs (vibe-kanban).

- ★ Toggle between the chat view and the CLI's terminal (O).
- ★ A streaming timeline: Markdown, diagrams, images, grouped tool calls that expand to their diffs (P, O).
- ★ Inline cards for plans, questions and permissions (P, O).
- ★ While a turn runs: interrupt, steer, or queue the message, with an editable queue (P, O).
- ★ Subagents, todo-list and background-work pills (P, O).
- Composer: `@` files, `/` commands and skills, attachments, drafts that persist, prompt recall (P, O).
- Rewind to a message; fork into a new tab or workspace (P, O).
- A context meter with session cost (P, O).
- Find in chat, a chat outline, quoting a selection into the composer (P, O).

### Terminals

- ★ Daemon-owned PTYs that survive app and daemon restarts (P, O).
- ★ xterm.js on desktop and web: tabs, splits, search, links (P, O).
- ★ A mobile terminal: a key row (Tab, Esc, arrows), live typing, resizing to fit the phone (O, P).

### Git, review, PRs and CI

- ★ A diff viewer: uncommitted, branch or per-commit scope, split or unified, file tree (P, O).
- ★ Comments on diff lines, sent to the agent in one batch (P, O).
- ★ Git actions: commit with an AI message, push, create a PR with an AI draft, merge, auto-merge (P, O).
- ★ A PR panel: checks, job logs, review threads, "add to chat", "fix broken checks" (P, O).
- ★ CI results and PR comments wake the agent that owns the branch (P).
- Stacked PRs (O).
- A branch switcher (P).
- Several forges: GitHub, GitLab, Gitea, Forgejo (P, O).
- A board of issues and tasks (O).

### Orchestration

- ★ Every action is available to agents through the `mesh` CLI (P over MCP, O over its CLI).
- ★ Agents spawn agents, linked as parent and child, with status rolled up the tree and caps a child can't raise (P, O; "Architecture").
- ★ When an agent's process dies, its parent gets a message with the reason and its last message (AionUi).
- Wake an agent on a terminal's output matching a pattern, or on its process exiting, for shells and builds, never agent TUIs (omnara).
- ★ The root answers its workers' permission prompts, or escalates them to the user (O, P).
- Schedules by cron or preset, with run history (P, O).
- Event triggers: GitHub, webhooks (O).
- Heartbeats that re-prompt the same agent (P).
- Bundled skills installed into each CLI (P).
- Persistent named agents with memory (Orca's bots).

### Notifications

- ★ Desktop notifications with sound when an agent finishes, fails or needs you, on every client regardless of focus or idle time (Paseo's presence rules hid them), plus a bell inbox and a tray badge (P, O).
- ★ Mobile push with deep links (P, O). Notifications fire only on live transitions, never from replayed history (omnara #215 sent thousands on a resume); a later notification replaces an earlier one for the same agent; taps reuse the open screen.
- An optional chat-bot channel (Telegram) with buttons to approve a plan or answer a question, beside ntfy (AionUi).
- ★ A dashboard across workspaces: needs you, working, done (O).
- An activity feed (O).
- Mutes per host, workspace or agent; custom sounds (O).

### Files, editor and browser

- A file tree with file operations (P, O).
- A code and Markdown editor, preview on mobile (P, O).
- Open in an external editor (P, O).
- A built-in browser: annotate an element into chat; agents can drive it (P, O).

### Settings, search and keyboard

- ★ A command palette across hosts and workspaces, with actions (P, O).
- ★ Host settings generated from the schema, the same on every device (P).
- ★ Rebindable shortcuts (P, O).
- Themes and fonts (P, O).

### Usage and cost

- ★ Quota windows per account with reset times and a warning before the reserve (P, O).
- ★ Cost per session and per run (P, ours).
- Daily charts per provider (O).

### Extensibility

- Plugins: panels, commands, providers, hooks, actions (P, O).
- A client SDK generated from the registry (P).

### Other

- ★ A Docker image and a static binary for VPS installs (P). The daemon serving the UI to a browser comes later.
- ★ First-run onboarding: detect CLIs, add projects, pair a phone (P).
- Voice dictation; a realtime voice mode (P, O).
- A diagnostics report (P, O).
- Auto-updates (P, O).
- Multiple windows (P).

## Lessons from the peers' stacks

- **Reading agent state from a TUI is brittle.** A slow start looked idle and dispatches were lost (Orca #9976), and a menu was read as a question. Use hooks and structured channels.
- **Mobile connections fail quietly.** Orca's sessions looked alive but ignored input: its reconnect gave up, nothing reconnected on foreground, and half-open sockets took 28 s to detect. From the start: reconnect on foreground, heartbeat the socket, show the connection state.
- **Remote terminal output is its own pipeline.** Orca's phone saw sends accepted but no output, because the PTY daemon fed only the desktop. Stream terminals through the daemon API for every client.
- **Version drift between app and host is everywhere.** Keep protocol fields optional, gate features on the host's advertised capabilities, and tag compatibility shims with a removal date (Paseo's `docs/protocol-compatibility.md`).
- **Long chats need virtualized lists, and terminals need backpressure,** from day one.
- **React Native struggles with long streaming chats on Android.** happy moved its phone apps to native Kotlin and Swift (2026-10/11; its issues: tablet freezes in large chats, scroll jank, a 282-line doc on scroll workarounds). Paseo's Android app slows down and dies out of native memory in Fabric's commit path during long streamed answers (#4408, open), and Orca's mobile chat jumps on every send (#31979, #44746). Hence a native Android app.
- **Provider adapters are the largest bug source** (Paseo: Claude 60, OpenCode 42, Codex 34 fix commits). Keep each CLI's specifics in one adapter, as `bin/mesh`'s `Cli` classes do (design §22).
- **A PTY daemon's socket must be owned carefully.** A departing daemon unlinked its replacement's socket; seven review rounds found 23 defects (Orca `src/main/daemon/AGENTS.md`).
- **Never edit or regenerate a CLI's session file, and never carry a provider's turn or request id across a resume** (omnara #514, #368).
- **Persistent terminals through tmux cost emdash about 30 fix commits** and reset on boot instead of reconciling; a separate terminal process works, with the lifecycle rules above.
- **Windows and WSL take sustained work.** Orca has about 120 Windows and 78 WSL commits.

## Testing

- A scripted fake CLI per adapter for most tests, plus captured session fixtures per CLI version and one live suite per CLI ("mocked tests don't count" for adapters).
- A parity test that a stream and its replay give the same state.
- Restart the daemon mid-run, then resume, per CLI; SIGKILL the terminal host mid-command; `go test -race`.
- An isolated HOME for every test (Paradev #6583: tests killed a developer's real shells).
- The desktop app through Chrome DevTools Protocol (Playwright), the Android app with Maestro.
- A performance bar for the Android chat on a cheap phone: a 2,000-message transcript streaming for 10 minutes, with no memory growth, no scroll jumps and no dropped frames, the failures the peers' React Native apps hit.

## Order of work

1. **Daemon core (Go, Linux):** the protobuf action registry and ConnectRPC API with event streams, PTY agents with state from hooks (Claude first, then agy), SQLite run state, and the generated `mesh` CLI. At the end of this step, agents run without herdr.
2. **Desktop app, first version:** hosts, workspaces and the lead-agent view, terminals, plan approval, questions, reports, settings, native notifications, tray icon.
3. **Connections:** the relay and pairing by QR, then built-in Tailscale.
4. **Android app:** status, notifications with actions through UnifiedPush, plans, questions, reports, the terminal.
5. **Then:** the git, PR and CI features, the structured chat view, schedules, usage, plugins, the browser client, and the rest of the list.

## Open decisions

- Whether the apps live in this repo (`daemon/`, `desktop/`, `android/`, `proto/`; the pick, so the schemas and every client change in one PR) or in their own repos.
- Whether to drive agents through PTYs plus structured channels (the pick), or structured channels only.
- Terminal replay: a server-side terminal emulator that can always repaint any client (happy, unconfirmed), or reanchor plus a forced repaint (Paradev).
- agy's hooks: one peer says a hook with no output denies the call, another that it times out at about 30 s and fails open. Test on our agy before relying on its hooks for permissions.
- Tailcat (a Tailscale-compatible library with no account and your own relay server, per happy) instead of `tsnet`, later.
