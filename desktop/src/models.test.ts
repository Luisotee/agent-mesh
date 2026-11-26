import { create } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'
import { AgentSchema, ReopenSchema } from './gen/agentmesh/v1/types_pb'
import { familiesOf, pendingText, refusal, runsOn } from './models'

describe('models', () => {
  it('says what an agent runs on', () => {
    expect(runsOn(create(AgentSchema, { kind: 'claude', model: 'sonnet', effort: 'high' }))).toBe('claude · sonnet · high')
    expect(runsOn(create(AgentSchema, { kind: 'agy' }))).toBe('agy')
  })

  it('says what a reopen under way will run, naming only what changes', () => {
    const a = (reopen?: Parameters<typeof create<typeof ReopenSchema>>[1]) => create(AgentSchema, { model: 'sonnet', effort: 'high', pendingReopen: reopen ? create(ReopenSchema, reopen) : undefined })
    expect(pendingText(a())).toBe('')
    expect(pendingText(a({ model: 'opus', effort: 'high', why: 'moved' }))).toBe('Switching to opus (moved)…')
    expect(pendingText(a({ model: 'sonnet', effort: 'max', why: 'moved' }))).toBe('Switching to max effort (moved)…')
    expect(pendingText(a({ model: 'opus', effort: 'max' }))).toBe('Switching to opus at max effort…')
    expect(pendingText(a({ model: 'sonnet', effort: 'high', why: 'resumed' }))).toBe('Reopening (resumed)…')
  })

  it('shows the host\'s words for a refusal, without its code', () => {
    expect(refusal(new Error('[failed_precondition] app-fix already runs on opus at high effort'))).toBe('app-fix already runs on opus at high effort')
    expect(refusal('plain')).toBe('plain')
  })

  it('lists the families of one kind', () => {
    const kinds = [{ kind: 'claude', listed: [{ family: 'opus' }, { family: 'sonnet' }] }, { kind: 'agy', listed: [{ family: 'neon' }] }]
    expect(familiesOf(kinds, 'claude')).toEqual(['opus', 'sonnet'])
    expect(familiesOf(kinds, 'copilot')).toEqual([])
  })
})
