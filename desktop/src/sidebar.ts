import { AgentRole, AgentState, RunStatus, type Agent, type Run } from './gen/agentmesh/v1/types_pb'
import { buildTrees, repoName, type TreeNode } from './derive'

/** What the sidebar needs of one host. */
export interface SidebarHost {
  hostId: string
  hostName: string
  /** The host is this machine's own daemon: its projects carry no host mark. */
  here: boolean
  /** Its connection is up (a host that is down greys its projects out). */
  up: boolean
  /** Why it is down, for the tooltip. */
  down?: string
  /** Its first snapshot hasn't arrived on a connection that is coming up. */
  loading: boolean
  runs: Record<string, Run>
  agents: Record<string, Agent>
  /** The agents that wait on the user (`ListWaiting`). */
  waiting: ReadonlySet<string>
}

/** A worker of a run, with its read-only agents (reviewers, askers) folded into it and the workers it started under it. */
export interface WorkerRow {
  agent: Agent
  helpers: Agent[]
  workers: WorkerRow[]
  /** It works in another repo than its lead: that repo shows as a tag. */
  elsewhere: boolean
}

/** A run, shown as its lead. */
export interface LeadRow {
  hostId: string
  run: Run
  /** The run's lead; none for a run that has no agent yet. */
  lead?: Agent
  helpers: Agent[]
  workers: WorkerRow[]
  /** Agents of the run that wait on the user. */
  needs: number
  /** The run's latest activity (ms), for the order. */
  last: number
  /** Closed, or nothing in it is alive: it folds under "Earlier". */
  earlier: boolean
}

export interface ProjectRow {
  /** `hostId` and repo path: the same folder on two hosts is two projects. */
  key: string
  hostId: string
  hostName: string
  here: boolean
  up: boolean
  down?: string
  repo: string
  name: string
  /** Runs led from this project, live ones. */
  runs: LeadRow[]
  /** Its closed and finished runs. */
  earlier: LeadRow[]
  /** Runs led from another project with a worker here ("Also working here"). */
  also: LeadRow[]
  needs: number
  last: number
  pinned: boolean
}

export const projectKey = (hostId: string, repo: string) => `${hostId}\n${repo}`

const alive = (a: Agent) => a.state !== AgentState.EXITED && a.state !== AgentState.RETIRED
const helper = (a: Agent) => a.role === AgentRole.REVIEWER || a.role === AgentRole.ASKER
const ms = (t?: { seconds: bigint; nanos: number }) => (t ? Number(t.seconds) * 1000 + Math.floor(t.nanos / 1e6) : 0)
const activity = (a: Agent) => Math.max(ms(a.lastActivity), ms(a.createdAt))

/** The nodes minus the archived ones, an archived agent's children lifted into its place (they may still run). */
const unarchived = (nodes: TreeNode[]): TreeNode[] => nodes.flatMap((n) => (n.agent.archived ? unarchived(n.children) : [n]))

/** A tree node's workers and helpers: read-only agents fold into the agent above them, at any depth under it. */
function split(nodes: TreeNode[], leadRepo: string): { helpers: Agent[]; workers: WorkerRow[] } {
  const helpers: Agent[] = []
  const workers: WorkerRow[] = []
  for (const n of unarchived(nodes)) {
    if (helper(n.agent)) {
      helpers.push(n.agent)
      // Whatever a read-only agent started folds in with it.
      const under = split(n.children, leadRepo)
      helpers.push(...under.helpers, ...flatten(under.workers))
      continue
    }
    const under = split(n.children, leadRepo)
    workers.push({ agent: n.agent, helpers: under.helpers, workers: under.workers, elsewhere: !!n.agent.repo && n.agent.repo !== leadRepo })
  }
  return { helpers, workers }
}

const flatten = (rows: WorkerRow[]): Agent[] => rows.flatMap((w) => [w.agent, ...w.helpers, ...flatten(w.workers)])

/** Every agent of a lead row: the lead, its workers and their helpers. */
export const leadAgents = (r: LeadRow): Agent[] => [...(r.lead ? [r.lead] : []), ...r.helpers, ...flatten(r.workers)]

/** The workers of a lead row, at any depth. */
export const allWorkers = (rows: WorkerRow[]): WorkerRow[] => rows.flatMap((w) => [w, ...allWorkers(w.workers)])

/** The runs of one host as lead rows (archived agents left out, their children lifted; a run whose every agent is archived left out). */
export function leadRows(h: SidebarHost): LeadRow[] {
  const out: LeadRow[] = []
  for (const t of buildTrees(h.runs, h.agents)) {
    const roots = unarchived(t.roots)
    if (t.roots.length > 0 && roots.length === 0) continue
    // The lead is the run's root agent; any other top-level agent (its parent unknown) shows as one of its workers.
    const leadAt = Math.max(0, roots.findIndex((n) => n.agent.role === AgentRole.ROOT))
    const lead = roots[leadAt]
    const rest = roots.filter((_, i) => i !== leadAt)
    const leadRepo = lead?.agent.repo ?? ''
    const parts = split([...(lead?.children ?? []), ...rest], leadRepo)
    const row: LeadRow = { hostId: h.hostId, run: t.run, lead: lead?.agent, helpers: parts.helpers, workers: parts.workers, needs: 0, last: 0, earlier: false }
    const all = leadAgents(row)
    row.needs = all.filter((a) => a.state === AgentState.NEEDS_INPUT || h.waiting.has(a.id)).length
    row.last = Math.max(ms(t.run.createdAt), ...all.map(activity))
    row.earlier = t.run.status === RunStatus.CLOSED || (all.length > 0 && !all.some(alive))
    out.push(row)
  }
  return out.sort((a, b) => Number(b.needs > 0) - Number(a.needs > 0) || b.last - a.last)
}

/**
 * The sidebar's projects across every host: a project is a repo some run is led from or works in, plus the ones the
 * user pinned. Pinned first (in pin order), then those with something waiting on the user, then by latest activity.
 * A run sits under its lead's project; a worker in another repo lists the run under that project's "Also working here".
 */
export function buildProjects(hosts: SidebarHost[], pins: Record<string, string[]>): ProjectRow[] {
  const by = new Map<string, ProjectRow>()
  const get = (h: SidebarHost, repo: string): ProjectRow => {
    const key = projectKey(h.hostId, repo)
    let p = by.get(key)
    if (!p) {
      const pinned = (pins[h.hostId] ?? []).includes(repo)
      p = { key, hostId: h.hostId, hostName: h.hostName, here: h.here, up: h.up, down: h.down, repo, name: repoName(repo) || 'No project', runs: [], earlier: [], also: [], needs: 0, last: 0, pinned }
      by.set(key, p)
    }
    return p
  }
  for (const h of hosts) {
    for (const repo of pins[h.hostId] ?? []) get(h, repo)
    for (const r of leadRows(h)) {
      const home = r.lead?.repo ?? ''
      const p = get(h, home)
      ;(r.earlier ? p.earlier : p.runs).push(r)
      if (!r.earlier) p.needs += r.needs
      p.last = Math.max(p.last, r.last)
      const others = new Set(allWorkers(r.workers).filter((w) => w.elsewhere).map((w) => w.agent.repo))
      for (const repo of others) {
        if (r.earlier) continue
        const q = get(h, repo)
        q.also.push(r)
        q.last = Math.max(q.last, r.last)
      }
    }
  }
  const pinAt = (p: ProjectRow) => {
    const i = (pins[p.hostId] ?? []).indexOf(p.repo)
    return i < 0 ? Number.MAX_SAFE_INTEGER : i
  }
  return [...by.values()].sort(
    (a, b) => Number(b.pinned) - Number(a.pinned) || (a.pinned && b.pinned ? pinAt(a) - pinAt(b) || a.hostName.localeCompare(b.hostName) : 0) || Number(b.needs > 0) - Number(a.needs > 0) || b.last - a.last || a.name.localeCompare(b.name),
  )
}

/** "2 reviews", "1 asker", "2 reviews, 1 asker": a worker's read-only agents as one chip, or nothing. */
export function helpersLabel(helpers: Agent[]): string {
  const reviews = helpers.filter((a) => a.role === AgentRole.REVIEWER).length
  const asks = helpers.length - reviews
  return [reviews > 0 ? `${reviews} ${reviews === 1 ? 'review' : 'reviews'}` : '', asks > 0 ? `${asks} ${asks === 1 ? 'asker' : 'askers'}` : ''].filter(Boolean).join(', ')
}

/** Whether any of these agents is working now (the helpers chip spins while one is). */
export const anyWorking = (agents: Agent[]) => agents.some((a) => a.state === AgentState.WORKING || a.state === AgentState.STARTING)
