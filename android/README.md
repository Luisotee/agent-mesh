# android

The agent-mesh Android app: Kotlin and Jetpack Compose. It is step 4 of `docs/apps.md` ("Order of work"); the plan, the principles it follows and what is decided are there. Nothing here is installed or put on PATH.

The app is a client of the daemon (`daemon/`). Everything it does to a host is a call on the host's action registry (`proto/`, ConnectRPC with a generated client), so an agent or the CLI can do all it can. It has no account, no hosted service and no telemetry, and talks only to the hosts it is told about.

## What the first version does

Hosts (several, each with routes tried in order), a connection per host with a banner and a log, runs and agents live from the event stream (each run a tree from its lead, state icons whose shape differs by state, a plain status sentence), the host's settings generated from its schema, and six themes (Dracula by default, chosen per device). A read-only terminal per agent, plans, questions and reports, and notifications (`TODO.md` lists what's next).

## The terminal

An agent's **Terminal** tab shows its CLI's screen as the host holds it, live, with 2,000 lines of history to scroll (drag; the view stays on the lines being read as new ones arrive; "Latest" jumps back). Pinch to zoom and pan, double tap to fit the host's width to the screen. It is read only: the app asks for no input right (`OpenTerminal` with `input: false`) and sends nothing but the attach, so it can't type, and the host answers terminal queries itself (the emulator's replies are dropped here).

How it follows the host's protocol (`docs/apps.md`, "Terminal protocol"; `term/TermClient.kt`): the first attach asks for a repaint; after a drop it continues from the last (epoch, byte offset) with a fresh one-time ticket and a growing wait (the host falls back to a repaint, marked reanchored, when it can't); a "slow" close (4001) reattaches at once; output of another epoch means someone resized, so it repaints and takes the new size. It attaches with size 0 ("unknown"), so a phone never shrinks the terminal the desktop is using; the host's size is drawn, fitted to the width and panned when that would be unreadable. The emulator is Termux's `terminal-emulator` module (Apache-2.0) copied into `terminal-emulator/` (its README says what changed); the drawing is ours (`ui/TerminalCanvas.kt`). Not yet: typing and a key row, selecting and copying text.

## Plans, questions, reports and notifications

An agent's screen has an Overview and a Messages tab: its conversation with the rest of its run, oldest first, in Markdown, with a composer (task, answer or info). A plan or question that waits on the user (derived as on the desktop: sent to the run's lead, nobody has answered its author since, the author is still there) shows its form in place: **Approve** (`ApproveAgent` for a planning worker, which moves it to its execution model in the same conversation, else an `answer` "approved"), **Request changes** (`answer` "changes: …") and **Send answer**. The host screen's "Waiting on you" opens the right tab. History is paged in with `ListMessages` after each sync, then kept by events.

Notifications are local first: the event streams stay open while the app is in the background (a foreground service with a quiet notification of its own; a device setting turns it off), and live events become notifications by the desktop's rules (`notify/Notice.kt`: only live events, judged by the host's clock; one per agent; what reaches a run's lead; never what the user did), filtered by the host's own `app.notify.enabled` and `app.notify.events`. A plan notification has **Approve**, a question an inline **Reply**, a tap opens the agent; a failed action says why in the notification.

**Push (notifications with the app closed).** With a UnifiedPush distributor on the phone (the ntfy app, pointed at the user's own ntfy server or ntfy.sh), the hosts push instead, with no connection kept open: Settings → "Notifications with the app closed" turns it on (the library picks the distributor, asking when there are several), shows each host's state, and sends a test. How it fits together:

- `notify/PushChannel.kt` wraps the UnifiedPush library. The app registers one *instance* per host (the instance is the host's id), so each host has its own endpoint and keys.
- The distributor hands the endpoint to `notify/MeshPushService.kt` (Android starts the process for it), and `notify/PushRegistrar.kt` gives it to the host with `RegisterPush`, then again on every connection that becomes ready, so a new endpoint or a host that lost it heals. Endpoints are kept sealed with the Keystore key, like tokens.
- A push is the notice itself, encrypted by the host for this phone's keys (RFC 8291) and decrypted by the library; one it couldn't decrypt isn't from the host and is dropped. `notify/PushPayload.kt` reads it, and `AppContainer.onPush` posts it as the live stream would (the host already applied its notify settings). A push from a daemon other than the host's, or for the agent on screen, isn't posted.
- The same event can come from the live stream and from a push: `Notifier` posts each (host, event seq) once.
- Turning push off drops the instances at the distributor at once, then tells each reachable host (`UnregisterPush`) in the background. A host's `RegisterPush` and `UnregisterPush` calls go one at a time, in the order asked (`PushRegistrar`), and a registration re-checks that push is still on once its turn comes, so quick toggles end as the switch says. Removing a host drops its instance, and the host deletes the endpoint once the push server says it is gone. The connector forgets the chosen distributor when its last instance goes, so `UnifiedPushChannel.unregister` puts it back: otherwise a host added after removing the only one would never register.

With push on, "Stay connected in the background" can be turned off. What wasn't run here (no phone, no emulator): the distributor hand-off and the system showing the notification on a device. `DaemonE2ETest` covers the rest end to end: a real meshd's push, decrypted with the library's own WebPush code.

## Performance

The message list is a lazy list keyed by message id with a content type per message type, so a transcript of any length composes only the rows on screen (`MessagesTest`: 2,000 messages compose at most 40 rows). Plain bodies are plain text and a long one is cut to a preview until "Show all", so a row has its final height at once and the list doesn't shift under the user's thumb; only an expanded long body is parsed off the main thread. The list follows new messages only while the user is at the bottom. What can't be measured on the JVM is the frame-time half of the bar in `docs/apps.md`: a 2,000-message transcript streaming for 10 minutes with no dropped frames and no memory growth. To check it on a phone: connect to a host, start a run whose lead and workers exchange messages (or replay a transcript with `mesh send` in a loop), then watch `adb shell dumpsys gfxinfo dev.agentmesh.android.debug` (janky frames) and `adb shell dumpsys meminfo dev.agentmesh.android.debug` over the ten minutes.

## Connecting a host

A host is a machine running meshd. The usual way is **pairing**; adding an address and a device token by hand still works (for a debug host, or one you already have a token for).

**Pairing.** On the host, `mesh pair --name "my phone"` prints a link (`agentmesh://pair?v=1&h=…&fp=…&t=…`, with the routes the host offers; `docs/relay.md`, "Pairing flow"). `mesh` prints the link only, so for a QR code make one from it with any QR tool (for example `qrencode -t ansiutf8 '<link>'`; the desktop's pair screen will show one). In the app, tap **Pair with a host** and then **Scan the QR code**, or paste the link; or open the link on the phone (a camera app that reads the QR code offers to open it with agent-mesh). The screen shows which host the code names and where the app would reach it, and nothing is sent until you tap **Pair**: the app then redeems the one-time token (`PairingService/RedeemPairing`) on the code's first route that answers, checks that the host is the one the code names, and saves it with its routes, the certificate pin and the new device token (the Keystore seals both the device token and the relay token). A code works once and expires after ten minutes by default: if the host accepted it and something then failed, make a new one. Pairing the same host again gives its entry the new token and the code's routes (a route with the same address takes the code's version) and keeps the routes the code doesn't name, such as a Tailscale address you added by hand. A host reached only through a relay can be edited with the address left empty.

**Removing a host** asks whether to also unpair the phone: when the host is connected, the box is ticked, and the app calls `RevokeSelf` (`mesh revoke-self`) so the host revokes this phone's token, streams and relay token before the app forgets the host. If the call fails the host stays, with Try again (which only retries the unpair, and waits while the host is unreachable) and Remove here only; a host that isn't reachable can only be removed here, and its owner revokes the device with `mesh revoke-device`.

**Routes.** A pairing code can carry two, tried in order, so the same host is reached at home and away:
- *Direct*: `https://` to `meshd --listen`'s TLS address (a LAN or tailnet address), pinned to the daemon's certificate. A host's built-in Tailscale node (`meshd --tailscale`) puts its tailnet address (`100.x`, or a `*.ts.net` name) in the link: the app labels such a route "tailnet" and says the Tailscale app must be on the phone. The phone can't read the node's status (`TailnetStatus` is for the host's own device).
- *Relay*: your own `mesh-relay`'s raw TCP endpoint. Every connection sends `MESH1 <host id> <relay client token>`, waits for `OK`, and then runs TLS to the host on the same socket, pinned to its certificate, so the relay only splices bytes (`net/RelaySocketFactory.kt`; the relay's refusals, such as the host being offline or the token not accepted, are shown in its own words). A token the relay refuses is shown with an auth message (pair again); with other routes the app keeps trying them, and stops only once every route was refused in a row.

- `http://` is accepted only for `localhost`, `127.0.0.1` and the emulator's `10.0.2.2` (a debug host: `adb reverse tcp:<port> tcp:<port>`, then `http://127.0.0.1:<port>` with `meshd --tcp`).
- Anywhere else the address is `https://`: for a machine on your tailnet, `tailscale serve --https=443 http://127.0.0.1:<port>` gives it a real certificate. A host with its own certificate takes its SHA-256 as a pin, and then only that certificate is accepted.
- **The QR scanner's camera wiring is untested here** (`pair/QrScanner.kt`, CameraX): there is no emulator or phone on the machine that built it. What is tested is everything around it: ZXing decoding a generated QR code of a real link at several rotations (`QrDecoderTest`), the paste, deep-link and pairing flow, and the relay and TLS routes against a stand-in relay (`RelayRouteTlsTest`) and the real `mesh-relay` and `meshd --listen` on loopback (`PairingE2ETest`). The first time on a phone: Pair with a host, Scan, the permission prompt, and a code from `mesh pair`.

## Layout

- `app/src/main/kotlin/dev/agentmesh/android/`: `net/` (routes and pinning, the relay socket, pairing links, the one client wrapper `MeshApi`, `HostConnection`; the only package that reaches the network), `pair/` (the QR decoder and the camera scanner), `store/` (`HostState` and `applyFrame`, built only from snapshot and events), `derive/` (run trees, status sentences, what waits on the user), `hosts/` (the host list, sealed tokens, the pairing flow), `notify/` (local notifications and their actions, the background service, push through UnifiedPush), `theme/`, `ui/` (Compose screens), `HostRuntime.kt` (one host's connection and state), `MeshApp.kt` (what lives as long as the process).
- `app/src/gen/`: generated Java (protobuf lite) and Kotlin (ConnectRPC) code, committed; reviewers can skip it.
- `../shared/themes/`: one JSON file per theme, shared with the desktop and built into the APK as `assets/themes/`.
- `app/src/test/`: JVM tests; `DaemonHarness` runs a real meshd with the fake `claude`.
- `scripts/`: `setup.sh`, `env.sh`, `gen.sh`, `check.sh`, `protoc-gen-connect-kotlin`.

## Toolchain

`scripts/setup.sh` installs, under `~/.local/opt` and with no sudo: the Android SDK (command-line tools, platform 37.2, build-tools 37.0.0), a Temurin 21 JDK (the machine had a JRE only, and the generated Java needs `javac`) and the Gradle distribution that bootstraps the wrapper. `scripts/env.sh` puts them on PATH for the other scripts and points `GRADLE_USER_HOME` at `~/.local/opt/gradle-home`. Go (`~/.local/go`) and buf (`~/.local/opt/buf`) are the daemon's, as in `daemon/README.md`.

## Commands

```bash
android/scripts/setup.sh      # once: the SDK, the JDK, Gradle
android/scripts/check.sh      # the CI equivalent: generated code current, unit + Compose + meshd end-to-end tests, lint, debug APK
android/scripts/gen.sh        # after a .proto change: regenerate app/src/gen
cd android && source scripts/env.sh && ./gradlew testDebugUnitTest      # one task
```

`check.sh` builds `app/build/outputs/apk/debug/app-debug.apk`. The end-to-end tests build meshd, mesh-term, mesh, mesh-relay and the fake `claude` with Go into `app/build/e2e-bin`, then give each test a state dir, HOME and loopback ports (and, for pairing, a relay with its own secret) of its own under a temp dir: they read and write nothing of yours, and are skipped when Go isn't found or `SKIP_E2E=1` is set. Dependencies are pinned by `gradle/verification-metadata.xml`; after adding one, run `./gradlew --write-verification-metadata sha256 testDebugUnitTest lintDebug assembleDebug writeCodegenTools` and review the diff.

## Emulator

There is no emulator run in `check.sh`: Compose screens run on the JVM under Robolectric. Maestro flows (`maestro/`) drive the installed APK on an emulator or a phone; WSL needs `/dev/kvm`, and this user isn't in the `kvm` group, so they haven't run here.
