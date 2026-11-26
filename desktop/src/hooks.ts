import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from 'react'
import type { HostView } from './dock'
import type { HostRuntime } from './model'

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
      return { hostId: h.info.id, hostName: h.info.name, runs: s.runs, agents: s.agents, messages: s.messages, clockSkewMs: s.clockSkewMs }
    })
    const prev = last.current
    const same =
      prev.length === next.length &&
      next.every((n, i) => {
        const p = prev[i]!
        return p.hostId === n.hostId && p.hostName === n.hostName && p.runs === n.runs && p.agents === n.agents && p.messages === n.messages && p.clockSkewMs === n.clockSkewMs
      })
    if (!same) last.current = next
    return last.current
  }, [hosts])
  return useSyncExternalStore(subscribe, snapshot)
}
