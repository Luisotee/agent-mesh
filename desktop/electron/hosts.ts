import { randomUUID } from 'node:crypto'
import { existsSync } from 'node:fs'
import { mkdir, readFile, rename, writeFile } from 'node:fs/promises'
import path from 'node:path'
import type { HostInfo, NewHost } from '../shared/api.js'

/** What main keeps per host. A tcp host's token lives here (0600) and nowhere else. */
export type StoredHost =
  | { id: string; name: string; kind: 'state-dir'; path: string }
  | { id: string; name: string; kind: 'tcp'; address: string; token: string }

export function toInfo(h: StoredHost): HostInfo {
  return h.kind === 'state-dir'
    ? { id: h.id, name: h.name, kind: h.kind, target: h.path }
    : { id: h.id, name: h.name, kind: h.kind, target: h.address }
}

/**
 * The saved hosts. The list is the one thing that can't live on a host, since it comes before any connection.
 * `fixed` (dev and tests, from the environment) makes it one host that can't be changed.
 */
export class Hosts {
  private list: StoredHost[] = []
  /** Entries this build doesn't understand (a newer kind, say): kept as they are and written back. */
  private foreign: unknown[] = []
  readonly fixed: boolean

  constructor(
    private readonly file: string,
    fixedStateDir?: string,
  ) {
    this.fixed = !!fixedStateDir
    if (fixedStateDir) this.list = [{ id: 'local', name: 'local', kind: 'state-dir', path: fixedStateDir }]
  }

  async load(): Promise<void> {
    if (this.fixed || !existsSync(this.file)) return
    try {
      const parsed: unknown = JSON.parse(await readFile(this.file, 'utf8'))
      const entries = Array.isArray(parsed) ? parsed : []
      this.list = entries.filter(isStored)
      this.foreign = entries.filter((e) => !isStored(e))
    } catch {
      // A damaged file is left alone on disk and not used; the next add writes a fresh one.
      this.list = []
    }
  }

  all(): StoredHost[] {
    return this.list
  }

  get(id: string): StoredHost | undefined {
    return this.list.find((h) => h.id === id)
  }

  async add(n: NewHost): Promise<StoredHost> {
    if (this.fixed) throw new Error('the host list is fixed by the environment')
    const name = n.name.trim()
    if (!name) throw new Error('give the host a name')
    let host: StoredHost
    if (n.kind === 'state-dir') {
      if (!path.isAbsolute(n.path)) throw new Error('the state dir must be an absolute path')
      host = { id: randomUUID(), name, kind: 'state-dir', path: n.path }
    } else {
      const m = /^(localhost|127(?:\.\d{1,3}){3}|\[::1\]):(\d{1,5})$/.exec(n.address)
      if (!m) {
        // The bridge speaks plain HTTP with a bearer token: only to this machine until the connection is encrypted
        // (docs/apps.md, "Reaching a host"). meshd's own --tcp listener is loopback only too.
        throw new Error('the address must be a loopback host:port (127.0.0.1:7777, localhost:7777 or [::1]:7777)')
      }
      const port = Number(m[2])
      if (port < 1 || port > 65535) throw new Error('the port must be 1-65535')
      if (!n.token.trim()) throw new Error('give the host its device token')
      host = { id: randomUUID(), name, kind: 'tcp', address: n.address, token: n.token.trim() }
    }
    if (this.list.some((x) => sameTarget(x, host))) throw new Error('that host is already in the list')
    this.list = [...this.list, host]
    await this.save()
    return host
  }

  async remove(id: string): Promise<void> {
    if (this.fixed) throw new Error('the host list is fixed by the environment')
    this.list = this.list.filter((h) => h.id !== id)
    await this.save()
  }

  private saving: Promise<void> = Promise.resolve()

  /** Saves run one at a time, each writing the list as it is then, so overlapping adds can't clobber one another. */
  private save(): Promise<void> {
    const run = async () => {
      await mkdir(path.dirname(this.file), { recursive: true })
      const tmp = `${this.file}.tmp`
      await writeFile(tmp, JSON.stringify([...this.list, ...this.foreign], null, 2), { mode: 0o600 })
      await rename(tmp, this.file)
    }
    const next = this.saving.then(run, run)
    this.saving = next.catch(() => {})
    return next
  }
}

function sameTarget(a: StoredHost, b: StoredHost): boolean {
  if (a.kind === 'state-dir' && b.kind === 'state-dir') return path.resolve(a.path) === path.resolve(b.path)
  return a.kind === 'tcp' && b.kind === 'tcp' && a.address === b.address
}

function isStored(v: unknown): v is StoredHost {
  if (typeof v !== 'object' || v === null) return false
  const h = v as Record<string, unknown>
  if (typeof h.id !== 'string' || typeof h.name !== 'string') return false
  if (h.kind === 'state-dir') return typeof h.path === 'string'
  return h.kind === 'tcp' && typeof h.address === 'string' && typeof h.token === 'string'
}
