import { AgentRole, type Agent } from './gen/agentmesh/v1/types_pb'
import type { GetUsageResponse, ModelSwap } from './gen/agentmesh/v1/mesh_pb'
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

export interface QuotaRow {
  label: string
  /** Percent used, 0 to 100. */
  used: number
  atReserve: boolean
  resetsAt?: Date
}

/** Claude's windows with the reserve line, and agy's fullest quota row; empty when the host has no reading. */
export function quotaRows(u: GetUsageResponse): { reserve: number; rows: QuotaRow[] } {
  const rows: QuotaRow[] = (u.claude?.windows ?? []).map((w) => ({
    label: `Claude ${w.label}`,
    used: w.used,
    atReserve: (u.claude?.atReserve ?? []).includes(w.label),
    resetsAt: w.resetsAt ? new Date(Number(w.resetsAt.seconds) * 1000) : undefined,
  }))
  // agy reports what is left; the host's guard treats no more than `min_quota` left as at the reserve.
  const agy = [...u.agyRows].sort((a, b) => a.left - b.left)[0]
  if (agy) rows.push({ label: `agy ${agy.group} ${agy.label}`.trim(), used: 100 - agy.left, atReserve: u.quotaStop < 100 && agy.left <= u.minQuota })
  return { reserve: u.quotaStop, rows }
}

/** "resets in 2 h 10 min" within a day, else the weekday and time. */
export function resetsIn(at: Date | undefined, now = new Date()): string {
  if (!at) return ''
  const min = Math.round((at.getTime() - now.getTime()) / 60_000)
  if (min <= 0) return 'resetting'
  if (min < 60) return `resets in ${min} min`
  if (min < 24 * 60) return `resets in ${Math.floor(min / 60)} h ${min % 60} min`
  return `resets ${at.toLocaleString([], { weekday: 'short', hour: '2-digit', minute: '2-digit' })}`
}

export const isLead = (a: Agent) => a.role === AgentRole.ROOT

/** One line of an agent's model history: what it ran on from when (the host's `ModelSwap`, oldest first). */
export function historyRows(swaps: ModelSwap[]): Array<{ text: string; why: string; at?: number }> {
  return swaps.map((m) => ({ text: [m.model, m.effort].filter(Boolean).join(' · ') || 'the CLI\'s default', why: m.why, at: m.time ? Number(m.time.seconds) * 1000 : undefined }))
}
