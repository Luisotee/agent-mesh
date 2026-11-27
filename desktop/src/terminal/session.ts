import { create, fromBinary, toBinary, type MessageInitShape } from '@bufbuild/protobuf'
import { Code, ConnectError } from '@connectrpc/connect'
import type { SocketEvent, SocketHandle } from '../../shared/api'
import type { Timers } from '../connection'
import { AttachMode, TermFrameSchema, type Attached } from '../gen/agentmesh/term/v1/term_pb'

/** The WebSocket close codes of the terminal stream (daemon/internal/termhost/ws.go). */
export const CLOSE_EXITED = 4000
export const CLOSE_SLOW = 4001
export const CLOSE_SHUTDOWN = 4002
export const CLOSE_BAD_REQUEST = 4003

export type TermState =
  | { kind: 'connecting' }
  /** Attached; the host is sending what happened while we were away. Input is off. */
  | { kind: 'replaying' }
  | { kind: 'live'; input: boolean }
  | { kind: 'reconnecting'; error: string }
  | { kind: 'exited'; exitCode: number; signal: number }
  /** Won't try again: the host refused, or the terminal doesn't exist. */
  | { kind: 'closed'; message: string }

export interface Ticket {
  path: string
  input: boolean
}

/** What a session writes to: xterm in the app, a recorder in a test. */
export interface TermSink {
  write(data: Uint8Array): void
  /** Clear the screen and scrollback before a repaint. */
  reset(): void
  size(): { cols: number; rows: number }
}

export interface SessionOptions {
  /** `OpenTerminal`: a one-time ticket for a stream; `input` asks for the right to type. */
  ticket(): Promise<Ticket>
  socket(path: string, onEvent: (e: SocketEvent) => void): SocketHandle
  sink: TermSink
  onState(s: TermState): void
  /** The size the host's terminal has (at attach, and when it changes): the smallest of the attached clients'. */
  onHostSize?(cols: number, rows: number): void
  timers?: Pick<Timers, 'setTimeout' | 'clearTimeout' | 'random'>
  backoffBaseMs?: number
  backoffMaxMs?: number
}

const realTimers = {
  setTimeout: (fn: () => void, ms: number) => setTimeout(fn, ms) as unknown,
  clearTimeout: (id: unknown) => clearTimeout(id as ReturnType<typeof setTimeout>),
  random: () => Math.random(),
}

/**
 * One terminal's stream from the client's side (docs/apps.md, "Protocols and lifecycle"):
 *  - the first attach asks for a repaint; after a drop it asks for exactly the bytes after the last one it
 *    wrote (epoch and offset), and clears the screen if the host had to repaint instead;
 *  - input is dropped until the replay is done and `Ready` is sent, so what a replayed query makes the terminal
 *    answer never reaches the program;
 *  - a client dropped for being slow (4001) reattaches at once; any other close backs off; an exit, a refusal
 *    or the terminal host shutting down (4002: its programs go with it, so there is nothing to reattach to) ends the
 *    session;
 *  - a late event of an old socket is ignored (generation fences).
 */
export class TerminalSession {
  private readonly t: NonNullable<SessionOptions['timers']>
  private gen = 0
  private socket?: SocketHandle
  private attempt = 0
  private retryTimer?: unknown
  private attached?: Attached
  private ready = false
  private finished = false
  private disposed = false
  /** Where we are in the output: the epoch it is in, and the offset of the next byte we expect. */
  private epoch = 0n
  private next?: bigint
  /** The size we attached with. */
  private attachSize = { cols: 0, rows: 0 }

  constructor(private readonly o: SessionOptions) {
    this.t = o.timers ?? realTimers
  }

  start(): void {
    void this.connect(true)
  }

  /** Bytes typed by the user. Dropped unless the replay is done and the host granted input. */
  input(data: Uint8Array): void {
    if (!this.ready || !this.attached?.input) return
    this.send({ case: 'input', value: { data } })
  }

  /** The terminal's size changed. The host sizes the terminal to the smallest client; only a typing client counts. */
  resize(cols: number, rows: number): void {
    if (!this.ready || !this.attached?.input) return
    this.send({ case: 'resize', value: { cols, rows } })
  }

  dispose(): void {
    this.disposed = true
    this.gen++
    if (this.retryTimer !== undefined) this.t.clearTimeout(this.retryTimer)
    this.socket?.close()
    this.socket = undefined
  }

  private send(frame: MessageInitShape<typeof TermFrameSchema>['frame']): void {
    this.socket?.send(toBinary(TermFrameSchema, create(TermFrameSchema, { frame })))
  }

  private async connect(first: boolean): Promise<void> {
    const gen = ++this.gen
    this.ready = false
    this.attached = undefined
    if (first) this.o.onState({ kind: 'connecting' })
    let ticket: Ticket
    try {
      ticket = await this.o.ticket()
    } catch (e) {
      if (gen !== this.gen) return
      const err = ConnectError.from(e)
      const final = [Code.Unauthenticated, Code.PermissionDenied, Code.FailedPrecondition, Code.NotFound].includes(err.code)
      if (final) this.close(err.rawMessage)
      else this.retryLater(err.rawMessage)
      return
    }
    if (gen !== this.gen) return
    this.socket = this.o.socket(ticket.path, (e) => this.onSocket(gen, e))
  }

  private onSocket(gen: number, e: SocketEvent): void {
    if (gen !== this.gen) return
    switch (e.type) {
      case 'open': {
        const { cols, rows } = this.o.sink.size()
        this.attachSize = { cols, rows }
        const resume = this.next !== undefined
        this.send({
          case: 'attach',
          value: {
            mode: resume ? AttachMode.EXACT : AttachMode.REPAINT,
            epoch: this.epoch,
            offset: this.next ?? 0n,
            cols,
            rows,
          },
        })
        return
      }
      case 'message':
        this.onFrame(gen, e.data)
        return
      case 'close':
        this.dropped(gen, e.code, e.reason)
        return
      case 'error':
        // An unknown host won't get better; anything else (a spent ticket, a daemon restarting) might.
        if (e.code === 'unknown-host') this.close(e.message)
        else this.retryLater(e.message)
    }
  }

  private onFrame(gen: number, data: Uint8Array): void {
    let f
    try {
      f = fromBinary(TermFrameSchema, data)
    } catch {
      // A frame we can't read means we can't trust our place in the stream: start over with a repaint.
      this.resync(gen)
      return
    }
    const frame = f.frame
    switch (frame.case) {
      case 'attached':
        this.attached = frame.value
        this.o.onHostSize?.(frame.value.cols, frame.value.rows)
        this.o.onState({ kind: 'replaying' })
        return
      case 'output': {
        const { epoch, offset, data: bytes } = frame.value
        let chunk = bytes
        if (this.next !== undefined && offset !== this.next) {
          if (offset > this.next) {
            // Bytes are missing: our screen is wrong. Start over rather than draw it.
            this.resync(gen)
            return
          }
          // Part of this chunk we already wrote.
          const skip = Number(this.next - offset)
          if (skip >= bytes.length) return
          chunk = bytes.subarray(skip)
        }
        this.epoch = epoch
        this.next = offset + BigInt(bytes.length)
        this.o.sink.write(chunk)
        return
      }
      case 'resized':
        // Another client (or this one) changed the size: the output from here on is in the new epoch, and a reattach asks for it.
        this.epoch = frame.value.epoch
        this.o.onHostSize?.(frame.value.cols, frame.value.rows)
        return
      case 'screen': {
        // A repaint replaces what we show; an exact replay doesn't send one.
        if (this.attached?.mode === AttachMode.REPAINT) this.o.sink.reset()
        this.epoch = frame.value.epoch
        this.next = frame.value.offset
        this.o.sink.write(frame.value.data)
        return
      }
      case 'replayDone':
        this.ready = true
        this.attempt = 0
        this.send({ case: 'ready', value: {} })
        {
          // The view may have been resized while we attached, and a resize before Ready is dropped. A viewer's size was set at attach and
          // the host takes no resize from it, so its size is not even read (the sink notes what it is asked).
          if (this.attached?.input) {
            const { cols, rows } = this.o.sink.size()
            if (cols !== this.attachSize.cols || rows !== this.attachSize.rows) this.send({ case: 'resize', value: { cols, rows } })
          }
        }
        this.o.onState({ kind: 'live', input: !!this.attached?.input })
        return
      case 'exited':
        this.finished = true
        this.o.onState({ kind: 'exited', exitCode: frame.value.exitCode, signal: frame.value.signal })
        return
    }
  }

  /** Forget our place and attach again for a full repaint. */
  private resync(gen: number): void {
    if (gen !== this.gen) return
    this.next = undefined
    this.socket?.close()
    this.socket = undefined
    void this.connect(false)
  }

  private dropped(gen: number, code: number, reason: string): void {
    if (this.finished || code === CLOSE_EXITED) {
      this.finished = true
      return
    }
    if (code === CLOSE_BAD_REQUEST) {
      this.close(reason || 'the host refused the stream')
      return
    }
    if (code === CLOSE_SHUTDOWN) {
      this.close('the terminal host shut down')
      return
    }
    if (code === CLOSE_SLOW) {
      // We fell behind: reattach by offset right away (a short pause keeps a stuck client from spinning).
      this.gen++
      this.o.onState({ kind: 'reconnecting', error: 'the terminal was too busy for this client' })
      this.retryTimer = this.t.setTimeout(() => void this.connect(false), 200)
      return
    }
    this.retryLater(reason || `the stream closed (${code})`, gen)
  }

  private retryLater(message: string, gen = this.gen): void {
    if (gen !== this.gen || this.disposed) return
    this.gen++
    this.socket = undefined
    const base = this.o.backoffBaseMs ?? 500
    const max = this.o.backoffMaxMs ?? 8000
    const wait = Math.min(max, base * 2 ** this.attempt) * (0.5 + this.t.random() / 2)
    this.attempt++
    this.o.onState({ kind: 'reconnecting', error: message })
    this.retryTimer = this.t.setTimeout(() => void this.connect(false), wait)
  }

  private close(message: string): void {
    this.gen++
    this.finished = true
    this.o.onState({ kind: 'closed', message })
  }
}
