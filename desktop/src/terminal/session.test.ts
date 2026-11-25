import { create, fromBinary, toBinary, type MessageInitShape } from '@bufbuild/protobuf'
import { Code, ConnectError } from '@connectrpc/connect'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { SocketEvent } from '../../shared/api'
import { AttachMode, TermFrameSchema, type TermFrame } from '../gen/agentmesh/term/v1/term_pb'
import { TerminalSession, type TermState } from './session'

const bytes = (s: string) => new TextEncoder().encode(s)
const text = (b: Uint8Array) => new TextDecoder().decode(b)

class FakeSocket {
  sent: TermFrame[] = []
  closed = false
  constructor(
    readonly path: string,
    readonly emit: (e: SocketEvent) => void,
  ) {}
  send(data: Uint8Array) {
    this.sent.push(fromBinary(TermFrameSchema, data))
  }
  close() {
    this.closed = true
  }
  /** The host's side. */
  open() {
    this.emit({ type: 'open' })
  }
  host(frame: MessageInitShape<typeof TermFrameSchema>['frame']) {
    this.emit({ type: 'message', data: toBinary(TermFrameSchema, create(TermFrameSchema, { frame })) })
  }
  drop(code: number, reason = '') {
    this.emit({ type: 'close', code, reason })
  }
}

function setup(opts: { input?: boolean; ticket?: () => Promise<{ path: string; input: boolean }> } = {}) {
  const sockets: FakeSocket[] = []
  const states: TermState[] = []
  const written: string[] = []
  let resets = 0
  let tickets = 0
  let size = { cols: 100, rows: 30 }
  const session = new TerminalSession({
    ticket:
      opts.ticket ??
      (async () => {
        tickets++
        return { path: `/v1/term/a?ticket=t${tickets}`, input: opts.input ?? true }
      }),
    socket: (path, emit) => {
      const s = new FakeSocket(path, emit)
      sockets.push(s)
      return s
    },
    sink: { write: (d) => written.push(text(d)), reset: () => resets++, size: () => size },
    onState: (s) => states.push(s),
    backoffBaseMs: 1000,
    backoffMaxMs: 8000,
    timers: { setTimeout: (fn, ms) => setTimeout(fn, ms), clearTimeout: (id) => clearTimeout(id as never), random: () => 1 },
  })
  const last = () => states[states.length - 1]
  const attach = (s: FakeSocket, o: { mode?: AttachMode; input?: boolean; reanchored?: boolean; epoch?: bigint; offset?: bigint } = {}) => {
    s.host({ case: 'attached', value: { terminalId: 'x', mode: o.mode ?? AttachMode.REPAINT, input: o.input ?? true, reanchored: o.reanchored ?? false, epoch: o.epoch ?? 1n, offset: o.offset ?? 0n, cols: 100, rows: 30 } })
  }
  return { session, setSize: (cols: number, rows: number) => (size = { cols, rows }), sockets, states, written, resets: () => resets, last, attach, tickets: () => tickets }
}

const flush = () => vi.advanceTimersByTimeAsync(0)
const done = (s: FakeSocket, offset = 0n) => s.host({ case: 'replayDone', value: { offset } })
const out = (s: FakeSocket, offset: bigint, data: string, epoch = 1n, replay = false) => s.host({ case: 'output', value: { epoch, offset, data: bytes(data), replay } })

beforeEach(() => vi.useFakeTimers())
afterEach(() => vi.useRealTimers())

describe('TerminalSession', () => {
  it('attaches for a repaint at our size, shows it, and sends Ready after the replay', async () => {
    const t = setup()
    t.session.start()
    await flush()
    const s = t.sockets[0]!
    expect(s.path).toBe('/v1/term/a?ticket=t1')
    s.open()
    expect(s.sent[0]!.frame).toMatchObject({ case: 'attach', value: { mode: AttachMode.REPAINT, cols: 100, rows: 30 } })
    t.attach(s)
    s.host({ case: 'screen', value: { epoch: 1n, offset: 5n, data: bytes('SCREEN') } })
    expect(t.last()).toEqual({ kind: 'replaying' })
    expect(s.sent.map((f) => f.frame.case)).toEqual(['attach'])
    done(s, 5n)
    expect(t.resets()).toBe(1)
    expect(t.written).toEqual(['SCREEN'])
    expect(s.sent.map((f) => f.frame.case)).toEqual(['attach', 'ready'])
    expect(t.last()).toEqual({ kind: 'live', input: true })
  })

  it('drops input until the replay is done, and always when the host gave no input right', async () => {
    const t = setup()
    t.session.start()
    await flush()
    const s = t.sockets[0]!
    s.open()
    t.attach(s)
    t.session.input(bytes('early'))
    done(s)
    t.session.input(bytes('late'))
    expect(s.sent.filter((f) => f.frame.case === 'input').map((f) => text((f.frame.value as { data: Uint8Array }).data))).toEqual(['late'])

    const v = setup({ input: false })
    v.session.start()
    await flush()
    const vs = v.sockets[0]!
    vs.open()
    v.attach(vs, { input: false })
    done(vs)
    v.session.input(bytes('x'))
    v.session.resize(50, 20)
    expect(vs.sent.map((f) => f.frame.case)).toEqual(['attach', 'ready'])
    expect(v.last()).toEqual({ kind: 'live', input: false })
  })

  it('sends the size again after the replay when the view changed while attaching', async () => {
    const t = setup()
    t.session.start()
    await flush()
    const s = t.sockets[0]!
    s.open()
    t.attach(s)
    t.setSize(120, 40)
    done(s)
    expect(s.sent.at(-1)!.frame).toMatchObject({ case: 'resize', value: { cols: 120, rows: 40 } })
  })

  it('resizes only once live', async () => {
    const t = setup()
    t.session.start()
    await flush()
    const s = t.sockets[0]!
    s.open()
    t.attach(s)
    t.session.resize(80, 24)
    done(s)
    t.session.resize(80, 24)
    expect(s.sent.filter((f) => f.frame.case === 'resize')).toHaveLength(1)
  })

  it('after a drop, asks for exactly the bytes after the last one written, and does not clear the screen', async () => {
    const t = setup()
    t.session.start()
    await flush()
    const a = t.sockets[0]!
    a.open()
    t.attach(a)
    a.host({ case: 'screen', value: { epoch: 1n, offset: 10n, data: bytes('S') } })
    done(a, 10n)
    out(a, 10n, 'abc')
    out(a, 13n, 'de', 2n)
    a.drop(1006)
    expect(t.last()).toMatchObject({ kind: 'reconnecting' })
    await vi.advanceTimersByTimeAsync(1000)
    const b = t.sockets[1]!
    expect(b.path).toBe('/v1/term/a?ticket=t2')
    b.open()
    expect(b.sent[0]!.frame).toMatchObject({ case: 'attach', value: { mode: AttachMode.EXACT, epoch: 2n, offset: 15n } })
    t.attach(b, { mode: AttachMode.EXACT, epoch: 2n, offset: 15n })
    out(b, 15n, 'fg', 2n, true)
    done(b, 17n)
    expect(t.written).toEqual(['S', 'abc', 'de', 'fg'])
    expect(t.resets()).toBe(1) // the first repaint only
    expect(t.last()).toEqual({ kind: 'live', input: true })
  })

  it('keeps going when the epoch moves in the middle of the stream (a resize), offsets being continuous', async () => {
    const t = setup()
    t.session.start()
    await flush()
    const a = t.sockets[0]!
    a.open()
    t.attach(a)
    done(a)
    out(a, 0n, 'ab', 1n)
    out(a, 2n, 'cd', 2n)
    expect(t.written).toEqual(['ab', 'cd'])
    expect(a.closed).toBe(false)
    a.drop(1006)
    await vi.advanceTimersByTimeAsync(1000)
    t.sockets[1]!.open()
    expect(t.sockets[1]!.sent[0]!.frame).toMatchObject({ value: { mode: AttachMode.EXACT, epoch: 2n, offset: 4n } })
  })

  it('clears the screen when the host had to repaint instead of replaying exactly', async () => {
    const t = setup()
    t.session.start()
    await flush()
    const a = t.sockets[0]!
    a.open()
    t.attach(a)
    a.host({ case: 'screen', value: { epoch: 1n, offset: 0n, data: bytes('FIRST') } })
    done(a)
    out(a, 0n, 'x')
    a.drop(1006)
    await vi.advanceTimersByTimeAsync(1000)
    const b = t.sockets[1]!
    b.open()
    expect(b.sent[0]!.frame).toMatchObject({ value: { mode: AttachMode.EXACT } })
    t.attach(b, { mode: AttachMode.REPAINT, reanchored: true, epoch: 3n })
    b.host({ case: 'screen', value: { epoch: 3n, offset: 99n, data: bytes('FRESH') } })
    done(b, 99n)
    expect(t.resets()).toBe(2)
    expect(t.written.at(-1)).toBe('FRESH')
  })

  it('backs off after drops, resets once a replay finishes, and reattaches at once when dropped for being slow', async () => {
    const t = setup()
    t.session.start()
    await flush()
    t.sockets[0]!.open()
    t.sockets[0]!.drop(1006)
    expect(t.last()).toMatchObject({ kind: 'reconnecting' })
    await vi.advanceTimersByTimeAsync(999)
    expect(t.sockets).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(t.sockets).toHaveLength(2)
    t.sockets[1]!.open()
    t.sockets[1]!.drop(1006)
    await vi.advanceTimersByTimeAsync(1999)
    expect(t.sockets).toHaveLength(2) // the second wait is 2 s
    await vi.advanceTimersByTimeAsync(1)
    const c = t.sockets[2]!
    c.open()
    t.attach(c)
    done(c)
    c.drop(4001, 'slow')
    await vi.advanceTimersByTimeAsync(200)
    expect(t.sockets).toHaveLength(4)
    t.sockets[3]!.open()
    t.attach(t.sockets[3]!)
    done(t.sockets[3]!)
    t.sockets[3]!.drop(1006)
    await vi.advanceTimersByTimeAsync(1000) // back to the base wait after a good attach
    expect(t.sockets).toHaveLength(5)
  })

  it('ends on an exit, and does not reconnect', async () => {
    const t = setup()
    t.session.start()
    await flush()
    const s = t.sockets[0]!
    s.open()
    t.attach(s)
    done(s)
    s.host({ case: 'exited', value: { exitCode: 7, signal: 0 } })
    s.drop(4000, 'exited')
    await vi.advanceTimersByTimeAsync(60_000)
    expect(t.last()).toEqual({ kind: 'exited', exitCode: 7, signal: 0 })
    expect(t.sockets).toHaveLength(1)
  })

  it('stops on a bad request, and on a ticket the host refuses; retries when the daemon is just down', async () => {
    const bad = setup()
    bad.session.start()
    await flush()
    bad.sockets[0]!.open()
    bad.sockets[0]!.drop(4003, 'the first frame must be Attach')
    expect(bad.last()).toEqual({ kind: 'closed', message: 'the first frame must be Attach' })

    const refused = setup({ ticket: async () => { throw new ConnectError('agent has no terminal', Code.FailedPrecondition) } })
    refused.session.start()
    await flush()
    expect(refused.last()).toEqual({ kind: 'closed', message: 'agent has no terminal' })

    let n = 0
    const down = setup({ ticket: async () => { if (n++ === 0) throw new ConnectError('down', Code.Unavailable); return { path: '/v1/term/a?ticket=z', input: true } } })
    down.session.start()
    await flush()
    expect(down.last()).toMatchObject({ kind: 'reconnecting', error: 'down' })
    await vi.advanceTimersByTimeAsync(1000)
    expect(down.sockets).toHaveLength(1)
  })

  it('starts over with a repaint when output skips bytes, and trims what it already wrote', async () => {
    const t = setup()
    t.session.start()
    await flush()
    const a = t.sockets[0]!
    a.open()
    t.attach(a)
    done(a)
    out(a, 0n, 'abcd')
    out(a, 2n, 'cdef') // overlaps: only "ef" is new
    expect(t.written).toEqual(['abcd', 'ef'])
    out(a, 10n, 'zz') // a gap
    expect(a.closed).toBe(true)
    await flush()
    const b = t.sockets[1]!
    b.open()
    expect(b.sent[0]!.frame).toMatchObject({ case: 'attach', value: { mode: AttachMode.REPAINT } })
    expect(t.written).not.toContain('zz')
  })

  it('ignores the late events of a socket it gave up on, and dispose closes the stream', async () => {
    const t = setup()
    t.session.start()
    await flush()
    const a = t.sockets[0]!
    a.open()
    a.drop(1006)
    await vi.advanceTimersByTimeAsync(1000)
    const before = t.written.length
    a.host({ case: 'output', value: { epoch: 1n, offset: 0n, data: bytes('late'), replay: false } })
    expect(t.written).toHaveLength(before)
    const b = t.sockets[1]!
    t.session.dispose()
    expect(b.closed).toBe(true)
    await vi.advanceTimersByTimeAsync(60_000)
    expect(t.sockets).toHaveLength(2)
  })
})
