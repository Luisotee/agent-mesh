// @vitest-environment node
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import http from 'node:http'
import os from 'node:os'
import path from 'node:path'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import WebSocket, { WebSocketServer } from 'ws'
import type { SocketEvent } from '../shared/api'
import type { StoredHost } from './hosts'
import { allowedSocketPath, openSocket } from './socket'

let dir: string
let server: http.Server
let wss: WebSocketServer
let upgrades: Array<{ url?: string; headers: http.IncomingHttpHeaders }>
let serverSockets: WebSocket[]

const PATH = '/v1/term/ag_1?ticket=tkt_abc123'
const stateHost = (): StoredHost => ({ id: 'h', name: 'h', kind: 'state-dir', path: dir })

function open(host: StoredHost | undefined, p: unknown) {
  const events: SocketEvent[] = []
  const sock = openSocket(host, p, (e) => events.push(e))
  const waitFor = async (pred: (e: SocketEvent[]) => boolean) => {
    for (let i = 0; i < 200 && !pred(events); i++) await new Promise((r) => setTimeout(r, 10))
    if (!pred(events)) throw new Error(`timed out; events: ${JSON.stringify(events)}`)
  }
  return { sock, events, waitFor }
}

beforeEach(async () => {
  dir = mkdtempSync(path.join(os.tmpdir(), 'ams-'))
  writeFileSync(path.join(dir, 'device.token'), 'tok')
  upgrades = []
  serverSockets = []
  server = http.createServer()
  wss = new WebSocketServer({ noServer: true })
  server.on('upgrade', (req, socket, head) => {
    upgrades.push({ url: req.url, headers: req.headers })
    wss.handleUpgrade(req, socket, head, (ws) => {
      serverSockets.push(ws)
      ws.on('message', (data, isBinary) => ws.send(data, { binary: isBinary }))
    })
  })
  await new Promise<void>((r) => server.listen(path.join(dir, 'meshd.sock'), r))
})

afterEach(async () => {
  for (const s of serverSockets) s.terminate()
  wss.close()
  server.closeAllConnections()
  await new Promise((r) => server.close(r))
  rmSync(dir, { recursive: true, force: true })
})

describe('terminal socket bridge', () => {
  it('opens the stream over the unix socket with no Origin and no token, and passes binary frames both ways', async () => {
    const t = open(stateHost(), PATH)
    await t.waitFor((e) => e.some((x) => x.type === 'open'))
    expect(upgrades[0]!.url).toBe(PATH)
    expect(upgrades[0]!.headers.origin).toBeUndefined()
    expect(upgrades[0]!.headers.authorization).toBeUndefined()
    t.sock.send(new Uint8Array([1, 2, 3]))
    await t.waitFor((e) => e.some((x) => x.type === 'message'))
    const m = t.events.find((x) => x.type === 'message') as Extract<SocketEvent, { type: 'message' }>
    expect([...m.data]).toEqual([1, 2, 3])
  })

  it('reports the close code and reason of the host', async () => {
    const t = open(stateHost(), PATH)
    await t.waitFor((e) => e.some((x) => x.type === 'open'))
    serverSockets[0]!.close(4001, 'slow')
    await t.waitFor((e) => e.some((x) => x.type === 'close'))
    expect(t.events.at(-1)).toEqual({ type: 'close', code: 4001, reason: 'slow' })
  })

  it('closing from the renderer ends the host side and emits nothing more', async () => {
    const t = open(stateHost(), PATH)
    await t.waitFor((e) => e.some((x) => x.type === 'open'))
    t.sock.close()
    await new Promise((r) => serverSockets[0]!.once('close', r))
    const n = t.events.length
    await new Promise((r) => setTimeout(r, 50))
    expect(t.events).toHaveLength(n)
  })

  it('allows a project shell stream, with a ticket, as it does an agent terminal', () => {
    expect(allowedSocketPath('/v1/shell/t_abc123?ticket=tkt_abc123')).toBe(true)
    expect(allowedSocketPath('/v1/shell/t_abc123')).toBe(false)
    expect(allowedSocketPath('/v1/shells/t_abc123?ticket=tkt_abc123')).toBe(false)
  })

  it('refuses any path but a terminal stream, without touching the host', async () => {
    for (const bad of ['/agentmesh.v1.MeshService/Hello', '/v1/term/x', '/v1/term/../x?ticket=a', '/v1/term/a?ticket=a&x=1', '/v1/term/a?ticket=', undefined, 5]) {
      expect(allowedSocketPath(bad)).toBe(false)
      const t = open(stateHost(), bad)
      expect(t.events).toEqual([{ type: 'error', code: 'refused', message: expect.any(String) }])
    }
    expect(upgrades).toHaveLength(0)
  })

  it('reports an unknown host, a host with no token and one that is down', async () => {
    expect(open(undefined, PATH).events[0]).toMatchObject({ type: 'error', code: 'unknown-host' })
    rmSync(path.join(dir, 'device.token'))
    const a = open(stateHost(), PATH)
    await a.waitFor((e) => e.length > 0)
    expect(a.events[0]).toMatchObject({ type: 'error', code: 'no-token' })
    writeFileSync(path.join(dir, 'device.token'), 't')
    await new Promise((r) => server.close(r))
    const b = open(stateHost(), PATH)
    await b.waitFor((e) => e.length > 0)
    expect(b.events[0]).toMatchObject({ type: 'error', code: 'unreachable' })
  })

  it('a rejected upgrade (a spent ticket) is an error', async () => {
    server.removeAllListeners('upgrade')
    server.on('upgrade', (_req, socket) => socket.end('HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\n'))
    const t = open(stateHost(), PATH)
    await t.waitFor((e) => e.length > 0)
    expect(t.events[0]).toMatchObject({ type: 'error', code: 'refused' })
  })
})
