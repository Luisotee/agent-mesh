import { compareMessages } from './order'
import { AgentRole, AgentState, MessageType, RunStatus, type Agent, type Message, type Run } from './gen/agentmesh/v1/types_pb'
import { AgentWaitKind, WaitingKind, type AgentWait, type Waiting as WaitingRow } from './gen/agentmesh/v1/mesh_pb'

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
 * How an exited agent ended, from what the host stores of it: a clean exit, a stop (a signal, or a stop recorded for a
 * process already gone: `killed by signal`, `stopped`), and anything else, which includes a lost terminal (`lost`, or
 * exit code -1 with no detail, as an older host stored it). The live `agentExited` event's reason (`killed`, `stopped`,
 * `lost`, `reopen failed: …`) stands in for the detail until the app reloads the agent.
 */
export function exitKind(a: Agent): ExitKind {
  if (a.stateDetail === 'lost') return 'failed'
  if (a.stateDetail === 'killed' || a.stateDetail === 'killed by signal' || a.stateDetail === 'stopped') return 'stopped'
  return a.exitCode === 0 ? 'finished' : 'failed'
}

/**
 * What an idle agent waits on, in words (`GetAgentProgress`'s wait): its background job, a wait it declared (with when it
 * ends), the answer to its question or plan, nothing after a report, or a message, from whom when the host says.
 */
export function waitSentence(w: AgentWait): string {
  const cut = (s: string) => (s.length > 60 ? `${s.slice(0, 59)}…` : s)
  switch (w.kind) {
    case AgentWaitKind.BACKGROUND:
      return w.detail ? `Waiting on its background job: ${cut(w.detail)}` : 'Waiting on a background job'
    case AgentWaitKind.DECLARED: {
      const until = w.until ? new Date(Number(w.until.seconds) * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : ''
      return `Waiting${w.detail ? `: ${cut(w.detail)}` : ''}${until ? `, until ${until}` : ''}`
    }
    case AgentWaitKind.ANSWER:
      return w.from === 'user' ? 'Waiting for your answer' : `Asked ${w.from || 'its parent'}, waiting for the answer`
    case AgentWaitKind.REPORTED:
      return `Reported to ${w.from || 'its parent'}, nothing owed`
    default:
      return !w.from || w.from === 'user' ? 'Idle, waiting for you' : `Idle, waiting for a task from ${w.from}`
  }
}

/** One plain sentence for an agent's state; an idle one says what it waits on when the host's `wait` is known. */
export function statusSentence(a: Agent, wait?: AgentWait): string {
  switch (a.state) {
    case AgentState.STARTING:
      return 'Starting up'
    case AgentState.IDLE:
      return wait ? waitSentence(wait) : 'Idle, waiting for a message'
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
          // The host says why when it can (`reopen failed: …`, `launch failed: …`); exit code -1 with nothing said is a lost terminal.
          if (a.stateDetail && a.stateDetail !== 'lost' && a.stateDetail !== 'exited') return `Exited: ${a.stateDetail}`
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

/** A folder as one spelling: no repeated or trailing slashes ("/" stays "/"). */
export const cleanPath = (p: string) => {
  const c = p.replace(/\/{2,}/g, '/').replace(/\/+$/, '')
  return c === '' && p.startsWith('/') ? '/' : c
}

/** Whether `tilde` is `~/x` and `abs` is `x` under a home folder (`/home/<user>`, `/Users/<user>`, `/root`). */
function tildeOf(tilde: string, abs: string): boolean {
  if (!tilde.startsWith('~/')) return false
  const rest = tilde.slice(1)
  return abs.endsWith(rest) && /^(\/home\/[^/]+|\/Users\/[^/]+|\/root)$/.test(abs.slice(0, abs.length - rest.length))
}

/**
 * Whether two spellings of a folder (`~/projects/shop`, `/home/u/projects/shop/`) name the same folder, in either
 * order: a setting's entry and a repo's folder. The host decides this itself (it expands `~` and resolves
 * symlinks); the app has no home folder to expand with, so a `~/x` is taken to match `x` under a home folder
 * (`/home/<user>`, `/Users/<user>`, `/root`). A symlink the host resolves is not seen here, so it shows as a row of its own.
 */
export function sameRepo(a: string, b: string): boolean {
  const x = cleanPath(a)
  const y = cleanPath(b)
  return x === y || tildeOf(x, y) || tildeOf(y, x)
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

/**
 * What waits on the user, as the number every place shows (the list, the host's badge, the tray): the host's waiting rows, with
 * an agent stopped at a prompt the host holds for its lead counted once, as the prompt it is, and a held prompt of an agent
 * the app doesn't have yet left out.
 */
export function waitingCount(rows: WaitingRow[], permissions: Array<{ agentId: string }>, runs: Record<string, Run>, agents: Record<string, Agent>): number {
  const held = new Set(permissions.map((p) => p.agentId))
  const items = joinWaiting(rows, runs, agents).filter((w) => !(w.kind === 'input' && held.has(w.agent.id)))
  return items.length + permissions.filter((p) => agents[p.agentId]).length
}
