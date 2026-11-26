# Relay, TLS pinning, and QR/link pairing

`mesh-relay` connects mobile and remote clients to `meshd` instances running behind NAT without opening home router ports or trusting the relay with plaintext traffic.

## Architecture

- **Persistent host TLS identity:** On first start `meshd` generates an ECDSA P-256 self-signed certificate in `<state>/tls.crt` and `<state>/tls.key` (mode `0600`) and records its canonical SHA-256 DER fingerprint (`sha256:<64-lower-hex>`) in `meshd.json` (`cert_fingerprint`). If one half of the pair is missing or corrupt, `meshd` refuses to start rather than rotating the certificate and breaking paired clients' pins.
- **Zero-trust relay:** `meshd` dials out to `mesh-relay`'s `--listen` HTTP/WebSocket endpoint and keeps a control WebSocket open. For each client connection, the relay asks `meshd` to dial back with a single-use ticket and splices the raw byte streams (`TCP_NODELAY` on every socket). Inner TLS runs end to end between the client and `meshd`, pinned to the host's certificate fingerprint; the relay never terminates inner TLS and inspects zero payload bytes.
- **Direct TLS route (LAN / Tailscale):** Passing `meshd --listen <addr>` (off by default; `--tcp` remains plain loopback HTTP) serves the same `d.Handler()` over TLS with the same persistent certificate.
- **Built-in Tailscale:** `meshd --tailscale` joins a tailnet itself and serves the same TLS API there ("Built-in Tailscale" below).

## Endpoints and wire protocols

### 1. Relay HTTP/WebSocket listener (`mesh-relay --listen`, default `:8080`)

Designed to run behind `nginx-proxy-manager` (TLS on port `443` with WebSockets enabled).

- `GET /healthz` — returns `200 ok\n` for uptime monitoring.
- `GET /v1/daemon/control` — daemon control WebSocket (`Authorization: Bearer <relay_secret>`). Invalid or missing bearer token returns HTTP `401` before upgrade (daemon stops retrying on `401`/`403`).
  1. Daemon sends binary `agentmesh.relay.v1.ControlFrame` (`Register{host_id, host_name, daemon_version, device_tokens, client_tokens}`).
  2. Relay replaces any older session for `host_id` with close code `4001` (`replaced`), then replies `Registered{host_id}`.
  3. Daemon sends `Ping{seq}` heartbeats; relay replies `Pong{seq}`. Client-backoff resets only after the first `Pong`.
  4. When an app connects, relay sends `DialBack{ticket, stream_id}`.
  5. Whenever its devices change, the daemon sends `ClientTokens{tokens}`, which replaces the host's set ("Client relay tokens").
- `GET /v1/daemon/stream?ticket=<rtk_...>` — daemon dial-back WebSocket (`Authorization: Bearer <relay_secret>`). Pre-upgrade HTTP errors: `401` on bad token, `400` on missing ticket, `404` on an unknown, already-used or expired ticket (tickets are atomically deleted on claim, and the relay drops an unclaimed one when its dial-back wait ends, so a late claim finds nothing).
- `GET /v1/client/connect?host_id=<host_id>` — optional WebSocket client endpoint (`Authorization: Bearer <client_relay_token>`). Pre-upgrade HTTP errors: `400` on missing `host_id`, `401` on bad token, `503` if the daemon is offline (whatever the token: see "Client relay tokens"), `429` if too many pending streams are queued for the host, `504` on dial-back timeout. Each WebSocket message is capped at 4 MiB here: unlike the raw-TCP endpoint, a browser client gets no chunking, so it must send smaller messages.

Typed WebSocket close codes sent after upgrade on `/v1/daemon/control`:
- `4001` replaced by a newer daemon session
- `4003` bad request / invalid handshake
- `4006` relay shutdown

### 2. Relay raw TCP client listener (`mesh-relay --client-listen`, default `:8443`)

Native apps (Android OkHttp, desktop Node) connect here directly over TCP (`TCP_NODELAY`) so they do not have to layer TLS over a WebSocket:

1. Client connects via TCP to `<relay_host>:8443`.
2. Client sends one ASCII line (max 512 bytes, 10 s timeout):
   ```text
   MESH1 <host_id> <relay_client_token>\n
   ```
   The `relay_client_token` (`amr_...`) is the paired device's own token ("Client relay tokens"). It authorizes connections only to `<host_id>`, and cannot register daemons or access other hosts.
3. Relay replies with one line:
   - `OK\n` — the daemon dialed back and the raw TCP socket is now spliced directly to the daemon's TLS listener.
   - `ERR <code> <message>\n` (`bad_request`, `auth`, `offline`, `busy`, `timeout`, `unavailable`) — connection closes immediately.
4. Immediately after reading `OK\n`, the client starts a TLS 1.2+ handshake (`h2` or `http/1.1` ALPN) on the same socket, verifying that the server's leaf certificate SHA-256 matches the pinned `cert_fingerprint` from the pairing payload (`tlsutil.ErrPinMismatch` on mismatch).
5. Inside TLS, the client speaks standard ConnectRPC (`POST /agentmesh.v1.PairingService/RedeemPairing`, `POST /agentmesh.v1.MeshService/<Method>`) and terminal WebSockets (`GET /v1/term/<agent>?ticket=...`, using `http/1.1` ALPN).

## Client relay tokens

Each paired device has a relay token of its own, so the relay refuses a revoked device at the preamble, before it asks the daemon to dial back:

- **The token** is `"amr_" + base64url_nopad(HMAC-SHA256(relay_secret, "agentmesh-device-v1:" + host_id + ":" + device_id))`. The daemon stores nothing new: it derives each device's token again when it needs it.
- **The link carries it.** `CreatePairing` mints the device's id together with the pairing (`pairings.device_id`). The link's `rt` is that device's token, valid for the redemption itself and, once redeemed, for the device's whole life. Apps store it as before.
- **The daemon tells the relay which tokens are valid**, by SHA-256 hash (the relay never holds a token):
  - `Register.device_tokens = true` carries the first set in `client_tokens`, atomically with registration.
  - Each change sends a `ClientTokens` frame that replaces the set: a pairing made, redeemed or pruned, a device revoked. A failed read of the set retries (1 s doubling to 30 s) until the frame is sent; the relay keeps the last set meanwhile.
  - The set holds every device whose token isn't revoked, and every open pairing with its remaining lifetime (`ClientToken.ttl_seconds`, relative so the relay's clock needn't match the host's), which the relay enforces by itself. At most 1000 entries per host, open pairings first.
- **The relay's check:**
  - A host registered with `device_tokens` is reached only by a token in its set (and not expired); anything else gets `ERR auth` / `401`, the host-wide token included.
  - A host registered without it (an older daemon) keeps the host-wide token, `"amr_" + base64url_nopad(HMAC-SHA256(relay_secret, "agentmesh-client-v1:" + host_id))`.
  - A host that isn't connected answers `offline` / `503` to any token, since its set is unknown.
- **Versions:**
  - The preamble is unchanged (`MESH1 <host_id> <token>`).
  - A new relay serves an old daemon by the host-wide token.
  - An old relay ignores the new fields and frame and refuses the new tokens, so upgrade the relay first.
  - A device paired before per-device tokens holds the host-wide token, which a new daemon's relay refuses: it re-pairs to use the relay (direct routes are unaffected). So does a link made before the upgrade and not yet redeemed.

## Pairing flow and URI contract

1. On the host, the local device (`device-local`) calls `MeshService/CreatePairing` (`mesh pair --name "Pixel 9"`). Paired remote devices are refused `CreatePairing`, `ListDevices`, and `RevokeDevice` (`PermissionDenied`).
2. `CreatePairing` stores only the SHA-256 hash of a one-time `amp_...` token (default TTL 10 minutes, max 24 h) and returns structured fields plus a canonical URI:
   ```text
   agentmesh://pair?v=1&h=<host_id>&fp=<sha256_fp>&t=<amp_token>[&hn=<host_name>][&r=<relay_url>][&rtcp=<relay_tcp>][&rt=<relay_client_token>][&d=<direct_url>][&n=<device_name>]
   ```
3. The app connects over either `Direct` (`d`, `https://...` with leaf pin `fp`) or `Relay` (`rtcp` + `h` + `rt` + pin `fp`), calls `PairingService/RedeemPairing{token, device_name}` (unauthenticated by bearer token, guarded by `RefuseBrowsers` and the single-use `amp_...` token), receives a permanent per-device `amt_...` bearer token (`ACTOR_CLASS_DEVICE` or `ACTOR_CLASS_READ_ONLY_DEVICE`), and persists `{host_id, device_token, cert_fingerprint, direct_url, relay_tcp, relay_client_token}`.
4. `CreatePairing`'s `mutations` record is scrubbed on redemption and expiry so the one-time pairing token is not retained on disk. A retry of `CreatePairing` with the same mutation id after that returns `FailedPrecondition` (`pairing already redeemed`), not an empty link. `DevicePaired` and `DeviceRevoked` events reach only the host's own device. A `relay_url` override keeps the configured relay's TCP address and client token when it names the same relay (scheme and host case-insensitive, default port and trailing slash ignored); any other relay gets neither.
5. `ListDevices` and `RevokeDevice` list paired devices and revoke a device's `amt_...` token; revocation immediately cancels the device's open `SubscribeEvents` and `/v1/term/*` streams, and the relay refuses the device's relay token from then on.

## Built-in Tailscale

`meshd --tailscale` makes the daemon a node of a tailnet, through tsnet, with nothing installed on the host. It works with Tailscale or with Headscale.

- **Flags** (each with an environment twin, for a systemd drop-in):
  - `--tailscale` (`AGENT_MESH_TAILSCALE=1`): join the tailnet.
  - `--tailscale-hostname` (`AGENT_MESH_TAILSCALE_HOSTNAME`, default `mesh-<host name>`, so it doesn't collide with the machine's own Tailscale node).
  - `--tailscale-port` (`AGENT_MESH_TAILSCALE_PORT`, default 443: tsnet has its own port space).
  - `--tailscale-control-url` (`AGENT_MESH_TAILSCALE_CONTROL_URL`): a **Headscale** server; empty is Tailscale's.
  - `--tailscale-auth-key-file` (`AGENT_MESH_TAILSCALE_AUTH_KEY_FILE`, mode `0600`, or `AGENT_MESH_TAILSCALE_AUTH_KEY`): an auth key (Tailscale's admin console, or `headscale preauthkeys create`), used only while the node needs login. meshd drops the variable from its environment at start, with or without `--tailscale`.
- **Login:**
  - Without an auth key, the node waits for login. Its URL is in meshd's log (once per URL) and in `mesh tailnet` (`TailnetStatus`), with the node's state, addresses and the direct address links get.
  - Only the host's own device may read it: whoever opens the URL attaches the node to their tailnet.
  - An expired node key shows as `NeedsLogin` again.
- **State** lives in `<state>/tailscale` (mode `0700`): the node key, so the node keeps its name and address across restarts. Delete it and the node joins again under a new address, and links to the old one stop working.
- **What it serves:** the same TLS API as `--listen`: same handler, same certificate and fingerprint, same device tokens. There is no plain HTTP, Funnel, Tailscale SSH, web client or Tailscale certificate.
- **Pairing** puts `https://<tailnet IPv4>[:port]` in the link's `d` once the node runs (an explicit `direct_url` wins; `--listen` is the fallback). The IP works whether or not the device resolves MagicDNS names. The device must be on the same tailnet (the Tailscale app), and it pins the fingerprint as on any direct route. The relay stays the link's fallback route.
- **Build:** meshd must be built with `-tags ts_omit_logtail,ts_omit_peerapiserver` (`install.sh` does). Otherwise tsnet uploads its logs to log.tailscale.com and runs Tailscale's peerapi on the node, so meshd refuses `--tailscale` in a build without them.
  - tsnet adds about 17 MB to meshd (28.6 to 45.8 MB) and about a minute to a cold build; the other binaries don't change.
  - It sets the module's floor at Go 1.27.4.
- **With systemd**, put the variables in a drop-in, `~/.config/systemd/user/agent-mesh.service.d/tailscale.conf` (`install.sh` leaves it alone):
  ```ini
  [Service]
  Environment=AGENT_MESH_TAILSCALE=1
  Environment=AGENT_MESH_TAILSCALE_AUTH_KEY_FILE=%h/.config/agent-mesh/tailscale.key
  ```
  Then `systemctl --user daemon-reload && systemctl --user restart agent-mesh` and `mesh tailnet`.
- **Without tsnet:** a host that already runs Tailscale can use `meshd --listen <tailscale IP>:<port>` instead.

## Integration notes for Android and Desktop workers

- **Android (`android/`):**
  - `Route.Direct(httpsUrl, pinSha256)`: OkHttp client with custom `X509TrustManager` (checks `MessageDigest.getInstance("SHA-256")` of `chain[0].encoded` against `pinSha256`) and permissive `HostnameVerifier` (pinned leaf cert replaces DNS SAN checks).
  - `Route.Relay(relayTcpHostPort, hostId, relayClientToken, pinSha256)`: OkHttp `SocketFactory` subclass whose `createSocket()` opens a TCP `Socket` with `tcpNoDelay = true`, writes `MESH1 <hostId> <relayClientToken>\n`, reads a line expecting `OK`, and returns that connected `Socket` to OkHttp's `SSLSocketFactory` + the same pinning `TrustManager` (use HTTP/2 for ConnectRPC and HTTP/1.1 for terminal WebSockets).
- **Desktop (`desktop/`):** done. On the host's own device (`device-local`, a state-dir host) the "Pair a device" screen calls `CreatePairing` (name, read-only, lifetime, direct address), shows the link as a QR code and text, lists `ListDevices` and revokes with `RevokeDevice`, following `DevicePaired` and `DeviceRevoked`. Another machine adds the host by pasting the link: main (`electron/route.ts`, `electron/pair.ts`) parses and checks it, redeems the one-time token as Connect JSON over the link's route, and keeps `{host id, device id, token, pin, direct address, relay}`. Every request and terminal stream then goes over TLS pinned to the link's fingerprint: direct (`d`) first, the relay's raw TCP route (`MESH1`, `OK`) when direct can't be reached (and for a minute after), never an unpinned connection. A paired app is refused `CreatePairing`, `ListDevices` and `RevokeDevice` by the host, so it has no Pair a device screen. `desktop/e2e/pairing.e2e.ts` runs both routes against a real `meshd` and `mesh-relay`.

## Deploying `mesh-relay` to the Oracle ARM VPS

1. **Build or prepare the artifact locally:**
   - Binary: run `daemon/packaging/relay/build-arm64.sh` to produce `daemon/bin/mesh-relay-linux-arm64` (~10 MB static binary), or use `daemon/packaging/relay/Dockerfile` + `daemon/packaging/relay/compose.yaml` via Dockge.
2. **Generate the relay shared secret** (keep mode `0600`):
   ```bash
   ./mesh-relay --gen-token > relay.token
   chmod 600 relay.token
   # Required for the Docker container (USER 65532:65532, cap_drop: ALL; mesh-relay enforces mode 0600):
   sudo chown 65532:65532 relay.token
   ```
   For the root-owned systemd unit (`agent-mesh-relay.service`), skip the `chown`. For Dockge without a host-chowned file, drop `--token-file` and the volume from `compose.yaml` and set `AGENT_MESH_RELAY_TOKEN` in Dockge's `.env`.
3. **Open both ports (required):**
   - **Port `443` (via `nginx-proxy-manager`):** create a Proxy Host (e.g. `relay.example.com`) forwarding to `127.0.0.1:8080` (or `agent-mesh-relay:8080` on NPM's Docker network). Enable **WebSockets Support** and SSL, and in the Advanced tab set `proxy_read_timeout 3600s; proxy_send_timeout 3600s;` so idle control/stream WebSockets are not closed every 60 s.
   - **Port `8443/tcp` (raw TCP client stream):** because native clients run end-to-end pinned TLS directly over TCP port `8443` without NPM terminating TLS, open **TCP 8443** in **both**:
     1. The Oracle Cloud VCN **Security List** (Ingress Rule: Source `0.0.0.0/0`, IP Protocol `TCP`, Destination Port Range `8443`).
     2. The VPS host firewall (`iptables` / `ufw`, e.g. `sudo iptables -I INPUT -p tcp --dport 8443 -j ACCEPT` or `sudo ufw allow 8443/tcp` and save rules).
4. **Start `meshd` on your home machine:**
   ```bash
   meshd --state "$HOME/.local/state/agent-mesh-daemon" \
     --relay-url https://relay.example.com \
     --relay-tcp relay.example.com:8443 \
     --relay-token-file "$HOME/.config/agent-mesh/relay.token"
   ```
5. **Create a pairing payload on the home machine:**
   ```bash
   mesh --state "$HOME/.local/state/agent-mesh-daemon" pair --name "phone"
   ```
6. **Monitoring:** point uptime-kuma at `https://relay.example.com/healthz`.
