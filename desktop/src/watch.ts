import { TranscriptKind, type TranscriptEntry } from './gen/agentmesh/v1/chat_pb'
import { toBlocks, toolsSummary } from './transcript'

/** One line of an agent's recent conversation, for a tile of the grid. */
export interface TailLine {
  id: string
  /** Who or what: the user's prompt, a mesh message, the agent's reply, or tool calls. */
  who: 'you' | 'mesh' | 'agent' | 'tools'
  text: string
}

const clip = (s: string, n: number) => {
  const one = s.replace(/\s+/g, ' ').trim()
  return one.length > n ? `${one.slice(0, n - 1)}…` : one
}

/** The last `n` things said or done in a conversation, a line each (a reply or prompt cut to its first words, tool calls folded). */
export function tailLines(entries: TranscriptEntry[], n = 5, width = 140): TailLine[] {
  const lines: TailLine[] = []
  for (const b of toBlocks(entries)) {
    if (b.kind === 'tools') lines.push({ id: b.id, who: 'tools', text: toolsSummary(b.calls) })
    else if (b.entry.kind === TranscriptKind.USER) lines.push({ id: b.id, who: 'you', text: clip(b.entry.text, width) })
    else if (b.entry.kind === TranscriptKind.MESH) lines.push({ id: b.id, who: 'mesh', text: clip(b.entry.text, width) })
    else lines.push({ id: b.id, who: 'agent', text: clip(b.entry.text, width) })
  }
  return lines.filter((l) => l.text).slice(-n)
}
