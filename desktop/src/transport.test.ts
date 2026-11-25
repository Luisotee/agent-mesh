import { Code, ConnectError } from '@connectrpc/connect'
import { describe, expect, it } from 'vitest'
import type { BridgeEvent, BridgeRequest } from '../shared/api'
import { bridgeFetch } from './transport'

/** A bridge a test controls: it records the request and lets the test push events. */
function fakeBridge() {
  const state = { req: undefined as BridgeRequest | undefined, hostId: '', aborted: false, emit: (_e: BridgeEvent) => {} }
  const request = (hostId: string, req: BridgeRequest, onEvent: (e: BridgeEvent) => void) => {
    state.req = req
    state.hostId = hostId
    state.emit = onEvent
    return () => {
      state.aborted = true
    }
  }
  return { state, request }
}

const post = (url = 'http://meshd/agentmesh.v1.MeshService/Hello') => ({ method: 'POST', headers: { 'content-type': 'application/proto' }, body: new Uint8Array([9, 8]) as BodyInit, url })

describe('bridgeFetch', () => {
  it('turns a fetch into a bridge request and the answer into a Response', async () => {
    const b = fakeBridge()
    const p = bridgeFetch(b, 'h1')(post().url, post())
    await new Promise((r) => setTimeout(r, 0))
    expect(b.state.hostId).toBe('h1')
    expect(b.state.req).toMatchObject({ method: 'POST', path: '/agentmesh.v1.MeshService/Hello' })
    expect([...(b.state.req!.body ?? [])]).toEqual([9, 8])
    b.state.emit({ type: 'head', status: 200, headers: { 'content-type': 'application/proto' } })
    b.state.emit({ type: 'data', chunk: new Uint8Array([1, 2]) })
    b.state.emit({ type: 'end' })
    const res = await p
    expect(res.status).toBe(200)
    expect([...new Uint8Array(await res.arrayBuffer())]).toEqual([1, 2])
  })

  it('streams: the Response resolves at the head, and chunks arrive as they are pushed', async () => {
    const b = fakeBridge()
    const p = bridgeFetch(b, 'h')(post().url, post())
    await new Promise((r) => setTimeout(r, 0))
    b.state.emit({ type: 'head', status: 200, headers: {} })
    const res = await p
    const reader = res.body!.getReader()
    b.state.emit({ type: 'data', chunk: new Uint8Array([1]) })
    expect((await reader.read()).value).toEqual(new Uint8Array([1]))
    b.state.emit({ type: 'data', chunk: new Uint8Array([2]) })
    expect((await reader.read()).value).toEqual(new Uint8Array([2]))
    b.state.emit({ type: 'end' })
    expect((await reader.read()).done).toBe(true)
  })

  it('rejects with the right Connect code when the bridge fails before an answer', async () => {
    const cases: Array<[Extract<BridgeEvent, { type: 'error' }>['code'], Code]> = [
      ['no-token', Code.Unauthenticated],
      ['unreachable', Code.Unavailable],
      ['refused', Code.FailedPrecondition],
      ['failed', Code.Unavailable],
    ]
    for (const [code, want] of cases) {
      const b = fakeBridge()
      const p = bridgeFetch(b, 'h')(post().url, post())
      await new Promise((r) => setTimeout(r, 0))
      b.state.emit({ type: 'error', code, message: 'nope' })
      const err = await p.catch((e: unknown) => e)
      expect(ConnectError.from(err).code).toBe(want)
    }
  })

  it('errors the body when the bridge fails mid-stream', async () => {
    const b = fakeBridge()
    const p = bridgeFetch(b, 'h')(post().url, post())
    await new Promise((r) => setTimeout(r, 0))
    b.state.emit({ type: 'head', status: 200, headers: {} })
    const res = await p
    b.state.emit({ type: 'error', code: 'failed', message: 'reset' })
    await expect(res.arrayBuffer()).rejects.toThrow(/reset/)
  })

  it('aborts the host request when the caller aborts, or cancels the body', async () => {
    const b = fakeBridge()
    const ctl = new AbortController()
    const p = bridgeFetch(b, 'h')(post().url, { ...post(), signal: ctl.signal })
    await new Promise((r) => setTimeout(r, 0))
    ctl.abort()
    expect(ConnectError.from(await p.catch((e: unknown) => e)).code).toBe(Code.Canceled)
    expect(b.state.aborted).toBe(true)

    const c = fakeBridge()
    const q = bridgeFetch(c, 'h')(post().url, post())
    await new Promise((r) => setTimeout(r, 0))
    c.state.emit({ type: 'head', status: 200, headers: {} })
    await (await q).body!.cancel()
    expect(c.state.aborted).toBe(true)
  })

  it('ignores events that arrive after the request was cancelled', async () => {
    const b = fakeBridge()
    const ctl = new AbortController()
    const p = bridgeFetch(b, 'h')(post().url, { ...post(), signal: ctl.signal })
    await new Promise((r) => setTimeout(r, 0))
    b.state.emit({ type: 'head', status: 200, headers: {} })
    const res = await p
    ctl.abort()
    expect(() => {
      b.state.emit({ type: 'data', chunk: new Uint8Array([1]) })
      b.state.emit({ type: 'end' })
    }).not.toThrow()
    await expect(res.arrayBuffer()).rejects.toThrow()
  })
})
