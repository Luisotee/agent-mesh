import { createHash } from 'node:crypto'
import net from 'node:net'
import tls from 'node:tls'
import type { BridgeErrorCode } from '../shared/api.js'

/**
 * How main reaches a paired host (docs/relay.md): straight over TLS, or through the relay's raw TCP listener, and
 * either way end to end over TLS that is pinned to the host's certificate, the one fingerprint in the pairing link.
 * The relay sees only bytes it can't read. A pin that doesn't match is a hard failure: no fallback, no prompt.
 */

/** Where a host listens, and the certificate that proves it is the host. */
export interface Route {
  hostId: string
  /** `sha256:<64 lower hex>` of the host's leaf certificate. */
  fingerprint: string
  direct?: { host: string; port: number }
  relay?: { host: string; port: number; token: string }
}

/** What a pairing link says (`agentmesh://pair?...`, docs/relay.md). */
export interface PairingLink {
  route: Route
  /** The one-time `amp_` token. */
  token: string
  hostName: string
  deviceName: string
}

/** A failure to reach a host that says why, in the bridge's terms. */
export class RouteError extends Error {
  constructor(
    readonly code: BridgeErrorCode,
    message: string,
  ) {
    super(message)
  }
}

const FINGERPRINT = /^(?:sha256:)?([0-9a-f]{64})$/i
const HOST_PORT = /^(\[[0-9a-f:.]+\]|[A-Za-z0-9.-]+):(\d{1,5})$/i

function hostPort(s: string, what: string): { host: string; port: number } {
  const m = HOST_PORT.exec(s.trim())
  const port = m ? Number(m[2]) : 0
  if (!m || port < 1 || port > 65535) throw new Error(`the pairing link's ${what} isn't host:port`)
  return { host: m[1]!.replace(/^\[(.*)\]$/, '$1'), port }
}

/** Reads and checks a pairing link. The link is text from the user, so every part is checked here. */
export function parsePairingLink(text: string): PairingLink {
  let u: URL
  try {
    u = new URL(text.trim())
  } catch {
    throw new Error("that isn't a pairing link (it starts with agentmesh://pair)")
  }
  if (u.protocol !== 'agentmesh:' || u.hostname !== 'pair') throw new Error("that isn't a pairing link (it starts with agentmesh://pair)")
  const q = u.searchParams
  if (q.get('v') !== '1') throw new Error(`this app doesn't know pairing links of version ${q.get('v') ?? '?'}`)
  const hostId = q.get('h') ?? ''
  const token = q.get('t') ?? ''
  const fp = FINGERPRINT.exec(q.get('fp') ?? '')
  if (!hostId || !token) throw new Error('the pairing link is missing its host or its token')
  if (!fp) throw new Error("the pairing link's certificate fingerprint isn't sha256:<64 hex digits>")
  const route: Route = { hostId, fingerprint: `sha256:${fp[1]!.toLowerCase()}` }
  const d = q.get('d')
  if (d) {
    let du: URL
    try {
      du = new URL(d)
    } catch {
      throw new Error("the pairing link's direct address isn't a URL")
    }
    if (du.protocol !== 'https:') throw new Error('the pairing link\'s direct address must be https')
    route.direct = { host: du.hostname.replace(/^\[(.*)\]$/, '$1'), port: du.port ? Number(du.port) : 443 }
  }
  const rtcp = q.get('rtcp')
  const rt = q.get('rt')
  if (rtcp && rt) route.relay = { ...hostPort(rtcp, 'relay address'), token: rt }
  if (!route.direct && !route.relay) throw new Error('the pairing link has no way to reach the host (no direct address and no relay)')
  return { route, token, hostName: q.get('hn') ?? '', deviceName: q.get('n') ?? '' }
}

/** The pin of a certificate: `sha256:` and the lower-case hex of its SHA-256 over the DER bytes. */
export function fingerprintOf(cert: Buffer): string {
  return `sha256:${createHash('sha256').update(cert).digest('hex')}`
}

const DIRECT_TIMEOUT_MS = 4_000
const RELAY_TIMEOUT_MS = 15_000

function fail(e: NodeJS.ErrnoException, what: string): RouteError {
  const unreachable = ['ECONNREFUSED', 'ECONNRESET', 'ETIMEDOUT', 'EHOSTUNREACH', 'ENETUNREACH', 'ENOTFOUND', 'EAI_AGAIN']
  return new RouteError(e.code && unreachable.includes(e.code) ? 'unreachable' : 'failed', `${what}: ${e.message}`)
}

/**
 * TLS over `socket` (or a new connection to host:port) that must present the pinned certificate. The host's
 * certificate is self-signed, so the usual chain check is off and the pin is the whole check; the socket is handed
 * on only once it has matched.
 */
function pinned(pin: string, to: { host?: string; port?: number; socket?: net.Socket }, timeoutMs: number): Promise<tls.TLSSocket> {
  return new Promise((resolve, reject) => {
    const opts: tls.ConnectionOptions = { rejectUnauthorized: false, ALPNProtocols: ['http/1.1'], minVersion: 'TLSv1.2' }
    if (to.socket) opts.socket = to.socket
    else {
      opts.host = to.host
      opts.port = to.port
      // A name is the SNI; an address has none.
      if (to.host && !net.isIP(to.host)) opts.servername = to.host
    }
    const s = tls.connect(opts)
    const timer = setTimeout(() => s.destroy(new RouteError('unreachable', 'timed out connecting to the host')), timeoutMs)
    s.once('secureConnect', () => {
      clearTimeout(timer)
      const cert = s.getPeerCertificate()
      if (!cert?.raw || fingerprintOf(cert.raw) !== pin) {
        s.destroy()
        reject(new RouteError('refused', "the host's certificate isn't the one it was paired with: not connecting"))
        return
      }
      s.setNoDelay(true)
      resolve(s)
    })
    s.once('error', (e: Error) => {
      clearTimeout(timer)
      reject(e instanceof RouteError ? e : fail(e, 'TLS to the host'))
    })
  })
}

/** The relay's preamble (docs/relay.md): `MESH1 <host> <token>\n`, then `OK\n`, after which the socket is the host's. */
function throughRelay(route: Route): Promise<net.Socket> {
  const r = route.relay!
  return new Promise((resolve, reject) => {
    const raw = net.connect({ host: r.host, port: r.port })
    raw.setNoDelay(true)
    const timer = setTimeout(() => raw.destroy(new RouteError('unreachable', 'the relay did not answer in time')), RELAY_TIMEOUT_MS)
    let buf = Buffer.alloc(0)
    const done = (err?: Error) => {
      clearTimeout(timer)
      raw.off('data', onData)
      raw.off('error', onError)
      if (err) {
        raw.destroy()
        reject(err)
      }
    }
    const onError = (e: Error) => done(e instanceof RouteError ? e : fail(e, 'the relay'))
    const onData = (chunk: Buffer) => {
      buf = Buffer.concat([buf, chunk])
      const nl = buf.indexOf(0x0a)
      if (nl < 0) {
        if (buf.length > 512) done(new RouteError('failed', 'the relay answered with something that is not a line'))
        return
      }
      const line = buf.subarray(0, nl).toString('latin1').trim()
      const rest = buf.subarray(nl + 1)
      if (line !== 'OK') {
        const [, code = '', ...msg] = /^ERR\s/.test(line) ? line.split(' ') : ['', '']
        const text = msg.join(' ') || line
        done(new RouteError(code === 'offline' || code === 'timeout' || code === 'unavailable' || code === 'busy' ? 'unreachable' : code === 'auth' ? 'refused' : 'failed', `the relay says: ${text}`))
        return
      }
      done()
      // Paused until the TLS layer takes it over; anything the relay sent behind its line belongs to that stream.
      raw.pause()
      if (rest.length) raw.unshift(rest)
      resolve(raw)
    }
    raw.once('connect', () => raw.write(`MESH1 ${route.hostId} ${r.token}\n`))
    raw.on('data', onData)
    raw.on('error', onError)
  })
}

/** Remembers, per host, that the direct address failed, so the next connections go to the relay for a while. */
const DIRECT_RETRY_MS = 60_000

export class Dialer {
  private directFailedAt = 0

  constructor(readonly route: Route) {}

  /** A pinned TLS connection to the host: the direct address first, the relay when that can't be reached. */
  async connect(now = Date.now()): Promise<tls.TLSSocket> {
    const { route } = this
    const skipDirect = route.relay && now - this.directFailedAt < DIRECT_RETRY_MS
    let direct: unknown
    if (route.direct && !skipDirect) {
      try {
        return await pinned(route.fingerprint, route.direct, DIRECT_TIMEOUT_MS)
      } catch (e) {
        // A wrong certificate is the host not being who it was paired as: the relay can't fix that.
        if (e instanceof RouteError && e.code === 'refused') throw e
        if (!route.relay) throw e
        this.directFailedAt = now
        direct = e
      }
    }
    if (!route.relay) throw direct ?? new RouteError('failed', 'no route to the host')
    const raw = await throughRelay(route)
    try {
      return await pinned(route.fingerprint, { socket: raw }, RELAY_TIMEOUT_MS)
    } catch (e) {
      raw.destroy()
      throw e
    }
  }
}

const dialers = new WeakMap<object, Dialer>()

/** The dialer of a stored host, kept with it so what it learned about its routes lasts. */
export function dialerFor(owner: object, route: Route): Dialer {
  let d = dialers.get(owner)
  if (!d) {
    d = new Dialer(route)
    dialers.set(owner, d)
  }
  return d
}
