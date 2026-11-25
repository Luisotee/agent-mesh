# Apps: desktop, mobile and web

Status: planning (2026-11-25). Nothing is built yet. The technology picks below are recommendations, chosen for quality over simplicity, to be revisited before each part is built.

agent-mesh moves from herdr panes and a terminal CLI to its own apps. Windows, Linux and Android are required; macOS, iOS (once someone pays for the Apple developer account) and a browser client would be nice to have. herdr and the terminal workflow are replaced, not kept alongside: there is one user so far, so nothing legacy has to survive.

## Principles

- **Self-hosted and free.** Everything runs on the user's own machines and VPS, or on free tiers. Nothing depends on a service run by us: the relay and the push server are ones the user hosts.
- **Agents can do everything the UI can.** Every UI action is an API action, settings included, and so is opening views in the user's app. The UI has no abilities of its own.
- **Every action records its actor.** Each action is logged with who did it, a device or an agent by name, so it's always clear who approved or changed what.
- **Our own product.** Paseo and stablyai/orca (both registered peers) are sources of ideas, not templates. We don't build a smaller copy of them, and we don't cap the scope at theirs.

## Architecture

```
 desktop app (Electron + React) ─┐                       ┌─ daemon at home (WSL), dials out
 Android app (React Native)      ┼─ ConnectRPC over TLS ─┤   ├─ action registry (protobuf)
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
  - Each agent can toggle between that chat view and its terminal (Orca's Chat | CLI switch).
- **One action registry, defined as protobuf schemas.** Each action has a name, a request and response schema, an actor permission and a handler. ConnectRPC generates the Go server and the TypeScript client both apps use, and the agents' `mesh` CLI is generated from the same schemas, with no per-command code. Agents call it from their shell, which needs no setup per CLI and costs context only when it's used. An MCP server can be generated from the schemas later if a client needs one.
  - A test fails when an app calls something outside the registry.
  - UI-control actions are actions too: open a view, focus an agent, show a diff, notify.
  - Protobuf's rules for evolving a schema (new fields optional, numbers never reused) plus capabilities the host advertises are the answer to the version drift both peers fought.
- **Live events and terminals** are server streams on the same API. The event log is the run's log, as `log.jsonl` is today.
- **One store: SQLite.** Runs, agents, mail, the event log and full-text session search live in one database file per host. Plans and reports stay Markdown files that agents write, referenced by path. design.md's lesson is against *two* stores that drift, not against a database.
- **Settings are data on the host,** the same from every device. Today's `POLICY` leaves become the host settings schema, and the settings screen is generated from it. App preferences (theme, layout) are stored on the host too, so an agent can change them.

## Stack (recommended)

| Part | Pick | Why |
|---|---|---|
| Daemon, `mesh` CLI and relay | Go, one static binary | A long-running supervisor of processes, terminals and connections is Go's home ground (Docker, Tailscale, Syncthing). One binary cross-compiled for x86 and ARM, with no runtime or native modules to install. Hooks call `mesh` on every turn, and a Go binary starts in milliseconds. Rust would be as solid, but its strengths don't matter here and its slower iteration does; TypeScript's argument (shared types) is moot with generated clients. |
| API | Protobuf schemas with ConnectRPC | Schema-first, with generated server and clients in every language we use, streaming for events and terminals, and well-understood schema evolution. |
| Storage | SQLite | One atomic, queryable store with full-text search. |
| Desktop | Electron + React + TypeScript (Windows, Linux; macOS later) | For a terminal- and diff-heavy developer tool, the web has the best parts (xterm.js, diff viewers, Markdown and diagrams), and Electron gives the same Chromium on every OS (VS Code is the proof). Tauri uses WebKitGTK on Linux; Flutter, Avalonia, Qt and Compose Desktop have weaker terminals and rich content. |
| Android | React Native with Expo (TypeScript) | Native views on the phone, where a web view would show, in the same language as the desktop, with React skills carrying over and a far larger contributor pool. Push through `expo-unified-push` (it wraps UnifiedPush's official Android library); notification actions, including inline replies (approve a plan, answer a question), through Expo's notifications; the terminal as Termux's terminal view (MIT) in a thin native module, or xterm.js in a web view. Builds run locally (`expo prebuild` and Gradle), and app updates can come from a self-hosted update server. Kotlin + Compose was the close runner-up: slightly better on Android alone, at the cost of a third language. It starts as a focused companion and grows. |
| iOS (later) | The same React Native app | Once someone pays for the Apple account; the terminal needs an iOS view (SwiftTerm) behind the same module. |
| Browser (nice to have) | The desktop's React app, served by the daemon | Comes almost free. |
| Push | UnifiedPush, with ntfy (on the user's VPS, or the free ntfy.sh) as the distributor | The user's daemon sends to the user's own ntfy. Firebase would let only the app's publisher send pushes, which forces a central server of ours. The phone needs the ntfy app installed once. |
| Auth | A token per device, paired by a QR code the daemon shows, revocable per device; agents on the host use a local token | No accounts. |
| Distribution | GitHub Releases (desktop builds, APK, daemon binaries) and F-Droid; a Docker image for the daemon and relay; Google Play optional later | Free. |

## Reaching a host

An app can hold several routes per host and uses whichever works, so the same machine is reached directly at home and through the relay elsewhere.

1. **Direct:** a daemon already reachable, such as one on the VPS behind the user's reverse proxy, or on the LAN. From Windows to WSL it's `localhost` (mirrored networking).
2. **The user's own relay** (`mesh relay`, part of the same binary; the default for anything behind NAT):
   - The daemon dials out to the relay on the user's VPS and keeps the connection open, so nothing at home needs an open port.
   - Apps connect to the relay's address. The relay matches each app to its daemon and forwards the bytes through the daemon's outbound link.
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
- Restart recovery after a crash; release idle agents; hibernation (P, O).
- A system prompt appended to every agent on a host (P).
- Saved prompts and quick commands (O).

### Conversation

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
- ★ Agents spawn agents, linked as parent and child, with status rolled up to the parent (P, O).
- ★ The root answers its workers' permission prompts, or escalates them to the user (O, P).
- Schedules by cron or preset, with run history (P, O).
- Event triggers: GitHub, webhooks (O).
- Heartbeats that re-prompt the same agent (P).
- Bundled skills installed into each CLI (P).
- Persistent named agents with memory (Orca's bots).

### Notifications

- ★ Desktop notifications with sound when an agent finishes, fails or needs you, on every client regardless of focus or idle time (Paseo's presence rules hid them), plus a bell inbox and a tray badge (P, O).
- ★ Mobile push with deep links (P, O).
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
- **Provider adapters are the largest bug source** (Paseo: Claude 60, OpenCode 42, Codex 34 fix commits). Keep each CLI's specifics in one adapter, as `bin/mesh`'s `Cli` classes do (design §22).
- **A PTY daemon's socket must be owned carefully.** A departing daemon unlinked its replacement's socket; seven review rounds found 23 defects (Orca `src/main/daemon/AGENTS.md`).
- **Windows and WSL take sustained work.** Orca has about 120 Windows and 78 WSL commits.

## Order of work

1. **Daemon core (Go):** the protobuf action registry and ConnectRPC API with event streams, PTY agents with state from hooks (Claude first, then agy), SQLite run state, and the generated `mesh` CLI. At the end of this step, agents run without herdr.
2. **Desktop app, first version:** hosts, workspaces and the lead-agent view, terminals, plan approval, questions, reports, settings, native notifications, tray icon.
3. **Connections:** the relay and pairing by QR, then built-in Tailscale.
4. **Android app:** status, notifications with actions through UnifiedPush, plans, questions, reports, the terminal.
5. **Then:** the git, PR and CI features, the structured chat view, schedules, usage, plugins, the browser client, and the rest of the list.

## Open decisions

- Whether the apps live in this repo (`daemon/`, `desktop/`, `android/`, `proto/`; the pick, so the schemas and every client change in one PR) or in their own repos.
- Whether to drive agents through PTYs plus structured channels (the pick), or structured channels only.
- Check before building the Android app: that ConnectRPC's server streams work in React Native (Expo's streaming `fetch`), and how a long chat and a live terminal perform on a cheap phone.
