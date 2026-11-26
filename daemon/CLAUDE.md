# daemon/: rules for agents

The Go daemon (`meshd`), its terminal host (`mesh-term`), the generated CLI (`mesh`, built from `cmd/mesh`) and the stream relay (`mesh-relay`), step 1 and 3 of `docs/apps.md`. It isn't live yet: `bin/mesh` is still the mesh, and nothing here is installed or put on PATH except by the opt-in `./install.sh --daemon` (`README.md`, "Installing"). `README.md` is the full reference (layout, behaviour, protocol notes) and `docs/apps.md` the plan; for this code `docs/apps.md` overrides `docs/design.md` and the root `CLAUDE.md` where they disagree (here a worker may start read-only agents, for one). The API is in `../proto/` (read `proto/CLAUDE.md` before changing it). context7 and WebSearch/WebFetch are available to every agent: check a library's current API there (ConnectRPC, the SQLite driver, `charmbracelet/x/vt`) rather than from memory.

## Toolchain

Off PATH on this machine: Go in `~/.local/go`, buf in `~/.local/opt/buf/bin`, and `~/.local/opt/zigcc/cc` (a `zig cc` wrapper) as the C compiler `go test -race` needs. `scripts/check.sh` finds them itself; for anything else run `export PATH="$PATH:$HOME/.local/go/bin:$HOME/.local/opt/buf/bin"` first. The code generators are pinned as `tool` directives in `go.mod`, so `buf generate` installs nothing.

An agent's shell carries `AGENT_MESH_TOKEN` and `AGENT_MESH_STATE`: `check.sh` unsets every `AGENT_MESH_*`, and the daemon and e2e `TestMain`s call `testutil/cleanenv`, so a hand-run `go test` works too.

## Commands

```bash
daemon/scripts/check.sh                      # the CI equivalent: buf lint and breaking, gen current, gofmt, vet, test -race, pytest
SKIP_PYTEST=1 daemon/scripts/check.sh        # without the Python suite
cd daemon && go test ./...                   # the Go tests only
cd daemon && go test ./internal/daemon -run TestName   # one package, one test
cd daemon && gofmt -w cmd internal e2e       # check.sh fails on unformatted code
cd daemon && go test ./internal/registry -update       # accept a reviewed change of who may call what
cd daemon && go build -o bin/ ./cmd/...      # binaries into daemon/bin/ (gitignored)
```

Run `check.sh` before you report a change here: the repo runs no hosted CI, so it is the gate.

## Layout

`cmd/` holds the four binaries (and `protoc-gen-flagdocs`, a build-time buf plugin, not shipped); `internal/` one package per concern (`daemon` the server and handlers including `cost.go`/`models.go`/`conflicts.go`/`prs.go` (PRs per agent)/`evidence.go` (what an alert adds)/`worktree.go`/`log.go`/`ring.go`/`open.go`, `registry` who may call what, `store` SQLite and migrations, `settings` the settings table, `events`, `termhost`/`termclient`, `adapter/claude`, `adapter/agy`, `adapter/opencode` and `adapter/copilot`, `inputbox` (what an agent's input box holds, read from a repaint), `shellmatch`, `reducer`, `hook`, `relay`, `pairing`, `tlsutil`, `auth`, `cli` including `cli/open.go`); `gen/` the generated Go; `e2e/` the end-to-end test of the real binaries; `packaging/` the systemd units and the relay's container. `share/` the protocol and skills daemon agents read (`README.md`, "Protocol and skills"). `README.md` "Layout" maps every package.

## Conventions

- **Generated code:** never edit `gen/` by hand; change the `.proto` and regenerate (`cd proto && buf generate`; it also writes each package's `FieldDocs`, the field comments `mesh help` shows, by `cmd/protoc-gen-flagdocs`). `check.sh` fails when it is stale.
- **Who may call what** is pinned by `internal/registry/testdata/actors.golden`. Update it only with `-update` after reading its diff: a changed line changes an actor class's rights.
- **State dirs:** a dev or test meshd always gets a temp state dir of its own (meshd has no default, so it can't land in a real one). Keep the path short: a unix socket path holds about 107 bytes, and meshd refuses a longer one.
- **No real CLIs in tests:** tests run the fakes (`internal/testutil/fakeclaude`, `internal/testutil/fakeagy`, `internal/testutil/fakeopencode`, `internal/testutil/fakecopilot`) in an isolated HOME. `scripts/capture-claude-hooks.sh` and `scripts/capture-agy-hooks.sh` run the real CLI on the user's account: run them **by hand only**, never from a test or CI, and read the fixtures' diff before committing it.
- **Store migrations** are numbered, embedded SQL files (`internal/store/migrations/`, numbered 1, 2, … with no gaps). Add a new one for a schema change; never edit one that has shipped, because a host that already ran it won't run it again.
- **Settings:** a new policy knob is a leaf in `internal/settings/schema.go` (key, type, default, summary, choices), read with `Daemon.Setting`; the settings screens are generated from it.
- **Path fields:** a request field holding a filesystem path (a repo, a local source) carries `(agentmesh.v1.path) = true` in its proto; the CLI makes it absolute against the caller's cwd and `~` before sending, because meshd's own cwd is not the caller's (`mesh ask --repo .`). Without the option a new path field breaks for relative values.
- **Doorbells:** `internal/daemon/delivery.go` (README, "Doorbell delivery"): a message counts as delivered only once the agent took its doorbell (`ref` token, activity after the ring, read, or the token above the box), a doorbell is never typed into a human's draft (`ReadInputBox`), a retry never at a screen with no box (a first try does, as `bin/mesh` does), and a Claude agent's is written into its inbox socket (`DeliverNative`). Delivery tests use the fake claude's `FAKECLAUDE_BOX`, `FAKECLAUDE_DROP`, `FAKECLAUDE_STUCK`, `FAKECLAUDE_DIALOG` and `FAKECLAUDE_INBOX`.
- **Same conversation:** a model change reopens the agent's own conversation (`--resume`), never a fresh agent briefed again (root `CLAUDE.md`, "Conventions").
- **Relay:** `cmd/mesh-relay` speaks the protocol in `docs/relay.md`, deployed from `packaging/relay/`; keep it free of SQLite and the terminal host.
- **Docs:** `README.md` describes behaviour, so a change to behaviour updates it in the same PR, as it does this file when the commands, layout or conventions change.
