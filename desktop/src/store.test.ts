import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { applyFrame, emptyHost, type HostState } from './store'
import { AgentSchema, AgentState, EventSchema, RunSchema, type Event } from './gen/agentmesh/v1/types_pb'
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

  it('the stream and its replay give the same state', () => {
    const frames = [
      snapshot(),
      ev(6, { case: 'agentStateChanged', value: { $typeName: 'agentmesh.v1.AgentStateChanged', from: AgentState.STARTING, to: AgentState.WORKING, detail: '', needsInput: 0 } }),
      ev(7, { case: 'agentStateChanged', value: { $typeName: 'agentmesh.v1.AgentStateChanged', from: AgentState.WORKING, to: AgentState.IDLE, detail: '', needsInput: 0 } }),
    ]
    // A client that dropped after the snapshot and resumed gets the later events after a Resumed frame.
    const live = run(frames)
    const resumed = run([frames[0]!, { case: 'resumed', value: { $typeName: 'agentmesh.v1.Resumed' } }, ...frames.slice(1)])
    expect(resumed.agents).toEqual(live.agents)
    expect(resumed.cursor).toEqual(live.cursor)
  })
})
