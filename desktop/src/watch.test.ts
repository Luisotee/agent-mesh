import { create, type MessageInitShape } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'
import { TranscriptEntrySchema, TranscriptKind } from './gen/agentmesh/v1/chat_pb'
import { tailLines } from './watch'

const e = (id: string, kind: TranscriptKind, o: MessageInitShape<typeof TranscriptEntrySchema> = {}) => create(TranscriptEntrySchema, { id, kind, ...o })

describe('tailLines', () => {
  const entries = [
    e('1', TranscriptKind.USER, { text: 'fix the typo' }),
    e('2', TranscriptKind.ASSISTANT, { text: 'Looking at\n  the   file now.' }),
    e('3', TranscriptKind.TOOL_CALL, { tool: 'Read', toolUseId: 'a' }),
    e('4', TranscriptKind.TOOL_RESULT, { toolUseId: 'a', text: 'contents' }),
    e('5', TranscriptKind.TOOL_CALL, { tool: 'Edit', toolUseId: 'b' }),
    e('6', TranscriptKind.MESH, { text: '📬 [mesh] task from lead' }),
    e('7', TranscriptKind.ASSISTANT, { text: 'x'.repeat(300) }),
  ]

  it('gives a line each for prompts, replies, doorbells and folded tool calls, with whitespace folded', () => {
    expect(tailLines(entries, 10).map((l) => [l.who, l.text.slice(0, 30)])).toEqual([
      ['you', 'fix the typo'],
      ['agent', 'Looking at the file now.'],
      ['tools', 'Ran 2 tools: Read, Edit'],
      ['mesh', '📬 [mesh] task from lead'],
      ['agent', 'x'.repeat(30)],
    ])
  })

  it('leaves out what a typed command printed: it is neither the agent speaking nor a tool', () => {
    const lines = tailLines([e('1', TranscriptKind.USER, { text: '/context' }), e('2', TranscriptKind.COMMAND_OUTPUT, { text: 'Context 12k/200k' })], 10)
    expect(lines.map((l) => l.who)).toEqual(['you'])
  })

  it('keeps the last lines only, and cuts a long one to the width', () => {
    const lines = tailLines(entries, 2, 50)
    expect(lines.map((l) => l.who)).toEqual(['mesh', 'agent'])
    expect(lines[1]!.text).toHaveLength(50)
    expect(lines[1]!.text.endsWith('…')).toBe(true)
  })

  it('has nothing for an empty record, and drops an entry with no text', () => {
    expect(tailLines([])).toEqual([])
    expect(tailLines([e('1', TranscriptKind.ASSISTANT, { text: '' })])).toEqual([])
  })
})
