# android/: rules for agents

The Android app: Kotlin and Jetpack Compose, minSdk 26, a client of the daemon (`daemon/`), step 4 of `docs/apps.md`. Nothing here is installed or put on PATH. `README.md` is the full reference (what the app does, connecting a host, the toolchain); `docs/apps.md` has the plan and what is decided ("Stack", "Decided"). context7 and WebSearch/WebFetch are available to every agent: check Compose's, connect-kotlin's and the Android SDK's current API there (the app targets SDK 37) rather than from memory.

## Toolchain

All of it lives under `~/.local/opt`, off PATH, with nothing global: the Android SDK, a Temurin 21 JDK (the machine has only a JRE, and the generated Java needs `javac`) and the Gradle distribution that bootstraps the wrapper. `scripts/setup.sh` installs them, needs no sudo and is safe to re-run. **`source scripts/env.sh` before any `./gradlew`**: it puts them, Go and buf on PATH and points `GRADLE_USER_HOME` at `~/.local/opt/gradle-home`. The scripts source it themselves.

## Commands

Run from `android/`.

```bash
scripts/setup.sh                             # once: the SDK, the JDK, Gradle
scripts/check.sh                             # the CI equivalent: gen current, unit + Compose + meshd e2e tests, lint, debug APK
SKIP_E2E=1 scripts/check.sh                  # without the meshd end-to-end tests
scripts/gen.sh                               # after a .proto change: regenerate app/src/gen
source scripts/env.sh && ./gradlew testDebugUnitTest                                       # the JVM tests
source scripts/env.sh && ./gradlew testDebugUnitTest --tests 'dev.agentmesh.android.StoreTest'   # one class
source scripts/env.sh && ./gradlew lintDebug assembleDebug
```

The end-to-end tests (`DaemonE2ETest`, through `DaemonHarness`) build meshd, mesh-term, mesh, mesh-relay and the fake `claude` with Go into `app/build/e2e-bin`, and give each test its own state dir, HOME and loopback port, so they touch nothing of the user's; they are skipped when Go isn't found or `SKIP_E2E=1` is set. Run `scripts/check.sh` before you report a change here: the repo runs no hosted CI.

## Layout

Code is in `app/src/main/kotlin/dev/agentmesh/android/`: `net/` (routes and pinning, `RelaySocketFactory`, `Pairing` links, `MeshApi`, `HostConnection`), `pair/` (the QR decoder and camera scanner), `store/` (`HostState` and `applyFrame`), `derive/` (run trees, status sentences, what waits on the user), `hosts/` (the host list, sealed tokens, `PairingFlow`), `notify/` (notifications and their actions, `ConnectionService`, UnifiedPush: `PushChannel`, `MeshPushService`, `PushRegistrar`), `theme/`, `ui/` (Compose screens), `HostRuntime.kt` (one host's connection and state) and `MeshApp.kt` (what lives as long as the process). Generated Java and Kotlin is in `app/src/gen/` (committed), one JSON file per theme is in `../shared/themes/` (shared with the desktop; the build adds `../shared` as an assets dir), JVM tests are in `app/src/test/`, and Maestro flows are in `maestro/`.

## Conventions

- **Only `net/` reaches the network** or the generated client, and every method it calls must be a `MeshService` action a device may call (`net`'s `UI_ACTIONS`), apart from `PairingService/RedeemPairing`, the one call a device makes before it has a token. `RegistryPinTest` checks both, so a new call starts there. Anything the app does is an action an agent or the CLI can call too (`docs/apps.md`, "Principles"): a new capability is a registry action first (`proto/CLAUDE.md`).
- **State** is built only from the snapshot and events (`applyFrame`, pure), and a live stream and its replay must give the same state (the parity test in `StoreTest`). What waits on the user is derived (`derive/`), never stored.
- **Same behaviour as the desktop:** the connection rules (handshake, heartbeat, backoff, generation fence) and their test cases mirror the desktop's `src/connection.ts`, and the theme files are the desktop's own (`shared/themes/`: change a token there and both apps change). The theme follows the `app.theme` of the first host (list order) that has set one itself (an unset host reports its default, which is no choice), unless the device chose one.
- **Secrets** (a device token, a relay client token, a pairing code's token, a push endpoint and its keys) are sealed with an Android Keystore key (`hosts/`) or held only in memory; never store them in the clear, in saved state or in a log or `toString`. A pairing link only fills the screen: nothing is sent until the user has seen the host it names and taps Pair.
- **Generated code:** never edit `app/src/gen/` by hand. Regenerate it with `scripts/gen.sh`; `check.sh` fails when it is stale.
- **Dependencies** are pinned by `gradle/verification-metadata.xml`. After adding one, run `./gradlew --write-verification-metadata sha256 testDebugUnitTest lintDebug assembleDebug writeCodegenTools` and review the diff.
- **No emulator here:** this user isn't in the `kvm` group, so Compose screens are tested on the JVM under Robolectric (`ScreensTest`), and the Maestro flows are written but haven't run. Don't report a Maestro flow as passing unless one actually ran.
- A change to these commands, the layout or the conventions updates this file in the same PR.
