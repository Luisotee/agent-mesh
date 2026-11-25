import { Code, ConnectError } from '@connectrpc/connect'
import type { Cursor } from './gen/agentmesh/v1/types_pb'
import type { HelloResponse, SubscribeEventsResponse } from './gen/agentmesh/v1/mesh_pb'

/** The API's major version this app speaks (the daemon's `ProtocolVersion`). */
export const PROTOCOL_VERSION = 1

export type ConnState =
  | { kind: 'connecting' }
  | { kind: 'ready'; hello: HelloResponse }
  /** Not connected; trying again at `retryAt` (ms since the epoch). */
  | { kind: 'reconnecting'; attempt: number; retryAt: number; error: string }
  /** Won't retry until asked: the host refused the token or speaks another protocol, or the config is wrong. */
  | { kind: 'stopped'; reason: 'auth' | 'incompatible' | 'config'; message: string }

export type Frame = NonNullable<SubscribeEventsResponse['frame']>

/** The two calls a connection makes; the generated client satisfies it, and so does a test's fake. */
export interface ConnClient {
  hello(req: { client: string; protocolVersion: number }, opts?: { signal?: AbortSignal }): Promise<HelloResponse>
  subscribeEvents(req: { cursor?: Cursor }, opts?: { signal?: AbortSignal }): AsyncIterable<SubscribeEventsResponse>
}

export interface Timers {
  setTimeout(fn: () => void, ms: number): unknown
  clearTimeout(id: unknown): void
  setInterval(fn: () => void, ms: number): unknown
  clearInterval(id: unknown): void
  now(): number
  random(): number
}

export const realTimers: Timers = {
  setTimeout: (fn, ms) => setTimeout(fn, ms),
  clearTimeout: (id) => clearTimeout(id as ReturnType<typeof setTimeout>),
  setInterval: (fn, ms) => setInterval(fn, ms),
  clearInterval: (id) => clearInterval(id as ReturnType<typeof setInterval>),
  now: () => Date.now(),
  random: () => Math.random(),
}

export interface ConnOptions {
  client: ConnClient
  /** Where to resume the event stream from; unset starts from a snapshot. */
  cursor(): Cursor | undefined
  onState(s: ConnState): void
  /** Each answer to `Hello` (the first and every heartbeat): the host's clock reading is in it. */
  onHello?(h: HelloResponse): void
  onFrame(f: Frame): void
  log(line: string): void
  timers?: Timers
  heartbeatMs?: number
  helloTimeoutMs?: number
  backoffBaseMs?: number
  backoffMaxMs?: number
}

/**
 * One host's connection (docs/apps.md, "Connections"):
 *  - ready only when `Hello` answers, not when a socket opens;
 *  - "online" is a heartbeat (`Hello` on a timer) that keeps answering; one that doesn't drops the connection;
 *  - the backoff resets only after the first heartbeat of a connection is acknowledged, so a host that accepts
 *    and then drops us keeps backing off;
 *  - every attempt has a generation: a late answer or a stream frame of an older one is ignored;
 *  - a refused token stops the retries; any other close, an `End` frame included, resumes from the cursor.
 */
export class HostConnection {
  private readonly t: Timers
  private readonly heartbeatMs: number
  private readonly helloTimeoutMs: number
  private readonly baseMs: number
  private readonly maxMs: number
  private gen = 0
  private attempt = 0
  private running = false
  private abort?: AbortController
  private retryTimer?: unknown
  private beatTimer?: unknown
  private beaten = false
  private ready = false

  constructor(private readonly o: ConnOptions) {
    this.t = o.timers ?? realTimers
    this.heartbeatMs = o.heartbeatMs ?? 15_000
    this.helloTimeoutMs = o.helloTimeoutMs ?? 10_000
    this.baseMs = o.backoffBaseMs ?? 500
    this.maxMs = o.backoffMaxMs ?? 15_000
  }

  start(): void {
    if (this.running) return
    this.running = true
    void this.connect()
  }

  stop(): void {
    this.running = false
    this.ready = false
    this.attempt = 0
    this.gen++
    this.teardown()
  }

  /** Try again now: after a stop for the token or config, or to skip a backoff wait. */
  retryNow(): void {
    if (!this.running || this.ready) return
    this.attempt = 0
    void this.connect()
  }

  private teardown(): void {
    this.ready = false
    this.abort?.abort()
    this.abort = undefined
    if (this.retryTimer !== undefined) this.t.clearTimeout(this.retryTimer)
    this.retryTimer = undefined
    if (this.beatTimer !== undefined) this.t.clearInterval(this.beatTimer)
    this.beatTimer = undefined
  }

  private async connect(): Promise<void> {
    this.teardown()
    const gen = ++this.gen
    const ctl = new AbortController()
    this.abort = ctl
    this.beaten = false
    if (this.attempt === 0) this.o.onState({ kind: 'connecting' })
    let hello: HelloResponse
    try {
      hello = await this.hello(ctl)
    } catch (e) {
      if (gen === this.gen) this.failed(gen, e)
      return
    }
    if (gen !== this.gen) return
    this.o.onHello?.(hello)
    this.o.log(`connected to ${hello.daemonId} (instance ${hello.instanceId}, ${hello.version})`)
    this.ready = true
    this.o.onState({ kind: 'ready', hello })
    this.beatTimer = this.t.setInterval(() => void this.beat(gen, ctl), this.heartbeatMs)
    void this.follow(gen, ctl)
  }

  private async hello(ctl: AbortController): Promise<HelloResponse> {
    const timeout = this.t.setTimeout(() => ctl.abort(new ConnectError('no answer to hello', Code.DeadlineExceeded)), this.helloTimeoutMs)
    try {
      return await this.o.client.hello({ client: 'agent-mesh-desktop', protocolVersion: PROTOCOL_VERSION }, { signal: ctl.signal })
    } finally {
      this.t.clearTimeout(timeout)
    }
  }

  private async beat(gen: number, ctl: AbortController): Promise<void> {
    // A heartbeat uses its own signal: aborting it must not close the event stream unless it fails.
    const own = new AbortController()
    const timeout = this.t.setTimeout(() => own.abort(new ConnectError('no answer to the heartbeat', Code.DeadlineExceeded)), this.helloTimeoutMs)
    try {
      // (Not `onHello?.(await …)`: an absent callback would skip evaluating the call.)
      const hello = await this.o.client.hello({ client: 'agent-mesh-desktop', protocolVersion: PROTOCOL_VERSION }, { signal: own.signal })
      this.o.onHello?.(hello)
    } catch (e) {
      this.t.clearTimeout(timeout)
      if (gen === this.gen) {
        ctl.abort()
        this.failed(gen, e)
      }
      return
    }
    this.t.clearTimeout(timeout)
    if (gen === this.gen && !this.beaten) {
      this.beaten = true
      this.attempt = 0
    }
  }

  private async follow(gen: number, ctl: AbortController): Promise<void> {
    try {
      for await (const msg of this.o.client.subscribeEvents({ cursor: this.o.cursor() }, { signal: ctl.signal })) {
        if (gen !== this.gen) return
        const frame = msg.frame
        if (!frame) continue
        if (frame.case === 'end') {
          this.o.log(`the host ended the stream: ${frame.value.reason || 'no reason given'}`)
          break
        }
        this.o.onFrame(frame)
      }
      if (gen === this.gen) this.failed(gen, new Error('the event stream closed'))
    } catch (e) {
      if (gen === this.gen) this.failed(gen, e)
    }
  }

  private failed(gen: number, e: unknown): void {
    if (gen !== this.gen || !this.running) return
    this.teardown()
    const err = ConnectError.from(e)
    const message = err.rawMessage || String(e)
    if (err.code === Code.Unauthenticated || err.code === Code.PermissionDenied) {
      this.stopWith('auth', message)
    } else if (err.code === Code.FailedPrecondition) {
      // From Hello this is a protocol mismatch ("upgrade the client"); from the bridge, a bad host entry.
      this.stopWith(/protocol/i.test(message) ? 'incompatible' : 'config', message)
    } else {
      this.retryLater(message)
    }
  }

  private stopWith(reason: 'auth' | 'incompatible' | 'config', message: string): void {
    this.gen++
    this.o.log(`stopped (${reason}): ${message}`)
    this.o.onState({ kind: 'stopped', reason, message })
  }

  private retryLater(message: string): void {
    this.gen++
    const wait = Math.min(this.maxMs, this.baseMs * 2 ** this.attempt) * (0.5 + this.t.random() / 2)
    this.attempt++
    const retryAt = this.t.now() + wait
    this.o.log(`lost the host (${message}); retrying in ${(wait / 1000).toFixed(1)} s`)
    this.o.onState({ kind: 'reconnecting', attempt: this.attempt, retryAt, error: message })
    this.retryTimer = this.t.setTimeout(() => void this.connect(), wait)
  }
}
