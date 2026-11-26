import type { Agent } from './gen/agentmesh/v1/types_pb'

/** The effort levels the host takes, lowest first (`daemon/internal/settings/schema.go`, `Efforts`). */
export const EFFORTS = ['low', 'medium', 'high', 'xhigh', 'max'] as const

/** "claude · sonnet · high": the CLI, model and effort an agent runs on, as far as the host says. */
export function runsOn(a: Pick<Agent, 'kind' | 'model' | 'effort'>): string {
  return [a.kind, a.model, a.effort].filter(Boolean).join(' · ')
}

/** What a reopen under way will run on, for "switching to …" (the host fills the model and effort it resolved). */
export function pendingText(a: Pick<Agent, 'pendingReopen' | 'model' | 'effort'>): string {
  const r = a.pendingReopen
  if (!r || (!r.model && !r.effort)) return ''
  const model = r.model && r.model !== a.model ? r.model : ''
  const effort = r.effort && r.effort !== a.effort ? `${r.effort} effort` : ''
  const to = [model, effort].filter(Boolean).join(' at ')
  const why = r.why ? ` (${r.why})` : ''
  return to ? `Switching to ${to}${why}…` : `Reopening${why}…`
}

/** A host's refusal as its words, without the code in front of it (`[failed_precondition] x already runs on …`). */
export const refusal = (e: unknown): string => (e instanceof Error ? e.message.replace(/^\[[a-z_]+\]\s*/, '') : String(e))

/** The families the host lists for a CLI kind, from a `CheckModels` answer. */
export function familiesOf(kinds: Array<{ kind: string; listed: Array<{ family: string }> }>, kind: string): string[] {
  return kinds.find((k) => k.kind === kind)?.listed.map((l) => l.family) ?? []
}
