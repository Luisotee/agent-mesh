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
- `internal/daemon`: the server and its handlers; `internal/client`: how local callers dial it.

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

Always give it a state dir of its own: meshd has no default, so it can't land in a real one by accident.

```bash
state=$(mktemp -d)
daemon/bin/meshd --state "$state"            # socket at $state/meshd.sock, device token at $state/device.token
```

Every call needs a token. The host's own CLI uses the device token meshd writes on first start (`device.token`, mode 0600); each agent gets its own at spawn, in `AGENT_MESH_TOKEN`. The actor of every action comes from the token, never from the request. Requests that carry a browser's `Origin` or `Sec-Fetch-Site` header are refused, and `--tcp` binds loopback only.

## Protocol notes

- **Writes** carry `RequestMeta`: a repeated `mutation_id` returns the first response without running again, and a write past `expires_at` is refused. Events a write causes carry its mutation id.
- **Events** have a `seq` and the log's `generation`. `SubscribeEvents` with a cursor of the same generation resumes (`Resumed`, then the backlog, then live); anything else starts with a `Snapshot`. Only an `End` frame ends a stream on purpose; after any other close, resubscribe from the last cursor. A subscriber more than `events.LiveBuffer` events behind is dropped (`resource_exhausted`) and resumes the same way.
