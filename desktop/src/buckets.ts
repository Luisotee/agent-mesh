import { AgentRole, AgentState, RunStatus, type Agent, type Run } from './gen/agentmesh/v1/types_pb'
import type { AgentPullRequests } from './gen/agentmesh/v1/pr_pb'
import { exitKind, type Waiting } from './derive'

/**
 * The status buckets of `docs/apps.md` ("Status buckets per workspace"): what a daily user triages by. An agent is in
 * exactly one, the first that fits.
 */
export type Bucket = 'needs' | 'failed' | 'review' | 'working' | 'idle' | 'done'

/** In the order the sidebar shows them: what asks for the user first. */
export const BUCKET_ORDER: readonly Bucket[] = ['needs', 'failed', 'review', 'working', 'idle', 'done']

export const BUCKET_LABEL: Record<Bucket, string> = {
  needs: 'Needs you',
  failed: 'Failed',
  review: 'Ready to review',
  working: 'Working',
  idle: 'Idle',
  done: 'Done',
}

/** What a bucket is decided from besides the agent: who waits on the user (`ListWaiting`) and the PRs (`ListPullRequests`). */
export interface BucketContext {
  waiting: ReadonlySet<string>
  pullRequests: Record<string, AgentPullRequests>
}

export const waitingIds = (rows: Waiting[]): Set<string> => new Set(rows.map((w) => w.agent.id))

const open = (p: { state: string; draft: boolean }) => p.state === 'open' && !p.draft

export function bucketOf(a: Agent, ctx: BucketContext): Bucket {
  if (a.state === AgentState.NEEDS_INPUT || ctx.waiting.has(a.id)) return 'needs'
  const failedExit = a.state === AgentState.EXITED && exitKind(a) === 'failed'
  if (a.state === AgentState.ERROR || a.state === AgentState.LIMITED || failedExit) return 'failed'
  if (a.state === AgentState.WORKING || a.state === AgentState.STARTING || a.state === AgentState.UNKNOWN) return 'working'
  // Not working and nothing wrong: a change worker with an open PR waits for the user's review; one whose PRs are
  // all merged or closed is done; the rest is told by whether its process is still there.
  const prs = ctx.pullRequests[a.id]?.pullRequests ?? []
  if (a.role === AgentRole.WORKER && !a.peer) {
    if (prs.some(open)) return 'review'
    if (prs.length > 0 && prs.every((p) => p.state !== 'open')) return 'done'
  }
  return a.state === AgentState.IDLE ? 'idle' : 'done'
}

export interface Group {
  key: string
  label: string
  /** A longer name (a project's full path). */
  title?: string
  agents: Agent[]
  /** How many agents are in each bucket. */
  counts: Record<Bucket, number>
}

const emptyCounts = (): Record<Bucket, number> => ({ needs: 0, failed: 0, review: 0, working: 0, idle: 0, done: 0 })

const ms = (a: Agent) => Number(a.createdAt?.seconds ?? 0n) * 1000

/** Agents by bucket first, then newest first within one. */
function byBucket(ctx: BucketContext) {
  return (a: Agent, b: Agent) => BUCKET_ORDER.indexOf(bucketOf(a, ctx)) - BUCKET_ORDER.indexOf(bucketOf(b, ctx)) || ms(b) - ms(a) || a.name.localeCompare(b.name)
}

function counted(agents: Agent[], ctx: BucketContext): Record<Bucket, number> {
  const c = emptyCounts()
  for (const a of agents) c[bucketOf(a, ctx)]++
  return c
}

/**
 * The agents of the runs still open (a closed run's agents are history, in the run view), grouped by the repo they work
 * in: a project's section holds its agents from every run, needing-you first.
 */
export function groupByProject(runs: Record<string, Run>, agents: Record<string, Agent>, ctx: BucketContext, repoName: (r: string) => string): Group[] {
  const by = new Map<string, Agent[]>()
  for (const a of Object.values(agents)) {
    if (runs[a.runId]?.status === RunStatus.CLOSED) continue
    by.set(a.repo, [...(by.get(a.repo) ?? []), a])
  }
  const groups = [...by.entries()].map(([repo, list]): Group => {
    const sorted = [...list].sort(byBucket(ctx))
    return { key: repo, label: repoName(repo) || 'No repo', title: repo, agents: sorted, counts: counted(sorted, ctx) }
  })
  // Projects with something that needs you first, then the busier, then by name.
  return groups.sort((a, b) => b.counts.needs - a.counts.needs || b.agents.length - a.agents.length || a.label.localeCompare(b.label))
}

/** The agents of the open runs by bucket, in the order of `BUCKET_ORDER`; an empty bucket is left out. */
export function groupByState(runs: Record<string, Run>, agents: Record<string, Agent>, ctx: BucketContext): Group[] {
  const by = new Map<Bucket, Agent[]>()
  for (const a of Object.values(agents)) {
    if (runs[a.runId]?.status === RunStatus.CLOSED) continue
    const b = bucketOf(a, ctx)
    by.set(b, [...(by.get(b) ?? []), a])
  }
  return BUCKET_ORDER.filter((b) => by.has(b)).map((b): Group => {
    const list = [...by.get(b)!].sort((x, y) => ms(y) - ms(x) || x.name.localeCompare(y.name))
    return { key: b, label: BUCKET_LABEL[b], agents: list, counts: { ...emptyCounts(), [b]: list.length } }
  })
}

/**
 * Why no PR is read on the host, as a sentence for the sidebar (`ListPullRequests`'s `githubProblem`), or nothing when
 * GitHub can be read. Without it "Ready to review" is empty for a reason the user can fix, and looks like there is nothing to review.
 */
export function githubNote(problem: string): string {
  switch (problem) {
    case 'no_gh':
      return "Ready to review is empty because the GitHub CLI (gh) isn't installed on the host: install it to see open pull requests."
    case 'not_logged_in':
      return "Ready to review is empty because the GitHub CLI (gh) on the host isn't logged in: run `gh auth login` there."
    default:
      return ''
  }
}
