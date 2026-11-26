import { compareMessages } from './order'
import { AgentState, MessageType, RunStatus, type Agent, type Message, type Run } from './gen/agentmesh/v1/types_pb'
import { NEW_FOR_MS, tabKey, type WorkspaceState } from './workspace'

/** What the dock needs of one host: its runs, agents and the messages the app has seen. */
export interface HostView {
  hostId: string
  hostName: string
  runs: Record<string, Run>
  agents: Record<string, Agent>
  messages: Record<string, Message>
  /** How far the host's clock is ahead of ours (ms). */
  clockSkewMs: number
}

export interface Tile {
  key: string
  hostId: string
  hostName: string
  clockSkewMs: number
  agent: Agent
  run?: Run
  /** Started while the app was open and not looked at yet. */
  isNew: boolean
  /** The tool the agent is running now, while it works. */
  activity: string
  /** The last message the agent sent that the app has seen. */
  lastSent?: Message
}

const alive = (a: Agent) => a.state !== AgentState.EXITED && a.state !== AgentState.RETIRED
const attention = (a: Agent) => a.state === AgentState.NEEDS_INPUT || a.state === AgentState.ERROR || a.state === AgentState.LIMITED
const at = (a: Agent) => Number(a.lastActivity?.seconds ?? 0n) * 1000 + Math.floor((a.lastActivity?.nanos ?? 0) / 1e6)

/**
 * A tile per live agent of the open runs on every host: those that need the user first, then new ones, then the
 * rest by latest activity. A tile the user dismissed stays away until the agent's state changes.
 */
export function dockTiles(hosts: HostView[], ws: Pick<WorkspaceState, 'newTiles' | 'dismissed'>, now: number): Tile[] {
  const tiles: Tile[] = []
  for (const h of hosts) {
    const lastSent = new Map<string, Message>()
    for (const m of Object.values(h.messages)) {
      const from = m.from?.id
      const had = from ? lastSent.get(from) : undefined
      if (from && (!had || compareMessages(had, m) < 0)) lastSent.set(from, m)
    }
    for (const a of Object.values(h.agents)) {
      const run = h.runs[a.runId]
      if (!alive(a) || (run && run.status === RunStatus.CLOSED)) continue
      const key = tabKey({ hostId: h.hostId, agentId: a.id })
      if (key in ws.dismissed && ws.dismissed[key] === a.state) continue
      const marked = ws.newTiles[key]
      tiles.push({
        key,
        hostId: h.hostId,
        hostName: h.hostName,
        clockSkewMs: h.clockSkewMs,
        agent: a,
        run,
        isNew: marked !== undefined && now - marked < NEW_FOR_MS,
        activity: a.state === AgentState.WORKING ? a.stateDetail : '',
        lastSent: lastSent.get(a.id),
      })
    }
  }
  const rank = (t: Tile) => (attention(t.agent) ? 0 : t.isNew ? 1 : 2)
  return tiles.sort((x, y) => rank(x) - rank(y) || at(y.agent) - at(x.agent) || x.agent.name.localeCompare(y.agent.name))
}

/** "sent a plan", for the last thing an agent said. */
export function sentLabel(m: Message): string {
  switch (m.type) {
    case MessageType.PLAN:
      return 'sent a plan'
    case MessageType.QUESTION:
      return 'asked a question'
    case MessageType.DONE:
      return 'reported done'
    case MessageType.FAILED:
      return 'reported a failure'
    case MessageType.TASK:
      return 'sent a task'
    case MessageType.ANSWER:
      return 'sent an answer'
    default:
      return 'sent a message'
  }
}

/** How long ago, short: "now", "12 s", "4 min", "3 h", "2 d". */
export function ago(ms: number, now: number): string {
  if (ms <= 0) return ''
  const s = Math.max(0, Math.round((now - ms) / 1000))
  if (s < 5) return 'now'
  if (s < 60) return `${s} s`
  if (s < 3600) return `${Math.floor(s / 60)} min`
  if (s < 86_400) return `${Math.floor(s / 3600)} h`
  return `${Math.floor(s / 86_400)} d`
}

/** An agent's last activity as epoch milliseconds in the host's clock (0 when it has none). */
export const activityMs = at
