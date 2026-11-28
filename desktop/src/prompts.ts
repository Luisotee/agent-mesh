import type { PromptQuestion } from './gen/agentmesh/v1/chat_pb'
import type { PendingPermission } from './gen/agentmesh/v1/mesh_pb'

/**
 * The prompts the host holds for the user (`ListPermissions`, `for_user`): a question an agent asked with its
 * AskUserQuestion tool, a plan to approve, a tool it wants to run. Pure functions over them, so what a form
 * sends back is checked without a window.
 */

/** What the user chose for one question: the options picked, and the text written under "Other". */
export interface Pick {
  labels: string[]
  other: string
}
export type Picks = Record<string, Pick>

const NONE: Pick = { labels: [], other: '' }
const of = (picks: Picks, q: PromptQuestion): Pick => picks[q.question] ?? NONE

/** The prompts of one agent only the user answers. */
export function promptsFor(permissions: PendingPermission[], agentId: string): PendingPermission[] {
  return permissions.filter((p) => p.forUser && p.agentId === agentId)
}

/** Picks an option: a single choice replaces what was chosen (and the text written, which is another choice), a multi-select toggles. */
export function pickOption(picks: Picks, q: PromptQuestion, label: string): Picks {
  const cur = of(picks, q)
  if (!q.multiSelect) return { ...picks, [q.question]: { labels: [label], other: '' } }
  const labels = cur.labels.includes(label) ? cur.labels.filter((l) => l !== label) : [...cur.labels, label]
  return { ...picks, [q.question]: { ...cur, labels } }
}

/** Writes under "Other": for a single choice that is the answer instead of an option, for a multi-select it joins the options. */
export function writeOther(picks: Picks, q: PromptQuestion, text: string): Picks {
  const cur = of(picks, q)
  return { ...picks, [q.question]: { labels: !q.multiSelect && text.trim() ? [] : cur.labels, other: text } }
}

export const isAnswered = (q: PromptQuestion, picks: Picks): boolean => {
  const p = of(picks, q)
  return p.labels.length > 0 || p.other.trim() !== ''
}

/** Whether every question has an answer: Submit waits for it. */
export const isComplete = (questions: PromptQuestion[], picks: Picks): boolean => questions.every((q) => isAnswered(q, picks))

/** The answers as `DecidePermission` takes them: one for each question, the text written trimmed. */
export function answersOf(questions: PromptQuestion[], picks: Picks) {
  return questions.map((q) => {
    const p = of(picks, q)
    return { question: q.question, labels: p.labels, other: p.other.trim() }
  })
}

/** The preview to show beside a question's options: the one being looked at, else the chosen one, else the first. */
export function previewOf(q: PromptQuestion, picks: Picks, peek?: string): string {
  const label = peek ?? of(picks, q).labels[0]
  return (q.options.find((o) => o.label === label) ?? q.options[0])?.preview ?? ''
}

/** Whether any option of a question has a preview (then the options and the preview sit side by side). */
export const hasPreviews = (q: PromptQuestion): boolean => q.options.some((o) => o.preview !== '')

/**
 * The answers Claude recorded for a question call, from its result (`Your questions have been answered: "Q"="A", …`
 * from the terminal, `The user answered: "Q"="A", …` from an app), by question text. Empty when the text has no such pairs
 * (a dismissed question, a result in another shape): the caller shows the text itself.
 */
export function answersFromResult(text: string, questions: PromptQuestion[]): Map<string, string> {
  const out = new Map<string, string>()
  for (const q of questions) {
    const at = text.indexOf(`"${q.question}"="`)
    if (at < 0) continue
    const start = at + q.question.length + 4
    // The answer ends at the `", ` that starts the next pair (or at the closing quote of the last).
    const next = questions.map((o) => text.indexOf(`", "${o.question}"="`, start)).filter((i) => i >= 0)
    const end = next.length > 0 ? Math.min(...next) : text.lastIndexOf('". ')
    out.set(q.question, text.slice(start, end >= 0 ? end : text.lastIndexOf('"')))
  }
  return out
}

