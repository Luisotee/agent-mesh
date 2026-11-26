import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { ActorClass } from './gen/agentmesh/v1/actor_pb'
import { AgentRole, AgentSchema, AgentState, EventSchema, MessageSchema, MessageType, type Event } from './gen/agentmesh/v1/types_pb'
import { LIVE_WITHIN_MS, noticeFor } from './notify'
import { DEFAULT_PREFS } from './prefs'

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

  it('says an agent hit its usage limit or is in error, once, and not when it recovers', () => {
    const e = (from: AgentState, to: AgentState, detail = '') => ev({ case: 'agentStateChanged', value: { $typeName: 'agentmesh.v1.AgentStateChanged', from, to, detail, needsInput: 0 } })
    expect(noticeFor(e(AgentState.WORKING, AgentState.LIMITED, 'resets at 5pm'), agents, NOW)).toMatchObject({ kind: 'limited', tag: 'w1', title: 'app-w1 hit its usage limit', body: 'resets at 5pm', tab: 'terminal' })
    expect(noticeFor(e(AgentState.WORKING, AgentState.ERROR, 'API error 500'), agents, NOW)).toMatchObject({ kind: 'error', tag: 'w1', title: 'app-w1 is in error', body: 'API error 500' })
    expect(noticeFor(e(AgentState.WORKING, AgentState.ERROR), agents, NOW)!.body).not.toBe('') // never an empty notification
    expect(noticeFor(e(AgentState.LIMITED, AgentState.LIMITED), agents, NOW)).toBeUndefined()
    expect(noticeFor(e(AgentState.ERROR, AgentState.ERROR), agents, NOW)).toBeUndefined()
    expect(noticeFor(e(AgentState.LIMITED, AgentState.WORKING), agents, NOW)).toBeUndefined()
    expect(noticeFor(e(AgentState.WORKING, AgentState.ERROR), {}, NOW)).toBeUndefined()
  })

  it('notifies on an error out of the box, and on a usage limit only when the user asks (the host\'s defaults)', () => {
    expect(DEFAULT_PREFS.notifyEvents.has('error')).toBe(true)
    expect(DEFAULT_PREFS.notifyEvents.has('limited')).toBe(false)
    expect(DEFAULT_PREFS.notifyEvents.has('exited')).toBe(false)
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

  it('judges "recent" by the host\'s clock: a host minutes behind still notifies, and one ahead still ignores history', () => {
    const e = sent(MessageType.PLAN, 'w1', 'lead', 'x', ActorClass.WORKER, 5 * 60_000 + 1000) // stamped 5 min 1 s before our now
    expect(noticeFor(e, agents, NOW)).toBeUndefined() // we think it is old...
    // ...but the host's clock is 5 minutes behind ours, so it is 1 s old by the host's own reading.
    expect(noticeFor(e, agents, NOW, -5 * 60_000)).toBeDefined()
    // A host 5 minutes ahead: an event stamped just now (by it) is live; one a minute and a bit old is not.
    expect(noticeFor(sent(MessageType.PLAN, 'w1', 'lead', 'x', ActorClass.WORKER, -5 * 60_000 + 1000), agents, NOW, 5 * 60_000)).toBeDefined()
    expect(noticeFor(sent(MessageType.PLAN, 'w1', 'lead', 'x', ActorClass.WORKER, -5 * 60_000 + LIVE_WITHIN_MS + 1000), agents, NOW, 5 * 60_000)).toBeUndefined()
  })

  it('stays quiet for history: an event older than a minute is a replay', () => {
    expect(noticeFor(sent(MessageType.PLAN, 'w1', 'lead', 'x', ActorClass.WORKER, LIVE_WITHIN_MS - 1), agents, NOW)).toBeDefined()
    expect(noticeFor(sent(MessageType.PLAN, 'w1', 'lead', 'x', ActorClass.WORKER, LIVE_WITHIN_MS + 1), agents, NOW)).toBeUndefined()
  })
})
