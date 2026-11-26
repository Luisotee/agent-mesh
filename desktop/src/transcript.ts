import { TranscriptKind, type ReadTranscriptResponse, type TranscriptEntry } from './gen/agentmesh/v1/chat_pb'

/**
 * What the chat view holds of an agent's conversation: the entries read so far (a window of its CLI's record) and
 * where that window begins and ends, so it can read earlier ones and follow new ones. Pure functions over it, so
 * what reading back, following and a rewritten record do is checked without a window.
 */
export interface Transcript {
  /** False when the host reads no record for this CLI. */
  supported: boolean
  sessionId: string
  entries: TranscriptEntry[]
  start: bigint
  end: bigint
  atStart: boolean
}

export const emptyTranscript = (): Transcript => ({ supported: true, sessionId: '', entries: [], start: 0n, end: 0n, atStart: false })

/** The first read: the tail of the record. It replaces whatever was held. */
export function fromTail(res: ReadTranscriptResponse): Transcript {
  return { supported: res.supported, sessionId: res.sessionId, entries: res.entries, start: res.startOffset, end: res.endOffset, atStart: res.atStart }
}

/** Earlier entries, put before the ones held. */
export function withEarlier(t: Transcript, res: ReadTranscriptResponse): Transcript {
  if (res.sessionId !== t.sessionId) return t
  return { ...t, entries: [...res.entries, ...t.entries], start: res.startOffset, atStart: res.atStart }
}

export type Followed = { kind: 'more'; transcript: Transcript } | { kind: 'same' } | { kind: 'reload' }

/**
 * Entries after the ones held. A record that is another conversation now (a reopen, `/clear`) or that ends before where
 * we had read (it was rewritten) can't be followed: the view reads the tail again.
 */
export function withLater(t: Transcript, res: ReadTranscriptResponse): Followed {
  if (res.sessionId !== t.sessionId || res.endOffset < t.end) return { kind: 'reload' }
  if (res.entries.length === 0 && res.endOffset === t.end) return { kind: 'same' }
  return { kind: 'more', transcript: { ...t, entries: [...t.entries, ...res.entries], end: res.endOffset } }
}

/** What the chat shows, in order: a prompt, a mesh doorbell, a reply, or a run of tool calls with their answers. */
export type Block =
  | { kind: 'user'; id: string; entry: TranscriptEntry }
  | { kind: 'mesh'; id: string; entry: TranscriptEntry }
  | { kind: 'assistant'; id: string; entry: TranscriptEntry }
  | { kind: 'tools'; id: string; calls: ToolCall[] }

export interface ToolCall {
  id: string
  tool: string
  input: string
  /** What the tool answered; unset until it did. */
  result?: TranscriptEntry
}

/** Entries as blocks: consecutive tool calls fold into one block, each call with the result that carries its id. */
export function toBlocks(entries: TranscriptEntry[]): Block[] {
  const blocks: Block[] = []
  const calls = new Map<string, ToolCall>()
  let run: Extract<Block, { kind: 'tools' }> | undefined
  for (const e of entries) {
    switch (e.kind) {
      case TranscriptKind.TOOL_CALL: {
        const call: ToolCall = { id: e.id, tool: e.tool, input: e.toolInput }
        calls.set(e.toolUseId, call)
        if (!run) blocks.push((run = { kind: 'tools', id: e.id, calls: [] }))
        run.calls.push(call)
        break
      }
      case TranscriptKind.TOOL_RESULT: {
        const call = calls.get(e.toolUseId)
        if (call) call.result = e
        else {
          // A result whose call is before the window we hold: shown on its own, as a call we can't name.
          const orphan: ToolCall = { id: e.id, tool: 'tool', input: '', result: e }
          if (!run) blocks.push((run = { kind: 'tools', id: e.id, calls: [] }))
          run.calls.push(orphan)
        }
        break
      }
      case TranscriptKind.USER:
        run = undefined
        blocks.push({ kind: 'user', id: e.id, entry: e })
        break
      case TranscriptKind.MESH:
        run = undefined
        blocks.push({ kind: 'mesh', id: e.id, entry: e })
        break
      case TranscriptKind.ASSISTANT:
        run = undefined
        blocks.push({ kind: 'assistant', id: e.id, entry: e })
        break
    }
  }
  return blocks
}

/** "Ran 3 tools: Bash ×2, Read", for the folded block. */
export function toolsSummary(calls: ToolCall[]): string {
  const counts = new Map<string, number>()
  for (const c of calls) counts.set(c.tool, (counts.get(c.tool) ?? 0) + 1)
  const names = [...counts].map(([t, n]) => (n > 1 ? `${t} ×${n}` : t))
  return `Ran ${calls.length} ${calls.length === 1 ? 'tool' : 'tools'}: ${names.join(', ')}`
}
