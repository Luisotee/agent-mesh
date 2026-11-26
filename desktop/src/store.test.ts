import { create, fromJson, toJson } from '@bufbuild/protobuf'
import { ValueSchema } from '@bufbuild/protobuf/wkt'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { applyFrame, createHostStore, emptyHost, mergeMessages, storeSink, type HostState } from './store'
import { SettingSchema$ as SettingDescriptor, AgentSchema, AgentState, EventSchema, MessageSchema, ReopenSchema, AgentRole, RunSchema, RunStatus, type Event } from './gen/agentmesh/v1/types_pb'
import type { Frame } from './connection'

const agent = (id: string, extra = {}) => create(AgentSchema, { id, runId: 'r1', name: id, state: AgentState.STARTING, ...extra })
const ev = (seq: number, payload: Event['payload']): Frame => ({ case: 'event', value: create(EventSchema, { seq: BigInt(seq), generation: 'g', runId: 'r1', agentId: 'a1', time: timestampFromMs(seq * 1000), payload }) })
const snapshot = (agents = [agent('a1')]): Frame => ({ case: 'snapshot', value: { $typeName: 'agentmesh.v1.Snapshot', cursor: { $typeName: 'agentmesh.v1.Cursor', generation: 'g', seq: 5n }, runs: [create(RunSchema, { id: 'r1', title: 'run' })], agents } })

const run = (frames: Frame[], from: HostState = emptyHost()) => frames.reduce(applyFrame, from)

describe('applyFrame', () => {
  it('a snapshot replaces runs and agents and sets the cursor', () => {
    const s = run([snapshot([agent('stale')]), snapshot()])
    expect(Object.keys(s.agents)).toEqual(['a1'])
    expect(s.cursor?.seq).toBe(5n)
    expect(s.synced).toBe(true)
  })

  it('a resume keeps what we have', () => {
    const s = run([snapshot(), { case: 'resumed', value: { $typeName: 'agentmesh.v1.Resumed' } }])
    expect(Object.keys(s.agents)).toEqual(['a1'])
  })

  it('events change one agent and advance the cursor', () => {
    const s = run([
      snapshot(),
      ev(6, { case: 'agentStateChanged', value: { $typeName: 'agentmesh.v1.AgentStateChanged', from: AgentState.STARTING, to: AgentState.NEEDS_INPUT, detail: 'Bash', needsInput: 1 } }),
    ])
    expect(s.agents.a1).toMatchObject({ state: AgentState.NEEDS_INPUT, stateDetail: 'Bash', needsInput: 1 })
    expect(s.cursor?.seq).toBe(6n)
  })

  it('a retire, a stopped run and a reopen change what the host says they change', () => {
    const waiting = agent('a1', { state: AgentState.NEEDS_INPUT, needsInput: 2, stateDetail: 'Bash', model: 'sonnet' })
    let s = run([snapshot([waiting]), ev(6, { case: 'agentRetired', value: { $typeName: 'agentmesh.v1.AgentRetired', reason: 'requested' } })])
    expect(s.agents.a1).toMatchObject({ state: AgentState.RETIRED, needsInput: 0, stateDetail: '' })
    s = run([
      ev(7, { case: 'agentReopenRequested', value: { $typeName: 'agentmesh.v1.AgentReopenRequested', reopen: create(ReopenSchema, { id: 'ro', model: 'opus' }) } }),
    ], s)
    expect(s.agents.a1!.pendingReopen?.model).toBe('opus')
    s = run([ev(8, { case: 'agentReopened', value: { $typeName: 'agentmesh.v1.AgentReopened', reopen: create(ReopenSchema, { id: 'ro', model: 'opus', effort: 'high' }), terminalId: 't2', sessionId: 's2' } })], s)
    expect(s.agents.a1).toMatchObject({ model: 'opus', effort: 'high', terminalId: 't2', sessionId: 's2', pendingReopen: undefined })
    s = run([ev(9, { case: 'agentReopenRequested', value: { $typeName: 'agentmesh.v1.AgentReopenRequested', reopen: create(ReopenSchema, { id: 'r2' }) } }), ev(10, { case: 'agentReopenDropped', value: { $typeName: 'agentmesh.v1.AgentReopenDropped', reason: 'stopped' } })], s)
    expect(s.agents.a1!.pendingReopen).toBeUndefined()
    s = run([ev(11, { case: 'runStopped', value: { $typeName: 'agentmesh.v1.RunStopped' } })], s)
    expect(s.runs.r1!.status).toBe(RunStatus.CLOSED)
  })

  it('an exit records its code and reason', () => {
    const s = run([snapshot(), ev(6, { case: 'agentExited', value: { $typeName: 'agentmesh.v1.AgentExited', exitCode: 7, signal: 0, reason: 'exited' } })])
    expect(s.agents.a1).toMatchObject({ state: AgentState.EXITED, exitCode: 7, stateDetail: 'exited' })
  })

  it('spawns and runs are added', () => {
    const s = run([
      snapshot([]),
      ev(6, { case: 'agentSpawned', value: { $typeName: 'agentmesh.v1.AgentSpawned', agent: agent('a2') } }),
      ev(7, { case: 'runStarted', value: { $typeName: 'agentmesh.v1.RunStarted', run: create(RunSchema, { id: 'r2' }) } }),
    ])
    expect(Object.keys(s.agents)).toEqual(['a2'])
    expect(Object.keys(s.runs).sort()).toEqual(['r1', 'r2'])
  })

  it('an event of an agent we don\'t know changes nothing but the cursor', () => {
    const s = run([snapshot([]), ev(6, { case: 'agentStateChanged', value: { $typeName: 'agentmesh.v1.AgentStateChanged', from: 0, to: AgentState.IDLE, detail: '', needsInput: 0 } })])
    expect(s.agents).toEqual({})
    expect(s.cursor?.seq).toBe(6n)
  })

  it('an adoption records the terminal', () => {
    const s = run([snapshot(), ev(6, { case: 'agentAdopted', value: { $typeName: 'agentmesh.v1.AgentAdopted', terminalId: 't9' } })])
    expect(s.agents.a1?.terminalId).toBe('t9')
  })

  it('a sent message is kept, and its delivery marks it', () => {
    const m = create(MessageSchema, { id: 'm1', runId: 'r1', body: 'plan' })
    const s = run([
      snapshot(),
      ev(6, { case: 'messageSent', value: { $typeName: 'agentmesh.v1.MessageSent', message: m } }),
      ev(7, { case: 'messageDelivered', value: { $typeName: 'agentmesh.v1.MessageDelivered', messageId: 'm1', via: '', tries: 0 } }),
      ev(8, { case: 'messageDelivered', value: { $typeName: 'agentmesh.v1.MessageDelivered', messageId: 'unknown', via: '', tries: 0 } }),
    ])
    expect(Object.keys(s.messages)).toEqual(['m1'])
    expect(s.messages.m1?.deliveredAt).toBeTruthy()
    expect(s.cursor?.seq).toBe(8n)
  })

  it('a loaded page never replaces a message events already updated', () => {
    const base = emptyHost()
    const updated = create(MessageSchema, { id: 'm1', body: 'x', deliveredAt: timestampFromMs(5000) })
    const stale = create(MessageSchema, { id: 'm1', body: 'x' })
    const next = { ...base, ...mergeMessages({ ...base, messages: { m1: updated } }, [stale, create(MessageSchema, { id: 'm2' })]) }
    expect(next.messages.m1?.deliveredAt).toBeTruthy()
    expect(Object.keys(next.messages).sort()).toEqual(['m1', 'm2'])
  })

  it('a snapshot drops the messages of runs the host no longer lists', () => {
    const kept = create(MessageSchema, { id: 'm1', runId: 'r1' })
    const gone = create(MessageSchema, { id: 'm2', runId: 'old' })
    const s = applyFrame({ ...emptyHost(), messages: { m1: kept, m2: gone } }, snapshot())
    expect(Object.keys(s.messages)).toEqual(['m1'])
  })

  it('a reset falls back to the default when the event carries no value', () => {
    const cur = create(SettingDescriptor, { key: 'quota_stop', value: fromJson(ValueSchema, 80), default: fromJson(ValueSchema, 90), isSet: true })
    const s = applyFrame({ ...emptyHost(), settings: { quota_stop: cur } }, ev(6, { case: 'settingChanged', value: { $typeName: 'agentmesh.v1.SettingChanged', key: 'quota_stop', toDefault: true } }))
    expect(s.settings.quota_stop).toMatchObject({ isSet: false })
    expect(toJson(ValueSchema, s.settings.quota_stop!.value!)).toBe(90)
  })

  it('measures the host\'s clock from its hello', () => {
    const store = createHostStore()
    storeSink(store).onHello({ $typeName: 'agentmesh.v1.HelloResponse', serverTime: timestampFromMs(Date.now() + 120_000) } as never)
    expect(Math.abs(store.getState().clockSkewMs - 120_000)).toBeLessThan(1000)
  })

  it('the stream and its replay give the same state', () => {
    const frames = [
      snapshot(),
      ev(6, { case: 'agentStateChanged', value: { $typeName: 'agentmesh.v1.AgentStateChanged', from: AgentState.STARTING, to: AgentState.WORKING, detail: '', needsInput: 0 } }),
      ev(7, { case: 'agentStateChanged', value: { $typeName: 'agentmesh.v1.AgentStateChanged', from: AgentState.WORKING, to: AgentState.IDLE, detail: '', needsInput: 0 } }),
    ]
    // A client that dropped after the snapshot and resumed gets the later events after a Resumed frame.
    frames.push(ev(8, { case: 'agentRoleChanged', value: { $typeName: 'agentmesh.v1.AgentRoleChanged', from: AgentRole.ASKER, to: AgentRole.WORKER, why: 'change task' } }))
    const live = run(frames)
    expect(live.agents.a1!.role).toBe(AgentRole.WORKER)
    const resumed = run([frames[0]!, { case: 'resumed', value: { $typeName: 'agentmesh.v1.Resumed' } }, ...frames.slice(1)])
    expect(resumed.agents).toEqual(live.agents)
    expect(resumed.cursor).toEqual(live.cursor)
  })
})
