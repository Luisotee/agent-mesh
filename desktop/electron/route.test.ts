// @vitest-environment node
import { execFileSync } from 'node:child_process'
import { X509Certificate } from 'node:crypto'
import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import http from 'node:http'
import https from 'node:https'
import net from 'node:net'
import os from 'node:os'
import path from 'node:path'
import { WebSocketServer } from 'ws'
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest'
import type { BridgeEvent, SocketEvent } from '../shared/api'
import { forward } from './bridge'
import type { StoredHost } from './hosts'
import { Dialer, fingerprintOf, parsePairingLink, type Route } from './route'
import { openSocket } from './socket'

const FP = `sha256:${'ab'.repeat(32)}`

describe('parsePairingLink', () => {
  const q = (s: string) => `agentmesh://pair?v=1&h=h_1&fp=${FP}&t=amp_x${s}`
  it('reads the route, the token and the names', () => {
    const l = parsePairingLink(` ${q(`&hn=Home&n=Pixel&d=${encodeURIComponent('https://10.0.0.2:9443')}&rtcp=relay.example.com:8443&rt=amr_k`)} `)
    expect(l).toMatchObject({ token: 'amp_x', hostName: 'Home', deviceName: 'Pixel' })
    expect(l.route).toEqual({ hostId: 'h_1', fingerprint: FP, direct: { host: '10.0.0.2', port: 9443 }, relay: { host: 'relay.example.com', port: 8443, token: 'amr_k' } })
  })
  it('takes a bare 64-hex fingerprint in any case, normalised', () => {
    expect(parsePairingLink(`agentmesh://pair?v=1&h=h&fp=${'AB'.repeat(32)}&t=t&d=https://a.example:1`).route.fingerprint).toBe(FP)
  })
  it('defaults the direct port and unbrackets IPv6', () => {
    expect(parsePairingLink(q('&d=https://host.example')).route.direct).toEqual({ host: 'host.example', port: 443 })
    expect(parsePairingLink(q('&rtcp=[::1]:8443&rt=k')).route.relay).toMatchObject({ host: '::1', port: 8443 })
  })
  it('refuses what is not a usable link', () => {
    for (const [text, why] of [
      ['hello', /pairing link/],
      ['https://example.com/pair?v=1', /pairing link/],
      ['agentmesh://other?v=1', /pairing link/],
      [q('').replace('v=1', 'v=2') + '&d=https://a:1', /version/],
      [q('').replace('&t=amp_x', '') + '&d=https://a:1', /missing/],
      [q('').replace(FP, 'sha256:12') + '&d=https://a:1', /fingerprint/],
      [q('&d=http://a:1'), /https/],
      [q('&d=%%%'), /URL/],
      [q('&rtcp=nohostport&rt=k'), /host:port/],
      [q('&rtcp=h:99999&rt=k'), /host:port/],
      [q(''), /no way to reach/],
      [q('&rtcp=relay:8443'), /no way to reach/], // a relay address without its token isn't a route
    ] as Array<[string, RegExp]>) {
      expect(() => parsePairingLink(text), text).toThrow(why)
    }
  })
})

// A real TLS endpoint with a self-signed certificate, as meshd's is.
let dir: string
let cert: string
let key: string
let pin: string
beforeAll(() => {
  dir = mkdtempSync(path.join(os.tmpdir(), 'amr-'))
  execFileSync('openssl', ['req', '-x509', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:prime256v1', '-nodes', '-keyout', path.join(dir, 'k.pem'), '-out', path.join(dir, 'c.pem'), '-subj', '/CN=meshd', '-days', '2'], { stdio: 'pipe' })
  cert = readFileSync(path.join(dir, 'c.pem'), 'utf8')
  key = readFileSync(path.join(dir, 'k.pem'), 'utf8')
  pin = fingerprintOf(new X509Certificate(cert).raw)
})
afterAll(() => rmSync(dir, { recursive: true, force: true }))

let closers: Array<() => void> = []
afterEach(() => {
  closers.forEach((c) => c())
  closers = []
})

interface Seen {
  url?: string
  auth?: string
}
/** A host: HTTPS with the certificate, answering every call "ok", with a terminal-stream WebSocket that sends "hello". */
async function startHost(): Promise<{ server: https.Server; port: number; seen: Seen[] }> {
  const seen: Seen[] = []
  const server = https.createServer({ cert, key }, (req, res) => {
    seen.push({ url: req.url, auth: req.headers.authorization })
    res.end('ok')
  })
  const wss = new WebSocketServer({ server })
  wss.on('connection', (w) => w.send(Buffer.from('hello'), { binary: true }))
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r))
  closers.push(() => {
    server.closeAllConnections()
    server.close()
  })
  return { server, port: (server.address() as net.AddressInfo).port, seen }
}

/** The relay's raw TCP listener, as docs/relay.md has it: a line of preamble, then `OK` and the host's own stream. */
async function startRelay(answer: (line: string) => string | undefined, host?: https.Server): Promise<{ port: number; lines: string[] }> {
  const lines: string[] = []
  const server = net.createServer((sock) => {
    let buf = ''
    sock.on('data', function onData(c) {
      buf += c.toString('latin1')
      const nl = buf.indexOf('\n')
      if (nl < 0) return
      sock.off('data', onData)
      const line = buf.slice(0, nl)
      lines.push(line)
      const reply = answer(line)
      if (reply === undefined) return
      sock.write(`${reply}\n`)
      if (reply === 'OK' && host) host.emit('connection', sock)
    })
  })
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r))
  closers.push(() => {
    server.close()
  })
  return { port: (server.address() as net.AddressInfo).port, lines }
}

const deadPort = async (): Promise<number> => {
  const s = net.createServer()
  await new Promise<void>((r) => s.listen(0, '127.0.0.1', r))
  const p = (s.address() as net.AddressInfo).port
  await new Promise((r) => s.close(r))
  return p
}

const paired = (route: Route): StoredHost => ({ id: 'p', name: 'p', kind: 'paired', deviceId: 'dev', token: 'amt_secret', route })
const call = { method: 'POST', path: '/agentmesh.v1.MeshService/Hello', headers: { 'content-type': 'application/proto' }, body: new Uint8Array([1]) }

function request(host: StoredHost) {
  const events: BridgeEvent[] = []
  return new Promise<BridgeEvent[]>((resolve) => {
    forward(host, call, (e) => {
      events.push(e)
      if (e.type === 'end' || e.type === 'error') resolve(events)
    })
  })
}

describe('a paired host over a direct address', () => {
  it('serves the bridge\'s calls over TLS pinned to the certificate, with the device token', async () => {
    const h = await startHost()
    const events = await request(paired({ hostId: 'h', fingerprint: pin, direct: { host: '127.0.0.1', port: h.port } }))
    expect(events.map((e) => e.type)).toEqual(['head', 'data', 'end'])
    expect(h.seen).toEqual([{ url: '/agentmesh.v1.MeshService/Hello', auth: 'Bearer amt_secret' }])
  })

  it('refuses a certificate that is not the pinned one, and sends nothing to it', async () => {
    const h = await startHost()
    const other = `sha256:${'cd'.repeat(32)}`
    const events = await request(paired({ hostId: 'h', fingerprint: other, direct: { host: '127.0.0.1', port: h.port } }))
    expect(events).toEqual([{ type: 'error', code: 'refused', message: expect.stringMatching(/isn't the one it was paired with/) }])
    expect(h.seen).toEqual([])
  })

  it('says unreachable for an address nobody listens on', async () => {
    const events = await request(paired({ hostId: 'h', fingerprint: pin, direct: { host: '127.0.0.1', port: await deadPort() } }))
    expect(events).toEqual([{ type: 'error', code: 'unreachable', message: expect.any(String) }])
  })

  it('opens the terminal stream through the same pinned connection', async () => {
    const h = await startHost()
    const events: SocketEvent[] = []
    const done = new Promise<void>((r) => {
      openSocket(paired({ hostId: 'h', fingerprint: pin, direct: { host: '127.0.0.1', port: h.port } }), '/v1/term/a1?ticket=tk', (e) => {
        events.push(e)
        if (e.type === 'message') r()
        if (e.type === 'error' || e.type === 'close') r()
      })
    })
    await done
    expect(events.map((e) => e.type)).toEqual(['open', 'message'])
    expect(Buffer.from((events[1] as { data: Uint8Array }).data).toString()).toBe('hello')
  })
})

describe('a paired host through the relay', () => {
  it('sends the preamble, waits for OK, then speaks pinned TLS over the same socket', async () => {
    const h = await startHost()
    const relay = await startRelay(() => 'OK', h.server)
    const events = await request(paired({ hostId: 'h_9', fingerprint: pin, relay: { host: '127.0.0.1', port: relay.port, token: 'amr_tok' } }))
    expect(events.map((e) => e.type)).toEqual(['head', 'data', 'end'])
    expect(relay.lines).toEqual(['MESH1 h_9 amr_tok'])
    expect(h.seen).toHaveLength(1)
  })

  it('refuses a wrong certificate behind the relay too', async () => {
    const h = await startHost()
    const relay = await startRelay(() => 'OK', h.server)
    const events = await request(paired({ hostId: 'h', fingerprint: `sha256:${'cd'.repeat(32)}`, relay: { host: '127.0.0.1', port: relay.port, token: 't' } }))
    expect(events[0]).toMatchObject({ type: 'error', code: 'refused' })
    expect(h.seen).toEqual([])
  })

  it('maps the relay\'s refusals: offline is unreachable, a bad token is refused', async () => {
    for (const [reply, code] of [
      ['ERR offline daemon offline', 'unreachable'],
      ['ERR timeout dial-back timed out', 'unreachable'],
      ['ERR auth bad token', 'refused'],
      ['ERR bad_request nope', 'failed'],
      ['what', 'failed'],
    ] as const) {
      const relay = await startRelay(() => reply)
      const events = await request(paired({ hostId: 'h', fingerprint: pin, relay: { host: '127.0.0.1', port: relay.port, token: 't' } }))
      expect(events[0], reply).toMatchObject({ type: 'error', code })
    }
    const events = await request(paired({ hostId: 'h', fingerprint: pin, relay: { host: '127.0.0.1', port: await deadPort(), token: 't' } }))
    expect(events[0]).toMatchObject({ type: 'error', code: 'unreachable' })
  })

  it('opens the terminal stream through the relay', async () => {
    const h = await startHost()
    const relay = await startRelay(() => 'OK', h.server)
    const events: SocketEvent[] = []
    await new Promise<void>((r) => {
      openSocket(paired({ hostId: 'h', fingerprint: pin, relay: { host: '127.0.0.1', port: relay.port, token: 't' } }), '/v1/term/a1?ticket=tk', (e) => {
        events.push(e)
        if (e.type !== 'open') r()
      })
    })
    expect(events.map((e) => e.type)).toEqual(['open', 'message'])
  })
})

describe('both routes', () => {
  it('falls back to the relay when the direct address can\'t be reached, and tries direct again after a minute', async () => {
    const h = await startHost()
    const relay = await startRelay(() => 'OK', h.server)
    // A direct address that accepts a connection and drops it: counted, so we see when it is tried.
    let directTries = 0
    const flaky = net.createServer((s) => {
      directTries++
      s.destroy()
    })
    await new Promise<void>((r) => flaky.listen(0, '127.0.0.1', r))
    closers.push(() => flaky.close())
    const d = new Dialer({ hostId: 'h', fingerprint: pin, direct: { host: '127.0.0.1', port: (flaky.address() as net.AddressInfo).port }, relay: { host: '127.0.0.1', port: relay.port, token: 't' } })
    const t0 = 1_000_000
    ;(await d.connect(t0)).destroy()
    expect(directTries).toBe(1)
    expect(relay.lines).toHaveLength(1)
    ;(await d.connect(t0 + 30_000)).destroy()
    expect(directTries).toBe(1) // remembered
    expect(relay.lines).toHaveLength(2)
    ;(await d.connect(t0 + 61_000)).destroy()
    expect(directTries).toBe(2)
  })

  it('does not go to the relay when the direct host showed the wrong certificate', async () => {
    const h = await startHost()
    const relay = await startRelay(() => 'OK', h.server)
    const d = new Dialer({ hostId: 'h', fingerprint: `sha256:${'cd'.repeat(32)}`, direct: { host: '127.0.0.1', port: h.port }, relay: { host: '127.0.0.1', port: relay.port, token: 't' } })
    await expect(d.connect()).rejects.toMatchObject({ code: 'refused' })
    expect(relay.lines).toEqual([])
  })
})
