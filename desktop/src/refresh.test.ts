import { create } from '@bufbuild/protobuf'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ListAgentsResponseSchema, ListPermissionsResponseSchema, ListWaitingResponseSchema, PendingPermissionSchema, WaitingKind, WaitingSchema } from './gen/agentmesh/v1/mesh_pb'
import { ListPullRequestsResponseSchema, AgentPullRequestsSchema, PullRequestSchema } from './gen/agentmesh/v1/pr_pb'
import { AgentSchema, AgentState, EventSchema, MessageSchema, MessageType, RunSchema, RunStatus, type Event } from './gen/agentmesh/v1/types_pb'
import { hostRefresher } from './refresh'
import { createHostStore } from './store'

const agent = (id: string, state: AgentState) => create(AgentSchema, { id, name: id, runId: 'r1', state })
const row = (agentId: string) => create(WaitingSchema, { kind: WaitingKind.INPUT, runId: 'r1', agentId, forUser: true })
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
  const client = { listWaiting: vi.fn(), listAgents: vi.fn(), listPullRequests: vi.fn().mockResolvedValue(create(ListPullRequestsResponseSchema)), listPermissions: vi.fn().mockResolvedValue(create(ListPermissionsResponseSchema)) }
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

describe('the pull requests', () => {
  const run = (id: string, status = RunStatus.OPEN) => create(RunSchema, { id, status })
  const rows = (runId: string, agentId: string, n: number) =>
    create(ListPullRequestsResponseSchema, { agents: [create(AgentPullRequestsSchema, { agentId, pullRequests: [create(PullRequestSchema, { agentId, number: n, url: `u${n}`, state: 'open' })] })] })

  it('are read for each open run at connect and every minute (a device names the run), and a failure is said once', async () => {
    const { store, client, log, r } = setup()
    store.setState({ runs: { r1: run('r1'), r2: run('r2'), old: run('old', RunStatus.CLOSED) }, agents: { w1: agent('w1', AgentState.IDLE), w2: { ...agent('w2', AgentState.IDLE), runId: 'r2' } } })
    client.listWaiting.mockResolvedValue(create(ListWaitingResponseSchema))
    client.listPullRequests.mockImplementation(async (req: { runId: string }) => rows(req.runId, req.runId === 'r1' ? 'w1' : 'w2', req.runId === 'r1' ? 7 : 8))
    r.start()
    r.synced()
    await vi.advanceTimersByTimeAsync(0)
    expect(client.listPullRequests.mock.calls.map((c) => c[0])).toEqual([{ runId: 'r1', refresh: false }, { runId: 'r2', refresh: false }])
    expect(Object.keys(store.getState().pullRequests).sort()).toEqual(['w1', 'w2'])

    // r2 fails: what is known of its agent stays, r1's is replaced, and the failure is logged once.
    client.listPullRequests.mockImplementation(async (req: { runId: string }) => {
      if (req.runId === 'r2') throw new Error('gh missing')
      return rows('r1', 'w1', 9)
    })
    await vi.advanceTimersByTimeAsync(60_000)
    await vi.advanceTimersByTimeAsync(60_000)
    expect(store.getState().pullRequests.w1?.pullRequests[0]?.number).toBe(9)
    expect(store.getState().pullRequests.w2?.pullRequests[0]?.number).toBe(8)
    expect(log.mock.calls.filter(([l]) => /pull requests/.test(l as string))).toHaveLength(1)
    r.stop()
  })

  it('does not let a read that was under way undo a PR event that came meanwhile, and asks again', async () => {
    const { store, client, r } = setup()
    store.setState({ runs: { r1: run('r1') }, agents: { w1: agent('w1', AgentState.IDLE) } })
    client.listWaiting.mockResolvedValue(create(ListWaitingResponseSchema))
    let release!: (v: unknown) => void
    client.listPullRequests.mockImplementationOnce(() => new Promise((res) => (release = res)))
    client.listPullRequests.mockResolvedValue(rows('r1', 'w1', 12))
    r.synced()
    await vi.advanceTimersByTimeAsync(0)
    // A hold arrives while the first call is on its way; the call then answers with the older state.
    store.setState({ pullRequests: { w1: create(AgentPullRequestsSchema, { agentId: 'w1', pullRequests: [create(PullRequestSchema, { agentId: 'w1', number: 12, url: 'u12', state: 'open', held: true })] }) } })
    r.event(create(EventSchema, { payload: { case: 'pullRequestChanged', value: { $typeName: 'agentmesh.v1.PullRequestChanged', fromState: 'open', fromChecks: '' } } }))
    release(rows('r1', 'w1', 12))
    await vi.advanceTimersByTimeAsync(0)
    // The older answer was dropped, and a second read was made.
    expect(client.listPullRequests).toHaveBeenCalledTimes(2)
    await vi.advanceTimersByTimeAsync(0)
    expect(store.getState().pullRequests.w1?.pullRequests[0]?.number).toBe(12)
  })
})

describe('the permission prompts the host holds', () => {
  const perm = (id: string) => create(PendingPermissionSchema, { id, agentId: 'a', tool: 'Bash' })
  const changed = (): Event => create(EventSchema, { agentId: 'a', payload: { case: 'permissionChanged', value: { $typeName: 'agentmesh.v1.PermissionChanged', id: 'p1', agentName: 'a', state: 'asked' } } })

  it('are listed at a snapshot and again whenever the host says one was held or ended', async () => {
    const { store, client, r } = setup()
    client.listWaiting.mockResolvedValue(create(ListWaitingResponseSchema))
    client.listPermissions.mockResolvedValueOnce(create(ListPermissionsResponseSchema, { permissions: [perm('p1')] })).mockResolvedValue(create(ListPermissionsResponseSchema))
    r.synced()
    await vi.advanceTimersByTimeAsync(0)
    expect(store.getState().permissions.map((p) => p.id)).toEqual(['p1'])
    r.event(changed())
    await vi.advanceTimersByTimeAsync(0)
    expect(client.listPermissions).toHaveBeenCalledTimes(2)
    expect(store.getState().permissions).toEqual([])
  })

  it('are listed one call at a time, and once more when an event came while a call was on its way', async () => {
    const { client, r } = setup()
    client.listWaiting.mockResolvedValue(create(ListWaitingResponseSchema))
    const slow = gate<ReturnType<typeof create<typeof ListPermissionsResponseSchema>>>()
    client.listPermissions.mockReturnValueOnce(slow.p).mockResolvedValue(create(ListPermissionsResponseSchema))
    r.event(changed())
    r.event(changed())
    r.event(changed())
    expect(client.listPermissions).toHaveBeenCalledTimes(1)
    slow.resolve(create(ListPermissionsResponseSchema))
    await vi.advanceTimersByTimeAsync(0)
    expect(client.listPermissions).toHaveBeenCalledTimes(2)
  })

  it('keep the last list when the host does not know the action, and say so once', async () => {
    const { store, client, log, r } = setup()
    store.setState({ permissions: [perm('p1')] })
    client.listPermissions.mockRejectedValue(new Error('unimplemented'))
    r.event(changed())
    await vi.advanceTimersByTimeAsync(0)
    r.event(changed())
    await vi.advanceTimersByTimeAsync(0)
    expect(store.getState().permissions).toHaveLength(1)
    expect(log).toHaveBeenCalledTimes(1)
  })
})

describe('why GitHub cannot be read', () => {
  it('is kept from the pull request lists and cleared when they work again', async () => {
    const { store, client, r } = setup()
    store.setState({ runs: { r1: create(RunSchema, { id: 'r1', status: RunStatus.OPEN }) } })
    client.listWaiting.mockResolvedValue(create(ListWaitingResponseSchema))
    client.listPullRequests.mockResolvedValueOnce(create(ListPullRequestsResponseSchema, { githubProblem: 'no_gh' })).mockResolvedValue(create(ListPullRequestsResponseSchema))
    r.synced()
    await vi.advanceTimersByTimeAsync(0)
    expect(store.getState().githubProblem).toBe('no_gh')
    r.start()
    await vi.advanceTimersByTimeAsync(61_000)
    expect(store.getState().githubProblem).toBe('')
  })
})
