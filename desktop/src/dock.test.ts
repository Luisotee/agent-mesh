import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { ActorClass } from './gen/agentmesh/v1/actor_pb'
import { WaitingKind, WaitingSchema } from './gen/agentmesh/v1/mesh_pb'
import { AgentRole, AgentSchema, AgentState, MessageSchema, MessageType, RunSchema, RunStatus } from './gen/agentmesh/v1/types_pb'
import { ago, dockTiles, sentLabel, treeTiles, type HostView } from './dock'
import { NEW_FOR_MS } from './workspace'

const agent = (id: string, o: Parameters<typeof create<typeof AgentSchema>>[1] = {}) => create(AgentSchema, { id, name: id, runId: 'r1', state: AgentState.IDLE, ...o })
const by = <T extends { id: string }>(xs: T[]) => Object.fromEntries(xs.map((x) => [x.id, x]))
const host = (agents: ReturnType<typeof agent>[], extra: Partial<HostView> = {}): HostView => ({
  hostId: 'h',
  hostName: 'home',
  runs: { r1: create(RunSchema, { id: 'r1', status: RunStatus.OPEN }), closed: create(RunSchema, { id: 'closed', status: RunStatus.CLOSED }) },
  agents: by(agents),
  messages: {},
  clockSkewMs: 0,
  ...extra,
})
const none = { newTiles: {}, dismissed: {} }

describe('dockTiles', () => {
  it('shows live agents of open runs, not exited, retired or closed ones', () => {
    const tiles = dockTiles([host([agent('a'), agent('x', { state: AgentState.EXITED }), agent('r', { state: AgentState.RETIRED }), agent('c', { runId: 'closed' })])], none, 0)
    expect(tiles.map((t) => t.agent.id)).toEqual(['a'])
  })

  it('puts what needs the user first, then new ones, then the most recently active', () => {
    const at = (ms: number) => timestampFromMs(ms)
    const tiles = dockTiles(
      [host([agent('old', { lastActivity: at(1000) }), agent('recent', { lastActivity: at(9000) }), agent('fresh', { lastActivity: at(500) }), agent('asks', { state: AgentState.NEEDS_INPUT, lastActivity: at(100) })])],
      { newTiles: { 'h/fresh': 1000 }, dismissed: {} },
      2000,
    )
    expect(tiles.map((t) => t.agent.id)).toEqual(['asks', 'fresh', 'recent', 'old'])
    expect(tiles.map((t) => t.isNew)).toEqual([false, true, false, false])
  })

  it('stops marking a tile new after a while', () => {
    const [t] = dockTiles([host([agent('a')])], { newTiles: { 'h/a': 0 }, dismissed: {} }, NEW_FOR_MS + 1)
    expect(t!.isNew).toBe(false)
  })

  it('hides a dismissed tile until the agent\'s state changes', () => {
    const dismissed = { 'h/a': AgentState.IDLE }
    expect(dockTiles([host([agent('a')])], { newTiles: {}, dismissed }, 0)).toEqual([])
    expect(dockTiles([host([agent('a', { state: AgentState.WORKING })])], { newTiles: {}, dismissed }, 0)).toHaveLength(1)
  })

  it('shows the tool a working agent runs, and the last thing it sent', () => {
    const m = (id: string, type: MessageType, ms: number) => create(MessageSchema, { id, type, createdAt: timestampFromMs(ms), from: { $typeName: 'agentmesh.v1.Actor', id: 'a', name: 'a', class: ActorClass.WORKER, runId: 'r1' } })
    const [t] = dockTiles([host([agent('a', { state: AgentState.WORKING, stateDetail: 'Edit' })], { messages: by([m('m1', MessageType.QUESTION, 1), m('m2', MessageType.PLAN, 2)]) })], none, 0)
    expect(t).toMatchObject({ activity: 'Edit' })
    expect(sentLabel(t!.lastSent!)).toBe('sent a plan')
    // An idle agent's detail isn't an activity.
    expect(dockTiles([host([agent('a', { stateDetail: 'Edit' })])], none, 0)[0]!.activity).toBe('')
  })

  it('covers every host, and says which', () => {
    const tiles = dockTiles([host([agent('a')]), { ...host([agent('b')]), hostId: 'g', hostName: 'vps' }], none, 0)
    expect(tiles.map((t) => [t.hostName, t.agent.id])).toEqual([['home', 'a'], ['vps', 'b']])
  })
})

describe('ago', () => {
  it('is short', () => {
    expect([2, 12, 240, 7200, 200_000].map((s) => ago(0 + 1, 1 + s * 1000))).toEqual(['now', '12 s', '4 min', '2 h', '2 d'])
    expect(ago(0, 5000)).toBe('')
  })
})

describe('dockTiles waiting', () => {
  it('marks the agents that wait on an answer', () => {
    const waitingAll = [create(WaitingSchema, { kind: WaitingKind.QUESTION, runId: 'r1', agentId: 'b' })]
    const tiles = dockTiles([host([agent('a'), agent('b')], { waitingAll })], none, 0)
    expect(Object.fromEntries(tiles.map((t) => [t.agent.id, t.waiting]))).toEqual({ a: false, b: true })
  })
})

describe('treeTiles', () => {
  const tiles = dockTiles(
    [
      host([
        agent('lead', { role: AgentRole.ROOT }),
        agent('w', { role: AgentRole.WORKER, parentId: 'lead' }),
        agent('rv', { role: AgentRole.REVIEWER, parentId: 'w' }),
        agent('ask', { role: AgentRole.ASKER, parentId: 'gone' }),
        agent('other', { role: AgentRole.ROOT, runId: 'r2' }),
      ], { runs: { r1: create(RunSchema, { id: 'r1', status: RunStatus.OPEN }), r2: create(RunSchema, { id: 'r2', status: RunStatus.OPEN }) } }),
    ],
    none,
    0,
  )
  it('shows the open agent\'s run, a reviewer folded into its worker, and one whose agent is not there on its own', () => {
    const t = treeTiles(tiles, { hostId: 'h', runId: 'r1' })
    expect(t.map((x) => [x.tile.agent.id, x.helpers.map((a) => a.id)])).toEqual(expect.arrayContaining([['lead', []], ['w', ['rv']], ['ask', []]]))
    expect(t).toHaveLength(3)
  })
  it('shows every live lead with no agent open', () => {
    expect(treeTiles(tiles).map((x) => x.tile.agent.id).sort()).toEqual(['lead', 'other'])
  })
})
