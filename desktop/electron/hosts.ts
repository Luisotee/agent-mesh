import { randomUUID } from 'node:crypto'
import { existsSync } from 'node:fs'
import { mkdir, readFile, rename, writeFile } from 'node:fs/promises'
import path from 'node:path'
import type { HostInfo, NewHost } from '../shared/api.js'
import { isRoute, parsePairingLink, type PairingLink, type Route } from './route.js'

/**
 * What main keeps per host. A token lives here (0600) and nowhere else. A `tcp` host (a loopback address and a
 * pasted token) can no longer be added, but the ones saved before pairing keep working.
 */
export type StoredHost =
  | { id: string; name: string; kind: 'state-dir'; path: string }
  | { id: string; name: string; kind: 'tcp'; address: string; token: string }
  | { id: string; name: string; kind: 'paired'; deviceId: string; token: string; route: Route }

export function toInfo(h: StoredHost): HostInfo {
  switch (h.kind) {
    case 'state-dir':
      return { id: h.id, name: h.name, kind: h.kind, target: h.path }
    case 'tcp':
      return { id: h.id, name: h.name, kind: h.kind, target: h.address }
    case 'paired': {
      const r = h.route
      const where = r.direct ? `${r.direct.host}:${r.direct.port}` : r.relay ? `via the relay at ${r.relay.host}` : r.hostId
      return { id: h.id, name: h.name, kind: h.kind, target: where }
    }
  }
}

/** What redeeming a pairing link gave: the device's own token, from the host that the link named. */
export interface Redeemed {
  deviceId: string
  deviceName: string
  token: string
  hostId: string
  hostName: string
  fingerprint: string
}

/** Redeems a link's one-time token over the link's route. Injected so the list can be tested without a host. */
export type Redeemer = (link: PairingLink, deviceName: string) => Promise<Redeemed>

/**
 * The saved hosts. The list is the one thing that can't live on a host, since it comes before any connection.
 * `fixed` (dev and tests, from the environment) makes it one host that can't be changed.
 */
export class Hosts {
  private list: StoredHost[] = []
  /** Entries this build doesn't understand (a newer kind, say): kept as they are and written back. */
  private foreign: unknown[] = []
  /** Hosts whose pairing link is being redeemed right now. */
  private readonly pending = new Set<string>()
  readonly fixed: boolean

  constructor(
    private readonly file: string,
    fixedStateDir?: string,
    private readonly redeem?: Redeemer,
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
    let host: StoredHost
    if (n.kind === 'state-dir') {
      if (!name) throw new Error('give the host a name')
      if (!path.isAbsolute(n.path)) throw new Error('the state dir must be an absolute path')
      host = { id: randomUUID(), name, kind: 'state-dir', path: n.path }
    } else if (n.kind === 'pair' && typeof n.link === 'string' && typeof n.deviceName === 'string') {
      host = await this.pair(name, n.link, n.deviceName)
    } else {
      // The renderer is untrusted: a shape this build doesn't know is refused, not guessed at.
      throw new Error('unknown kind of host')
    }
    if (this.list.some((x) => sameTarget(x, host))) throw new Error('that host is already in the list')
    this.list = [...this.list, host]
    await this.save()
    return host
  }

  /**
   * Pairs with the host a link names: the link is read and checked, a host already paired is refused before the
   * one-time token is spent, and what the host answers has to be the host the link named.
   */
  private async pair(name: string, text: string, deviceName: string): Promise<StoredHost> {
    if (!this.redeem) throw new Error('pairing is not available')
    const link = parsePairingLink(text)
    const id = link.route.hostId
    // Another add for this host may be mid-redeem: its token is spent by then, so this one must not spend its own.
    if (this.pending.has(id)) throw new Error('this host is being paired right now: wait for that to finish')
    if (this.list.some((h) => h.kind === 'paired' && h.route.hostId === id)) throw new Error('that host is already in the list')
    this.pending.add(id)
    let got: Redeemed
    try {
      got = await this.redeem(link, deviceName.trim() || link.deviceName || 'Desktop')
    } finally {
      // add() runs on in the same turn and puts the host in the list, so no other add can slip in between.
      this.pending.delete(id)
    }
    if (got.hostId !== link.route.hostId) {
      throw new Error(`the host that answered isn't the one the link named, so it was not saved. The link is used up, and that host now lists a device "${got.deviceName}": revoke it there.`)
    }
    return { id: randomUUID(), name: name || link.hostName || 'host', kind: 'paired', deviceId: got.deviceId, token: got.token, route: link.route }
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
  if (a.kind === 'paired' && b.kind === 'paired') return a.route.hostId === b.route.hostId
  return a.kind === 'tcp' && b.kind === 'tcp' && a.address === b.address
}

function isStored(v: unknown): v is StoredHost {
  if (typeof v !== 'object' || v === null) return false
  const h = v as Record<string, unknown>
  if (typeof h.id !== 'string' || typeof h.name !== 'string') return false
  if (h.kind === 'state-dir') return typeof h.path === 'string'
  if (h.kind === 'paired') return typeof h.deviceId === 'string' && typeof h.token === 'string' && isRoute(h.route)
  return h.kind === 'tcp' && typeof h.address === 'string' && typeof h.token === 'string'
}
