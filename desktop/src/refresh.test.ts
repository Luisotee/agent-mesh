import { create } from '@bufbuild/protobuf'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ListAgentsResponseSchema, ListWaitingResponseSchema, WaitingKind, WaitingSchema } from './gen/agentmesh/v1/mesh_pb'
import { AgentSchema, AgentState, EventSchema, MessageSchema, MessageType, type Event } from './gen/agentmesh/v1/types_pb'
import { hostRefresher } from './refresh'
import { createHostStore } from './store'

const agent = (id: string, state: AgentState) => create(AgentSchema, { id, name: id, runId: 'r1', state })
const row = (agentId: string) => create(WaitingSchema, { kind: WaitingKind.INPUT, runId: 'r1', agentId })
const sent = (type: MessageType): Event => create(EventSchema, { payload: { case: 'messageSent', value: { $typeName: 'agentmesh.v1.MessageSent', message: create(MessageSchema, { type }) } } })
const stateChanged = (): Event => create(EventSchema, { agentId: 'a', payload: { case: 'agentStateChanged', value: { $typeName: 'agentmesh.v1.AgentStateChanged', from: AgentState.WORKING, to: AgentState.IDLE, detail: '', needsInput: 0 } } })

/** A promise a test settles when it likes: a call that is "on its way". */
function gate<T>() {
  let resolve!: (v: T) => void
  const p = new Promise<T>((r) => (resolve = r))
  return { p, resolve }
}

function setup() {
  const store = createHostStore()
  store.setState({ conn: { kind: 'ready', hello: {} as never }, synced: true })
  const client = { listWaiting: vi.fn(), listAgents: vi.fn() }
  const log = vi.fn()
  const r = hostRefresher(client as never, store, log, undefined, { debounceMs: 200, agentsEveryMs: 30_000 })
  return { store, client, log, r }
}

beforeEach(() => vi.useFakeTimers())
afterEach(() => vi.useRealTimers())

describe('what waits on the user', () => {
  it('is listed at once on a snapshot, and after a pause for the events that can change it (a burst is one call)', async () => {
    const { store, client, r } = setup()
    client.listWaiting.mockResolvedValue(create(ListWaitingResponseSchema, { waiting: [row('a')] }))
    r.synced()
    await vi.advanceTimersByTimeAsync(0)
    expect(client.listWaiting).toHaveBeenCalledTimes(1)
    expect(store.getState().waiting).toHaveLength(1)

    r.event(sent(MessageType.PLAN))
    r.event(sent(MessageType.ANSWER))
    r.event(stateChanged())
    expect(client.listWaiting).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(250)
    expect(client.listWaiting).toHaveBeenCalledTimes(2)
  })

  it('is not listed again for an event that cannot change it', async () => {
    const { client, r } = setup()
    client.listWaiting.mockResolvedValue(create(ListWaitingResponseSchema))
    r.event(sent(MessageType.DONE))
    r.event(sent(MessageType.INFO))
    r.event(create(EventSchema, { payload: { case: 'messageDelivered', value: { $typeName: 'agentmesh.v1.MessageDelivered', messageId: 'x', via: '', tries: 0 } } }))
    await vi.advanceTimersByTimeAsync(1000)
    expect(client.listWaiting).not.toHaveBeenCalled()
  })

  it('shows a list that an event overtook, then asks again and shows that one', async () => {
    const { store, client, r } = setup()
    const first = gate<ReturnType<typeof create<typeof ListWaitingResponseSchema>>>()
    client.listWaiting.mockReturnValueOnce(first.p).mockResolvedValueOnce(create(ListWaitingResponseSchema, { waiting: [row('fresh')] }))
    r.synced()
    await vi.advanceTimersByTimeAsync(0)
    // The answer lands while the first list is on its way; the first list still says the plan waits.
    r.event(sent(MessageType.ANSWER))
    first.resolve(create(ListWaitingResponseSchema, { waiting: [row('stale')] }))
    await vi.advanceTimersByTimeAsync(10)
    expect(store.getState().waiting.map((w) => w.agentId)).toEqual(['stale'])
    await vi.advanceTimersByTimeAsync(300)
    expect(client.listWaiting).toHaveBeenCalledTimes(2)
    expect(store.getState().waiting.map((w) => w.agentId)).toEqual(['fresh'])
  })

  it('keeps up under events faster than the host answers: every list is shown, so the list never stays stale for the burst', async () => {
    const { store, client, r } = setup()
    let n = 0
    client.listWaiting.mockImplementation(async () => {
      const mine = ++n
      await new Promise((res) => setTimeout(res, 300))
      return create(ListWaitingResponseSchema, { waiting: [row(`list${mine}`)] })
    })
    r.synced()
    for (let i = 0; i < 20; i++) {
      r.event(sent(MessageType.PLAN))
      await vi.advanceTimersByTimeAsync(100)
    }
    expect(store.getState().waiting[0]?.agentId).toMatch(/^list\d+$/)
  })

  it('does not call back to back under a steady stream of events: one call per pause', async () => {
    const { client, r } = setup()
    client.listWaiting.mockResolvedValue(create(ListWaitingResponseSchema))
    r.synced()
    await vi.advanceTimersByTimeAsync(0)
    // An event every 50 ms for two seconds: each list is overtaken, none may be asked for in a tight loop.
    for (let i = 0; i < 40; i++) {
      r.event(sent(MessageType.PLAN))
      await vi.advanceTimersByTimeAsync(50)
    }
    expect(client.listWaiting.mock.calls.length).toBeLessThanOrEqual(1 + 2000 / 200)
    expect(client.listWaiting.mock.calls.length).toBeGreaterThan(2)
  })

  it('asks again on the next tick after a failure, and says so again if it fails once more later', async () => {
    const { store, client, log, r } = setup()
    client.listWaiting.mockRejectedValueOnce(new Error('network')).mockResolvedValueOnce(create(ListWaitingResponseSchema, { waiting: [row('a')] }))
    client.listAgents.mockResolvedValue(create(ListAgentsResponseSchema))
    r.synced()
    await vi.advanceTimersByTimeAsync(0)
    expect(store.getState().waiting).toEqual([])
    expect(log).toHaveBeenCalledTimes(1)
    r.start()
    await vi.advanceTimersByTimeAsync(30_001)
    expect(store.getState().waiting.map((w) => w.agentId)).toEqual(['a'])
    client.listWaiting.mockRejectedValueOnce(new Error('again'))
    r.event(sent(MessageType.PLAN))
    await vi.advanceTimersByTimeAsync(300)
    expect(log).toHaveBeenCalledTimes(2)
  })

  it('keeps the last list when the host fails, and says so once', async () => {
    const { store, client, log, r } = setup()
    client.listWaiting.mockResolvedValueOnce(create(ListWaitingResponseSchema, { waiting: [row('a')] })).mockRejectedValue(new Error('unimplemented'))
    r.synced()
    await vi.advanceTimersByTimeAsync(0)
    r.event(sent(MessageType.PLAN))
    await vi.advanceTimersByTimeAsync(300)
    r.event(sent(MessageType.PLAN))
    await vi.advanceTimersByTimeAsync(300)
    expect(store.getState().waiting).toHaveLength(1)
    expect(log).toHaveBeenCalledTimes(1)
  })
})

describe('agents that went quiet', () => {
  it('are listed again on a timer, so a working agent the host now calls unknown shows as unknown, and what waits is asked again', async () => {
    const { store, client, r } = setup()
    store.setState({ agents: { a: agent('a', AgentState.WORKING), b: agent('b', AgentState.IDLE) } })
    const b = store.getState().agents.b!
    client.listAgents.mockResolvedValue(create(ListAgentsResponseSchema, { agents: [agent('a', AgentState.UNKNOWN), agent('b', AgentState.IDLE)] }))
    client.listWaiting.mockResolvedValue(create(ListWaitingResponseSchema))
    r.start()
    await vi.advanceTimersByTimeAsync(29_000)
    expect(client.listAgents).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(2_000)
    expect(client.listAgents).toHaveBeenCalledTimes(1)
    expect(store.getState().agents.a!.state).toBe(AgentState.UNKNOWN)
    // An agent that didn't change keeps its object, so nothing that shows it redraws.
    expect(store.getState().agents.b).toBe(b)
    expect(client.listWaiting).toHaveBeenCalledTimes(1)
    r.stop()
    await vi.advanceTimersByTimeAsync(60_000)
    expect(client.listAgents).toHaveBeenCalledTimes(1)
  })

  it('keep what an event said when the event came while the list was on its way', async () => {
    const { store, client, r } = setup()
    store.setState({ agents: { a: agent('a', AgentState.WORKING) } })
    const g = gate<ReturnType<typeof create<typeof ListAgentsResponseSchema>>>()
    client.listAgents.mockReturnValue(g.p)
    client.listWaiting.mockResolvedValue(create(ListWaitingResponseSchema))
    r.start()
    await vi.advanceTimersByTimeAsync(30_001)
    store.setState({ agents: { a: agent('a', AgentState.IDLE) } })
    r.event(stateChanged())
    g.resolve(create(ListAgentsResponseSchema, { agents: [agent('a', AgentState.UNKNOWN)] }))
    await vi.advanceTimersByTimeAsync(10)
    expect(store.getState().agents.a!.state).toBe(AgentState.IDLE)
  })

  it('keep what a reopen or an adoption said when it came while the list was on its way', async () => {
    for (const payload of [
      { case: 'agentReopened', value: { $typeName: 'agentmesh.v1.AgentReopened', terminalId: 't2', sessionId: 's2' } },
      { case: 'agentAdopted', value: { $typeName: 'agentmesh.v1.AgentAdopted', terminalId: 't2', launched: false } },
      { case: 'agentPhaseChanged', value: { $typeName: 'agentmesh.v1.AgentPhaseChanged', why: '' } },
      { case: 'agentRoleChanged', value: { $typeName: 'agentmesh.v1.AgentRoleChanged', why: '' } },
    ] as const) {
      const { store, client, r } = setup()
      store.setState({ agents: { a: agent('a', AgentState.WORKING) } })
      const g = gate<ReturnType<typeof create<typeof ListAgentsResponseSchema>>>()
      client.listAgents.mockReturnValue(g.p)
      r.start()
      await vi.advanceTimersByTimeAsync(30_001)
      store.setState({ agents: { a: create(AgentSchema, { id: 'a', name: 'a', terminalId: 't2' }) } })
      r.event(create(EventSchema, { agentId: 'a', payload: payload as never }))
      g.resolve(create(ListAgentsResponseSchema, { agents: [agent('a', AgentState.UNKNOWN)] }))
      await vi.advanceTimersByTimeAsync(10)
      expect(store.getState().agents.a!.terminalId, payload.case).toBe('t2')
      r.stop()
    }
  })

  it('are not listed while the host is unreachable', async () => {
    const { store, client, r } = setup()
    store.setState({ conn: { kind: 'reconnecting', error: 'x', attempt: 1 } as never })
    r.start()
    await vi.advanceTimersByTimeAsync(31_000)
    expect(client.listAgents).not.toHaveBeenCalled()
  })
})
