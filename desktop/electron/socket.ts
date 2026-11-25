import path from 'node:path'
import WebSocket from 'ws'
import type { BridgeErrorCode, SocketEvent } from '../shared/api.js'
import { resolve } from './bridge.js'
import type { StoredHost } from './hosts.js'

/**
 * The terminal side of the host bridge. A terminal's stream is a WebSocket the daemon splices to the terminal
 * host after `OpenTerminal` hands out a one-time ticket in the path. As with requests, the renderer can't open it
 * (a browser's WebSocket carries an Origin, which meshd refuses): main opens it, without one, and passes the
 * frames through as bytes. It reads none of them.
 */

/** The only path a socket may open: a terminal's stream with its ticket. */
const TERM_PATH = /^\/v1\/term\/[A-Za-z0-9_-]+\?ticket=[A-Za-z0-9_%.~-]+$/

export function allowedSocketPath(p: unknown): p is string {
  return typeof p === 'string' && p.length < 512 && TERM_PATH.test(p)
}

export interface Socket {
  send(data: Uint8Array): void
  close(): void
}

/** Opens one terminal stream. Events always end with `close` or `error`, once. */
export function openSocket(host: StoredHost | undefined, p: unknown, emit: (e: SocketEvent) => void): Socket {
  let done = false
  let ws: WebSocket | undefined
  let closeRequested = false
  const finish = (e: SocketEvent) => {
    if (done) return
    done = true
    emit(e)
  }
  const fail = (code: BridgeErrorCode, message: string) => finish({ type: 'error', code, message })
  const handle: Socket = {
    send(data) {
      if (ws?.readyState === WebSocket.OPEN && data instanceof Uint8Array) ws.send(data, { binary: true })
    },
    close() {
      closeRequested = true
      done = true
      // A socket still connecting has nothing to close cleanly: drop the handshake.
      if (ws?.readyState === WebSocket.CONNECTING) ws.terminate()
      else ws?.close()
    },
  }
  if (!host) {
    fail('unknown-host', 'no such host')
    return handle
  }
  if (!allowedSocketPath(p)) {
    fail('refused', 'the bridge opens only a terminal stream with its ticket')
    return handle
  }
  void resolve(host).then(
    (t) => {
      if (closeRequested) return
      // The ticket in the path is the credential; the host's token isn't sent. No Origin header is set.
      const url = t.socketPath ? `ws+unix://${path.resolve(t.socketPath)}:${p}` : `ws://${t.host!.includes(':') ? `[${t.host}]` : t.host}:${t.port}${p}`
      ws = new WebSocket(url, { perMessageDeflate: false, maxPayload: 16 << 20 })
      ws.binaryType = 'nodebuffer'
      ws.on('open', () => {
        if (!done) emit({ type: 'open' })
      })
      ws.on('message', (data: Buffer, isBinary: boolean) => {
        if (done || !isBinary) return
        emit({ type: 'message', data: new Uint8Array(data) })
      })
      ws.on('close', (code, reason) => finish({ type: 'close', code, reason: reason.toString() }))
      ws.on('unexpected-response', (_req, res) => {
        res.resume()
        fail(res.statusCode === 401 ? 'refused' : 'failed', `the host answered ${res.statusCode}`)
      })
      ws.on('error', (e: NodeJS.ErrnoException) => {
        if (e.code === 'ENOENT' || e.code === 'ECONNREFUSED' || e.code === 'ECONNRESET') fail('unreachable', e.message)
        else fail('failed', e.message)
      })
    },
    (e: unknown) => fail((e as { code?: BridgeErrorCode }).code ?? 'failed', e instanceof Error ? e.message : String(e)),
  )
  return handle
}
