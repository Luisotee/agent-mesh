# desktop

The agent-mesh desktop app: Electron, React and TypeScript, styled with Tailwind and shadcn/ui-style components (Base UI), no CSS beyond Tailwind's one entry file. It is the second step of `docs/apps.md` ("Order of work"); the plan, the principles it follows and what is decided are there. Nothing here is installed or put on PATH.

The app is a client of the daemon (`daemon/`). Everything it does to a host is a call on the host's action registry (`proto/`, ConnectRPC with generated clients), so an agent or the CLI can do all the UI can. It has no account, no hosted service and no telemetry, and talks only to the hosts it is told about.

## What the first version does

Hosts (a daemon on this machine by its state dir, or a loopback TCP address with its device token: plain HTTP, so never a remote address until the connection is encrypted), a connection per host with a banner and a log, and the lead-agent view: each run as a tree from its lead, workers and their read-only agents under it, each row with its role, a plain status sentence, repo and model, and a state icon whose shape differs by state. It starts runs, spawns agents, messages and stops them. Each agent has a terminal (xterm.js on the daemon's terminal stream: it takes typing, keeps its screen across a daemon restart, searches, opens links in the browser). A Messages tab shows what an agent sent and was sent, with plans, questions and reports as cards (Markdown, no raw HTML or images, links in the browser); a plan or question to a run's lead that nobody answered is listed under "Waiting on you" (with the agents stopped at a prompt) and counted on the host's badge, and the user approves a plan, asks for changes or answers a question in place: an `answer` message to its author, recorded with the device as its actor. A Settings screen (the gear by the host's name) is generated from the schema the host sends: a control per type, the host's own check on every value with its message shown, reset to the default, a filter, live changes from agents and other devices. Native notifications (an agent stopped at a prompt, a plan or question or report or failure sent to a run's lead; only live events, never history replayed after a reconnect, never what the user did, one per agent replacing the last) and the tray (an icon that shows when something waits on the user, a menu, and the window hiding to it on close) follow the host's `app.*` settings.

## Layout

- `electron/`: the main process, kept thin. `bridge.ts` is the host bridge (below), `hosts.ts` the saved host list, `main.ts` the window, `preload.cts` the API the renderer gets (`window.desktop`, from `shared/api.ts`).
- `electron/shell.ts`: notifications, the tray and what closing the window means; it knows nothing of agents (`shell-rules.ts` holds its checks). `scripts/make-icons.mjs` draws the tray icons into `electron/icons.ts`.
- `shared/api.ts`: types only, the contract between main and renderer.
- `src/terminal/session.ts`: one terminal's stream protocol (attach modes, replay, `Ready`, resume by offset, backoff), with no xterm or Electron in it; `components/terminal-view.tsx` wires it to xterm.js.
- `src/`: the renderer. `transport.ts` (a `fetch` over the bridge for the generated clients), `connection.ts` (one host's connection: handshake, heartbeat, backoff, resume), `store.ts` (the host's state, built from the snapshot and events, and the messages `ListMessages` loads: `messages.ts`), `derive.ts` (trees, status sentences, what waits on the user), `actions.ts` (every call the UI makes, with idempotency metadata), `notify.ts` (which events notify, and what they say), `prefs.ts` (the `app.*` settings), `settings-form.ts` (a control per setting type), `model.ts` (the hosts and the selection), `components/` (our own, with shadcn-style primitives in `components/ui/`, copied into the repo so we own them), `theme.ts` (a theme is a set of tokens written as CSS variables; Dracula first).
- `src/gen/`: generated TypeScript (`pnpm gen`; committed; reviewers can skip it).
- `e2e/`: the end-to-end tests and their harness (a real meshd, a fake Claude CLI, a throwaway git repo).

## The host bridge

meshd refuses any request a browser made (an `Origin` header), and a token in the renderer would defeat that. So the main process holds the tokens and forwards the registry's calls: the renderer's `fetch` becomes an IPC request, main sends it over the host's unix socket or TCP address with the token added (read at call time, so a restarted daemon or a new token works), and streams the answer back in chunks. It forwards only `POST /agentmesh.v1.MeshService/<Method>` and a few headers, and refuses the rest (`electron/bridge.test.ts`). A terminal's WebSocket goes the same way (`electron/socket.ts`): main opens `/v1/term/<agent>?ticket=…` without an Origin and passes the binary frames through unread; the ticket from `OpenTerminal` is its credential. The renderer runs sandboxed, with context isolation, a CSP, no navigation and no new windows.

## Commands

Run from `desktop/`. Needs Node and pnpm (pnpm 11 here); `pnpm install` also fetches Electron's binary. For `pnpm gen`, `buf` and Go as for `daemon/` (`daemon/README.md`, "Toolchain").

```bash
scripts/check.sh              # the CI equivalent: locked install, generated code current, typecheck, tests, build, e2e
SKIP_E2E=1 scripts/check.sh   # without the end-to-end tests
pnpm test                     # unit tests (vitest)
pnpm typecheck
pnpm gen                      # TypeScript from ../proto after a .proto change
pnpm build && pnpm e2e        # the end-to-end tests: the built app over CDP, against a real meshd
```

The e2e tests open a real Electron window, so they need a display (WSLg has one; `xvfb-run` otherwise). They build meshd, mesh-term, meshctl and the fake claude into a temp dir and give each run a state dir, HOME and user-data dir under the temp dir: they read and write nothing of yours, and refuse an `AGENT_MESH_STATE` aimed elsewhere by clearing it. They pass `--no-sandbox` to Electron (WSL has no SUID helper for Chromium's own sandbox); the page itself runs sandboxed either way.

## Trying it by hand

Give everything a state dir of its own, as the tests do:

```bash
state=$(mktemp -d)                                   # keep the path short: a unix socket path holds ~107 bytes
(cd ../daemon && go build -o bin/ ./cmd/... && go build -o bin/claude ./internal/testutil/fakeclaude)
../daemon/bin/meshd --state "$state" --cli claude=../daemon/bin/claude &
pnpm build
AGENT_MESH_DESKTOP_STATE=$state AGENT_MESH_DESKTOP_USERDATA=$(mktemp -d) pnpm start -- --no-sandbox
```

`AGENT_MESH_DESKTOP_SHELL_LOG=<file>` makes main record what it was asked to notify, count and configure instead of showing notifications (the e2e tests read it). `AGENT_MESH_DESKTOP_STATE` makes the state dir the app's one fixed host (no host list is read or written); without it the app asks for a host and keeps the list in its user-data dir.
