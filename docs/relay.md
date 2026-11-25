# Relay, TLS pinning, and QR/link pairing

`mesh-relay` connects mobile and remote clients to `meshd` instances running behind NAT without opening home router ports or trusting the relay with plaintext traffic.

## Architecture

- **Persistent host TLS identity:** On first start `meshd` generates an ECDSA P-256 self-signed certificate in `<state>/tls.crt` and `<state>/tls.key` (mode `0600`) and records its canonical SHA-256 DER fingerprint (`sha256:<64-lower-hex>`) in `meshd.json` (`cert_fingerprint`). If one half of the pair is missing or corrupt, `meshd` refuses to start rather than rotating the certificate and breaking paired clients' pins.
- **Zero-trust relay:** `meshd` dials out to `mesh-relay`'s `--listen` HTTP/WebSocket endpoint and keeps a control WebSocket open. For each client connection, the relay asks `meshd` to dial back with a single-use ticket and splices the raw byte streams (`TCP_NODELAY` on every socket). Inner TLS runs end to end between the client and `meshd`, pinned to the host's certificate fingerprint; the relay never terminates inner TLS and inspects zero payload bytes.
- **Direct TLS route (LAN / Tailscale):** Passing `meshd --listen <addr>` (off by default; `--tcp` remains plain loopback HTTP) serves the same `d.Handler()` over TLS with the same persistent certificate.

## Endpoints and wire protocols

### 1. Relay HTTP/WebSocket listener (`mesh-relay --listen`, default `:8080`)

Designed to run behind `nginx-proxy-manager` (TLS on port `443` with WebSockets enabled).

- `GET /healthz` — returns `200 ok\n` for uptime monitoring.
- `GET /v1/daemon/control` — daemon control WebSocket (`Authorization: Bearer <relay_secret>`).
  1. Daemon sends binary `agentmesh.relay.v1.ControlFrame` (`Register{host_id, host_name, daemon_version}`).
  2. Relay replaces any older session for `host_id` with close code `4001` (`replaced`), then replies `Registered{host_id}`.
  3. Daemon sends `Ping{seq}` heartbeats; relay replies `Pong{seq}`. Client-backoff resets only after the first `Pong`.
  4. When an app connects, relay sends `DialBack{ticket, stream_id}`.
- `GET /v1/daemon/stream?ticket=<rtk_...>` — daemon dial-back WebSocket (`Authorization: Bearer <relay_secret>`). The ticket is single-use (atomically deleted on claim) and expires after 15 s; reuse returns HTTP `404`.
- `GET /v1/client/connect?host_id=<host_id>` — optional WebSocket client endpoint (`Authorization: Bearer <client_relay_token>`), returning `503` if the daemon is offline or `401` on bad token.

Typed WebSocket close codes on control/stream connections:
- `4001` replaced by a newer daemon session
- `4002` authentication failed (daemon stops retrying)
- `4003` bad request / invalid handshake
- `4004` daemon offline
- `4005` dial-back timeout
- `4006` relay shutdown
- `4007` invalid or expired ticket

### 2. Relay raw TCP client listener (`mesh-relay --client-listen`, default `:8443`)

Native apps (Android OkHttp, desktop Node) connect here directly over TCP (`TCP_NODELAY`) so they do not have to layer TLS over a WebSocket:

1. Client connects via TCP to `<relay_host>:8443`.
2. Client sends one ASCII line (max 512 bytes, 10 s timeout):
   ```text
   MESH1 <host_id> <relay_client_token>\n
   ```
   The `relay_client_token` (`amr_...`) is derived from the relay secret as `"amr_" + base64url_nopad(HMAC-SHA256(relay_secret, "agentmesh-client-v1:" + host_id))`. It authorizes connections only to `<host_id>` and cannot register daemons or access other hosts.
3. Relay replies with one line:
   - `OK\n` — the daemon dialed back and the raw TCP socket is now spliced directly to the daemon's TLS listener.
   - `ERR <code> <message>\n` (`bad_request`, `auth`, `offline`, `busy`, `timeout`, `unavailable`) — connection closes immediately.
4. Immediately after reading `OK\n`, the client starts a TLS 1.2+ handshake (`h2` or `http/1.1` ALPN) on the same socket, verifying that the server's leaf certificate SHA-256 matches the pinned `cert_fingerprint` from the pairing payload (`tlsutil.ErrPinMismatch` on mismatch).
5. Inside TLS, the client speaks standard ConnectRPC (`POST /agentmesh.v1.PairingService/RedeemPairing`, `POST /agentmesh.v1.MeshService/<Method>`) and terminal WebSockets (`GET /v1/term/<agent>?ticket=...`, using `http/1.1` ALPN).

## Pairing flow and URI contract

1. On the host, the local device (`device-local`) calls `MeshService/CreatePairing` (`meshctl pair --name "Pixel 9"`). Paired remote devices are refused `CreatePairing`, `ListDevices`, and `RevokeDevice` (`PermissionDenied`).
2. `CreatePairing` stores only the SHA-256 hash of a one-time `amp_...` token (default TTL 10 minutes, max 24 h) and returns structured fields plus a canonical URI:
   ```text
   agentmesh://pair?v=1&h=<host_id>&fp=<sha256_fp>&t=<amp_token>[&hn=<host_name>][&r=<relay_url>][&rtcp=<relay_tcp>][&rt=<relay_client_token>][&d=<direct_url>][&n=<device_name>]
   ```
3. The app connects over either `Direct` (`d`, `https://...` with leaf pin `fp`) or `Relay` (`rtcp` + `h` + `rt` + pin `fp`), calls `PairingService/RedeemPairing{token, device_name}` (unauthenticated by bearer token, guarded by `RefuseBrowsers` and the single-use `amp_...` token), receives a permanent per-device `amt_...` bearer token (`ACTOR_CLASS_DEVICE` or `ACTOR_CLASS_READ_ONLY_DEVICE`), and persists `{host_id, device_token, cert_fingerprint, direct_url, relay_tcp, relay_client_token}`.
4. `CreatePairing`'s `mutations` record is scrubbed on redemption and expiry so the one-time pairing token is not retained on disk.
5. `ListDevices` and `RevokeDevice` list paired devices and revoke a device's `amt_...` token; revocation immediately cancels the device's open `SubscribeEvents` and `/v1/term/*` streams.

## Integration notes for Android and Desktop workers

- **Android (`android/`):**
  - `Route.Direct(httpsUrl, pinSha256)`: OkHttp client with custom `X509TrustManager` (checks `MessageDigest.getInstance("SHA-256")` of `chain[0].encoded` against `pinSha256`) and permissive `HostnameVerifier` (pinned leaf cert replaces DNS SAN checks).
  - `Route.Relay(relayTcpHostPort, hostId, relayClientToken, pinSha256)`: OkHttp `SocketFactory` subclass whose `createSocket()` opens a TCP `Socket` with `tcpNoDelay = true`, writes `MESH1 <hostId> <relayClientToken>\n`, reads a line expecting `OK`, and returns that connected `Socket` to OkHttp's `SSLSocketFactory` + the same pinning `TrustManager` (use HTTP/2 for ConnectRPC and HTTP/1.1 for terminal WebSockets).
- **Desktop (`desktop/`):**
  - `desktop/src/gen` should be regenerated (`cd desktop && pnpm gen`) when adding the "Pair a device" screen.
  - The local "Pair a device" UI only needs `MeshService/CreatePairing` (to render a QR code / copyable link from `res.uri` and `res.expiresAt`), `MeshService/ListDevices` (table of paired devices with class/created/revoked), and `MeshService/RevokeDevice` (plus handling `DevicePaired` and `DeviceRevoked` events). Note: `CreatePairing`, `ListDevices`, and `RevokeDevice` succeed only over the local host bridge (`device-local`).

## Deploying `mesh-relay` to the Oracle ARM VPS

1. **Build or prepare the artifact locally:**
   - Binary: run `daemon/packaging/relay/build-arm64.sh` to produce `daemon/bin/mesh-relay-linux-arm64` (~10 MB static binary), or use `daemon/packaging/relay/Dockerfile` + `daemon/packaging/relay/compose.yaml` via Dockge.
2. **Generate the relay shared secret** (keep mode `0600`):
   ```bash
   ./mesh-relay --gen-token > relay.token
   chmod 600 relay.token
   ```
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
   meshctl --state "$HOME/.local/state/agent-mesh-daemon" pair --name "phone"
   ```
6. **Monitoring:** point uptime-kuma at `https://relay.example.com/healthz`.
