import { TranscriptKind, type ReadTranscriptResponse, type TranscriptEntry } from './gen/agentmesh/v1/chat_pb'
import type { Message } from './gen/agentmesh/v1/types_pb'

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
  // A page of another conversation, or one that doesn't end where the held entries begin (the record was rewritten or
  // reloaded while it was out), would repeat or misplace entries.
  if (res.sessionId !== t.sessionId || res.endOffset !== t.start) return t
  return { ...t, entries: [...res.entries, ...t.entries], start: res.startOffset, atStart: res.atStart }
}

export type Followed = { kind: 'more'; transcript: Transcript } | { kind: 'same' } | { kind: 'reload' }

/**
 * Entries after the ones held, from a read that began at `from` (what the transcript held when it was asked: earlier
 * entries may have come in above since, so `t` is what is held now). A record that is another conversation now (a
 * reopen, `/clear`) or that ends before where we had read (it was rewritten) can't be followed: the view reads the tail
 * again. A read that began somewhere other than where the transcript ends now is not appended: it is `same`, and the
 * next read starts from the right place.
 */
export function withLater(t: Transcript, res: ReadTranscriptResponse, from: bigint = t.end): Followed {
  if (res.sessionId !== t.sessionId || res.endOffset < t.end) return { kind: 'reload' }
  if (from !== t.end) return { kind: 'same' }
  if (res.entries.length === 0 && res.endOffset === t.end) return { kind: 'same' }
  return { kind: 'more', transcript: { ...t, entries: [...t.entries, ...res.entries], end: res.endOffset } }
}

/** What the chat shows, in order: a prompt, a mesh doorbell, a reply, or a run of tool calls with their answers. */
export type Block =
  | { kind: 'user'; id: string; at: number; entry: TranscriptEntry }
  | { kind: 'mesh'; id: string; at: number; entry: TranscriptEntry }
  | { kind: 'assistant'; id: string; at: number; entry: TranscriptEntry }
  | { kind: 'tools'; id: string; at: number; calls: ToolCall[] }

const ms = (t?: { seconds: bigint; nanos: number }) => (t ? Number(t.seconds) * 1000 + Math.floor(t.nanos / 1e6) : 0)

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
        if (!run) blocks.push((run = { kind: 'tools', id: e.id, at: ms(e.time), calls: [] }))
        run.calls.push(call)
        break
      }
      case TranscriptKind.TOOL_RESULT: {
        const call = calls.get(e.toolUseId)
        if (call) call.result = e
        else {
          // A result whose call is before the window we hold: shown on its own, as a call we can't name.
          const orphan: ToolCall = { id: e.id, tool: 'tool', input: '', result: e }
          if (!run) blocks.push((run = { kind: 'tools', id: e.id, at: ms(e.time), calls: [] }))
          run.calls.push(orphan)
        }
        break
      }
      case TranscriptKind.USER:
        run = undefined
        blocks.push({ kind: 'user', id: e.id, at: ms(e.time), entry: e })
        break
      case TranscriptKind.MESH:
        run = undefined
        blocks.push({ kind: 'mesh', id: e.id, at: ms(e.time), entry: e })
        break
      case TranscriptKind.ASSISTANT:
        run = undefined
        blocks.push({ kind: 'assistant', id: e.id, at: ms(e.time), entry: e })
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

/** What the chat's timeline shows: the conversation's blocks and, where they were sent, the agent's mesh messages. */
export type Item = Block | { kind: 'message'; id: string; at: number; message: Message }

/**
 * The conversation with the agent's mesh messages (to it and from it) put in where they were sent, as cards. Once the
 * messages are there (`messages` given), the doorbells that announced them ("📬 [mesh] … run `mesh inbox`") go: the
 * cards stand for them. Only messages from the window held on are shown (`atStart`: all of them), so loading earlier
 * entries brings the earlier messages in with them; one still waiting on the user (`open`, its id) shows whatever the
 * window, at the top when it is older, so it can always be answered.
 */
export function timeline(blocks: Block[], messages: Message[] | undefined, atStart: boolean, open: ReadonlySet<string> = new Set()): Item[] {
  if (!messages) return blocks
  const from = atStart || blocks.length === 0 ? -Infinity : blocks[0]!.at
  const cards: Item[] = messages
    .map((m) => ({ kind: 'message' as const, id: `m-${m.id}`, at: ms(m.createdAt), message: m }))
    .filter((c) => c.at >= from || open.has(c.message.id))
    .sort((a, b) => a.at - b.at)
  const out: Item[] = []
  let i = 0
  for (const b of blocks) {
    while (i < cards.length && cards[i]!.at < b.at) out.push(cards[i++]!)
    if (b.kind !== 'mesh') out.push(b)
  }
  while (i < cards.length) out.push(cards[i++]!)
  return out
}
