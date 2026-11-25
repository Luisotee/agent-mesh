// @vitest-environment node
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import http from 'node:http'
import os from 'node:os'
import path from 'node:path'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import type { BridgeEvent } from '../shared/api'
import { allowed, forward } from './bridge'
import type { StoredHost } from './hosts'

let dir: string
let server: http.Server
let seen: Array<{ method?: string; url?: string; headers: http.IncomingHttpHeaders; body: string }>
let onRequest: (req: http.IncomingMessage, res: http.ServerResponse) => void

const stateHost = (): StoredHost => ({ id: 'h', name: 'h', kind: 'state-dir', path: dir })

function run(host: StoredHost | undefined, req: Parameters<typeof forward>[1]) {
  const events: BridgeEvent[] = []
  let abort = () => {}
  const done = new Promise<void>((resolve) => {
    abort = forward(host, req, (e) => {
      events.push(e)
      if (e.type === 'end' || e.type === 'error') resolve()
    })
  })
  return { events, done, abort: () => abort() }
}

const call = { method: 'POST', path: '/agentmesh.v1.MeshService/Hello', headers: { 'content-type': 'application/proto' }, body: new Uint8Array([1, 2, 3]) }

beforeEach(async () => {
  dir = mkdtempSync(path.join(os.tmpdir(), 'amb-'))
  writeFileSync(path.join(dir, 'device.token'), 'tok_secret\n')
  seen = []
  onRequest = (_req, res) => res.end('ok')
  server = http.createServer((req, res) => {
    const chunks: Buffer[] = []
    req.on('data', (c: Buffer) => chunks.push(c))
    req.on('end', () => {
      seen.push({ method: req.method, url: req.url, headers: req.headers, body: Buffer.concat(chunks).toString('hex') })
      onRequest(req, res)
    })
  })
  await new Promise<void>((r) => server.listen(path.join(dir, 'meshd.sock'), r))
})

afterEach(async () => {
  server.closeAllConnections()
  await new Promise((r) => server.close(r))
  rmSync(dir, { recursive: true, force: true })
})

describe('bridge', () => {
  it('forwards a registry call over the socket with the host\'s token, and only the headers it allows', async () => {
    const r = run(stateHost(), { ...call, headers: { 'content-type': 'application/proto', origin: 'file://', 'sec-fetch-site': 'same-origin', authorization: 'Bearer stolen', cookie: 'x=1', 'connect-protocol-version': '1' } })
    await r.done
    expect(seen).toHaveLength(1)
    const s = seen[0]!
    expect(s.url).toBe('/agentmesh.v1.MeshService/Hello')
    expect(s.body).toBe('010203')
    expect(s.headers.authorization).toBe('Bearer tok_secret')
    expect(s.headers['content-type']).toBe('application/proto')
    expect(s.headers['connect-protocol-version']).toBe('1')
    expect(s.headers.origin).toBeUndefined()
    expect(s.headers['sec-fetch-site']).toBeUndefined()
    expect(s.headers.cookie).toBeUndefined()
    expect(r.events.map((e) => e.type)).toEqual(['head', 'data', 'end'])
  })

  it('streams a response as it arrives', async () => {
    onRequest = (_req, res) => {
      res.writeHead(200, { 'content-type': 'application/connect+proto' })
      res.write('one')
      setTimeout(() => res.end('two'), 30)
    }
    const r = run(stateHost(), call)
    await r.done
    const data = r.events.filter((e): e is Extract<BridgeEvent, { type: 'data' }> => e.type === 'data').map((e) => Buffer.from(e.chunk).toString())
    expect(data.join('')).toBe('onetwo')
    expect(data.length).toBeGreaterThan(1)
  })

  it('passes an HTTP error status through, with its body', async () => {
    onRequest = (_req, res) => res.writeHead(401).end('no')
    const r = run(stateHost(), call)
    await r.done
    expect(r.events[0]).toMatchObject({ type: 'head', status: 401 })
  })

  it('refuses anything outside the registry without touching the host', async () => {
    for (const bad of [
      { ...call, path: '/v1/term/abc?ticket=x' },
      { ...call, path: '/agentmesh.v1.MeshService/../../etc' },
      { ...call, path: '/other.Service/Hello' },
      { ...call, method: 'GET' },
    ]) {
      expect(allowed(bad)).toBe(false)
      const r = run(stateHost(), bad)
      await r.done
      expect(r.events).toEqual([{ type: 'error', code: 'refused', message: expect.stringContaining('registry') }])
    }
    expect(seen).toHaveLength(0)
  })

  it('refuses a malformed request from the renderer instead of throwing', async () => {
    for (const bad of [undefined, null, 'x', {}, { method: 'POST', path: '/agentmesh.v1.MeshService/Hello' }, { ...call, headers: { a: 1 } }, { ...call, body: 'text' }, { ...call, headers: { 'content-type': 'a\r\nx-evil: 1' } }]) {
      const r = run(stateHost(), bad as never)
      await r.done
      expect(r.events).toEqual([{ type: 'error', code: 'refused', message: 'malformed request' }])
    }
    expect(seen).toHaveLength(0)
  })

  it('says so when the host has no token, can\'t be reached, or is unknown', async () => {
    rmSync(path.join(dir, 'device.token'))
    const a = run(stateHost(), call)
    await a.done
    expect(a.events[0]).toMatchObject({ type: 'error', code: 'no-token' })

    writeFileSync(path.join(dir, 'device.token'), 't')
    await new Promise((r) => server.close(r))
    const b = run(stateHost(), call)
    await b.done
    expect(b.events[0]).toMatchObject({ type: 'error', code: 'unreachable' })

    const c = run(undefined, call)
    await c.done
    expect(c.events[0]).toMatchObject({ type: 'error', code: 'unknown-host' })
  })

  it('reads the token at call time, so a changed one is used', async () => {
    await run(stateHost(), call).done
    writeFileSync(path.join(dir, 'device.token'), 'tok_new\n')
    await run(stateHost(), call).done
    expect(seen.map((s) => s.headers.authorization)).toEqual(['Bearer tok_secret', 'Bearer tok_new'])
  })

  it('aborting closes the host connection and emits nothing more', async () => {
    let closed!: () => void
    const gone = new Promise<void>((r) => (closed = r))
    onRequest = (_req, res) => {
      res.writeHead(200)
      res.write('x')
      res.on('close', closed)
    }
    const r = run(stateHost(), call)
    while (!r.events.some((e) => e.type === 'data')) await new Promise((x) => setTimeout(x, 10))
    r.abort()
    await gone
    const n = r.events.length
    await new Promise((x) => setTimeout(x, 50))
    expect(r.events).toHaveLength(n)
  })

  it('reaches a tcp host with its own token', async () => {
    const tcp = http.createServer((req, res) => {
      seen.push({ method: req.method, url: req.url, headers: req.headers, body: '' })
      res.end('ok')
    })
    await new Promise<void>((r) => tcp.listen(0, '127.0.0.1', r))
    try {
      const port = (tcp.address() as { port: number }).port
      await run({ id: 't', name: 't', kind: 'tcp', address: `127.0.0.1:${port}`, token: 'tok_tcp' }, call).done
      expect(seen[0]!.headers.authorization).toBe('Bearer tok_tcp')
    } finally {
      tcp.closeAllConnections()
      tcp.close()
    }
  })
})
