# desktop/: rules for agents

The desktop app: Electron, React and TypeScript, styled with Tailwind and shadcn/ui-style components on Base UI. It is a client of the daemon (`daemon/`), step 2 of `docs/apps.md`, and nothing here is installed or put on PATH. `README.md` is the full reference (what the app does, the host bridge, trying it by hand); `docs/apps.md` has the plan and what is decided ("Stack", "Decided"). context7 and WebSearch/WebFetch are available to every agent: the stack (Electron 45, React 19, TypeScript 7, Tailwind 4, Base UI, Vite 8, Vitest 5) is newer than most training data, so check its current API there rather than from memory.

## Toolchain

Node (from nvm) and pnpm 11, both on PATH. Run every command from `desktop/`. `pnpm install` also fetches Electron's binary. `pnpm gen` and the e2e tests also need buf and Go, which are off PATH (`daemon/CLAUDE.md`, "Toolchain"); `scripts/check.sh` adds them itself.

## Commands

```bash
scripts/check.sh              # the CI equivalent: locked install, generated code current, typecheck, tests, build, e2e
SKIP_E2E=1 scripts/check.sh   # without the end-to-end tests
pnpm test                     # unit tests (vitest, jsdom): src/**/*.test.ts(x) and electron/**/*.test.ts
pnpm test src/store.test.ts   # one file
pnpm typecheck                # the renderer and the main process (two tsconfigs)
pnpm gen                      # TypeScript from ../proto after a .proto change
pnpm build && pnpm e2e        # the end-to-end tests: the built app over CDP, against a real meshd
```

The e2e tests open a real Electron window. `pnpm e2e` (and `scripts/check.sh`) runs `scripts/e2e.sh`, which puts it on a virtual display with `xvfb-run` when installed, `DISPLAY` set or not: never set `DISPLAY=:0` to see it, since under WSLg that is the user's Windows screen. `E2E_VISIBLE=1` uses `DISPLAY` instead; without xvfb `DISPLAY` is required. They build meshd, mesh-term, mesh and the fake claude from `daemon/` into a temp dir and give every run its own state dir, HOME and user-data dir, so they touch nothing of the user's. Run `scripts/check.sh` before you report a change here: the repo runs no hosted CI.

## Layout

- `electron/`: the main process, kept thin: `main.ts` the window, `bridge.ts` and `socket.ts` the host bridge, `route.ts` and `pair.ts` how a paired host is reached (pinned TLS, direct or through the relay) and how a pairing link is redeemed, `hosts.ts` the saved host list, `links.ts` pairing links the OS opens the app with (read in main, shown for the user to confirm, never redeemed on their own), `shell.ts` notifications and the tray (it knows nothing of agents), `preload.cts` the API the renderer gets.
- `shared/api.ts`: types only, the contract between main and the renderer.
- `src/`: the renderer. `transport.ts`, `connection.ts`, `store.ts`, `refresh.ts`, `derive.ts`, `usage.ts` (the usage and cost screen's rows), `actions.ts`, `components/` (shadcn-style primitives in `components/ui/`), `terminal/session.ts` (the terminal stream protocol, with no xterm or Electron in it), `theme.ts` (loads the six theme files of `../shared/themes/`, shared with Android, and applies the host's `app.theme`).
- `src/gen/`: the generated TypeScript (committed).
- `e2e/`: the end-to-end tests and their harness.

`README.md` "Layout" says what each file holds.

## Conventions

- **Styling:** Tailwind classes and shadcn/ui-style components on Base UI only. No CSS beyond Tailwind's one entry file, `src/index.css`. The primitives in `src/components/ui/` are copied into the repo so we own them: restyle them there rather than wrapping them. Colours are theme tokens (`theme.ts`), not literals; a theme is a JSON file in `shared/themes/`, never CSS. The theme follows the selected host's effective `app.theme` (its default included); Android follows the first host that set one itself, so with several hosts the two may differ.
- **The renderer never reaches a host.** meshd refuses any request with an `Origin` header, and a token in the renderer would defeat that. The main process holds the tokens and forwards calls (`electron/bridge.ts`, `electron/socket.ts`). Keep the renderer sandboxed, with context isolation, its CSP, no navigation and no new windows.
- **A paired host's certificate pin is the whole check.** `electron/route.ts` hands a connection on only after its leaf certificate matched the pin from the pairing link; never add a fallback to an unpinned or unverified connection, and keep the pairing link's parsing in main (the renderer sends text).
- **Every call to a host** goes through `src/actions.ts` and must be a `MeshService` action a device may call. Only `src/transport.ts` reaches the network. `src/registry.test.ts` checks both, so a new call starts in `actions.ts`.
- **The UI has no abilities of its own** (`docs/apps.md`, "Principles"): anything the UI does is an action an agent or the CLI can call too. A new capability is a registry action first (`proto/CLAUDE.md`).
- **State** is built from the snapshot and events (`store.ts`). What the host derives, the app asks the host for rather than deriving it twice: what waits on the user is `ListWaiting` (`refresh.ts` lists it again after the events that change it), and an agent that went quiet shows as `unknown` only when the agents are listed again (every 30 s).
- **App preferences are host settings:** a new `app.*` row is a leaf in the daemon's `internal/settings/schema.go` (`AppLeaves`), with no action or event of its own.
- **Generated code:** never edit `src/gen/` or `electron/icons.ts` by hand. Regenerate them with `pnpm gen` and `node scripts/make-icons.mjs`.
- **Code style:** follow the existing code: single quotes, no semicolons, two-space indent (`@/` resolves to `src/`). There is no formatter or linter, so `pnpm typecheck` and the tests are the gate.
- **Trying it by hand:** give meshd a temp state dir, as `README.md` "Trying it by hand" shows. Never point the app at the user's real state dir.
- A change to these commands, the layout or the conventions updates this file in the same PR.
