import { ActorClass } from './gen/agentmesh/v1/actor_pb'
import { AgentRole, AgentState, MessageType, type Agent, type Event } from './gen/agentmesh/v1/types_pb'
import type { AgentTab } from './model'

export type NotifyKind = 'input' | 'plan' | 'question' | 'report' | 'failed' | 'exited'

export interface Notice {
  kind: NotifyKind
  /** A later notice with the same tag replaces the earlier: one per agent. */
  tag: string
  title: string
  body: string
  agentId: string
  tab: AgentTab
}

/** An event older than this wasn't "just now": history replayed after a reconnect doesn't notify. */
export const LIVE_WITHIN_MS = 60_000

const MAX_BODY = 200

function brief(text: string): string {
  // A notification is plain text: drop the Markdown's own marks (headings, bullets, emphasis, code ticks).
  const flat = text
    .split('\n')
    .map((l) => l.replace(/^\s*(#{1,6}\s+|>\s*|[-*+]\s+|\d+\.\s+)/, ''))
    .join(' ')
    .replace(/[*`]+/g, '')
    .replace(/\s+/g, ' ')
    .trim()
  return flat.length > MAX_BODY ? `${flat.slice(0, MAX_BODY - 1)}…` : flat
}

/**
 * What an event means to the person, if anything: an agent stopped at a prompt, a plan, a question, a report or a
 * failure sent to a run's lead, an agent that died. Only live events (history replayed after a reconnect stays
 * quiet), and never what the user did themselves. `agents` is the state before the event.
 */
export function noticeFor(e: Event, agents: Record<string, Agent>, now: number): Notice | undefined {
  const at = e.time ? Number(e.time.seconds) * 1000 + Math.floor(e.time.nanos / 1e6) : 0
  if (now - at > LIVE_WITHIN_MS) return undefined
  const p = e.payload
  switch (p.case) {
    case 'agentStateChanged': {
      const a = agents[e.agentId]
      if (!a || p.value.to !== AgentState.NEEDS_INPUT || p.value.from === AgentState.NEEDS_INPUT) return undefined
      return { kind: 'input', tag: a.id, title: `${a.name} needs you`, body: brief(p.value.detail) || 'It is stopped at a prompt in its terminal.', agentId: a.id, tab: 'terminal' }
    }
    case 'messageSent': {
      const m = p.value.message
      if (!m || m.from?.class === ActorClass.DEVICE) return undefined
      const to = agents[m.toId]
      const from = agents[m.from?.id ?? '']
      // What reaches a run's lead is what the user is waiting for; talk among workers isn't.
      if (!to || to.role !== AgentRole.ROOT || !from) return undefined
      const base = { tag: from.id, agentId: from.id, tab: 'messages' as const, body: brief(m.body) }
      switch (m.type) {
        case MessageType.PLAN:
          return { ...base, kind: 'plan', title: `${from.name} sent a plan` }
        case MessageType.QUESTION:
          return { ...base, kind: 'question', title: `${from.name} has a question` }
        case MessageType.DONE:
          return { ...base, kind: 'report', title: `${from.name} reported done` }
        case MessageType.FAILED:
          return { ...base, kind: 'failed', title: `${from.name} failed` }
      }
      return undefined
    }
    case 'agentExited': {
      const a = agents[e.agentId]
      // The user stopping an agent isn't news.
      if (!a || p.value.reason === 'killed') return undefined
      const clean = p.value.exitCode === 0 && p.value.reason !== 'lost'
      return {
        kind: clean ? 'exited' : 'failed',
        tag: a.id,
        title: clean ? `${a.name} finished` : p.value.reason === 'lost' ? `${a.name} was lost` : `${a.name} exited with code ${p.value.exitCode}`,
        body: '',
        agentId: a.id,
        tab: 'overview',
      }
    }
  }
  return undefined
}
