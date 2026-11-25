# daemon

The Go daemon that replaces herdr and the pane-handling parts of `bin/mesh` (`docs/apps.md`, "Order of work" step 1). It doesn't replace anything yet: the live mesh is still `bin/mesh`, and nothing here is installed or put on PATH.

## Layout

- `../proto/`: the API as protobuf. `agentmesh/v1` is the action registry (`MeshService`, each RPC with its allowed actor classes in the `action` option), `agentmesh/term/v1` the terminal host's API and WebSocket frames, `agentmesh/adapter/v1` the CLI-adapter plugin interface. `buf.yaml` and `buf.gen.yaml` live there.
- `gen/`: the generated Go code (committed; regenerate with `cd proto && buf generate`). Reviewers can skip it.
- `cmd/meshd`: the daemon.
- `internal/registry`: the action registry read from the descriptors; `testdata/actors.golden` pins what each actor class may call.
- `internal/auth`: tokens (stored as SHA-256), the interceptor that takes the actor from the token, and the browser refusal.
- `internal/store`: SQLite (pure Go, WAL), embedded numbered migrations, backed up before migrating.
- `internal/events`: the resumable event stream.
- `internal/daemon`: the server and its handlers (`agents.go`: spawning, hooks, adoption, doorbells, the terminal proxy); `internal/client`: how local callers dial it; `internal/statedir`: the state dir's files.
- `cmd/mesh-term`, `internal/termhost`: the terminal host. `internal/termclient`: the daemon's side of it (its API, terminal streams, starting it).
- `internal/adapter`: the CLI-adapter registry; `internal/adapter/claude`: Claude Code's adapter. `internal/reducer`: the pure state reducer. `internal/hook` and `cmd/meshctl`: the hook command and its spool. `internal/proc`: pids checked by start time.
- `internal/testutil/fakeclaude`: the fake `claude` the tests run (it acts out turns and fires the hooks from its `--settings`).
- `packaging/systemd/`: the two user units for production (shipped, not installed).
- `cmd/meshctl`, `internal/cli`: the CLI, generated from the registry at run time.
- `e2e/`: the end-to-end test of the real binaries (`TestEndToEnd`).

## Toolchain

Go (see `go.mod`), [buf](https://buf.build) for the protos, and for `go test -race` a C compiler. The code generators are pinned as `tool` directives in `go.mod`, so `buf generate` installs nothing. On the dev machine they are user-local, off PATH: Go in `~/.local/go`, buf in `~/.local/opt/buf/bin`, and `~/.local/opt/zigcc/cc` (a `zig cc` wrapper) as the C compiler. `scripts/check.sh` finds them there.

## Commands

```bash
daemon/scripts/check.sh                      # the CI equivalent: buf lint and breaking, gen current, vet, test -race, pytest
cd daemon && go test ./...                   # the Go tests only
cd proto && buf generate                     # after changing a .proto
cd daemon && go test ./internal/registry -update   # accept a reviewed change of who may call what
cd daemon && go build -o bin/ ./cmd/...      # binaries into daemon/bin/ (gitignored)
```

## Running a dev instance

Always give it a state dir of its own: meshd has no default, so it can't land in a real one by accident. A unix socket path holds about 107 bytes, so keep the state dir path short: meshd refuses to start with a longer one.

```bash
state=$(mktemp -d)
daemon/bin/meshd --state "$state"            # socket at $state/meshd.sock, device token at $state/device.token
```

Every call needs a token. The host's own CLI uses the device token meshd writes on first start (`device.token`, mode 0600); each agent gets its own at spawn, in `AGENT_MESH_TOKEN`. The actor of every action comes from the token, never from the request. Requests that carry a browser's `Origin` or `Sec-Fetch-Site` header are refused, and `--tcp` binds loopback only. A token of an agent names its run and reads only that run; a token without one is not stored.

A run is one trust domain for events for now: every agent of a run receives its run's events, so a reviewer or asker sees the bodies of the `MessageSent` events between the run's other agents, though it can send only to its parent and children. `TODO.md` tracks revisiting it.

## meshctl

Every action of the registry is a `meshctl` command, built at run time from the compiled schemas: the command is the action's `cli` name, each request field is a flag (`snake_case` → `--kebab-case`; enums by short name, `--role worker`; message fields as JSON; `@file` or `@-` for text), `--json` gives the whole request, and the response prints as JSON (a stream as one object per line). Writes get a fresh mutation id unless `--mutation-id` is given. `meshctl help` lists the commands, `meshctl help <command>` its flags. Only `hook` and `version` are written by hand.

```bash
export AGENT_MESH_STATE=$state
daemon/bin/meshctl start-run --title demo
daemon/bin/meshctl spawn --run-id <run> --repo ~/projects/shop-api --slug fix-typo --task @task.md
daemon/bin/meshctl agents --run-id <run>
daemon/bin/meshctl send --to shop-api-fix-typo --type task --body "…"
daemon/bin/meshctl events --run-id <run>                 # follow; resume with --cursor '{"generation":…,"seq":…}'
```

An agent's own shell has `AGENT_MESH_TOKEN` and `AGENT_MESH_STATE`, so the same commands act as the agent (`meshctl inbox`, `meshctl send --to parent …`).

## Terminals

`mesh-term` owns the agents' PTYs, in a process of its own so a daemon restart leaves them running; at boot the daemon adopts what it finds (`AgentAdopted`), records exits that happened while it was down, marks an agent whose terminal is gone as `lost`, and never kills a terminal no agent claims.

- **Supervision.** In production both run as systemd user units (`packaging/systemd/`): `agent-mesh-term.service` holds the PTYs in its own cgroup, and `meshd --term-launch systemd` starts it when no host answers. Without systemd (and in dev and tests), `--term-launch detached` starts `mesh-term` in its own session; tests pass `--max-lifetime` so a killed test leaves nothing behind. One host per dir (a held lock), a token in `term/term.token`, and no pid is ever signalled without its start time matching.
- **Streams.** `OpenTerminal` gives a one-time ticket; the client opens `/v1/term/<agent>?ticket=…` on the daemon, which splices it to the host. Frames are `agentmesh.term.v1.TermFrame` (binary). The first client frame is `Attach`: `EXACT` (from an epoch and offset, falling back to a repaint marked `reanchored`), `TAIL` (mode preamble, then live) or `REPAINT` (preamble and the screen from the host's emulator, then live). The replay ends with `ReplayDone`; input sent before the client's `Ready` is dropped, so replayed queries draw no answers. While no client that can type is attached, the host's emulator answers terminal queries itself. The size is the smallest client's, clamped to 20-500 columns by 5-300 rows; only a client with input rights may resize after attaching; a resize moves the epoch. A client that falls behind is closed with code 4001 and reattaches by offset; the PTY is never paused.

## Agents and hooks

An agent starts with an allowlist of meshd's environment, not all of it: the shell basics (`HOME`, `PATH`, `LANG`, `TMPDIR`, `XDG_*`, `SSH_AUTH_SOCK`), what Claude Code needs to reach its model (`CLAUDE_CONFIG_DIR`, `CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_*`, the proxy variables, the Bedrock and Vertex settings and credentials), a terminal, and its own identity. `meshd --agent-env KEY=VALUE` adds more, and `--cli claude=<path>` pins the binary; without it a bare `claude` is looked up on the agent's `PATH` by meshd, not on `mesh-term`'s. A meshd run by a systemd user unit has only what the unit sets, so put the login variables there (or in `--agent-env`).

`SpawnAgent` makes a worktree on `mesh/<run>/<slug>`, mints the agent's token (in `AGENT_MESH_TOKEN`, and in `<state>/agents/<id>/token` for its hooks), and starts its CLI through the adapter. Claude Code gets a `--session-id` and per-launch `--settings` hooks that run `meshctl hook claude --state <dir> --agent <id> <Event>`. The hook reads the daemon's address at call time and, when the daemon is down, spools to `<state>/spool/<agent>/`; the daemon drains the spool every few seconds. Hooks always exit 0 and print nothing (failures go to `<state>/hook.log`). The adapter turns each hook into neutral events and `reducer.Reduce` derives the state; a working agent, or one waiting on a prompt, that nothing was heard from for 10 minutes shows as unknown (Claude may fire no hook when a prompt is dismissed). A `SessionStart` from a compaction or a resume mid-turn does not make an agent idle. A spooled hook the daemon answers with an error ten times is dropped (kept as `.bad`, with a `HookDecodeFailed` event); idempotency records of hooks are pruned after a day, others after two weeks. Messages are stored and announced by a one-line doorbell typed into the agent's terminal once it is idle and no client with input rights has typed in the last 15 seconds (it retries every 2 s); the agent reads them from its inbox. An asker or reviewer launches with a `--disallowedTools` list (no edits under its worktree or the repo, no `git push` or `gh pr` writes), which holds under `--dangerously-skip-permissions`.

## Protocol notes

- **Writes** carry `RequestMeta`: a repeated `mutation_id` returns the first response without running again, and a write past `expires_at` is refused. Events a write causes carry its mutation id.
- **Events** have a `seq` and the log's `generation`. `SubscribeEvents` with a cursor of the same generation resumes (`Resumed`, then the backlog, then live); anything else starts with a `Snapshot`. Only an `End` frame ends a stream on purpose; after any other close, resubscribe from the last cursor. A subscriber more than `events.LiveBuffer` events behind is dropped (`resource_exhausted`) and resumes the same way.
