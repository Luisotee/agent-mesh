import { compareMessages } from './order'
import { AgentRole, AgentState, MessageType, RunStatus, type Agent, type Message, type Run } from './gen/agentmesh/v1/types_pb'
import { WaitingKind, type Waiting as WaitingRow } from './gen/agentmesh/v1/mesh_pb'

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
    // Built once per agent, so a parent loop in corrupt data can't recurse forever.
    const seen = new Set<string>()
    const node = (a: Agent): TreeNode => {
      seen.add(a.id)
      return { agent: a, children: (kids.get(a.id) ?? []).filter((c) => !seen.has(c.id)).sort(order).map(node) }
    }
    const nodes = roots.sort(order).map(node)
    // Agents in a parent loop are reached from no root: show each such group under its first agent.
    for (const a of [...members].sort(order)) if (!seen.has(a.id)) nodes.push(node(a))
    trees.push({
      run,
      roots: nodes,
      needsInput: members.filter((a) => a.state === AgentState.NEEDS_INPUT).length,
      live: members.filter((a) => a.state !== AgentState.EXITED && a.state !== AgentState.RETIRED).length,
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
export type ExitKind = 'finished' | 'stopped' | 'failed'

/**
 * How an exited agent ended, from what the host stores of it: a clean exit, one by a signal (a stop), and anything
 * else, which includes a lost terminal. The host stores a lost agent as exit code -1 with no detail, and a stop as
 * `killed by signal`; the live `agentExited` event's reason (`lost`, `killed`) stands in for the detail until the app
 * reloads the agent.
 */
export function exitKind(a: Agent): ExitKind {
  if (a.stateDetail === 'lost') return 'failed'
  if (a.stateDetail === 'killed' || a.stateDetail === 'killed by signal') return 'stopped'
  return a.exitCode === 0 ? 'finished' : 'failed'
}

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
      switch (exitKind(a)) {
        case 'stopped':
          return 'Stopped'
        case 'finished':
          return 'Finished'
        default:
          // Exit code -1 with no signal is how the host records a terminal it lost.
          return a.stateDetail === 'lost' || a.exitCode < 0 ? 'Lost: its terminal is gone' : `Exited with code ${a.exitCode}`
      }
    case AgentState.UNKNOWN:
      return 'Quiet for a while: state unknown'
    case AgentState.RETIRED:
      return 'Retired: stopped on purpose, a message wakes it'
    case AgentState.LIMITED:
      return `Hit a usage limit${a.stateDetail ? `: ${a.stateDetail}` : ''}; switch it to another model to go on`
    case AgentState.ERROR:
      return `Its last turn ended in an error${a.stateDetail ? `: ${a.stateDetail}` : ''}`
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

export interface Waiting {
  kind: 'plan' | 'question' | 'input'
  /** The agent that wrote the plan or asked, or is stopped at a prompt. */
  agent: Agent
  run?: Run
  /** The plan or question; unset for an agent that needs input in its terminal. */
  message?: Message
}

const KIND: Partial<Record<WaitingKind, Waiting['kind']>> = {
  [WaitingKind.PLAN]: 'plan',
  [WaitingKind.QUESTION]: 'question',
  [WaitingKind.INPUT]: 'input',
}

/**
 * What waits on the user, as the host's `ListWaiting` rows (a plan or question to the run's lead that nobody has
 * answered, and agents stopped at a prompt; the rule is the host's, daemon/internal/daemon/waiting.go), joined to
 * the agents and runs the app holds. A row whose agent hasn't reached the store yet is left out until it does.
 */
export function joinWaiting(rows: WaitingRow[], runs: Record<string, Run>, agents: Record<string, Agent>): Waiting[] {
  const out: Waiting[] = []
  for (const w of rows) {
    const kind = KIND[w.kind]
    const agent = agents[w.agentId]
    if (kind && agent) out.push({ kind, agent, run: runs[w.runId], message: w.message })
  }
  return out
}

/** The messages an agent sent or was sent, oldest first. */
export function messagesOf(messages: Record<string, Message>, agentId: string): Message[] {
  return Object.values(messages)
    .filter((m) => m.toId === agentId || m.from?.id === agentId)
    .sort(compareMessages)
}

export const MESSAGE_LABEL: Record<MessageType, string> = {
  [MessageType.UNSPECIFIED]: 'Message',
  [MessageType.TASK]: 'Task',
  [MessageType.PLAN]: 'Plan',
  [MessageType.QUESTION]: 'Question',
  [MessageType.ANSWER]: 'Answer',
  [MessageType.DONE]: 'Report',
  [MessageType.FAILED]: 'Failed',
  [MessageType.INFO]: 'Info',
  [MessageType.WAIT]: 'Waiting',
}
