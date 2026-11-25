# daemon

The Go daemon that replaces herdr and the pane-handling parts of `bin/mesh` (`docs/apps.md`, "Order of work" step 1). It doesn't replace anything yet: the live mesh is still `bin/mesh`, and nothing here is installed or put on PATH.

## Layout

- `../proto/`: the API as protobuf. `agentmesh/v1` is the action registry (`MeshService`, each RPC with its allowed actor classes in the `action` option), `agentmesh/term/v1` the terminal host's API and WebSocket frames, `agentmesh/adapter/v1` the CLI-adapter plugin interface. `buf.yaml` and `buf.gen.yaml` live there.
- `gen/`: the generated Go code (committed; regenerate with `cd proto && buf generate`). Reviewers can skip it.
- `cmd/meshd`: the daemon.
- `internal/registry`: the action registry read from the descriptors; `testdata/actors.golden` pins what each actor class may call.
- `internal/auth`: tokens (stored as SHA-256), the interceptor that takes the actor from the token, and the browser refusal.
- `internal/store`: SQLite (pure Go, WAL), embedded numbered migrations, backed up before migrating.
- `internal/settings`: the host's settings table (`schema.go`): each leaf's key, type, default, summary and choices, and the check a value must pass.
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

Every action of the registry is a `meshctl` command, built at run time from the compiled schemas: the command is the action's `cli` name, each request field is a flag (`snake_case` → `--kebab-case`; a bool is a switch, `--peek`; enums by short name, `--role worker`; message fields as JSON; `@file` or `@-` for text, `@@` for a literal leading `@`), `--json` gives the whole request (also before the command: `meshctl --json '{…}' set`) and the other flags override it (a repeated field's first flag replaces the list the JSON gave, the next ones add to it), a `google.protobuf.Value` field takes any JSON or, when the text isn't JSON, the string it spells (`--value sonnet`, `--value 75`, `--value '["haiku","sonnet"]'`), and the response prints as JSON (a stream as one object per line). Writes get a fresh mutation id unless `--mutation-id` is given. `--state` goes before or after the command. `meshctl help` lists the commands, `meshctl help <command>` its flags. Only `hook` and `version` are written by hand.

```bash
export AGENT_MESH_STATE=$state
daemon/bin/meshctl start-run --title demo
daemon/bin/meshctl spawn --run-id <run> --repo ~/projects/shop-api --slug fix-typo --task @task.md
daemon/bin/meshctl agents --run-id <run>
daemon/bin/meshctl send --to shop-api-fix-typo --type task --body "…"
daemon/bin/meshctl events --run-id <run>                 # follow; resume with --cursor '{"generation":…,"seq":…}'
```

An agent's own shell has `AGENT_MESH_TOKEN` and `AGENT_MESH_STATE`, so the same commands act as the agent (`meshctl inbox`, `meshctl send --to parent …`).

## Settings

The policy (`bin/mesh`'s `POLICY`, design §21) is host data, the same from every device and readable by the agents that run under it. `internal/settings/schema.go` is the one table: a leaf's key, type, default, summary, choices and whether it is read only at start. The settings screen is generated from it, and an app adds its own rows there (the desktop's `app.*`); it needs no action or event of its own. Only overrides are stored (the `settings` table, migration `0002`); a leaf nobody set is its default, so a new default reaches every host that never overrode it.

- `ListSettings {prefix}` returns `Setting`s: `key`, `value` (effective), `default`, `is_set`, `schema {type, summary, choices, restart}`, and who set it and when. Any actor class may call it. `meshctl settings --prefix tiers.claude.`.
- `SetSetting {meta, key, value}` and `ResetSetting {meta, key}` write: only a device or a root, idempotent like every write, and each emits a global `SettingChanged {key, value, previous, to_default}` with the actor and the mutation id. The event has no run, so every subscriber gets it, a run's agents included. A `Snapshot` doesn't carry settings: a client lists them after one, then follows the events. `meshctl set --key quota_stop --value 75`, `meshctl unset --key quota_stop`.
- A value is checked against its type: `string`, `strings`, `int`, `percent`, `bool`, `seconds`, `effort`, `chain`, `slots`, `kinds`, `mb_or_auto`, `json`. A failure is `InvalidArgument` naming the rule (`quota_stop must be a whole number 0 to 100`); a key no leaf has is `NotFound`.
- The daemon reads a leaf with `Daemon.Setting(reader, key)` (the override, else the default; a stored value the leaf's rule no longer takes is logged and the default used; a failed read is an error), so a change applies to the next read. A write that changes nothing (the same value again, a reset of a setting nobody set) emits no event. `seconds` is capped at 1e9 so a `time.Duration` can't overflow. Not ported yet, with the herdr-only leaves (views, `side_panes`, `root_reset.*`, `compact.*`, `claude.*`, `worker_cpus`, `worker_memory_high`): they wait for the switchover. `meshd` doesn't import `~/.config/agent-mesh.json`.

## Terminals

`mesh-term` owns the agents' PTYs, in a process of its own so a daemon restart leaves them running; at boot the daemon adopts what it finds (`AgentAdopted`), records exits that happened while it was down, marks an agent whose terminal is gone as `lost`, and never kills a terminal no agent claims.

- **Supervision.** In production both run as systemd user units (`packaging/systemd/`): `agent-mesh-term.service` holds the PTYs in its own cgroup, and `meshd --term-launch systemd` starts it when no host answers. Without systemd (and in dev and tests), `--term-launch detached` starts `mesh-term` in its own session; tests pass `--max-lifetime` so a killed test leaves nothing behind. One host per dir (a held lock), a token in `term/term.token`, and no pid is ever signalled without its start time matching.
- **Streams.** `OpenTerminal` gives a one-time ticket; the client opens `/v1/term/<agent>?ticket=…` on the daemon, which splices it to the host. Frames are `agentmesh.term.v1.TermFrame` (binary). The first client frame is `Attach`: `EXACT` (from an epoch and offset, falling back to a repaint marked `reanchored`), `TAIL` (mode preamble, then live) or `REPAINT` (preamble and the screen from the host's emulator, then live). The replay ends with `ReplayDone`; input sent before the client's `Ready` is dropped, so replayed queries draw no answers. While no client that can type is attached, the host's emulator answers terminal queries itself. The size is the smallest client's, clamped to 20-500 columns by 5-300 rows; only a client with input rights may resize after attaching; a resize moves the epoch. A client that falls behind is closed with code 4001 and reattaches by offset; the PTY is never paused.

## Agents and hooks

An agent starts with an allowlist of meshd's environment, not all of it: the shell basics (`HOME`, `PATH`, `LANG`, `TMPDIR`, `XDG_*`, `SSH_AUTH_SOCK`), what Claude Code needs to reach its model (`CLAUDE_CONFIG_DIR`, `CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_*`, the proxy variables, the Bedrock and Vertex settings and credentials), a terminal, and its own identity. `meshd --agent-env KEY=VALUE` adds more, and `--cli claude=<path>` pins the binary; without it a bare `claude` is looked up on the agent's `PATH` by meshd, not on `mesh-term`'s. A meshd run by a systemd user unit has only what the unit sets, so put the login variables there (or in `--agent-env`).

`SpawnAgent` makes a worktree on `mesh/<run>/<slug>`, mints the agent's token (in `AGENT_MESH_TOKEN`, and in `<state>/agents/<id>/token` for its hooks), and starts its CLI through the adapter. Claude Code gets a `--session-id` and per-launch `--settings` hooks that run `meshctl hook claude --state <dir> --agent <id> <Event>`. The hook reads the daemon's address at call time and, when the daemon is down, spools to `<state>/spool/<agent>/`; the daemon drains the spool every few seconds. Hooks always exit 0 and print nothing (failures go to `<state>/hook.log`). The adapter turns each hook into neutral events and `reducer.Reduce` derives the state; a working agent, or one waiting on a prompt, that nothing was heard from for 10 minutes shows as unknown (Claude may fire no hook when a prompt is dismissed). A compaction remembers the state it interrupted and restores it when the compacted session starts (idle stays idle, mid-turn stays working); a resume's `SessionStart` does not make an agent idle. A spooled hook the daemon's drain gets an error for (not "unreachable") at least ten times over at least five minutes is dropped (kept as `.bad`, with a `HookDecodeFailed` event); the hook process's own drain never drops one; idempotency records of hooks are pruned after a day, others after two weeks. Messages are stored and announced by a one-line doorbell typed into the agent's terminal once it is idle and no client with input rights has typed in the last 15 seconds (it retries every 2 s); the agent reads them with `meshctl inbox`. An asker or reviewer launches with a `--disallowedTools` list (no edits under its worktree or the repo, no `git push` or `gh pr` writes), which holds under `--dangerously-skip-permissions`.

## Same conversation: tiers, approve, switch, resume

A model change reopens the agent's own conversation (`claude --resume <session> --model … --effort …`), never a fresh agent briefed again, so it keeps its research (design §9, `bin/mesh`'s `swap_model`).

- **Tiers.** `SpawnAgent` takes a `tier` (`tiers.<kind>.<tier>` in the settings: the chain's first model at its effort) or a `model` the user named (at `effort`, else high). Neither is `InvalidArgument`: there is no default model. A named model or effort with no plan phase is `pinned`.
- **Plan phase.** `plan` starts the agent `PLANNING`, a Claude agent under the same `--disallowedTools` list as a read-only one (`deny_edits`), optionally with a preset `exec_tier`. A `plan` sent by an agent with no phase that isn't pinned enters one, without a reopen (it plans on the model it is on, with no deny list).
- **`ApproveAgent`** (its parent or a device, only when its newest report since its last task or answer is a `plan`) stores the `approved` answer, sets `EXECUTING`, and reopens it on the execution tier (given, else the preset; or a named model) without the deny list. `keep_model` executes on the planning model; only the deny list goes, which is still a reopen. An agent with no plan phase is approved as it is.
- **`SwitchAgent`** moves it to a tier, a model, an effort alone, or `next`: the next model of its phase's tier chain (the quota move; never back to one before, and `FailedPrecondition` at the chain's end). It stores a `task` telling the agent where it is now. Quota hits aren't detected yet: the root or the user calls `next`.
- **`ResumeAgent`** reopens a stopped agent: with a task it plans (`plan`, on the tier given, else the one it had), a direct task, or as it was.
- **The reopen.** Each of these stores a `Reopen` on the agent (`pending_reopen`, event `AgentReopenRequested`); a newer request replaces one not done yet. It runs when the agent is idle or stopped and no client with input rights is typing, else at its next `IDLE`, before any doorbell: the old process is stopped (SIGTERM, then SIGKILL), its exit is not recorded as one, a new token is minted, and the conversation is relaunched in a new terminal labelled with the reopen's id (`AgentReopened`). The agent keeps its id, name, worktree and session; the message the action stored rings in the new process. A pending reopen survives a meshd restart; a terminal a reopen had started when meshd died is found by its label and recorded, never launched twice. `StopAgent` drops a pending reopen (`AgentReopenDropped`); a relaunch that fails leaves the agent `EXITED`, for `ResumeAgent` to try again.

## Protocol notes

- **Writes** carry `RequestMeta`: a repeated `mutation_id` returns the first response without running again, and a write past `expires_at` is refused. Events a write causes carry its mutation id.
- **Events** have a `seq` and the log's `generation`. `SubscribeEvents` with a cursor of the same generation resumes (`Resumed`, then the backlog, then live); anything else starts with a `Snapshot`. Only an `End` frame ends a stream on purpose; after any other close, resubscribe from the last cursor. A subscriber more than `events.LiveBuffer` events behind is dropped (`resource_exhausted`) and resumes the same way.
