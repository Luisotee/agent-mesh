import http from 'node:http'
import { readFile } from 'node:fs/promises'
import path from 'node:path'
import type { BridgeErrorCode, BridgeEvent, BridgeRequest } from '../shared/api.js'
import type { StoredHost } from './hosts.js'

/**
 * The host bridge: the renderer's only way to a host. meshd refuses any request a browser made (an Origin
 * header), so the renderer can't dial it, and it must not hold a token. Main forwards the registry's calls,
 * adds the token, and passes the answer back in chunks. It knows nothing of what the calls mean.
 */

/** What the renderer may reach: the registry's unary and streaming calls, nothing else. */
const ALLOWED_PATH = /^\/agentmesh\.v1\.MeshService\/[A-Za-z]+$/

/** The only request headers forwarded; the token and the host header are main's. */
const FORWARDED = /^(content-type|accept|connect-[a-z-]+)$/i

/** The renderer is untrusted: check the shape of a request before reading it. */
export function wellFormed(req: unknown): req is BridgeRequest {
  if (typeof req !== 'object' || req === null) return false
  const r = req as Record<string, unknown>
  if (typeof r.method !== 'string' || typeof r.path !== 'string') return false
  if (typeof r.headers !== 'object' || r.headers === null) return false
  if (!Object.values(r.headers).every((v) => typeof v === 'string')) return false
  return r.body === undefined || r.body instanceof Uint8Array
}

export function allowed(req: BridgeRequest): boolean {
  return req.method === 'POST' && ALLOWED_PATH.test(req.path)
}

interface Target {
  socketPath?: string
  host?: string
  port?: number
  token: string
}

class BridgeError extends Error {
  constructor(
    readonly code: BridgeErrorCode,
    message: string,
  ) {
    super(message)
  }
}

/** Where a host listens and the token to use, read at call time so a restarted daemon or a new token works. */
export async function resolve(h: StoredHost): Promise<Target> {
  if (h.kind === 'tcp') {
    const i = h.address.lastIndexOf(':')
    // An IPv6 literal is bracketed in the address and bare for http.request.
    return { host: h.address.slice(0, i).replace(/^\[(.*)\]$/, '$1'), port: Number(h.address.slice(i + 1)), token: h.token }
  }
  let token: string
  try {
    token = (await readFile(path.join(h.path, 'device.token'), 'utf8')).trim()
  } catch {
    throw new BridgeError('no-token', `no device token in ${h.path}: is meshd started with this state dir?`)
  }
  return { socketPath: path.join(h.path, 'meshd.sock'), token }
}

/** Runs one request; returns an abort. Events always end with `end` or `error`, once. */
export function forward(
  host: StoredHost | undefined,
  req: BridgeRequest,
  emit: (e: BridgeEvent) => void,
): () => void {
  let done = false
  let abort = () => {}
  const finish = (e: BridgeEvent) => {
    if (done) return
    done = true
    emit(e)
  }
  const fail = (code: BridgeErrorCode, message: string) => finish({ type: 'error', code, message })
  if (!host) {
    fail('unknown-host', 'no such host')
    return () => {}
  }
  if (!wellFormed(req)) {
    fail('refused', 'malformed request')
    return () => {}
  }
  if (!allowed(req)) {
    fail('refused', `the bridge forwards only registry calls, not ${req.method} ${req.path}`)
    return () => {}
  }
  void resolve(host).then(
    (t) => {
      if (done) return
      const headers: Record<string, string> = {}
      for (const [k, v] of Object.entries(req.headers)) if (FORWARDED.test(k)) headers[k.toLowerCase()] = v
      headers.authorization = `Bearer ${t.token}`
      if (req.body) headers['content-length'] = String(req.body.byteLength)
      const out = http.request({
        method: req.method,
        path: req.path,
        socketPath: t.socketPath,
        host: t.host,
        port: t.port,
        headers: { ...headers, host: 'meshd' },
      })
      abort = () => out.destroy()
      out.on('response', (res) => {
        if (done) return
        const h: Record<string, string> = {}
        for (const [k, v] of Object.entries(res.headers)) if (v !== undefined) h[k] = Array.isArray(v) ? v.join(', ') : v
        emit({ type: 'head', status: res.statusCode ?? 0, headers: h })
        res.on('data', (chunk: Buffer) => {
          if (!done) emit({ type: 'data', chunk: new Uint8Array(chunk) })
        })
        res.on('end', () => finish({ type: 'end' }))
        res.on('error', (e) => fail('failed', e.message))
        res.on('aborted', () => fail('failed', 'the connection closed'))
      })
      out.on('error', (e: NodeJS.ErrnoException) => {
        if (e.code === 'ENOENT' || e.code === 'ECONNREFUSED' || e.code === 'ECONNRESET') fail('unreachable', e.message)
        else fail('failed', e.message)
      })
      out.end(req.body ? Buffer.from(req.body) : undefined)
    },
    (e: unknown) => {
      if (e instanceof BridgeError) fail(e.code, e.message)
      else fail('failed', String(e))
    },
  )
  return () => {
    done = true
    abort()
  }
}
