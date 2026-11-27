import { AgentRole, AgentState, type Agent } from './gen/agentmesh/v1/types_pb'
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

/**
 * Why no PR is read on the host, as a sentence for the sidebar (`ListPullRequests`'s `githubProblem`), or nothing when
 * GitHub can be read. Without it the pull requests (and Watch all's "Ready to review") are missing for a reason the
 * user can fix, and look like there is nothing to review.
 */
export function githubNote(problem: string): string {
  switch (problem) {
    case 'no_gh':
      return "No pull request is read because the GitHub CLI (gh) isn't installed on the host: install it to see them."
    case 'not_logged_in':
      return "No pull request is read because the GitHub CLI (gh) on the host isn't logged in: run `gh auth login` there."
    default:
      return ''
  }
}
