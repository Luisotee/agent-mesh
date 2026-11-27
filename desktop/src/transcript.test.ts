import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { ReadTranscriptResponseSchema, TranscriptEntrySchema, TranscriptKind } from './gen/agentmesh/v1/chat_pb'
import { MessageSchema } from './gen/agentmesh/v1/types_pb'
import { emptyTranscript, fromTail, sameBlock, timeline, toBlocks, toolsSummary, withEarlier, withLater } from './transcript'

const entry = (id: string, kind: TranscriptKind, o: Partial<{ text: string; tool: string; toolInput: string; toolUseId: string; isError: boolean }> = {}) => create(TranscriptEntrySchema, { id, kind, ...o })
const page = (entries: ReturnType<typeof entry>[], o: Partial<{ sessionId: string; startOffset: bigint; endOffset: bigint; atStart: boolean; supported: boolean }> = {}) =>
  create(ReadTranscriptResponseSchema, { supported: true, sessionId: 's1', entries, startOffset: 0n, endOffset: 100n, ...o })

describe('following a transcript', () => {
  const held = fromTail(page([entry('a', TranscriptKind.USER, { text: 'hi' })], { startOffset: 50n, endOffset: 100n }))

  it('puts earlier entries before and later ones after, and moves its ends', () => {
    const back = withEarlier(held, page([entry('z', TranscriptKind.ASSISTANT)], { startOffset: 0n, endOffset: 50n, atStart: true }))
    expect(back.entries.map((e) => e.id)).toEqual(['z', 'a'])
    expect(back).toMatchObject({ start: 0n, end: 100n, atStart: true })
    const next = withLater(held, page([entry('b', TranscriptKind.ASSISTANT)], { startOffset: 100n, endOffset: 160n }))
    expect(next.kind === 'more' && next.transcript.entries.map((e) => e.id)).toEqual(['a', 'b'])
    expect(next.kind === 'more' && next.transcript.end).toBe(160n)
  })

  it('has nothing to do when nothing is new', () => {
    expect(withLater(held, page([], { startOffset: 100n, endOffset: 100n }))).toEqual({ kind: 'same' })
  })

  it('starts over for another conversation or a record that got shorter, and ignores earlier entries of another one', () => {
    expect(withLater(held, page([], { sessionId: 's2', endOffset: 10n }))).toEqual({ kind: 'reload' })
    // A read that began elsewhere than where the held entries end is not appended.
    expect(withLater(held, page([entry('b', TranscriptKind.ASSISTANT)], { startOffset: 40n, endOffset: 160n }), 40n)).toEqual({ kind: 'same' })
    expect(withLater(held, page([], { endOffset: 60n }))).toEqual({ kind: 'reload' })
    expect(withEarlier(held, page([entry('z', TranscriptKind.USER)], { sessionId: 's2' }))).toBe(held)
    // A page that doesn't end where the held entries begin (in flight across a reload) would repeat entries.
    expect(withEarlier(held, page([entry('z', TranscriptKind.USER)], { startOffset: 0n, endOffset: 20n }))).toBe(held)
  })

  it('knows a CLI the host reads no record for', () => {
    expect(fromTail(page([], { supported: false }))).toMatchObject({ supported: false, entries: [] })
    expect(emptyTranscript().supported).toBe(true)
  })
})

describe('toBlocks', () => {
  it('shows what a typed command printed as its own block, which ends a run of tool calls', () => {
    const blocks = toBlocks([
      entry('c', TranscriptKind.TOOL_CALL, { tool: 'Bash', toolUseId: 't' }),
      entry('u', TranscriptKind.USER, { text: '/context' }),
      entry('o', TranscriptKind.COMMAND_OUTPUT, { text: 'Context 12k/200k' }),
      entry('c2', TranscriptKind.TOOL_CALL, { tool: 'Read', toolUseId: 't2' }),
    ])
    expect(blocks.map((b) => b.kind)).toEqual(['tools', 'user', 'output', 'tools'])
    expect(blocks[2]).toMatchObject({ kind: 'output', entry: { text: 'Context 12k/200k' } })
  })

  it('folds consecutive tool calls into one block, each with its own result', () => {
    const blocks = toBlocks([
      entry('1', TranscriptKind.USER, { text: 'go' }),
      entry('2', TranscriptKind.ASSISTANT, { text: 'ok' }),
      entry('3', TranscriptKind.TOOL_CALL, { tool: 'Bash', toolInput: 'ls', toolUseId: 't1' }),
      entry('4', TranscriptKind.TOOL_RESULT, { text: 'a b', toolUseId: 't1' }),
      entry('5', TranscriptKind.TOOL_CALL, { tool: 'Read', toolInput: '/x', toolUseId: 't2' }),
      entry('6', TranscriptKind.TOOL_CALL, { tool: 'Bash', toolInput: 'pwd', toolUseId: 't3' }),
      entry('7', TranscriptKind.TOOL_RESULT, { text: 'boom', toolUseId: 't3', isError: true }),
      entry('8', TranscriptKind.TOOL_RESULT, { text: 'file', toolUseId: 't2' }),
      entry('9', TranscriptKind.MESH, { text: '📬 [mesh] …' }),
      entry('10', TranscriptKind.ASSISTANT, { text: 'done' }),
    ])
    expect(blocks.map((b) => b.kind)).toEqual(['user', 'assistant', 'tools', 'mesh', 'assistant'])
    const tools = blocks[2]!
    if (tools.kind !== 'tools') throw new Error('not tools')
    expect(tools.calls.map((c) => [c.tool, c.input, c.result?.text])).toEqual([['Bash', 'ls', 'a b'], ['Read', '/x', 'file'], ['Bash', 'pwd', 'boom']])
    expect(tools.calls[2]!.result!.isError).toBe(true)
    expect(toolsSummary(tools.calls)).toBe('Ran 3 tools: Bash ×2, Read')
  })

  it('shows a result whose call is before the window, and a call still waiting for its result', () => {
    const [b] = toBlocks([entry('r', TranscriptKind.TOOL_RESULT, { text: 'late', toolUseId: 'gone' }), entry('c', TranscriptKind.TOOL_CALL, { tool: 'Bash', toolUseId: 'open' })])
    if (b?.kind !== 'tools') throw new Error('not tools')
    expect(b.calls.map((c) => [c.tool, c.result?.text])).toEqual([['tool', 'late'], ['Bash', undefined]])
    expect(toolsSummary(b.calls.slice(1))).toBe('Ran 1 tool: Bash')
  })
})

describe('timeline', () => {
  const at = (s: number) => timestampFromMs(s * 1000)
  const e = (id: string, kind: TranscriptKind, s: number) => create(TranscriptEntrySchema, { id, kind, text: id, time: at(s) })
  const m = (id: string, s: number) => create(MessageSchema, { id, body: id, createdAt: at(s) })
  const bell = create(TranscriptEntrySchema, { id: 'bell', kind: TranscriptKind.MESH, text: '📬 [mesh] task from lead (ref t1): run `mesh inbox`', time: at(20) })
  const blocks = toBlocks([e('u1', TranscriptKind.USER, 10), bell, e('a1', TranscriptKind.ASSISTANT, 30)])

  it('puts each message where it was sent, and drops the doorbells once the messages are there', () => {
    const items = timeline(blocks, [m('late', 40), m('task', 19), m('early', 5)], false)
    // A message from before the window held is left for "Load earlier".
    expect(items.map((i) => i.id)).toEqual(['u1', 'm-task', 'a1', 'm-late'])
    expect(timeline(blocks, [m('early', 5)], true).map((i) => i.id)).toEqual(['m-early', 'u1', 'a1'])
  })

  it('drops only the doorbells, and keeps any other mesh line (a root told of its reset)', () => {
    const text = (id: string, kind: TranscriptKind, s: number, t: string) => create(TranscriptEntrySchema, { id, kind, text: t, time: at(s) })
    const bs = toBlocks([
      text('one', TranscriptKind.MESH, 1, '📬 [mesh] task from lead (ref ab12): run `mesh inbox`'),
      text('two', TranscriptKind.MESH, 2, '📬 [mesh] 2 messages (ref cd34: task from lead, info from mesh): run `mesh inbox`'),
      text('reset', TranscriptKind.MESH, 3, '📬 [mesh] You are lead, the root of agent-mesh run r1, and `mesh reset` just compacted your session. Then: answer it (ref t9): run `mesh inbox`'),
    ])
    expect(timeline(bs, [], true).map((i) => i.id)).toEqual(['reset'])
  })

  it('shows a message still waiting on the user from before the window held, at the top', () => {
    expect(timeline(blocks, [m('plan', 5), m('old', 6)], false, new Set(['plan'])).map((i) => i.id)).toEqual(['m-plan', 'u1', 'a1'])
  })

  it('keeps the doorbells while the messages are not read yet, and shows every message of a conversation it cannot read', () => {
    expect(timeline(blocks, undefined, false).map((i) => i.id)).toEqual(['u1', 'bell', 'a1'])
    expect(timeline([], [m('x', 5)], false).map((i) => i.id)).toEqual(['m-x'])
  })
})

describe('sameBlock', () => {
  const blocks = (entries: ReturnType<typeof entry>[]) => toBlocks(entries)

  it('takes a block built anew from the same entries for the same, so a chat block is not redrawn for it', () => {
    const make = () => blocks([entry('u1', TranscriptKind.USER, { text: 'run it' }), entry('c1', TranscriptKind.TOOL_CALL, { tool: 'Bash', toolInput: 'ls', toolUseId: 't1' }), entry('r1', TranscriptKind.TOOL_RESULT, { text: 'a b', toolUseId: 't1' }), entry('a1', TranscriptKind.ASSISTANT, { text: 'done' })])
    const [x, y] = [make(), make()]
    expect(x.every((b, i) => b !== y[i] && sameBlock(b, y[i]!))).toBe(true)
  })

  it('sees a reply that grew, a tool call that got its result, and a block of another kind', () => {
    const [a1] = blocks([entry('a1', TranscriptKind.ASSISTANT, { text: 'part' })])
    const [a2] = blocks([entry('a1', TranscriptKind.ASSISTANT, { text: 'part and more' })])
    expect(sameBlock(a1!, a2!)).toBe(false)
    const [open] = blocks([entry('c1', TranscriptKind.TOOL_CALL, { tool: 'Bash', toolInput: 'ls', toolUseId: 't1' })])
    const [done] = blocks([entry('c1', TranscriptKind.TOOL_CALL, { tool: 'Bash', toolInput: 'ls', toolUseId: 't1' }), entry('r1', TranscriptKind.TOOL_RESULT, { text: 'ok', toolUseId: 't1' })])
    expect(sameBlock(open!, done!)).toBe(false)
    expect(sameBlock(a1!, open!)).toBe(false)
    // The same text cut or not is another block.
    const [cut] = blocks([entry('c1', TranscriptKind.TOOL_CALL, { tool: 'Bash', toolInput: 'ls', toolUseId: 't1' }), create(TranscriptEntrySchema, { id: 'r1', kind: TranscriptKind.TOOL_RESULT, text: 'ok', toolUseId: 't1', truncated: true })])
    expect(sameBlock(done!, cut!)).toBe(false)
  })
})
