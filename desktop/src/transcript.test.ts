import { create } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'
import { ReadTranscriptResponseSchema, TranscriptEntrySchema, TranscriptKind } from './gen/agentmesh/v1/chat_pb'
import { emptyTranscript, fromTail, toBlocks, toolsSummary, withEarlier, withLater } from './transcript'

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
  })

  it('knows a CLI the host reads no record for', () => {
    expect(fromTail(page([], { supported: false }))).toMatchObject({ supported: false, entries: [] })
    expect(emptyTranscript().supported).toBe(true)
  })
})

describe('toBlocks', () => {
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
