import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { ActorClass } from './gen/agentmesh/v1/actor_pb'
import { AgentRole, AgentSchema, AgentState, EventSchema, MessageSchema, MessageType, type Event } from './gen/agentmesh/v1/types_pb'
import { LIVE_WITHIN_MS, noticeFor } from './notify'

const NOW = 1_800_000_000_000
const agent = (id: string, role: AgentRole, o: { state?: AgentState } = {}) => create(AgentSchema, { id, name: `app-${id}`, role, state: o.state ?? AgentState.WORKING })
const agents = { lead: agent('lead', AgentRole.ROOT), w1: agent('w1', AgentRole.WORKER), w2: agent('w2', AgentRole.WORKER) }
const ev = (payload: Event['payload'], o: { agentId?: string; age?: number } = {}) => create(EventSchema, { seq: 1n, agentId: o.agentId ?? 'w1', time: timestampFromMs(NOW - (o.age ?? 1000)), payload })
const sent = (type: MessageType, from: string, to: string, body: string, fromClass = ActorClass.WORKER, age?: number) =>
  ev({ case: 'messageSent', value: { $typeName: 'agentmesh.v1.MessageSent', message: create(MessageSchema, { id: 'm', type, body, toId: to, from: { $typeName: 'agentmesh.v1.Actor', id: from, name: from, class: fromClass, runId: 'r' } }) } }, { age })

describe('noticeFor', () => {
  it('says an agent needs you, once, with what it is stopped at', () => {
    const e = (from: AgentState, to: AgentState) => ev({ case: 'agentStateChanged', value: { $typeName: 'agentmesh.v1.AgentStateChanged', from, to, detail: 'Claude needs your permission to use Bash', needsInput: 1 } })
    expect(noticeFor(e(AgentState.WORKING, AgentState.NEEDS_INPUT), agents, NOW)).toMatchObject({ kind: 'input', tag: 'w1', title: 'app-w1 needs you', body: 'Claude needs your permission to use Bash', tab: 'terminal' })
    expect(noticeFor(e(AgentState.NEEDS_INPUT, AgentState.NEEDS_INPUT), agents, NOW)).toBeUndefined()
    expect(noticeFor(e(AgentState.WORKING, AgentState.IDLE), agents, NOW)).toBeUndefined()
  })

  it('notifies for a plan, question, report or failure sent to the lead', () => {
    expect(noticeFor(sent(MessageType.PLAN, 'w1', 'lead', '# Plan\n\n- fix **the** typo'), agents, NOW)).toMatchObject({ kind: 'plan', title: 'app-w1 sent a plan', body: 'Plan fix the typo', tab: 'messages', agentId: 'w1' })
    expect(noticeFor(sent(MessageType.QUESTION, 'w1', 'lead', 'Which branch?'), agents, NOW)).toMatchObject({ kind: 'question' })
    expect(noticeFor(sent(MessageType.DONE, 'w1', 'lead', 'all done'), agents, NOW)).toMatchObject({ kind: 'report', title: 'app-w1 reported done' })
    expect(noticeFor(sent(MessageType.FAILED, 'w1', 'lead', 'broke'), agents, NOW)).toMatchObject({ kind: 'failed' })
  })

  it('stays quiet for talk that is not the user\'s: to a worker, from the user, answers, info', () => {
    expect(noticeFor(sent(MessageType.PLAN, 'w1', 'w2', 'x'), agents, NOW)).toBeUndefined()
    expect(noticeFor(sent(MessageType.ANSWER, 'lead', 'w1', 'approved', ActorClass.DEVICE), agents, NOW)).toBeUndefined()
    expect(noticeFor(sent(MessageType.QUESTION, 'dev', 'lead', 'x', ActorClass.DEVICE), agents, NOW)).toBeUndefined()
    expect(noticeFor(sent(MessageType.INFO, 'w1', 'lead', 'x'), agents, NOW)).toBeUndefined()
    expect(noticeFor(sent(MessageType.PLAN, 'ghost', 'lead', 'x'), agents, NOW)).toBeUndefined()
  })

  it('shortens a long body', () => {
    expect(noticeFor(sent(MessageType.DONE, 'w1', 'lead', 'x'.repeat(1000)), agents, NOW)!.body.length).toBeLessThanOrEqual(200)
  })

  it('tells a failure from a clean finish, and ignores the user stopping an agent', () => {
    const exit = (exitCode: number, reason: string) => ev({ case: 'agentExited', value: { $typeName: 'agentmesh.v1.AgentExited', exitCode, signal: 0, reason } })
    expect(noticeFor(exit(7, 'exited'), agents, NOW)).toMatchObject({ kind: 'failed', title: 'app-w1 exited with code 7' })
    expect(noticeFor(exit(0, 'exited'), agents, NOW)).toMatchObject({ kind: 'exited', title: 'app-w1 finished' })
    expect(noticeFor(exit(0, 'lost'), agents, NOW)).toMatchObject({ kind: 'failed', title: 'app-w1 was lost' })
    expect(noticeFor(exit(-1, 'killed'), agents, NOW)).toBeUndefined()
  })

  it('stays quiet for history: an event older than a minute is a replay', () => {
    expect(noticeFor(sent(MessageType.PLAN, 'w1', 'lead', 'x', ActorClass.WORKER, LIVE_WITHIN_MS - 1), agents, NOW)).toBeDefined()
    expect(noticeFor(sent(MessageType.PLAN, 'w1', 'lead', 'x', ActorClass.WORKER, LIVE_WITHIN_MS + 1), agents, NOW)).toBeUndefined()
  })
})
