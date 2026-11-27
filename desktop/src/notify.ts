import type { Event } from './gen/agentmesh/v1/types_pb'
import type { AgentTabId } from '../shared/api'

export type NotifyKind = 'input' | 'plan' | 'question' | 'report' | 'failed' | 'exited' | 'limited' | 'error' | 'finished' | 'ready'

export interface Notice {
  kind: NotifyKind
  /** A later notice with the same tag replaces the earlier: one per agent. */
  tag: string
  title: string
  body: string
  agentId: string
  tab: AgentTabId
}

/** An event older than this wasn't "just now": history replayed after a reconnect doesn't notify. */
export const LIVE_WITHIN_MS = 60_000

/** Whether an event just happened, by the host's clock (`clockSkewMs`: how far it is ahead of ours): history replayed after a reconnect isn't live. */
export function isLive(e: Event, now: number, clockSkewMs = 0): boolean {
  const at = e.time ? Number(e.time.seconds) * 1000 + Math.floor(e.time.nanos / 1e6) : 0
  return now + clockSkewMs - at <= LIVE_WITHIN_MS
}

/**
 * What an event means to the person: only the host's own decision, a `UserNotice` event (`internal/notice` on the host
 * decides once; the phones' pushes and this follow it). A notice is shown only when it is for the user and live
 * (history replayed after a reconnect stays quiet); the others are for the Activity list. `clockSkewMs` is how far the
 * host's clock is ahead of ours.
 */
export function noticeFor(e: Event, now: number, clockSkewMs = 0): Notice | undefined {
  const p = e.payload
  if (p.case !== 'userNotice' || !p.value.forUser || !isLive(e, now, clockSkewMs)) return undefined
  const n = p.value
  return { kind: n.kind as NotifyKind, tag: n.tag || n.agentId, title: n.title, body: n.body, agentId: n.agentId, tab: n.tab === 'terminal' ? 'terminal' : 'chat' }
}
