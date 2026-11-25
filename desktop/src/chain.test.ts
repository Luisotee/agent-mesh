// @vitest-environment node
import { create } from '@bufbuild/protobuf'
import { connectNodeAdapter } from '@connectrpc/connect-node'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import http from 'node:http'
import os from 'node:os'
import path from 'node:path'
import { afterEach, beforeEach, expect, it } from 'vitest'
import { forward } from '../electron/bridge'
import type { StoredHost } from '../electron/hosts'
import { createMeshClient } from './actions'
import { HelloResponseSchema, MeshService, SubscribeEventsResponseSchema } from './gen/agentmesh/v1/mesh_pb'
import { hostTransport } from './transport'

let dir: string
let server: http.Server
let tokenSeen: string | undefined
let release: () => void

beforeEach(async () => {
  dir = mkdtempSync(path.join(os.tmpdir(), 'amc-'))
  writeFileSync(path.join(dir, 'device.token'), 'tok_chain')
  const gate = new Promise<void>((r) => (release = r))
  const handler = connectNodeAdapter({
    routes: (router) =>
      router.service(MeshService, {
        hello: (_req, ctx) => {
          tokenSeen = ctx.requestHeader.get('authorization') ?? undefined
          return create(HelloResponseSchema, { daemonId: 'd1', instanceId: 'i1', version: 'chain' })
        },
        async *subscribeEvents() {
          yield create(SubscribeEventsResponseSchema, { frame: { case: 'snapshot', value: { cursor: { generation: 'g', seq: 1n }, runs: [], agents: [] } } })
          await gate
          yield create(SubscribeEventsResponseSchema, { frame: { case: 'end', value: { reason: 'bye' } } })
        },
      }),
  })
  server = http.createServer(handler)
  await new Promise<void>((r) => server.listen(path.join(dir, 'meshd.sock'), r))
})

afterEach(async () => {
  release()
  server.closeAllConnections()
  await new Promise((r) => server.close(r))
  rmSync(dir, { recursive: true, force: true })
})

// The renderer's generated client, through the fetch adapter and the main process's bridge, to a ConnectRPC server:
// unary calls and a server stream that stays open and delivers frames as the server sends them.
it('a generated client talks to a ConnectRPC server through the adapter and the bridge', async () => {
  const host: StoredHost = { id: 'h', name: 'h', kind: 'state-dir', path: dir }
  const api = { request: (_id: string, req: Parameters<typeof forward>[1], cb: Parameters<typeof forward>[2]) => forward(host, req, cb) }
  const client = createMeshClient(hostTransport(api, 'h'))

  const hello = await client.hello({ client: 'test', protocolVersion: 1 })
  expect(hello).toMatchObject({ daemonId: 'd1', version: 'chain' })
  expect(tokenSeen).toBe('Bearer tok_chain')

  const stream = client.subscribeEvents({})[Symbol.asyncIterator]()
  const first = await stream.next()
  expect(first.value?.frame.case).toBe('snapshot')
  // The stream is still open: the next frame waits for the server.
  let got = false
  const second = stream.next().then((r) => ((got = true), r))
  await new Promise((r) => setTimeout(r, 50))
  expect(got).toBe(false)
  release()
  expect((await second).value?.frame.case).toBe('end')
})
