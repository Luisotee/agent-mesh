import { Code, ConnectError } from '@connectrpc/connect'
import { createConnectTransport } from '@connectrpc/connect-web'
import type { BridgeErrorCode, BridgeEvent, DesktopApi } from '../shared/api'

/**
 * A `fetch` that goes through the host bridge, so the generated ConnectRPC clients run unchanged in a
 * renderer that can't reach the host itself. Streaming works: the response body is a stream fed by the
 * bridge's chunks, and aborting the request closes the host connection.
 */
export function bridgeFetch(api: Pick<DesktopApi, 'request'>, hostId: string): typeof fetch {
  return async (input, init) => {
    const req = new Request(input, init)
    const url = new URL(req.url)
    const headers: Record<string, string> = {}
    req.headers.forEach((v, k) => (headers[k] = v))
    const body = req.body ? new Uint8Array(await req.arrayBuffer()) : undefined
    const signal = init?.signal ?? req.signal

    return new Promise<Response>((resolve, reject) => {
      let controller!: ReadableStreamDefaultController<Uint8Array>
      let headed = false
      // Set once the body is closed, errored or cancelled: an event still in flight must not touch it.
      let closed = false
      const stream = new ReadableStream<Uint8Array>({
        start: (c) => (controller = c),
        cancel: () => {
          closed = true
          abort()
        },
      })
      const onEvent = (e: BridgeEvent) => {
        if (closed) return
        switch (e.type) {
          case 'head':
            headed = true
            resolve(new Response(stream, { status: e.status, headers: e.headers }))
            break
          case 'data':
            controller.enqueue(e.chunk)
            break
          case 'end':
            closed = true
            controller.close()
            break
          case 'error': {
            closed = true
            const err = bridgeError(e.code, e.message)
            if (headed) controller.error(err)
            else reject(err)
          }
        }
      }
      const abort = api.request(hostId, { method: req.method, path: url.pathname, headers, body }, onEvent)
      const onAbort = () => {
        if (closed) return
        closed = true
        abort()
        // Keep why it was aborted: a timeout is not a cancel.
        const reason: unknown = signal?.reason
        const err = reason instanceof ConnectError ? reason : new ConnectError('the request was cancelled', Code.Canceled)
        if (headed) controller.error(err)
        else reject(err)
      }
      if (signal?.aborted) onAbort()
      else signal?.addEventListener('abort', onAbort, { once: true })
    })
  }
}

/**
 * The error a failed bridge request becomes. A host with no token yet (meshd never ran there, or is about to)
 * is unreachable like any other and retried with a backoff; only a request the bridge refuses isn't.
 */
export function bridgeError(code: BridgeErrorCode, message: string): ConnectError {
  switch (code) {
    case 'unknown-host':
    case 'refused':
      return new ConnectError(message, Code.FailedPrecondition)
    default:
      return new ConnectError(message, Code.Unavailable)
  }
}

export function hostTransport(api: Pick<DesktopApi, 'request'>, hostId: string) {
  return createConnectTransport({
    baseUrl: 'http://meshd',
    useBinaryFormat: true,
    fetch: bridgeFetch(api, hostId),
  })
}
