import { AgentRole, type Agent } from './gen/agentmesh/v1/types_pb'
import type { ModelSwap } from './gen/agentmesh/v1/mesh_pb'
import type { AgentPullRequests } from './gen/agentmesh/v1/pr_pb'
import type { GetCostResponse } from './gen/agentmesh/v1/report_pb'

/** What the side panel shows of the open agent and its run, as rows. Nothing here asks the host for anything. */

export const dollars = (v: number) => `$${v.toFixed(2)}`

export interface CostRow {
  name: string
  usd: number
}

/** The run's cost: the total the host gives, and each agent's share, dearest first (an agent that cost nothing yet is left out). */
export function costRows(c: GetCostResponse): { total: number; rows: CostRow[] } {
  const rows = c.agents.map((a) => ({ name: a.name, usd: a.usd })).filter((r) => r.usd > 0).sort((a, b) => b.usd - a.usd || a.name.localeCompare(b.name))
  return { total: c.usd, rows }
}

/** What one agent cost in a run's table (the host names agents as the app does). */
export function costOf(c: GetCostResponse | undefined, agent: Agent): number | undefined {
  return c?.agents.find((a) => a.name === agent.name)?.usd
}

export interface PrRow {
  agentName: string
  agentId: string
  number: number
  url: string
  state: string
  draft: boolean
  checks: string
  stale: boolean
  held: boolean
}

/** The PRs of the agents `only` accepts, the ones to act on first: open before merged and closed, then newest. */
export function prRows(prs: Record<string, AgentPullRequests>, agents: Record<string, Agent>, only: (a: Agent) => boolean): PrRow[] {
  const rank = (s: string) => (s === 'open' ? 0 : s === 'merged' ? 1 : 2)
  return Object.values(prs)
    .flatMap((ap) => {
      const a = agents[ap.agentId]
      return a && only(a) ? ap.pullRequests.map((p): PrRow => ({ agentName: a.name, agentId: a.id, number: p.number, url: p.url, state: p.state, draft: p.draft, checks: p.checks, stale: p.stale, held: p.held })) : []
    })
    .sort((a, b) => rank(a.state) - rank(b.state) || b.number - a.number)
}

export const isLead = (a: Agent) => a.role === AgentRole.ROOT

/** One line of an agent's model history: what it ran on from when (the host's `ModelSwap`, oldest first). */
export function historyRows(swaps: ModelSwap[]): Array<{ text: string; why: string; at?: number }> {
  return swaps.map((m) => ({ text: [m.model, m.effort].filter(Boolean).join(' · ') || 'the CLI\'s default', why: m.why, at: m.time ? Number(m.time.seconds) * 1000 : undefined }))
}
