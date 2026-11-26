import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it, vi } from 'vitest'
import { UI_ACTIONS } from './actions'
import { buildCommands, filterCommands, keyedCommands, waitingTargets } from './commands'
import { WaitingKind, WaitingSchema } from './gen/agentmesh/v1/mesh_pb'
import { AgentRole, AgentSchema, AgentState, MessageSchema, MessageType, RunSchema, RunStatus } from './gen/agentmesh/v1/types_pb'
import { createAppModel, type HostRuntime } from './model'
import type { DesktopApi } from '../shared/api'
import { createHostStore } from './store'

/** A client that records every method called on it. */
function recordingClient() {
  const calls: Array<{ method: string; req: Record<string, unknown> }> = []
  const client = new Proxy({}, { get: (_t, method: string) => async (req: Record<string, unknown>) => (calls.push({ method, req }), {}) })
  return { client, calls }
}

const agent = (id: string, o: Parameters<typeof create<typeof AgentSchema>>[1] = {}) => create(AgentSchema, { id, name: `app-${id}`, runId: 'r1', repo: '/p/app', createdAt: timestampFromMs(1000), state: AgentState.IDLE, ...o })

function fixture(hostIds = ['h1']) {
  const rec = recordingClient()
  const hosts = hostIds.map((id) => {
    const store = createHostStore()
    const plan = create(MessageSchema, { id: 'm1', runId: 'r1', type: MessageType.PLAN, toId: 'lead', body: 'p' })
    store.setState({
      conn: { kind: 'ready', hello: { capabilities: [] } as never },
      synced: true,
      runs: { r1: create(RunSchema, { id: 'r1', title: 'ship it', status: RunStatus.OPEN }), r0: create(RunSchema, { id: 'r0', title: 'old', status: RunStatus.CLOSED }) },
      agents: Object.fromEntries(
        [agent('lead', { role: AgentRole.ROOT }), agent('w1', { role: AgentRole.WORKER, parentId: 'lead', state: AgentState.WORKING }), agent('w2', { role: AgentRole.WORKER, parentId: 'lead', state: AgentState.LIMITED }), agent('gone', { state: AgentState.EXITED })].map((a) => [a.id, a]),
      ),
      waiting: [create(WaitingSchema, { kind: WaitingKind.PLAN, runId: 'r1', agentId: 'w1', message: plan }), create(WaitingSchema, { kind: WaitingKind.INPUT, runId: 'r1', agentId: 'w2' })],
    })
    return { info: { id, name: `host-${id}`, kind: 'state-dir', target: '/s' }, store, client: rec.client, connection: { stop: vi.fn() }, loadMessages: vi.fn(), stop: vi.fn() } as unknown as HostRuntime
  })
  const api = { hosts: { list: async () => [], remove: vi.fn(async () => {}), fixed: false }, request: vi.fn(), links: { take: async () => undefined, onOpen: vi.fn(() => () => {}) }, shell: { notify: vi.fn(), setWaiting: vi.fn(), configure: vi.fn(), onNotificationClick: vi.fn(() => () => {}) } } as unknown as DesktopApi
  const model = createAppModel(api)
  model.store.setState({ loaded: true, hosts, hostId: hosts[0]!.info.id })
  return { model, rec, hosts }
}

describe('buildCommands', () => {
  it('lists agents, runs, waiting items and the actions an agent can take, and only the ones that make sense', () => {
    const { model } = fixture()
    const cmds = buildCommands(model)
    const titles = cmds.map((c) => c.title)
    expect(titles).toEqual(expect.arrayContaining(['Go to app-lead', 'Go to app-w1', 'Go to app-gone', 'Go to run ship it', 'Go to run old', 'app-w1 sent a plan', 'app-w2 is waiting in its terminal', 'Start a run…', 'Open the host settings', 'Keyboard shortcuts…']))
    // Actions only for live agents; the switch only for one at its limit; approving only where a plan waits.
    expect(titles).toContain('Stop app-w1')
    expect(titles).not.toContain('Stop app-gone')
    expect(titles).toContain('Move app-w2 to its next model')
    expect(titles).not.toContain('Move app-w1 to its next model')
    expect(titles).toContain('Approve the plan of app-w1')
    expect(titles).not.toContain('Approve the plan of app-w2')
    // A question waits on an answer, not an approval (ApproveAgent refuses it).
    const asked = fixture()
    asked.hosts[0]!.store.setState({ waiting: [create(WaitingSchema, { kind: WaitingKind.QUESTION, runId: 'r1', agentId: 'w1', message: create(MessageSchema, { id: 'm2', runId: 'r1', type: MessageType.QUESTION, toId: 'lead', body: 'which?' }) })] })
    const askedTitles = buildCommands(asked.model).map((c) => c.title)
    expect(askedTitles).toContain('app-w1 asked a question')
    expect(askedTitles).not.toContain('Approve the plan of app-w1')
    // No host switcher with one host; with two, each is named.
    expect(titles.some((t) => t.startsWith('Go to host'))).toBe(false)
    expect(buildCommands(fixture(['h1', 'h2']).model).map((c) => c.title)).toEqual(expect.arrayContaining(['Go to host host-h1', 'Go to host host-h2', 'Ring app-w1 (host-h2)']))
  })

  it('asks to confirm what stops or approves, and for the text a run needs', () => {
    const { model } = fixture()
    const by = Object.fromEntries(buildCommands(model).map((c) => [c.title, c]))
    expect(by['Stop app-w1']!.confirm).toMatch(/process ends/)
    expect(by['Retire app-w1']!.confirm).toMatch(/conversation is kept/)
    expect(by['Approve the plan of app-w1']!.confirm).toBeDefined()
    expect(by['Ring app-w1']!.confirm).toBeUndefined()
    expect(by['Start a run…']!.ask).toBeDefined()
  })
})

describe('every command that touches a host goes through the registry actions', () => {
  it('calls only methods the UI is allowed (a device may call each: registry.test.ts)', async () => {
    const { model, rec } = fixture()
    const cmds = buildCommands(model)
    const acting = cmds.filter((c) => c.group === 'Actions')
    expect(acting.length).toBeGreaterThan(5)
    for (const c of acting) await c.run(c.ask ? 'a title' : undefined)
    const used = new Set(rec.calls.map((c) => c.method))
    expect([...used].sort()).toEqual(['approveAgent', 'retireAgent', 'ringAgent', 'startRun', 'startSetup', 'stopAgent', 'switchAgent'])
    for (const m of used) expect(UI_ACTIONS, m).toContain(m)
    // Each is a write that carries its idempotency key, and says which agent.
    expect(rec.calls.every((c) => (c.req.meta as { mutationId?: string }).mutationId)).toBe(true)
    expect(rec.calls.find((c) => c.method === 'startRun')!.req).toMatchObject({ title: 'a title' })
    expect(rec.calls.find((c) => c.method === 'switchAgent')!.req).toMatchObject({ agent: 'w2', next: true })
    expect(rec.calls.find((c) => c.method === 'approveAgent')!.req).toMatchObject({ agent: 'w1' })
  })

  it('moving around calls the host for nothing', () => {
    const { model, rec } = fixture()
    // (Removing a host asks to be confirmed first: it is the app's own, tested where it is.)
    for (const c of buildCommands(model).filter((x) => x.group !== 'Actions' && !x.confirm)) c.run()
    expect(rec.calls).toEqual([])
  })
})

describe('the setup and host commands', () => {
  it('offers the setup assistant and removing a host, the second only after a confirmation', async () => {
    const { model, rec, hosts } = fixture()
    const by = Object.fromEntries(buildCommands(model).map((c) => [c.title, c]))
    expect(by['Set up with the assistant']).toMatchObject({ group: 'Actions' })
    expect(by['Remove host host-h1']!.confirm).toMatch(/daemon and its agents are untouched/)
    await by['Remove host host-h1']!.run()
    expect(model.api.hosts.remove).toHaveBeenCalledWith('h1')
    expect(model.store.getState().hosts).toEqual([])
    expect(rec.calls).toEqual([])
    void hosts
  })
})

describe('navigation commands', () => {
  it('opens an agent, a run\'s lead and a waiting item on the right view', () => {
    const { model } = fixture()
    const by = Object.fromEntries(buildCommands(model).map((c) => [c.title, c]))
    by['Go to app-w1']!.run()
    expect(model.store.getState()).toMatchObject({ agentId: 'w1', tabs: [{ agentId: 'w1' }] })
    by['Go to run ship it']!.run()
    expect(model.store.getState().agentId).toBe('lead')
    by['app-w2 is waiting in its terminal']!.run()
    expect(model.store.getState()).toMatchObject({ agentId: 'w2', tab: 'terminal' })
    by['app-w1 sent a plan']!.run()
    expect(model.store.getState()).toMatchObject({ agentId: 'w1', tab: 'messages' })
  })

  it('goes to the next thing that waits, round and round, and moves between tabs', () => {
    const { model } = fixture()
    const run = keyedCommands(model)
    expect(waitingTargets(model).map((t) => t.agentId)).toEqual(['w1', 'w2'])
    run.waiting_next()
    expect(model.store.getState().agentId).toBe('w1')
    run.waiting_next()
    expect(model.store.getState().agentId).toBe('w2')
    run.waiting_next()
    expect(model.store.getState().agentId).toBe('w1')
    run.tab_next()
    expect(model.store.getState().agentId).toBe('w2')
    run.tab_prev()
    expect(model.store.getState().agentId).toBe('w1')
    run.tab_close()
    expect(model.store.getState()).toMatchObject({ agentId: 'w2', tabs: [{ agentId: 'w2' }] })
    run.dock()
    expect(model.store.getState().dockOpen).toBe(false)
    run.palette()
    expect(model.store.getState().paletteOpen).toBe(true)
    run.settings()
    expect(model.store.getState().settingsOpen).toBe(true)
  })
})

describe('filterCommands', () => {
  const { model } = fixture()
  const cmds = buildCommands(model)
  it('needs every word, in the title or the detail, and puts title matches first', () => {
    expect(filterCommands(cmds, 'stop w1').map((c) => c.title)).toEqual(['Stop app-w1'])
    expect(filterCommands(cmds, 'worker usage limit').map((c) => c.title)).toContain('Go to app-w2')
    expect(filterCommands(cmds, 'zzz')).toEqual([])
    const first = filterCommands(cmds, 'dock')[0]!
    expect(first.title).toMatch(/dock/)
    expect(filterCommands(cmds, '').length).toBeGreaterThan(10)
  })
})
