import { AgentRole, AgentState, MessageType, RunStatus, type Agent, type Message, type Run } from './gen/agentmesh/v1/types_pb'

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
      if (a.stateDetail === 'killed' || a.exitCode < 0) return 'Stopped'
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

export interface Waiting {
  kind: 'plan' | 'question' | 'input'
  /** The agent that wrote the plan or asked, or is stopped at a prompt. */
  agent: Agent
  run?: Run
  /** The plan or question; unset for an agent that needs input in its terminal. */
  message?: Message
}

const when = (m: Message): number => Number(m.createdAt?.seconds ?? 0n) * 1000 + Math.floor((m.createdAt?.nanos ?? 0) / 1e6)
const before = (a: Message, b: Message): boolean => when(a) < when(b) || (when(a) === when(b) && a.id < b.id)

/**
 * What waits on the user (what `mesh status` calls waiting rows, for the person): a plan or a question sent to
 * the run's lead that nobody has answered since, from an agent that is still there to hear the answer, and every
 * agent stopped at a prompt in its terminal. The user may answer in the lead's place, and the lead answering
 * first closes the item. Plans and questions come oldest first, then the prompts.
 */
export function waitingOnYou(runs: Record<string, Run>, agents: Record<string, Agent>, messages: Record<string, Message>): Waiting[] {
  const byRun = new Map<string, Message[]>()
  for (const m of Object.values(messages)) byRun.set(m.runId, [...(byRun.get(m.runId) ?? []), m])
  const items: Array<Waiting & { message: Message }> = []
  for (const run of Object.values(runs)) {
    if (run.status !== RunStatus.OPEN) continue
    const lead = Object.values(agents)
      .filter((a) => a.runId === run.id && a.role === AgentRole.ROOT)
      .sort(order)[0]
    if (!lead) continue
    const msgs = byRun.get(run.id) ?? []
    for (const m of msgs) {
      const kind = m.type === MessageType.PLAN ? 'plan' : m.type === MessageType.QUESTION ? 'question' : undefined
      if (!kind || m.toId !== lead.id) continue
      const author = agents[m.from?.id ?? '']
      if (!author || author.state === AgentState.EXITED) continue
      const answered = msgs.some((x) => x.type === MessageType.ANSWER && x.toId === author.id && before(m, x))
      if (!answered) items.push({ kind, agent: author, run, message: m })
    }
  }
  items.sort((a, b) => (before(a.message, b.message) ? -1 : 1))
  const prompts: Waiting[] = Object.values(agents)
    .filter((a) => a.state === AgentState.NEEDS_INPUT)
    .sort(order)
    .map((agent) => ({ kind: 'input' as const, agent, run: runs[agent.runId] }))
  return [...items, ...prompts]
}

/** The messages an agent sent or was sent, oldest first. */
export function messagesOf(messages: Record<string, Message>, agentId: string): Message[] {
  return Object.values(messages)
    .filter((m) => m.toId === agentId || m.from?.id === agentId)
    .sort((a, b) => (before(a, b) ? -1 : 1))
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
