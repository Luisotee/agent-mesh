import type { GetAgentChangesResponse, IgnoredEntry, PlanItem } from './gen/agentmesh/v1/mesh_pb'
import type { Agent } from './gen/agentmesh/v1/types_pb'
import { AgentRole } from './gen/agentmesh/v1/types_pb'

/** What the side panel says of an agent's work: its plan, its context, and what its worktree holds. Nothing here asks the host for anything. */

/** How far the agent's own todo list is: the items done of all, and the one it is on. */
export function planSummary(plan: PlanItem[]): { done: number; total: number; current?: string } {
  const done = plan.filter((p) => p.status === 'completed').length
  return { done, total: plan.length, current: plan.find((p) => p.status === 'in_progress')?.content }
}

const k = (n: number) => (n >= 1_000_000 ? `${(n / 1_000_000).toFixed(n % 1_000_000 === 0 ? 0 : 1)}M` : n >= 1000 ? `${Math.round(n / 1000)}k` : String(n))

/** The context a conversation holds, against the model's window: "128k of 200k tokens (64%)"; nothing while the host doesn't know. */
export function contextMeter(tokens: bigint | number, window: bigint | number): { pct: number; text: string } | undefined {
  const t = Number(tokens)
  const w = Number(window)
  if (!(t > 0) || !(w > 0)) return undefined
  const pct = Math.min(100, Math.round((t / w) * 100))
  return { pct, text: `${k(t)} of ${k(w)} tokens (${pct}%)` }
}

/** A size as the meter says it: 84k, 1.2M. */
export const sizeText = k

/** Where on the meter (0 to 100) a mark of the daemon falls: nothing for one past the window. */
export const markPct = (tokens: number, window: number) => (window > 0 && tokens > 0 && tokens < window ? (tokens / window) * 100 : undefined)

/**
 * How the meter is coloured: amber once the conversation passes the first point where the mesh acts on it (the first
 * mark), red from 90% of the window, where the CLI itself will compact soon. With no mark, amber from 70% as before.
 */
export function contextTone(tokens: number, window: number, marks: number[]): 'ok' | 'warn' | 'over' {
  if (window > 0 && tokens / window >= 0.9) return 'over'
  const first = marks.length > 0 ? Math.min(...marks) : window * 0.7
  return tokens >= first ? 'warn' : 'ok'
}

const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`

/** One line for what the worktree holds: "2 files uncommitted · 1 commit not pushed", or that it is all on a remote. */
export function changeSummary(c: GetAgentChangesResponse): string {
  if (!c.available) return c.reason || 'Nothing to read.'
  const parts: string[] = []
  if (c.uncommitted > 0) parts.push(`${plural(c.uncommitted, 'file')} uncommitted`)
  if (c.unpushed > 0) parts.push(`${plural(c.unpushed, 'commit')} not pushed`)
  if (parts.length === 0) parts.push(c.commits > 0 || c.files.length > 0 ? 'everything is committed and pushed' : 'no changes yet')
  return parts.join(' · ')
}

const size = (n: bigint | number, partial: boolean): string => {
  const b = Number(n)
  const text = b >= 1 << 30 ? `${(b / (1 << 30)).toFixed(1)} GB` : b >= 1 << 20 ? `${(b / (1 << 20)).toFixed(1)} MB` : b >= 1 << 10 ? `${Math.round(b / (1 << 10))} KB` : `${b} B`
  return partial ? `at least ${text}` : text
}

/** The git-ignored files archiving would delete, one line each ("node_modules/ (212.4 MB)") and how many more the host did not list. */
export function ignoredLines(c: GetAgentChangesResponse): { lines: { path: string; size: string }[]; more: number } {
  return { lines: c.ignored.map((e: IgnoredEntry) => ({ path: e.path, size: size(e.bytes, e.partial) })), more: c.ignoredMore }
}

export const STATUS_MARK: Record<string, string> = { added: 'A', modified: 'M', deleted: 'D', renamed: 'R', untracked: '?' }

/**
 * What archiving this agent needs the user to know: `blocked` when the host would refuse whatever is chosen (uncommitted
 * work is never discarded), `warn` for commits that live only on this machine (archiving removes the worktree, not the
 * branch, but a branch that was never pushed is nowhere else).
 */
export function archiveAdvice(c: GetAgentChangesResponse | undefined): { blocked?: string; warn?: string } {
  if (!c) return {}
  if (!c.available) return { blocked: `Its work can't be read (${c.reason || 'git would not say'}), so nothing is archived on a guess.` }
  if (c.uncommitted > 0) return { blocked: `${plural(c.uncommitted, 'file')} ${c.uncommitted === 1 ? 'has' : 'have'} uncommitted changes. Commit or discard ${c.uncommitted === 1 ? 'it' : 'them'} first: archiving never deletes uncommitted work.` }
  if (c.unpushed > 0) return { warn: `${plural(c.unpushed, 'commit')} on its branch ${c.unpushed === 1 ? 'is' : 'are'} on no remote. The branch is kept, on this machine only: push it first if it matters.` }
  return {}
}

/** Whether the host would archive this agent at all: a worker of the daemon's own (not a root, asker or reviewer, a peer or an adopted session). */
export function canArchive(a: Agent): boolean {
  return a.role === AgentRole.WORKER && !a.peer && a.adoptedPid === 0 && !!a.worktree
}
