import { create } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'
import { archiveAdvice, changeSummary, contextMeter, planSummary } from './agent-work'
import { ChangedFileSchema, GetAgentChangesResponseSchema, PlanItemSchema } from './gen/agentmesh/v1/mesh_pb'
import { AgentSchema, EventSchema } from './gen/agentmesh/v1/types_pb'
import { applyFrame, emptyHost } from './store'

const changes = (o: Parameters<typeof create<typeof GetAgentChangesResponseSchema>>[1]) => create(GetAgentChangesResponseSchema, { available: true, ...o })

describe('planSummary', () => {
  it('counts what is done and names the item in progress', () => {
    const plan = [['a', 'completed'], ['b', 'in_progress'], ['c', 'pending']].map(([content, status]) => create(PlanItemSchema, { content, status }))
    expect(planSummary(plan)).toEqual({ done: 1, total: 3, current: 'b' })
    expect(planSummary([])).toEqual({ done: 0, total: 0, current: undefined })
  })
})

describe('contextMeter', () => {
  it('says how much of the window is used, in thousands, and caps at 100', () => {
    expect(contextMeter(128_000n, 200_000n)).toEqual({ pct: 64, text: '128k of 200k tokens (64%)' })
    expect(contextMeter(250_000, 200_000)?.pct).toBe(100)
    expect(contextMeter(400_000, 1_000_000)?.text).toBe('400k of 1M tokens (40%)')
    expect(contextMeter(900, 200_000)?.text).toBe('900 of 200k tokens (0%)')
  })
  it('shows nothing while the host does not know', () => {
    expect(contextMeter(0n, 200_000n)).toBeUndefined()
    expect(contextMeter(5000n, 0n)).toBeUndefined()
  })
})

describe('changeSummary and archiveAdvice', () => {
  it('says what is uncommitted and what is on no remote', () => {
    expect(changeSummary(changes({ uncommitted: 2, unpushed: 1, commits: 3 }))).toBe('2 files uncommitted · 1 commit not pushed')
    expect(changeSummary(changes({ commits: 2, files: [create(ChangedFileSchema, { path: 'a' })] }))).toBe('everything is committed and pushed')
    expect(changeSummary(changes({}))).toBe('no changes yet')
    expect(changeSummary(create(GetAgentChangesResponseSchema, { reason: 'it has no worktree of its own' }))).toBe('it has no worktree of its own')
  })
  it('blocks the archive for uncommitted work, warns for commits on no remote, and is silent otherwise', () => {
    expect(archiveAdvice(changes({ uncommitted: 1 })).blocked).toMatch(/1 file has uncommitted changes/)
    expect(archiveAdvice(changes({ uncommitted: 2, unpushed: 1 })).warn).toBeUndefined()
    expect(archiveAdvice(changes({ unpushed: 3 })).warn).toMatch(/3 commits on its branch are on no remote/)
    expect(archiveAdvice(changes({}))).toEqual({})
    expect(archiveAdvice(undefined)).toEqual({})
  })
})

describe('the store keeps an archived agent', () => {
  it('marks it from the event, and clears it when the agent is reopened', () => {
    const s = { ...emptyHost(), agents: { a: create(AgentSchema, { id: 'a' }) } }
    const archived = create(EventSchema, { agentId: 'a', seq: 1n, generation: 'g', payload: { case: 'agentArchived', value: { $typeName: 'agentmesh.v1.AgentArchived', worktree: '/w', branch: 'b', unpushed: 0, forced: false } } })
    const s2 = applyFrame(s, { case: 'event', value: archived })
    expect(s2.agents.a!.archived).toBe(true)
    const reopened = create(EventSchema, { agentId: 'a', seq: 2n, generation: 'g', payload: { case: 'agentReopened', value: { $typeName: 'agentmesh.v1.AgentReopened', terminalId: 't', sessionId: 's' } } })
    expect(applyFrame(s2, { case: 'event', value: reopened }).agents.a!.archived).toBe(false)
  })
})
