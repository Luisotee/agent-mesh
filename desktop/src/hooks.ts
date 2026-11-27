import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from 'react'
import type { AgentWait } from './gen/agentmesh/v1/mesh_pb'
import type { HostView } from './dock'
import type { HostRuntime } from './model'
import { joinWaiting } from './derive'
import type { SidebarHost } from './sidebar'
import type { HostState } from './store'
import { appKeys, type KeyedCommand } from './prefs'

/** The current time, ticking every `everyMs`, for a label like "12 s ago". */
export function useNow(everyMs = 10_000): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), everyMs)
    return () => clearInterval(t)
  }, [everyMs])
  return now
}

/**
 * What the dock shows of every host, together. It changes only when a host's runs, agents or messages do (a cursor
 * moving on every event isn't one), so the dock isn't redrawn for each event of a busy host.
 */
export function useHostViews(hosts: HostRuntime[]): HostView[] {
  const subscribe = useCallback(
    (notify: () => void) => {
      const offs = hosts.map((h) => h.store.subscribe(notify))
      return () => offs.forEach((off) => off())
    },
    [hosts],
  )
  const last = useRef<HostView[]>([])
  const snapshot = useCallback(() => {
    const next = hosts.map((h) => {
      const s = h.store.getState()
      return { hostId: h.info.id, hostName: h.info.name, runs: s.runs, agents: s.agents, messages: s.messages, clockSkewMs: s.clockSkewMs, waitingAll: s.waitingAll, agentWaits: s.agentWaits }
    })
    const prev = last.current
    const same =
      prev.length === next.length &&
      next.every((n, i) => {
        const p = prev[i]!
        return p.hostId === n.hostId && p.hostName === n.hostName && p.runs === n.runs && p.agents === n.agents && p.messages === n.messages && p.clockSkewMs === n.clockSkewMs && p.waitingAll === n.waitingAll && p.agentWaits === n.agentWaits
      })
    if (!same) last.current = next
    return last.current
  }, [hosts])
  return useSyncExternalStore(subscribe, snapshot)
}

const CONN_WORD: Record<string, string> = { connecting: 'connecting', reconnecting: 'reconnecting', stopped: 'disconnected' }

/** One host as the sidebar sees it; the waiting set is worked out only when what it is made of changed. */
function sidebarHost(h: HostRuntime, s: HostState, prev?: { from: HostState; out: SidebarHost }): SidebarHost {
  const f = prev?.from
  const up = s.conn.kind === 'ready'
  const down = up ? undefined : `${h.info.name} is ${CONN_WORD[s.conn.kind] ?? s.conn.kind}${s.conn.kind === 'reconnecting' || s.conn.kind === 'stopped' ? `: ${'error' in s.conn ? s.conn.error : s.conn.message}` : ''}`
  const loading = !s.synced && (s.conn.kind === 'connecting' || s.conn.kind === 'ready')
  const here = h.info.kind === 'state-dir' || !!h.info.here
  if (prev && f && prev.out.here === here && prev.out.loading === loading && f.runs === s.runs && f.agents === s.agents && f.waiting === s.waiting && prev.out.up === up && prev.out.down === down && prev.out.hostName === h.info.name) return prev.out
  return {
    hostId: h.info.id,
    hostName: h.info.name,
    here,
    up,
    down,
    loading,
    runs: s.runs,
    agents: s.agents,
    waiting: new Set(joinWaiting(s.waiting, s.runs, s.agents).map((w) => w.agent.id)),
  }
}

/** What one agent waits on (`ListAgentWaits`), re-rendering only when that agent's wait changes (a row of the sidebar asks, not its parent). */
export function useAgentWait(hosts: HostRuntime[], hostId: string, agentId: string): AgentWait | undefined {
  const host = hosts.find((h) => h.info.id === hostId)
  const subscribe = useCallback((notify: () => void) => host?.store.subscribe(notify) ?? (() => {}), [host])
  return useSyncExternalStore(subscribe, () => host?.store.getState().agentWaits[agentId])
}

/** What the sidebar shows of every host, together: it changes only when a host's runs, agents, waiting list or connection do. */
export function useSidebarHosts(hosts: HostRuntime[]): SidebarHost[] {
  const subscribe = useCallback(
    (notify: () => void) => {
      const offs = hosts.map((h) => h.store.subscribe(notify))
      return () => offs.forEach((off) => off())
    },
    [hosts],
  )
  const cache = useRef(new Map<string, { from: HostState; out: SidebarHost }>())
  const last = useRef<SidebarHost[]>([])
  const snapshot = useCallback(() => {
    const next = hosts.map((h) => {
      const s = h.store.getState()
      const out = sidebarHost(h, s, cache.current.get(h.info.id))
      cache.current.set(h.info.id, { from: s, out })
      return out
    })
    const prev = last.current
    if (prev.length !== next.length || next.some((n, i) => n !== prev[i])) last.current = next
    return last.current
  }, [hosts])
  return useSyncExternalStore(subscribe, snapshot)
}

const NO_SETTINGS = {}

/** The shortcuts' keys (the first host's `app.keys.*`), re-rendering when they are rebound. */
export function useKeys(hosts: HostRuntime[]): Record<KeyedCommand, string> {
  const h = hosts[0]
  const settings = useSyncExternalStore(
    useCallback((notify: () => void) => h?.store.subscribe(notify) ?? (() => {}), [h]),
    () => h?.store.getState().settings ?? NO_SETTINGS,
  )
  return appKeys(settings)
}
