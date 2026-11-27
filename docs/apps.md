# Apps: desktop, mobile and web

Status: planning (2026-11-25). Step 1 (the daemon, `daemon/`) is built; step 2 (the desktop app, `desktop/`) is in progress, first version stacked as PRs; step 4 (the Android app, `android/`) has its first version in progress, stacked as PRs. The technology picks below are recommendations, chosen for quality over simplicity, to be revisited before each part is built.

Mockups of the desktop and Android screens (all themes, the run tree, the live dock, autonomy, plugins, first run and connection states) are on a private design canvas: https://claude.ai/artifact/JhMyJsQPEQjepy4HHg1ua6.

This is a refactor of agent-mesh, not an add-on: where this plan and `docs/design.md` or `CLAUDE.md` disagree, this plan wins. design.md stays as the record of what the `bin/mesh` era learned, and its lessons apply unless this plan overrules them.

agent-mesh moves from herdr panes and a terminal CLI to its own apps. Windows, Linux and Android are required; macOS, iOS (once someone pays for the Apple developer account) would be nice to have; a browser client is deferred (2026-11-25). herdr and the terminal workflow are replaced, not kept alongside: there is one user so far, so nothing legacy has to survive.

## Principles

- **Self-hosted and free.** Everything runs on the user's own machines and VPS, or on free tiers. Nothing depends on a service run by us: the relay and the push server are ones the user hosts.
- **Agents can do everything the UI can.** Every UI action is an API action, settings included, and so is opening views in the user's app. The UI has no abilities of its own.
- **Every action records its actor.** Each action is logged with who did it, a device or an agent by name, so it's always clear who approved or changed what.
- **Modular, and extensible without a PR.** The built-in features use the same extension points as user plugins, and anyone installs a plugin from a git URL or a folder: nothing has to be merged into agent-mesh ("Plugins").
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
  - Each agent can toggle between that chat view and its terminal (Orca's Chat | CLI switch). One process writes a conversation at a time: the chat view of a PTY agent renders from its transcript and hooks, and its composer delivers into that same running process by typing into its PTY, as the user's own keystrokes do. Claude's inbox socket is not used for this (decided 2026-11-26): Claude presents what arrives there as "another Claude session sent a message… not typed by your user", so the text would carry a peer's authority and none of the user's; the doorbell is what it is for. Never a second process on the same conversation; moving an agent between a PTY and a headless structured channel is a handover (stop one, `--resume` in the other).
- **One action registry, defined as protobuf schemas.** Each action has a name, a request and response schema, an actor permission and a handler. ConnectRPC generates the Go server, the desktop's TypeScript client and the Android app's Kotlin client, and the agents' `mesh` CLI is generated from the same schemas, with no per-command code. Agents call it from their shell, which needs no setup per CLI and costs context only when it's used. An MCP server can be generated from the schemas later if a client needs one.
  - A test fails when an app calls something outside the registry.
  - UI-control actions are actions too: open a view, focus an agent, show a diff, notify.
  - Protobuf's rules for evolving a schema (new fields optional, numbers never reused) plus capabilities the host advertises are the answer to the version drift both peers fought.
- **Live events** are a server stream on the same API, resumable ("Protocols and lifecycle"). The event log is the run's log, as `log.jsonl` is today.
- **Terminals get their own binary stream** per terminal (a WebSocket, message types defined in the protobuf schemas), not ConnectRPC calls: Connect has no client or bidirectional streaming in the browser or Electron's renderer, and terminals sharing one HTTP/2 connection stall behind a busy one (emdash #3426: 9 s typing echo with 8 busy agents). Every peer that streams terminals does this.
- **One store: SQLite.** Runs, agents, mail, the event log and full-text session search live in one database file per host. Plans and reports stay Markdown files that agents write, referenced by path. design.md's lesson is against *two* stores that drift, not against a database.
- **Agents spawn agents, within roles.** Only the run's root starts change workers. A worker may start read-only agents of its own: its PR's reviewers, and askers on other repos or the peers, whose answers come back to it. Change work in another repo is never requested by a worker: it goes to the root, which assigns it (this overturns design §7's rule that workers start no agents, only for read-only ones). Nesting is capped by limits a child can't raise: depth, children per agent and agents per run, from host settings, plus message-rate windows against storms (omnara's caps, AionUi's storm windows). Status, usage and cost roll up the tree, and the lead-agent view shows it.
- **Settings are data on the host,** the same from every device. Today's `POLICY` leaves become the host settings schema, and the settings screen is generated from it. App preferences (theme, layout) are stored on the host too, so an agent can change them. Built (`daemon/README.md`, "Settings"; `repos.no_free_models` is the per-repo free-model switch, 2026-11-26: a repo listed there never gets OpenCode's zero-cost models, the desktop's settings screen has a switch for it, Android shows the raw list): `ListSettings`, `SetSetting`, `ResetSetting` and a global `SettingChanged` event over one Go table (`daemon/internal/settings/schema.go`); an app adds its rows to that table and no actions of its own.

## Protocols and lifecycle

From the peers' research round (2026-11-25, run m93a9: `reports/peers-20261125T111923945777Z.md`). Each is where at least one peer lost weeks.

- **Event streams resume.** Durable events carry a monotonic sequence number and a generation; a client subscribes `since` a cursor, and the first message is a snapshot or "resumed from". Ephemeral frames (text deltas, ticks) are never stored or replayed. Clients resync on every reconnect; replay is linear in size (vibe-kanban #3455); streams carry per-key events, not patched lists (emdash #3426: 271 resyncs an hour). Only an explicit end message ends a stream; any other close resumes (vibe-kanban #3227).
- **Terminal protocol.** Each session has an epoch and a byte offset over a ring buffer; a client attaches exact (from its offset), tail, or reanchored (repaint). A resize is a sync boundary, and the size is the smallest visible client's; every attached client is told in a `Resized` frame (the new epoch, the output offset it starts at, and the size), queued after the output of the old epoch and before that of the new, so a client follows without a repaint; a `Resize` from a client with input rights that moves nothing (another client is as small) is answered to that client alone with a `Resized` of the current epoch, its start offset and the size, so it never has to guess what its request came to (a client that doesn't know the frame still finds out by the epoch of the next output); repaint by shrinking the width first (Ink TUIs only redraw on a shrink). Replayed output never produces replies (the client's input is off while replaying, Paradev #9288), and the host answers terminal queries (DA1/DA2) itself while no client is attached (emdash #2720). A mode preamble is replayed on attach.
- **Backpressure.** Never pause a PTY on a remote client's acknowledgement (Paradev #5031 removed that: the PTY stayed blocked forever). Always drain; a bounded local pause at most; a slow client is dropped and resumes by offset. Input and the focused terminal go first; background terminals send only "has output" marks, with byte budgets per subscription (emdash #3442: 1.6 TB in 5 days).
- **Connections.** A connection is ready when the handshake completes (carrying the daemon's id and start time), not when the socket opens. "Online" comes from heartbeats; backoff resets only after the first heartbeat is acknowledged; foreground and network events are hints. Generation fences stop late callbacks from an old connection. A failed auth is its own close code and stops retrying (AionUi #4124: 28k 401s a day).
- **Tokens per actor.** Every agent gets a token at spawn and every device one at pairing; the actor is taken from the token, never from the request (AionUi #977), which is what makes "every action records its actor" true. Actor classes (root, agent, device, read-only device) each have a fixed set of actions, pinned by a test; terminal input is a permissioned action; a device can't manage other devices. The local API answers 401 to anything without a token and refuses browser origins (happy's control port was open until 2026-11-24).
- **Idempotent writes.** Every write carries a client id and an expiry, so a retried or delayed phone tap neither runs twice nor runs late; the resulting events echo the mutation id. Long actions are jobs with their own state.
- **Daemon and terminal-host lifecycle.** The terminal host runs in its own systemd unit or scope (with the default `KillMode=control-group`, a daemon restart kills detached children: happy #1751). Liveness is a held lock; the instance has an id and a token as a fence; adopting surviving terminals never kills them (Paradev #5721), and the daemon reconciles them at boot. Never signal a recorded pid without checking its start time. One instance per host, bounded shutdown waits (AionUi #884).
- **Hooks.** Claude keeps per-launch `--settings` hooks, as `bin/mesh` does. CLIs whose hooks are global (agy) get an env guard that exits 0 outside our agents, a version marker and a lock, and unparseable config is left alone. Events from another harness (Claude running `codex exec`) are dropped; subagents' events go to their own roster; SessionStart means idle; a parse failure is never read as Stop. Each account's config dir gets its own hooks. The hook resolves the daemon's address at call time and spools while the daemon restarts.
  - agy 1.5.17, tested (2026-11-25): hooks are a `hooks.json` of named hooks (`SessionStart`, `PreInvocation`, `PostInvocation`, `PreToolUse`, `PostToolUse`, `Stop`), global in `~/.gemini/config/hooks.json` and per workspace in `<workspace>/.agents/hooks.json`; the workspace file loaded in `agy -p` with no trust step, so each agent worktree can carry its own hooks and the global file stays untouched. The same load makes it a risk: any repo agy opens (a peer clone, a PR head under review) can run commands through its `.agents/hooks.json`, so the daemon checks or neutralises a worktree's file before starting agy there; and a hooks file the daemon writes must not dirty the worktree. The daemon gives an asker or a reviewer only its own hooks and keeps a worker's repo's committed hooks beside them (deep-merged into the committed hooks, with the mesh's own `"agent-mesh"` key replaced wholesale) when that version is on one of the repo's own refs (a local branch other than `mesh/…`, or an origin branch; a base at a fork's PR head, fetched by sha or into `refs/pull/…`, gets only the mesh's), marks a tracked file `--skip-worktree`, and ignores an untracked one with a self-ignoring `.agents/.gitignore` in that worktree alone: the repo's `info/exclude` is shared by every checkout of it, the user's included. A command runs via `sh -c` in the directory of its `hooks.json` with the agent's environment (`MESH_*` included, so the identity guard works), JSON on stdin (`conversationId`, `cwd`, `hookEventName`, `transcriptPath`, `lastUserInput`, `modelName`), default timeout 30 s and a per-hook `timeout`.
  - Observed failure modes: a `SessionStart` that hung was killed at its timeout (5 s) and the run went on; a `PreInvocation` printing non-JSON, exit 0, was ignored. `SessionStart` fires once per new conversation and **not** on `--conversation` resume (no `source` field); `PreInvocation` (`invocationNum`) and `Stop` fire on both. `Stop` fires when the turn ends in an API error too (`terminationReason: "ERROR"`, `error` set), and does not when `--print-timeout` cuts the run. In an interactive PTY (`agy`), **no hook fires at startup** on either a fresh launch or `--conversation` resume (`SessionStart` fires only when the first prompt is submitted, on fresh launches only), and an unseen workspace stops at a folder-trust dialog unless `--settings '{"trustedWorkspaces":["<cwd>"]}'` is passed. So `PlanLaunch` passes `--settings` for trust, the adapter's `Describe` omits `"startup-hook"`, and the **daemon** (`settleStartingAgents` / `ReadySettle`) transitions `STARTING -> IDLE` once initial terminal output settles (`Offset > 0` and `LastOutput` quiet for `ReadySettle`; a terminal still drawing `30 * ReadySettle` after its first output is marked anyway), while the adapter maps `PreInvocation` to working, `Stop` to idle (or `error`/`limited` when `terminationReason` is `ERROR`), and a resume to working at the first `PreInvocation`. Settling `STARTING -> IDLE` from output offset/timestamps is a narrow, explicit exception to "never from reading the screen" above: it inspects only PTY byte offset and idle duration rather than screen contents, and risks marking a slow-starting agent idle if startup pauses longer than `ReadySettle` (mitigated by `--settings` eliminating the folder-trust prompt and by requiring `Offset > 0`). Since an agy `PreToolUse` hook that outlives its timeout denies the tool, the hook command reports a `PreToolUse` quickly (within a second, spooling rather than waiting on the spool lock or a slow daemon). An agent stopped at `LIMITED` or `ERROR` sits at its input box, so it takes reopens and idle retire like an idle one; only `ERROR` takes doorbells: at `LIMITED` the turn a doorbell starts would fail on the same quota, so its messages wait for the switch or the `continue` at the reset. The conversation-id lookup runs only for an action that uses the id (a resume, or a reopen), for an agent the actor may act on, and skips conversations older than the agent.
  - `PreToolUse` tested (2026-11-25, `agy 1.5.17`, `gemini-3.8-flash-low`, `--dangerously-skip-permissions`, `run_command`): **fail-closed**, and `{"decision":"deny","reason":"..."}` overrides `--dangerously-skip-permissions`. Silent exit 0 allowed the call (`PostToolUse` fired), while `{"decision":"deny","reason":"..."}`, exit 1, exit 2, non-JSON stdout (exit 0), and hanging past the hook `timeout` all blocked the tool call (`PostToolUse` did not fire). So: (1) telemetry hooks (`mesh hook`) must exit 0 silently so a down daemon spools without blocking tools; (2) planning and read-only deny rules can be enforced directly by a local `PreToolUse` check (without needing the daemon up); and (3) a future daemon permission-gate hook in `PreToolUse` is also fail-closed. Other `hooks.md` responses (`PreToolUse` `"allow"|"ask"|"force_ask"` and `"overwrite"`, `Stop` `"continue"`) were not tested. `PostToolUse` leaves the tool's result alone on failure (changelog). agy still has no `Notification` or `PermissionRequest` event, so interactive permission prompts without `--dangerously-skip-permissions` raise no hook (its ACP server's permission requests are the structured route, untested).
- **One state reducer per agent.** Adapters turn hooks, streams and process events into neutral events, and one pure reducer derives the state. Needs-input is a counted sub-condition, and every state has an event that clears it; "working" with no event or output for a while shows as unknown; a cancel kills the process group.
- **Permission prompts.** Claude's `PermissionRequest` hook (long timeout) for PTY agents, `can_use_tool` for headless ones. The first answer wins across the root, desktop and phone, and a different later answer gets a conflict; a deny carries its reason verbatim; a timeout is its own outcome; pending prompts are re-sent after a restart, and the user's input waits while one is pending (happy #1735).
- **API compatibility.** `buf breaking` in CI against `main` and the last release, with an approved-breaks file; a protocol major version whose error says which side to upgrade; frozen old-client fixtures; hello and pairing are the most stable endpoints. Database migrations are tested against released versions and run with a backup first; a missing secret that already encrypted data is never regenerated (AionUi #722).
- **Agents' CLI versions.** Each adapter has a table of verified CLI versions and shows a banner outside it; decoding is tolerant; captured session fixtures per CLI version, and one live test suite per CLI.
- **Finding CLIs under a service manager.** PATH comes from the login shell (bounded, about 3 s) plus fallback dirs; `git`, `gh` and `ps` by absolute path; an env allowlist for agents, no secrets in argv, and Claude's session variables stripped as `bin/mesh` does (`CLAUDE_SESSION_VARS`).
- **Platform scope.** The first daemon runs on Linux, which on Windows means WSL. A native Windows daemon later needs ConPTY, Job Objects, named pipes and windowless spawns in one helper.

## Plugins

A plugin is a folder or git repo with a manifest, installed per host. The built-ins (each CLI adapter, ntfy, the GitHub integration) are written against the same extension points, so the core stays small and a plugin can do anything a built-in does.

- **Extension points:**
  - agent CLI adapters: a new CLI, or a variant of one;
  - actions: a plugin's actions join the registry, so the apps, the `mesh` CLI and agents get them with no extra code;
  - event subscribers and lifecycle hooks (before an agent starts, on a permission request, after a turn, at a run's end), which may block or change what their capabilities allow;
  - notification channels (ntfy, Telegram, email), connection routes, git hosts and trackers (GitHub, GitLab, Linear, Jira);
  - skills, prompts and protocol files, agent profiles, settings presets, themes, keybindings;
  - UI contributions (below).
- **Three kinds, by what runs:**
  - *Declarative:* data only (skills, profiles, themes, keybindings, prompt overrides, a hook that runs a shell command). No plugin code runs.
  - *Process plugins:* a program in any language that the daemon starts and that talks to it over the same ConnectRPC API, with its own token. Out of process, so a crash or a leak can't take down the daemon or its terminals, and no language or build is imposed (Go's own `plugin` package needs the daemon's exact build). The daemon restarts a crashed one with backoff and disables it after repeated failures.
  - *Web panels* (desktop and browser): a page in a sandboxed iframe that reaches the daemon only through a bridge holding the plugin's token.
- **UI that works on both apps.** The Android app can't load a plugin's code, so plugin UI is mostly declarative: forms from a schema, lists, status items, badges, buttons that call actions, Markdown. Both apps render it natively, the way the settings screen is generated from the settings schema. A web panel shows on desktop, and on Android in a web view.
- **Capabilities and consent.** The manifest declares what the plugin needs: events it reads, actions it calls or registers, network, paths, secrets. The user grants them at install, and again when an update asks for more. The plugin's token carries only those, and its actions are logged with the plugin as the actor.
- **Install without a PR.** `mesh plugin install <git URL | path>`, or the same from the apps or by an agent (the user approves the capabilities), pinned to a commit, with update, rollback, disable and remove; old versions are cleaned up (Orca's plugin staging folders grew by gigabytes, #7725). A community index is only a git repo that lists plugins. A project's own `.agent-mesh/` folder (setup scripts, profiles, the plugins it wants) is picked up on the user's yes.
- **Overrides without forking.** User and per-repo layers over the built-in skills, protocol files and prompts, so the orchestration itself changes by configuration.
- **Compatibility.** A plugin states the API versions it supports; the schema rules (`buf breaking`, capabilities the host advertises) cover plugins as they cover the apps. Generated SDKs, Go and TypeScript first, are the supported way to write one.

## Stack (recommended)

| Part | Pick | Why |
|---|---|---|
| Daemon, `mesh` CLI and relay | Go, one static binary | A long-running supervisor of processes, terminals and connections is Go's home ground (Docker, Tailscale, Syncthing). One binary cross-compiled for x86 and ARM, with no runtime or native modules to install. Hooks call `mesh` on every turn, and a Go binary starts in milliseconds. Rust would be as solid, but its strengths don't matter here and its slower iteration does; TypeScript's argument (shared types) is moot with generated clients. |
| API | Protobuf schemas with ConnectRPC; a binary WebSocket per terminal | Schema-first, with generated server and clients in every language we use, server streams for events, and well-understood schema evolution. Hand-maintained types drifted at every peer (Paradev's tRPC shape change crashed desktop 1.30, #7726). Terminals need a full-duplex stream of their own ("Architecture"). |
| Storage | SQLite (pure-Go driver, so builds stay static) | One atomic, queryable store with full-text search; check FTS5 and WAL behaviour on the chosen driver. |
| Desktop | Electron + React + TypeScript, styled with Tailwind and shadcn/ui (Windows, Linux; macOS later) | For a terminal- and diff-heavy developer tool, the web has the best parts (xterm.js, diff viewers, Markdown and diagrams), and Electron gives the same Chromium on every OS (VS Code is the proof). Tauri uses WebKitGTK on Linux; Flutter, Avalonia, Qt and Compose Desktop have weaker terminals and rich content. No hand-written CSS: Tailwind for styling, shadcn/ui components (copied into the repo so we own and restyle them), as the desktops of emdash, Orca and Paradev do, built on Base UI rather than Radix (shadcn supports both; Base UI is the actively developed successor from Radix's authors, and emdash uses it). Colors, spacing, type and a compact density (a terminal and diff tool, not shadcn's roomy default) are design tokens in one file, which the Tailwind theme and the Android app's Compose theme are both generated from, so the two apps look like one product. |
| Android | Kotlin + Jetpack Compose | Long, streaming, Markdown-heavy chats are the phone's main screen, and that is where the peers' React Native apps struggled ("Lessons from the peers' stacks"): happy rewrote its phone apps natively, Paseo's Android app still runs out of memory in React Native's UI pipeline. Native also removes the workarounds React Native needed: OkHttp pins the daemon's certificate per connection, UnifiedPush's official Android library and native notification actions (inline replies to approve a plan or answer a question) need no wrapper, and Termux's terminal emulator (Apache-2.0: only its `terminal-emulator` and `terminal-view` modules, the rest of Termux is GPLv3) is plain Java that needs no Android service. The API client is generated from the same protobuf schemas (connect-kotlin), so a third language adds no hand-kept wire types (happy's cost: hand-written wire, crypto and sync per platform). Builds with Gradle, released as an APK on GitHub Releases and F-Droid; no over-the-air updates. It starts as a focused companion and grows. |
| iOS (later) | Swift, with a client generated from the same schemas (connect-swift) and SwiftTerm for terminals | Once someone pays for the Apple account. Sharing the Android app's non-UI code through Kotlin Multiplatform is the alternative to weigh then. |
| Browser (deferred, 2026-11-25) | The desktop's React app, served by the daemon | Comes almost free, but is not being built now. |
| Push | UnifiedPush, with ntfy (on the user's VPS, or the free ntfy.sh) as the distributor | The user's daemon sends to the user's own ntfy. Firebase would let only the app's publisher send pushes, which forces a central server of ours. The phone needs the ntfy app installed once. Built (`daemon/README.md`, "Push"): the push carries the notice itself, encrypted for the phone's keys per RFC 8291 (UnifiedPush's WebPush), so ntfy.sh sees only ciphertext and the phone shows it without reaching the host; one UnifiedPush instance and endpoint per host, registered with `RegisterPush` and deleted with the device's revocation. |
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
   - Each device has its own relay token. The daemon gives the relay their hashes, so a revoked device is refused before the relay asks the daemon to dial back (`docs/relay.md`, "Client relay tokens").
3. **Tailscale built in** (`tsnet`: the daemon joins the tailnet itself, nothing to install on the host), or Headscale for a fully self-hosted tailnet. Built (`meshd --tailscale`, `docs/relay.md` "Built-in Tailscale"):
   - The node serves the same pinned TLS API and device tokens as `--listen`, and pairing links carry its tailnet address. The device runs the Tailscale app itself.
   - It is turned on by flags and environment, not settings: the auth key is a secret, and joining a network is the operator's call.
   - The login URL is in `mesh tailnet` (`TailnetStatus`), for the host's own device only.
   - meshd is built without tsnet's log upload and peerapi (`-tags ts_omit_logtail,ts_omit_peerapiserver`).
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
- ★ Autonomy per project, autonomous by default as agent-mesh is today: **Autonomous** (the root approves plans, agent reviewers review each PR, and it merges once checks pass and findings are fixed), **I merge** (PRs wait for the user's merge) or **I approve plans and merge**. A run can use another level without changing the project's. Built: the desktop's side panel sets a project's level in the host's `repos.user_merges` and `repos.user_approves_plans` lists (a repo in neither is handled as before), the root's brief lists them and `autonomy.ask_before` only when set, and a PR's "Hold merge" (`HoldPullRequest`) is the flag every root respects; not built: a per-run level, and the daemon enforcing any of it (the root acts on its brief). Reviewing is never a required step: the user can open any plan or PR and comment at any level, before or after the merge, and the comments go to the agent that wrote it; "Hold merge" pauses one PR. An "always ask me before" list (migrations, deploys, pushes to the default branch, deletes outside the worktree, a cost limit per run) applies at every level.
- ★ Plan approval: by the root, or by the user at the "I approve plans" level; either way the worker continues in the same conversation on the execution model.
- ★ Review rounds: reviewers picked by PR size, one merged findings file, findings checked by the PR's author (the worker that wrote it asks the round itself, judges each finding, fixes what it agrees with and reports once with its verdicts; the root checks what it declined; `review.by` / `--reviews root` leave a risky PR's relay to the root).
- ★ The quota reserve and model switching on a limit, per vendor, across accounts. Orca has no account failover (#20512).
- ★ Cost per run, agent, kind and phase (`mesh cost`): tokens first for every CLI (a plan is billed by tokens, not API prices), Claude's API-equivalent dollars beside them.
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

- ★ New workspace: project, host, isolation (project folder, new worktree or existing one), start from a branch or a PR (P, O). Built in the desktop (`components/workspace-dialog.tsx`, `ListProjects`): a project from the host's list (or a path), start from its default branch, a branch or a PR's branch, into a new run or an open one; the isolation is always a new worktree (the daemon makes no other), so "project folder" and "existing worktree" are not offered.
- ★ Worktree lifecycle: `.worktreeinclude`, archive with a warning about unpushed work, restore (built 2026-11-26: `ArchiveAgent` keeps the branch and `ResumeAgent` makes the worktree again; undo is that restore); setup and teardown scripts with a live log are not built (the daemon would run commands a repo supplies: a trust decision first, `TODO.md`) (P, O).
- ★ Status buckets per workspace: needs input, failed, ready to review, working, idle, done (built in the desktop: `desktop/src/buckets.ts`, from the agent's state, `ListWaiting` and `ListPullRequests`); the sidebar groups by run (the default: each run as a tree from its lead, workers and their read-only children (reviewers, askers) joined by lines, every row giving its role (lead, worker, asker, reviewer), a plain status sentence, its project and host, and its CLI and model), by project or by state. Runs get a descriptive name from the lead (renamable), with the id secondary. Agent states are animated icons, distinct in shape and not only colour (working, needs you, answering, waiting, done, failed), still under reduced motion. The side panel (built in the desktop: `components/side-panel.tsx` and `components/agent-work.tsx`) shows, for the lead, the whole run: total cost and cost per agent, what waits on you, PRs and quota used; for any other agent, a one-line location (project, host, branch) with terminal and editor buttons, its progress through its plan, files changed, model history, context and cost (P).
- ★ Tabs per project, one pane by default and split side by side or stacked only when the user asks (drag a tab to an edge, or the split button): any mix of agent chats (any CLI, several per project), terminals, browsers, diffs and files, from one "new tab" menu that plugins can add tab types to. The phone shows a project's tabs as a chip row. Tabs and splits are restored after a restart (O). Built in the desktop: a split is a pane beside the open agent (up to three, row or column, a button on the tab, the dock tile or the palette instead of dragging to an edge), and the layout is kept per device in the window's storage.
- ★ A live dock, which replaces herdr's panes popping up: each agent of the open runs is a small tile streaming its latest activity, and a newly spawned worker pops in marked new, without taking focus or the screen. Click a tile to open its chat as a tab, pin it as a split, or dismiss it; "Watch all" shows every agent as a grid, as herdr does today (built in the desktop: a tile per live agent with its last conversation lines; answering a plan or question in place is not: a click opens the agent on the right view). Whether new workers pop into the dock, only badge, open as a tab or do nothing is a setting.
  - Built in the desktop (2026-11-26): the dock, new marks, tabs and the `app.dock.new_agents` setting; the tool an agent runs arrives as an ephemeral `AgentActivity` frame (`daemon/README.md`, "Protocol notes"). Not yet: pinning a tile as a split, dismissing it for good, "Watch all" (they come with splits).
- Create a workspace from an issue, PR, Linear or Jira item (O).
- Sidebar: pin, labels, sort, choose the fields shown (P, O).
- Auto-archive once the PR merges (P).
- Scripts and services, with ports, health checks and URLs (P).
- Disk, CPU and memory per workspace (O).

### Agents

- ★ Provider, model, mode and effort per agent, switchable mid-session (P, O). Built in the desktop: the CLI, model and effort with the vendor's mark everywhere an agent is shown, and a Change control over `SwitchAgent` (tier, model or effort; the host's refusals shown); the side panel lists the models an agent ran on (`GetAgent`'s `model_history`, derived from the run's events); not built: mode (cut for now: it waits on the Plan-mode redesign in `TODO.md`).
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
  - Built in the desktop for Claude Code (2026-11-26): `ReadTranscript` and `PromptAgent` (`daemon/README.md`, "Chat view"); the tool calls fold, the composer types into the PTY. Every built-in CLI has it now: agy's and OpenCode's SQLite records and Copilot's `events.jsonl` are read through their adapters. OpenCode's is read from `opencode.db`, not its HTTP API (two route families side by side in 1.19.6, an unsecured server unless a password is set, and no live prompt could be checked); a CLI without the capability shows the terminal's note and the composer.
  - The chat's queue and stop (2026-11-27): a send is queued while the agent can't take it and typed at its next idle (`ListPrompts`, `WithdrawPrompt`: Up takes the newest back), and Esc interrupts a Claude turn (`InterruptAgent`); `daemon/README.md`, "Chat view".
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

**Deferred (the user's call, 2026-11-25):** nothing in this section is in the first versions, whatever its ★; it comes after the rest of step 5.

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
- ★ Agents spawn agents within roles: the root starts change workers, and a worker starts its own PR's reviewers and askers on other repos, never change workers. Linked as parent and child, with status rolled up the tree and caps a child can't raise (P, O; "Architecture").
- ★ When an agent's process dies, its parent gets a message with the reason and its last message (AionUi).
- Wake an agent on a terminal's output matching a pattern, or on its process exiting, for shells and builds, never agent TUIs (omnara).
- ★ The root answers its workers' permission prompts, or escalates them to the user (O, P). **Done for Claude** (a `PermissionRequest` hook held for the parent: `mesh permission`, `permission.wait`; README, "Permission prompts"); the apps list and answer pending prompts (`ListPermissions`, `DecidePermission`) and the `tier_lean` setting (`quality`, `balanced`, `thrifty`) come with the settings screens.
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
  - Built in the desktop (2026-11-26): agents, runs, hosts and what waits on the user to go to, and the registry actions for them (start a run, ring, retire, stop, approve a plan, move to the next model); each is a function of `src/actions.ts`, so an agent or the CLI can do the same. UI-control actions: `OpenView` (an agent or the CLI asks the user's app to show an agent's chat or terminal, its Messages tab or its newest plan, a pull request (its owner, with the side panel), a run or the settings; sent to the apps connected now, shown as a toast the user opens or dismisses; `app.open_views`, which only a device may set, makes it switch by itself unless the user is typing; an agent may ask once in 10 s).
- ★ Host settings generated from the schema, the same on every device (P).
- ★ Rebindable shortcuts (P, O).
  - Built in the desktop (2026-11-26): `app.keys.*` host settings of type `shortcut`, recorded in the settings screen or set by an agent (`mesh set --key app.keys.palette --value Mod+K`), followed live. They follow the first host's settings, as the tray does.
- ★ Several themes from the start: Dracula (the default), Alucard, Catppuccin Mocha and Latte, Tokyo Night and Nord, with fonts and density. A theme is one JSON file of semantic tokens: surfaces, text, accent, the five agent states and the terminal colours, with a font and a density (`shared/themes/`, built 2026-11-26). Both apps read the same files, no build step: the desktop writes the tokens as Tailwind's CSS variables and the font and density as `--font-ui`, `--font-code`, `--spacing` and the text size, Android maps them to a Compose colour scheme, and a missing terminal colour is derived from the others (the same rule in both). A theme is added by dropping a file into `~/.config/agent-mesh/themes/`, installing a declarative plugin, importing a VS Code, base16 or terminal scheme, or asking an agent to write one. The theme is a host setting with a per-device override, and it can follow the system's dark or light mode. Scrollbars follow the theme: thin and rounded on desktop, in the border colour; none on Android, which keeps its own fading scroll indicator (P, O).

### Usage and cost

- ★ Quota per account with both windows a vendor has (Claude's 5-hour and weekly ones), each with its reset time and the reserve line, and a warning before the reserve (P, O). Built on the desktop (2026-11-26): the Usage and cost screen from `GetUsage` (read again every minute while open) and `GetCost` (on opening, a change of span, or Refresh), tokens as the main figure and Claude's dollars at API prices beside them; a warning once a window is at the reserve (not before it); Android has the screen, and meters in its drawer.
- ★ Cost per session and per run (P, ours). Built on the desktop (2026-11-26) from `GetCost`, for a span (24 h, 7 d, 30 d) or one run, with the CLI's tables and an optional per-agent one.
- Daily charts per provider (O).

### Extensibility

- ★ The extension points ("Plugins"), with the built-ins written on them. Third-party install, consent and web panels can follow (P, O).
- A client SDK generated from the registry (P).

### Other

- ★ A Docker image and a static binary for VPS installs (P). The daemon serving the UI to a browser is deferred (2026-11-25).
- ★ First-run onboarding: detect CLIs, add projects, pair a phone (P). Built for the desktop (2026-11-26): the app finds the daemon on this machine (a state dir; on Windows a WSL distro through `wsl.exe`, paired with a link made by `mesh pair` inside it) and opens a chat with the setup agent (`StartSetup`: a lead agent in a `setup` run, instructed by `protocol/SETUP.md`, with the rights to pair devices and read the tailnet that only the host's own device had; that is a boundary for well-behaved agents through the API, not against a hostile one: every agent runs as the same unix user and can read the host device's token file (0600) in the state dir). Open: detecting CLIs and logins in the app itself, the AI doing the pre-connection fixes (before a connection there is no chat), Android's first run.
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

1. **Daemon core (Go, Linux):** the protobuf action registry and ConnectRPC API with event streams, PTY agents with state from hooks (Claude first, then agy), SQLite run state, and the generated `mesh` CLI, with the built-in adapters and channels written on the plugin interfaces. At the end of this step, agents run without herdr.
   - Milestone 1 is built (`daemon/README.md`): the registry, meshd with SQLite and resumable events, the terminal host, the Claude adapter with its hooks and the reducer, and the CLI (`mesh`) generated from the schemas. An end-to-end test drives a fake `claude` through a daemon SIGKILL and restart. What's left of the step is in `TODO.md`; the switchover is planned in "Switchover".
2. **Desktop app, first version:** hosts, workspaces and the lead-agent view, terminals, plan approval, questions, reports, settings, native notifications, tray icon.
3. **Connections:** the relay and pairing by QR, then built-in Tailscale.
4. **Android app:** status, notifications with actions through UnifiedPush, plans, questions, reports, the terminal.
5. **Then:** the structured chat view, schedules, usage, third-party plugins (install, consent, the index), and the rest of the list. **Deferred (the user's call, 2026-11-25):** the web (browser) client, and the git, PR and CI review features ("Git, review, PRs and CI": diff viewer, line comments, git actions, the PR panel, CI wake-ups). Agents keep doing git and PRs through their own shell, as today; the apps show and steer agents, not code review, until these are picked up again.

The switchover that ends step 1 is planned in "Switchover" below.

## Switchover

Done (planned 2026-11-25, run mfb6a; the cutover is PR 6). Every agent on the machine runs on the daemon: `bin/mesh`, herdr's plugin, the old `protocol/` and `skills/` and their Python tests are deleted (`docs/design.md` stays as the record of the `bin/mesh` era).

**The cutover.**
- `install.sh` is the daemon's install: it builds `daemon/` into `~/.local/lib/agent-mesh/{bin,share}`, installs and restarts the systemd user units (with a drop-in holding the installing shell's `PATH`), links `~/.local/bin/mesh` to the installed `mesh` and `~/.config/agent-mesh` to the installed `share/protocol`, links the skills of `share/skills` into `~/.claude/skills` and `~/.gemini/config/skills`, and writes the rules block into the Claude and agy global files. The installed copies are versioned by install, not by the checked-out branch.
- Upgrading from the old install is one run of `./install.sh` after the pull: the links into the deleted paths are re-pointed, the retired `delegate-claude` and `delegate-agy` links, the `meshctl` link and the herdr plugin are removed, the Copilot and OpenCode rules blocks, which name the herdr mesh, are taken out, and meshd is built and restarted. A failed build changes no link, so the old install keeps working. `~/.local/state/agent-mesh` (the old runs) is left in place and unread, and `~/.config/agent-mesh.json` (its policy) is left in place and imported once by meshd on its first start (`--policy-file`, read-only); the daemon's state is `~/.local/state/agent-mesh-daemon`.
- The folder `daemon/share` is a Claude plugin named `agent-mesh` (marketplace source `./daemon/share`): meshd loads it per launch with `--plugin-dir`, and Claude Code users may install it from the marketplace (meshd turns off a user-installed copy of the plugin in that launch's `enabledPlugins`, so the two never load together; `daemon/README.md`, "Protocol and skills"). `make-concise` moved into it.
- "meshctl" is "mesh" everywhere (the apps' text, the docs, the proto comments); `meshd --meshctl` and `AGENT_MESH_CLI` are gone.
- Rollback: check out the commit before the cutover, run its `./install.sh` (it re-links `bin/mesh`, `protocol/`, `skills/`, the old snippet and the herdr plugin; the old state was never touched), then revert the merge. meshd stays installed either way.
- Old runs' cost stays readable with the deleted script: `git show fd9b36e:bin/mesh > /tmp/mesh && python3 /tmp/mesh cost --run <id>`.

**PRs, in order.**
1. Docs: this section, and the deferrals above.
2. (Done.) `mesh` beside `bin/mesh`: `cmd/meshctl` became `cmd/mesh`, the PATH prepend, `whoami` and `mesh dir`, run folders (`plans/`, `reports/`, `tasks/`), positional arguments for the commands agents already use.
3. (Done.) `install.sh --daemon`, the daemon's install, opt-in until PR 6 made it the install.
4. (Done.) Worker accountability, which replaces `mesh watch` and its exit codes: the parent is told when a worker exits, is lost, hangs or hits a limit; a worker that ends its turn with no report is nudged once, then its parent is told; a declared `wait` pauses that; `send` refuses a report while a task or answer is unread.
5. (Done.) `ORCHESTRATE.md`, `PROTOCOL.md`, the root card and the skills rewritten for daemon events, in `daemon/share/{protocol,skills}`; meshd takes `--share`, gives every agent `$AGENT_MESH_SHARE`, launches Claude with `--plugin-dir <share>`, and names the protocol in the first doorbell of each process (agy has no plugin dir, so its protocol tells it to read a skill's `SKILL.md`). A real run on the daemon (run rwddfor) passed, and its fixes landed.
6. (Done.) Lean Claude launches on the daemon (`claude.lean`, `--mcp`), then the cutover above.

**Entry point.** The delegate skills run in the user's own session, which is no actor on the daemon. The skill starts a run and a daemon root, writes the request and what it knows to `tasks/`, and the user continues in the app. The user's session can instead become the root in place: `mesh adopt --run-id <run>` (`daemon/internal/daemon/adopt.go`, the delegate skills' opt-in "from here") makes the Claude Code session it runs in the run's root: an agent row with `adopted_pid`, no terminal or worktree, a root token the CLI finds by walking up its process tree, doorbells written into the session's inbox socket, and a state followed from its session record. It needs `crossSessionInbound: accept` in the user's own settings (never edited by mesh), takes over the run's spawned root (same agent id, mailbox and workers), and is never reset, opened, stopped or resumed by the daemon. Importing any other kind of session (agy, a plain terminal) is still open ("Import a session").

**What `bin/mesh` still does that the daemon doesn't** (checked 2026-11-26 against `bin/mesh`'s subcommands and `mesh help` of main after the parity PRs #150 to #161). Everything else it did is ported: identity and run folders, accountability, the folder-trust dialog, the protocol and skills, asks, review rounds, verdicts and peers (with their conversations, `mesh import-peers`), the quota reserve and limit detection, resume onto a finished PR, `cost`, `models`, `conflicts`, `model`, `worktree`, `log`, `ring`, `open`, lean sessions with `--mcp`, `tools add` and the Playwright lease, compaction on resume, approve and follow-up, root reset, `brief` (with its PR list per agent, `mesh prs`) and `protocol <part>`, the `mesh watch` notes as accountability alerts, the SessionStart reminder, inbox-socket delivery that never lands on a human's draft, the login shell's `PATH`, the policy file (`~/.config/agent-mesh.json`, imported at meshd's first start, `repos.dir`), the Claude, agy, OpenCode and Copilot adapters, and the live checks of the fakes' stand-ins (`daemon/README.md`, "Live checks"; a live Copilot turn stays unchecked, as this account's Copilot is policy-blocked). Each gap below is marked for the user's decision: **blocks** the switchover or **can follow** it.
- `claude --bg` views and the herdr notification plugin: **can follow**; they were herdr's, and the apps notify instead (they must be running).
Dropped on purpose: status rows (`mesh waiting`, `mesh agents` and the alerts cover them), `routing` (`mesh settings --prefix tiers`), plan review by the user in a pane (the app's approve), and a terminal in the CLI.

## Decided

Settled for step 1 (2026-11-25, run mfb6a):

- **Repo layout:** this repo. `proto/` holds the schemas (with `buf.yaml`), `daemon/` the Go module; `desktop/` and `android/` come later. The schemas and every client change in one PR.
- **PTYs plus structured channels:** both, as "Architecture" says. Step 1 builds only the PTY side.
- **Terminal replay: both mechanisms.** The terminal host keeps a ring buffer of raw output per terminal (epoch plus byte offset) for exact replay, and feeds the same bytes to a server-side emulator per terminal, so any client can get a repaint. The emulator is [`charmbracelet/x/vt`](https://github.com/charmbracelet/x/tree/main/vt) (MIT, maintained, used by Charm's crush): it keeps the screen and scrollback, renders a snapshot, tracks modes through callbacks, and answers DA1/DA2 itself, which the host forwards only while no client is attached. It has no tagged release past v0.1.0, so it is pinned by commit, behind a small interface so it can be swapped. `hinshun/vt10x` lost: no commit since 2023.
- **Roles and actor classes:** the registry knows root, worker, asker, reviewer, device and read-only device from the start, each with a fixed set of actions pinned by a test. Only the root (or a device) starts change workers; a worker starts only read-only agents (askers, reviewers); askers and reviewers start none.
- **Names:** `meshd` (the daemon), `mesh-term` (the terminal host), `mesh` (the generated CLI, built from `cmd/mesh`). Their environment variables use the prefix `AGENT_MESH_`.

Settled for step 2, the desktop app's first version (2026-11-25, run mfb6a):

- **The renderer never reaches a host itself.** meshd refuses any request with an `Origin` header, which every renderer `fetch` and `WebSocket` carries, and a token in the renderer (or a token-injecting loopback proxy in the main process) would be the open control port happy had. Electron's main process holds the tokens and runs a **host bridge**: a generic forwarder from the renderer over IPC to the host's unix socket or TCP address, with the token added, that passes only the registry's paths (`/agentmesh.v1.MeshService/<Method>`; and a terminal's WebSocket, `/v1/term/<agent>?ticket=…`, opened by main without an Origin and passed through as bytes) and a few headers. The renderer's ConnectRPC transport is the generated client over a `fetch` backed by the bridge, so streaming works and nothing in the UI knows the bridge exists. The main process knows nothing of the API's meaning.
- **The host list is client-side.** It comes before any connection, so it can't live on a host: `hosts.json` in the app's data dir (0600), a host being a daemon's state dir (socket and device token read from it at call time) or, since pairing, another machine added by its pairing link: main redeems the one-time link (`PairingService/RedeemPairing`, its own call, never the renderer's) and keeps the device token it gets with the route (direct TLS address, relay) and the certificate pin; the connection is TLS pinned to that certificate, direct first and through the relay when the direct address can't be reached. The loopback address with a pasted token is no longer offered (hosts saved with one still work). It is the one UI state that isn't on a host; everything else is a host action or a host setting.
- **The terminal protocol client is plain TypeScript** (`desktop/src/terminal/session.ts`), apart from xterm: first attach is a repaint, a reattach asks for exactly the bytes after the last one written (epoch and offset), a repaint instead clears the screen, input is dropped until `ReplayDone` and `Ready`, a client dropped as slow (4001) reattaches at once, and a gap in the output starts over with a repaint. A `Resized` frame moves its epoch (so a reattach is still exact) and tells the view the host's size, which the status line names when another client made it smaller than the window.
- **What waits on the user is the host's to say** (`ListWaiting`, a read-only action for devices and for a run's lead, in `daemon/internal/daemon/waiting.go`): a plan or question to the run's lead with no answer to its author since, from an author not exited; plus agents shown at a prompt, as `bin/mesh status` derives its waiting rows. The app lists it again, after a pause that folds a burst of events, whenever a message, state change, exit, retire or stopped run could change it, and drops a list an event overtook. It reads a run's messages with `ListMessages` only when that run's Messages tab is first shown (any run, open or closed; a later snapshot tops up the runs asked for); approving or answering is an `answer` message to the author. **`unknown` is read, not sent:** it is a function of time and of terminal output only the daemon sees, so no event carries it (and one could not say when an agent returns to working); the app lists the agents again every 30 s. The store also follows `AgentRetired`, `AgentReopened`, `AgentPhaseChanged` and `RunStopped`, and every agent state has an icon and a sentence. An agent entering `ERROR` or `LIMITED` has a notification kind each (`error`, `limited`); `DevicePaired` carries the `pairing_id` of the link the device redeemed, so the pair screen matches a device to its link exactly (a host from before the field is matched by name, or by being the only new device). A `agentmesh://pair` link the OS opens the app with fills the add-a-host form and redeems nothing until the user confirms: any web page can send one, and redeeming adds whichever host it names.
- **App preferences are host settings.** The desktop's own rows (`app.notify.enabled`, `app.notify.sound`, `app.notify.events` (kinds `input`, `plan`, `question`, `report`, `failed`, `exited`, `limited`, `error`; all on by default but `exited` and `limited`: the daemon usually moves a limited agent on by itself), `app.tray.close_to_tray`, `app.theme`, `app.dock.new_agents` (`dock`, `badge`, `tab` or `none`: what a worker that starts while the app is open does) and the shortcuts `app.keys.*` (`palette`, `settings`, `dock`, `chat_toggle`, `tab_next`, `tab_prev`, `tab_close`, `waiting_next`: a key chord, or `""` for none)) are leaves of the daemon's one settings table (`daemon/internal/settings/schema.go`, `AppLeaves`), with no action or event of their own, so an agent reads and changes them like any setting. Each host's events notify by that host's settings; what closing the window does follows the first host's. The settings screen is generated from `ListSettings`' schema, with no per-setting UI code, and follows `SettingChanged` events.
- **The main process only shows.** The renderer decides what to notify and when (live events only: older than a minute is history; never the user's own actions; one notification per agent, a newer replacing the older), and tells main to show it, to set the tray's count and to hide on close. Main checks the shape and knows nothing of agents. With no tray (a desktop without one) closing the window really closes it.
- **Generated TypeScript** comes from `proto/` by `desktop/buf.gen.yaml` (`protoc-gen-es`, pinned in the lockfile, nothing remote) into `desktop/src/gen`, committed like `daemon/gen`; `desktop/scripts/check.sh` fails when it is stale.
- **A test pins the UI to the registry**: every action the UI calls must be a `MeshService` method a device may call (read from the descriptor's `action` option), and no source file outside the transport reaches the network.
- **Tests:** vitest and Testing Library for the React side; the bridge against real sockets; the generated client through bridge and adapter to a ConnectRPC server; and the built app driven over the Chrome DevTools Protocol with `playwright-core` (`connectOverCDP`, no browser download: Electron's own Chromium) against a real meshd on a temp state dir with the fake Claude CLI, in a HOME and user-data dir of their own.

Settled for step 3, connections part 1 (`mesh-relay`, TLS pinning and pairing, 2026-11-25, run mfb6a):

- **Separate relay binary (`cmd/mesh-relay`), shared protocol (`docs/relay.md`):** keeps SQLite and PTY host dependencies out of the VPS binary while keeping everything in `daemon/`.
- **Two relay listeners:** `--listen` (HTTP/WebSocket, port `443` behind `nginx-proxy-manager`) for daemon control (`/v1/daemon/control`), single-use dial-back streams (`/v1/daemon/stream?ticket=...`), `/healthz`, and an optional browser WS endpoint (`/v1/client/connect`); `--client-listen` (raw TCP port `8443`) for native apps (`MESH1 <host_id> <client_token>\n` -> `OK\n` -> raw splice), so Android OkHttp and desktop clients run pinned end-to-end TLS directly on the TCP socket without layering TLS over WebSockets.
- **Opt-in direct TLS (`meshd --listen`):** uses the same persistent ECDSA P-256 certificate (`tls.crt`/`tls.key`) as the relay route (`--tcp` remains plain loopback HTTP).
- **Separate `PairingService`:** `RedeemPairing` exchanges the one-time `amp_...` token (stored hashed, mutation scrubbed on redeem/expiry) for a per-device `amt_...` token without relaxing `MeshService`'s bearer-token interceptor; `CreatePairing`, `ListDevices` and `RevokeDevice` are restricted to `device-local`, and revocation cancels the device's open event and terminal streams immediately.

Settled for step 4, the Android app's first version (2026-11-25, run mfb6a):

- **Client:** generated from `proto/` by `android/buf.gen.yaml` (buf's built-in Java generator in lite mode, and `protoc-gen-connect-kotlin`; protoc and the plugin come from Maven Central through Gradle, checksum-verified, so generation installs nothing), into `android/app/src/gen`, committed like `daemon/gen`; `android/scripts/check.sh` fails when it is stale. ConnectRPC over OkHttp; server streams work over HTTP/1.1 against meshd's loopback listener (`DaemonE2ETest`).
- **A test pins the app to the registry**: every method the app's one client wrapper calls must be an action a device may call (read from `mesh.proto`'s `action` options), and nothing outside `net/` touches sockets or the generated client.
- **Routes:** a host holds routes in order. `Route.Direct(url, pin?)` is an `https://` address (system trust, or the one certificate whose SHA-256 is pinned, with no name check) or `http://` to this device and the emulator's host only (`network_security_config.xml`); a pinned one is what `meshd --listen` serves. `Route.Relay(relayTcp, hostId, token, pin)` is the user's own relay (relay and pairing, 2026-11-25, run mfb6a): an OkHttp `SocketFactory` whose sockets dial the relay wherever OkHttp asks, send `MESH1 <host id> <token>`, read `OK` a byte at a time (so none of the TLS that follows is eaten) and are then handed to OkHttp's own TLS, pinned and without a name check, with no proxy and a placeholder host name that is never resolved. Every connection, a terminal's WebSocket included, is a relay stream of its own. A relay refusal (`ERR auth|offline|busy|timeout`) is shown in its own words, and `auth` stops the retries like a refused token. Without a relay or `--listen`, a phone still reaches a host through `tailscale serve --https=443 http://127.0.0.1:<port>` or `adb reverse`.
- **Pairing:** a code is the `agentmesh://pair` link (`docs/relay.md`) from `mesh pair`, or a QR code of it (`mesh pair` draws one in the terminal above the link; `--format json` prints the whole response); the app reads it by pasting, by opening it from outside (an intent filter; the link only fills the screen, nothing is sent until the user has seen the host it names and taps Pair) or by scanning (CameraX for frames and ZXing core for decoding: pure Java, Apache-2.0, no Google Play services). It redeems the token with `PairingService/RedeemPairing` on the code's first route that answers (the one unauthenticated call a device makes; `RegistryPinTest` allows exactly that), checks that the host and the certificate are the ones in the code, and saves the host with its `daemonId`. The relay client token is sealed with the Keystore like the device token. A link's secrets are kept out of every `toString`, log and saved state.
- **Connection:** one per host, ready on `Hello`, a heartbeat that drops a dead socket, a backoff that resets only after the first acknowledged heartbeat, a generation fence, a refused token that stops retrying, and a nudge on the app coming to the foreground or a network coming up. The same rules as the desktop's `connection.ts`, with the same test cases.
- **State** is built only from the snapshot and events (`reduce`, pure), with a parity test that a live stream and its replay agree; what waits on the user is derived as on the desktop.
- **Device tokens** are sealed with an Android Keystore key; the host list is `hosts.json` in the app's private storage.
- **Themes:** the six JSON files of `shared/themes/` (the desktop reads the same files), mapped to a Material 3 scheme plus the agent-state and terminal colours and the desktop overhaul's `hue-1…8`, `vendor-*` and `glow` (PR 1 of the Android overhaul, 2026-11-27: bundled Geist and JetBrains Mono as variable TTFs, the type scale at 16 for prose, vendor marks and project hues shared with the desktop, shimmer skeletons, list and fold motion, a short nav slide and predictive back, syntax-coloured code blocks (+65 KB debug APK), the fonts about +0.36 MB); the theme follows the host's `app.theme` unless this device chose one (or the system's mode). The files' fonts and density are the desktop's: Android keeps its own type and spacing.
- **Navigation** (PR 2 of the Android overhaul, 2026-11-27): one drawer instead of host, run and tab lists: projects across every host (the desktop's `sidebar.ts` (desktop PR #193) rules in `derive/Projects.kt`: pins, then what waits on the user, then activity; a lead per run with its workers under it, reviewers and askers as a chip, finished runs under "Earlier"), what waits on the user, search and a quota footer. It slides over home and the agent, and from 840 dp wide stays open beside them. Pins and hidden projects are device-local (`DevicePrefs`), hosts are in Settings › Hosts, and opening an agent replaces the open one so Back goes home. The desktop's status buckets and the Runs / By status switch are not ported: the phone has no use for a second grouping.
- **Agent screen** (PR 3 of the Android overhaul, 2026-11-27): chat first, no tabs. `>_` swaps the chat for the terminal; ⋯ opens Details (pull requests with Hold, the agent tree, cost folded, branch, worktree, conversation, Send a message, Stop, Retire); a strip under the bar holds the agent's tree; plans, questions, tasks and reports are cards in the chat at their place by time; a held-down drawer row offers Stop, Retire, Rename and Stop the run (each confirmed). The calls are the registry's `StopAgent`, `RetireAgent`, `RenameRun` and `StopRun`.
- **Terminal:** read-only first, with Termux's `terminal-emulator` module copied into `android/terminal-emulator/` (without its local-process `TerminalSession`) and our own Compose renderer, since `terminal-view` needs a local-process session. The viewer attaches with size 0 so it never resizes the shared terminal, drops the emulator's replies to terminal queries (the host answers them), and repaints on an epoch change. Typing and a key row follow.
- **Tests without an emulator:** JVM unit tests, the Compose screens under Robolectric, and the end-to-end tests against a real meshd on a temp state dir with the fake `claude`. Maestro flows are written; they run only where an emulator does (WSL without KVM: not here).

## Open decisions

- Tailcat (a Tailscale-compatible library with no account and your own relay server, per happy) instead of `tsnet`, later.
- agy's and the other CLIs' permission prompts reaching the root the way Claude's do (a hook held for the parent), and Plan mode through the same hook (`ExitPlanMode` is a `PermissionRequest` whose allow needs `updatedInput`; design in `TODO.md`).
- Interactive `agy` permission-prompt handling when `--dangerously-skip-permissions` is off: gate tool calls through a daemon `PreToolUse` hook vs switch `agy` to its ACP server's permission requests vs require `--dangerously-skip-permissions`.
