import { AgentRole, AgentState, RunStatus, type Agent, type Run } from './gen/agentmesh/v1/types_pb'

export interface TreeNode {
  agent: Agent
  children: TreeNode[]
}

export interface RunTree {
  run: Run
  roots: TreeNode[]
  /** Agents of the run that need the user: asking for input. */
  needsInput: number
  /** Agents still alive. */
  live: number
}

const ts = (a: Agent) => Number(a.createdAt?.seconds ?? 0n) * 1000 + Math.floor((a.createdAt?.nanos ?? 0) / 1e6)

/** Roots first (the run's lead), then the rest in start order. */
function order(a: Agent, b: Agent): number {
  const lead = Number(b.role === AgentRole.ROOT) - Number(a.role === AgentRole.ROOT)
  return lead || ts(a) - ts(b) || a.name.localeCompare(b.name)
}

/**
 * Each run as a tree: the lead (an agent with no parent in the run) on top, its workers under it, their read-only
 * agents under those. A run's agents whose parent isn't known are roots too, so nothing disappears.
 */
export function buildTrees(runs: Record<string, Run>, agents: Record<string, Agent>): RunTree[] {
  const byRun = new Map<string, Agent[]>()
  for (const a of Object.values(agents)) byRun.set(a.runId, [...(byRun.get(a.runId) ?? []), a])
  const trees: RunTree[] = []
  for (const run of Object.values(runs)) {
    const members = byRun.get(run.id) ?? []
    const known = new Set(members.map((a) => a.id))
    const kids = new Map<string, Agent[]>()
    const roots: Agent[] = []
    for (const a of members) {
      if (a.parentId && known.has(a.parentId)) kids.set(a.parentId, [...(kids.get(a.parentId) ?? []), a])
      else roots.push(a)
    }
    const node = (a: Agent): TreeNode => ({ agent: a, children: (kids.get(a.id) ?? []).sort(order).map(node) })
    trees.push({
      run,
      roots: roots.sort(order).map(node),
      needsInput: members.filter((a) => a.state === AgentState.NEEDS_INPUT).length,
      live: members.filter((a) => a.state !== AgentState.EXITED).length,
    })
  }
  // Runs needing the user first, then open before closed, then the newest.
  return trees.sort(
    (a, b) =>
      Number(b.needsInput > 0) - Number(a.needsInput > 0) ||
      Number(a.run.status === RunStatus.CLOSED) - Number(b.run.status === RunStatus.CLOSED) ||
      runTime(b.run) - runTime(a.run),
  )
}

const runTime = (r: Run) => Number(r.createdAt?.seconds ?? 0n)

export const ROLE_LABEL: Record<AgentRole, string> = {
  [AgentRole.UNSPECIFIED]: 'agent',
  [AgentRole.ROOT]: 'lead',
  [AgentRole.WORKER]: 'worker',
  [AgentRole.ASKER]: 'asker',
  [AgentRole.REVIEWER]: 'reviewer',
}

/** A plain sentence for what an agent is doing, for the tree row. */
export function statusSentence(a: Agent): string {
  switch (a.state) {
    case AgentState.STARTING:
      return 'Starting up'
    case AgentState.IDLE:
      return 'Idle, waiting for a message'
    case AgentState.WORKING:
      return 'Working'
    case AgentState.NEEDS_INPUT: {
      const why = a.stateDetail ? `: ${a.stateDetail}` : ''
      return a.needsInput > 1 ? `Needs you (${a.needsInput})${why}` : `Needs you${why}`
    }
    case AgentState.EXITED:
      if (a.stateDetail === 'lost') return 'Lost: its terminal is gone'
      if (a.stateDetail === 'killed') return 'Stopped'
      return a.exitCode === 0 ? 'Finished' : `Exited with code ${a.exitCode}`
    case AgentState.UNKNOWN:
      return 'Quiet for a while: state unknown'
    default:
      return 'Unknown'
  }
}

/** Shows `a/b` as the repo's folder name. */
export function repoName(repo: string): string {
  const parts = repo.replace(/[/\\]+$/, '').split(/[/\\]/)
  return parts[parts.length - 1] || repo
}

/** The repos agents on this host work in, most recent first: what the spawn form offers. */
export function knownRepos(agents: Record<string, Agent>): string[] {
  const seen = new Map<string, number>()
  for (const a of Object.values(agents)) seen.set(a.repo, Math.max(seen.get(a.repo) ?? 0, ts(a)))
  return [...seen.entries()].filter(([r]) => r).sort((a, b) => b[1] - a[1]).map(([r]) => r)
}
