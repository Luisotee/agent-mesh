import { create } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'
import { AgentRole, AgentSchema, AgentState } from './gen/agentmesh/v1/types_pb'
import { isActive } from './active'

const agent = (state: AgentState, role = AgentRole.WORKER) => create(AgentSchema, { id: 'a', name: 'a', state, role })
const no = { waiting: false, keep: false }

describe('isActive', () => {
  it('counts what works, starts, needs input, is limited, failed or unknown as live', () => {
    for (const s of [AgentState.WORKING, AgentState.STARTING, AgentState.NEEDS_INPUT, AgentState.LIMITED, AgentState.ERROR, AgentState.UNKNOWN]) expect(isActive(agent(s), no), String(s)).toBe(true)
  })

  it('counts an idle agent only when it waits on an answer', () => {
    expect(isActive(agent(AgentState.IDLE), no)).toBe(false)
    expect(isActive(agent(AgentState.IDLE), { waiting: true, keep: false })).toBe(true)
  })

  it('rests a finished or stopped agent, however it was waiting', () => {
    expect(isActive(agent(AgentState.EXITED), { waiting: true, keep: false })).toBe(false)
    expect(isActive(agent(AgentState.RETIRED), no)).toBe(false)
  })

  it('always keeps the lead and whatever the caller keeps (the open agent, a new worker)', () => {
    expect(isActive(agent(AgentState.IDLE, AgentRole.ROOT), no)).toBe(true)
    expect(isActive(agent(AgentState.EXITED, AgentRole.ROOT), no)).toBe(true)
    expect(isActive(agent(AgentState.IDLE), { waiting: false, keep: true })).toBe(true)
  })
})
