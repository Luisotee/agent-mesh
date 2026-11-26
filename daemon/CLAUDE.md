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

`cmd/` holds the four binaries (and `protoc-gen-flagdocs`, a build-time buf plugin, not shipped); `internal/` one package per concern (`daemon` the server and handlers, `registry` who may call what, `store` SQLite and migrations, `settings` the settings table, `events`, `termhost`/`termclient`, `adapter/claude` and `adapter/agy`, `shellmatch`, `reducer`, `hook`, `relay`, `pairing`, `tlsutil`, `auth`, `cli`); `gen/` the generated Go; `e2e/` the end-to-end test of the real binaries; `packaging/` the systemd units and the relay's container. `share/` the protocol and skills daemon agents read (`README.md`, "Protocol and skills"). `README.md` "Layout" maps every package.

## Conventions

- **Generated code:** never edit `gen/` by hand; change the `.proto` and regenerate (`cd proto && buf generate`; it also writes each package's `FieldDocs`, the field comments `mesh help` shows, by `cmd/protoc-gen-flagdocs`). `check.sh` fails when it is stale.
- **Who may call what** is pinned by `internal/registry/testdata/actors.golden`. Update it only with `-update` after reading its diff: a changed line changes an actor class's rights.
- **State dirs:** a dev or test meshd always gets a temp state dir of its own (meshd has no default, so it can't land in a real one). Keep the path short: a unix socket path holds about 107 bytes, and meshd refuses a longer one.
- **No real CLIs in tests:** tests run the fakes (`internal/testutil/fakeclaude`, `internal/testutil/fakeagy`) in an isolated HOME. `scripts/capture-claude-hooks.sh` and `scripts/capture-agy-hooks.sh` run the real CLI on the user's account: run them **by hand only**, never from a test or CI, and read the fixtures' diff before committing it.
- **Store migrations** are numbered, embedded SQL files (`internal/store/migrations/`, numbered 1, 2, … with no gaps). Add a new one for a schema change; never edit one that has shipped, because a host that already ran it won't run it again.
- **Settings:** a new policy knob is a leaf in `internal/settings/schema.go` (key, type, default, summary, choices), read with `Daemon.Setting`; the settings screens are generated from it.
- **Same conversation:** a model change reopens the agent's own conversation (`--resume`), never a fresh agent briefed again (root `CLAUDE.md`, "Conventions").
- **Relay:** `cmd/mesh-relay` speaks the protocol in `docs/relay.md`, deployed from `packaging/relay/`; keep it free of SQLite and the terminal host.
- **Docs:** `README.md` describes behaviour, so a change to behaviour updates it in the same PR, as it does this file when the commands, layout or conventions change.
