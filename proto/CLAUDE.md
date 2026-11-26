# proto/: rules for agents

The API as protobuf, one buf module (`buf.yaml`): `agentmesh/v1` is the action registry (`MeshService`) plus `PairingService` (redeeming a one-time pairing token, outside the registry: its caller has no device token yet), `agentmesh/term/v1` the terminal host's API and WebSocket frames, `agentmesh/relay/v1` the relay's, `agentmesh/adapter/v1` the CLI-adapter plugin interface. Three clients are generated from it: Go (`daemon/gen`), TypeScript (`desktop/src/gen`) and Java/Kotlin (`android/app/src/gen`). Background: `docs/apps.md` ("Protocols and lifecycle", "Decided") and `daemon/README.md`. context7 and WebSearch/WebFetch are available to every agent, for buf's and ConnectRPC's current docs.

## Commands

buf and Go are off PATH (`daemon/CLAUDE.md`, "Toolchain"): `export PATH="$PATH:$HOME/.local/go/bin:$HOME/.local/opt/buf/bin"`.

```bash
cd proto && buf lint && buf generate        # lint (STANDARD), then the Go code into daemon/gen
cd desktop && pnpm gen                      # the TypeScript into desktop/src/gen
android/scripts/gen.sh                      # the Java and Kotlin into android/app/src/gen (skips agentmesh/adapter)
daemon/scripts/check.sh                     # runs buf lint and buf breaking (FILE rules, against main)
```

## Conventions

- **One PR for a schema change and every client.** Regenerate all three clients in the same PR as the `.proto` change, commit the output, and run all three check scripts (`daemon/scripts/check.sh`, `desktop/scripts/check.sh`, `android/scripts/check.sh`): each fails when its generated code is stale.
- **Never edit generated code** in any of the three `gen` folders by hand.
- **Breaking changes:** `buf breaking` checks against `main` at file level. Evolve a message by adding fields; never renumber or reuse a field number, and reserve the number and name of a field you remove.
- **Every RPC of `MeshService` is an action:** it needs `option (action) = { actors: […], write: …, summary: … }` (`agentmesh/v1/options.proto`), or meshd refuses to start. A write takes a `RequestMeta`. `mesh` builds its commands from these options, so a new action is a CLI command with no extra code.
- **Who may call what** is pinned three times: `daemon/internal/registry/testdata/actors.golden` (accept a reviewed change with `cd daemon && go test ./internal/registry -update`), the desktop's `src/registry.test.ts` and Android's `RegistryPinTest`, which check that every action an app calls is one a device may call. Giving an actor class a new right is a security decision: say so in the PR.
- A change to these commands or conventions updates this file in the same PR.
