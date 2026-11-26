import { Code, ConnectError } from '@connectrpc/connect'
import { create } from '@bufbuild/protobuf'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { HostConnection, type ConnClient, type ConnState, type Frame } from './connection'
import { HelloResponseSchema, SubscribeEventsResponseSchema, type SubscribeEventsResponse } from './gen/agentmesh/v1/mesh_pb'

const hello = (instance = 'i1') => create(HelloResponseSchema, { daemonId: 'd1', instanceId: instance, version: 'test' })
const snap = (): SubscribeEventsResponse => create(SubscribeEventsResponseSchema, { frame: { case: 'snapshot', value: { cursor: { generation: 'g', seq: 1n }, runs: [], agents: [] } } })

/** A stream a test pushes frames into and can break. */
function pipe() {
  const queue: Array<{ v?: SubscribeEventsResponse; err?: unknown; done?: boolean }> = []
  let wake: (() => void) | undefined
  const push = (x: (typeof queue)[number]) => {
    queue.push(x)
    wake?.()
  }
  const iterable: AsyncIterable<SubscribeEventsResponse> = {
    async *[Symbol.asyncIterator]() {
      for (;;) {
        while (!queue.length) await new Promise<void>((r) => (wake = r))
        const x = queue.shift()!
        if (x.err) throw x.err
        if (x.done) return
        yield x.v!
      }
    },
  }
  return { iterable, send: (v: SubscribeEventsResponse) => push({ v }), fail: (err: unknown) => push({ err }), close: () => push({ done: true }) }
}

function setup(opts: { hello?: () => Promise<ReturnType<typeof hello>> } = {}) {
  const streams: ReturnType<typeof pipe>[] = []
  const subscribedFrom: Array<unknown> = []
  const states: ConnState[] = []
  const frames: Frame[] = []
  const calls = { hello: 0 }
  let helloImpl = opts.hello ?? (async () => hello())
  const client: ConnClient = {
    hello: async () => {
      calls.hello++
      return helloImpl()
    },
    subscribeEvents: (req) => {
      expect(req.activity).toBe(true) // the dock shows the tool an agent runs now
      subscribedFrom.push(req.cursor)
      const p = pipe()
      streams.push(p)
      return p.iterable
    },
  }
  let cursor: { generation: string; seq: bigint } | undefined
  const conn = new HostConnection({
    client,
    cursor: () => (cursor ? { $typeName: 'agentmesh.v1.Cursor', ...cursor } : undefined),
    onState: (s) => states.push(s),
    onFrame: (f) => frames.push(f),
    log: () => {},
    backoffBaseMs: 1000,
    backoffMaxMs: 8000,
    heartbeatMs: 15_000,
    helloTimeoutMs: 5000,
    timers: {
      setTimeout: (fn, ms) => setTimeout(fn, ms),
      clearTimeout: (id) => clearTimeout(id as never),
      setInterval: (fn, ms) => setInterval(fn, ms),
      clearInterval: (id) => clearInterval(id as never),
      now: () => Date.now(),
      random: () => 1,
    },
  })
  return { conn, streams, states, frames, calls, subscribedFrom, setHello: (f: typeof helloImpl) => (helloImpl = f), setCursor: (c: typeof cursor) => (cursor = c) }
}

const last = (s: ConnState[]) => s[s.length - 1]
const flush = () => vi.advanceTimersByTimeAsync(0)

beforeEach(() => vi.useFakeTimers())
afterEach(() => vi.useRealTimers())

describe('HostConnection', () => {
  it('is ready only once hello answers, and follows the event stream', async () => {
    const t = setup()
    t.conn.start()
    expect(last(t.states)?.kind).toBe('connecting')
    await flush()
    expect(last(t.states)?.kind).toBe('ready')
    t.streams[0]!.send(snap())
    await flush()
    expect(t.frames.map((f) => f.case)).toEqual(['snapshot'])
    t.conn.stop()
  })

  it('stays connecting while hello is unanswered, and gives up on it after the timeout', async () => {
    const t = setup({ hello: () => new Promise(() => {}) })
    t.conn.start()
    await vi.advanceTimersByTimeAsync(4000)
    expect(last(t.states)?.kind).toBe('connecting')
    t.conn.stop()
  })

  it('backs off exponentially, and resets only after the first heartbeat of a connection is acknowledged', async () => {
    const t = setup()
    // The host accepts hello and then drops the stream, four times: the waits keep growing (1, 2, 4, 8 s).
    const waits: number[] = []
    t.conn.start()
    for (let i = 0; i < 4; i++) {
      await flush()
      expect(last(t.states)?.kind).toBe('ready')
      t.streams[i]!.fail(new ConnectError('gone', Code.Unavailable))
      await flush()
      const s = last(t.states)
      if (s?.kind !== 'reconnecting') throw new Error('not reconnecting')
      waits.push(s.retryAt - Date.now())
      await vi.advanceTimersByTimeAsync(s.retryAt - Date.now())
    }
    expect(waits).toEqual([1000, 2000, 4000, 8000])
    // Now one connection lives through a heartbeat: the next drop waits the base again.
    await vi.advanceTimersByTimeAsync(15_000)
    t.streams[4]!.fail(new ConnectError('gone', Code.Unavailable))
    await flush()
    const s = last(t.states)
    if (s?.kind !== 'reconnecting') throw new Error('not reconnecting')
    expect(s.retryAt - Date.now()).toBe(1000)
    t.conn.stop()
  })

  it('resumes the stream from the last cursor after a close', async () => {
    const t = setup()
    t.conn.start()
    await flush()
    t.setCursor({ generation: 'g', seq: 41n })
    t.streams[0]!.close()
    await flush()
    await vi.advanceTimersByTimeAsync(1000)
    expect(t.subscribedFrom).toEqual([undefined, expect.objectContaining({ generation: 'g', seq: 41n })])
    t.conn.stop()
  })

  it('reconnects after an End frame instead of stopping', async () => {
    const t = setup()
    t.conn.start()
    await flush()
    t.streams[0]!.send(create(SubscribeEventsResponseSchema, { frame: { case: 'end', value: { reason: 'shutting down' } } }))
    await flush()
    expect(last(t.states)?.kind).toBe('reconnecting')
    await vi.advanceTimersByTimeAsync(1000)
    expect(last(t.states)?.kind).toBe('ready')
    t.conn.stop()
  })

  it('drops the connection when a heartbeat fails', async () => {
    const t = setup()
    t.conn.start()
    await flush()
    t.setHello(async () => {
      throw new ConnectError('down', Code.Unavailable)
    })
    await vi.advanceTimersByTimeAsync(15_000)
    expect(last(t.states)?.kind).toBe('reconnecting')
    t.conn.stop()
  })

  it('stops retrying on a refused token, and starts again on request', async () => {
    const t = setup({
      hello: async () => {
        throw new ConnectError('bad token', Code.Unauthenticated)
      },
    })
    t.conn.start()
    await flush()
    expect(last(t.states)).toMatchObject({ kind: 'stopped', reason: 'auth' })
    const before = t.calls.hello
    await vi.advanceTimersByTimeAsync(60_000)
    expect(t.calls.hello).toBe(before)
    t.setHello(async () => hello())
    t.conn.retryNow()
    await flush()
    expect(last(t.states)?.kind).toBe('ready')
    t.conn.stop()
  })

  it('stops on a protocol mismatch, saying so', async () => {
    const t = setup({
      hello: async () => {
        throw new ConnectError('protocol 2 is not this daemon\'s 1: upgrade the client', Code.FailedPrecondition)
      },
    })
    t.conn.start()
    await flush()
    expect(last(t.states)).toMatchObject({ kind: 'stopped', reason: 'incompatible' })
  })

  it('retryNow leaves a healthy connection alone', async () => {
    const t = setup()
    t.conn.start()
    await flush()
    t.conn.retryNow()
    await flush()
    expect(t.streams).toHaveLength(1)
    expect(t.calls.hello).toBe(1)
    t.conn.stop()
  })

  it('ignores a late answer of an older attempt', async () => {
    let release!: (h: ReturnType<typeof hello>) => void
    const t = setup({ hello: () => new Promise((r) => (release = r)) })
    t.conn.start()
    await flush()
    t.setHello(async () => hello('i2'))
    t.conn.retryNow() // a second attempt, answered at once
    await flush()
    const ready = last(t.states)
    expect(ready).toMatchObject({ kind: 'ready' })
    release(hello('stale'))
    await flush()
    expect(last(t.states)).toBe(ready)
    expect(t.streams).toHaveLength(1)
    t.conn.stop()
  })
})
