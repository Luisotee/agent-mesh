import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import type { DesktopApi } from '../shared/api'
import { App } from './App'
import { AgentPanel } from './components/agent-panel'
import { ConnectionBanner } from './components/connection-banner'
import { RunSidebar } from './components/run-sidebar'
import type { ConnState } from './connection'
import { AppContext } from './context'
import { AgentRole, AgentSchema, AgentState, MessageType, RunSchema, RunStatus } from './gen/agentmesh/v1/types_pb'
import { createAppModel, type HostRuntime } from './model'
import { createHostStore } from './store'

type AgentInit = Parameters<typeof create<typeof AgentSchema>>[1]
const agent = (id: string, o: AgentInit = {}) => create(AgentSchema, { id, name: id, runId: 'r1', repo: '/p/app', kind: 'claude', createdAt: timestampFromMs(1000), state: AgentState.IDLE, ...o })

function fixture(conn: ConnState = { kind: 'ready', hello: { $typeName: 'agentmesh.v1.HelloResponse', daemonId: 'd', instanceId: 'i', version: 'v', capabilities: [], protocolVersion: 1 } as never }) {
  const store = createHostStore()
  const agents = [
    agent('lead', { role: AgentRole.ROOT, name: 'app-lead' }),
    agent('w1', { role: AgentRole.WORKER, name: 'app-fix', parentId: 'lead', state: AgentState.NEEDS_INPUT, needsInput: 1, stateDetail: 'Bash', model: 'sonnet' }),
  ]
  store.setState({
    conn,
    synced: true,
    runs: { r1: create(RunSchema, { id: 'r1', title: 'ship it', status: RunStatus.OPEN }) },
    agents: Object.fromEntries(agents.map((a) => [a.id, a])),
  })
  const client = { startRun: vi.fn(async () => ({})), spawnAgent: vi.fn(async (_req: unknown) => ({})), stopAgent: vi.fn(async () => ({})), sendMessage: vi.fn(async () => ({})) }
  const connection = { retryNow: vi.fn(), stop: vi.fn() }
  const host = { info: { id: 'h1', name: 'home', kind: 'state-dir', target: '/s' }, store, client, connection } as unknown as HostRuntime
  const api = { hosts: { list: async () => [], add: vi.fn(), remove: vi.fn(), fixed: false }, request: vi.fn() } as unknown as DesktopApi
  const model = createAppModel(api)
  model.store.setState({ loaded: true, hosts: [host], hostId: 'h1' })
  const wrap = (ui: React.ReactNode) => <AppContext.Provider value={model}>{ui}</AppContext.Provider>
  return { host, client, connection, model, wrap, api }
}

describe('RunSidebar', () => {
  it('shows each run as a tree with plain status sentences, and opens an agent on click', async () => {
    const { host, wrap, model } = fixture()
    render(wrap(<RunSidebar host={host} />))
    const region = screen.getByRole('region', { name: 'ship it' })
    expect(region).toHaveTextContent('1 need you')
    const lead = screen.getByRole('button', { name: /app-lead/ })
    const fix = screen.getByRole('button', { name: /app-fix/ })
    expect(lead).toHaveTextContent('Idle, waiting for a message')
    expect(fix).toHaveTextContent('Needs you: Bash')
    expect(fix).toHaveTextContent('app · claude · sonnet')
    // The worker is nested under the lead.
    expect(lead.closest('li')).toContainElement(fix)
    await userEvent.click(fix)
    expect(model.store.getState()).toMatchObject({ hostId: 'h1', agentId: 'w1' })
  })

  it('invites starting a run when there are none', () => {
    const { host, wrap } = fixture()
    host.store.setState({ runs: {}, agents: {} })
    render(wrap(<RunSidebar host={host} />))
    expect(screen.getByText(/No runs yet/)).toBeInTheDocument()
  })
})

describe('AgentPanel', () => {
  it('messages the agent with a mutation id and an expiry', async () => {
    const { host, wrap, client } = fixture()
    render(wrap(<AgentPanel host={host} agentId="w1" />))
    await userEvent.type(screen.getByLabelText('Message', { exact: true }), 'look at the typo')
    await userEvent.click(screen.getByRole('button', { name: 'Send' }))
    await waitFor(() => expect(client.sendMessage).toHaveBeenCalledTimes(1))
    const req = (client.sendMessage.mock.calls[0] as unknown as [{ to: string; type: MessageType; body: string; meta: { mutationId: string; expiresAt: unknown } }])[0]
    expect(req).toMatchObject({ to: 'w1', type: MessageType.TASK, body: 'look at the typo' })
    expect(req.meta.mutationId).toMatch(/^[0-9a-f-]{36}$/)
    expect(req.meta.expiresAt).toBeTruthy()
    expect(await screen.findByText('Sent.')).toBeInTheDocument()
  })

  it('sends once on a double submit, and stops once on a double click', async () => {
    const { host, wrap, client } = fixture()
    let release!: () => void
    client.sendMessage.mockImplementationOnce(() => new Promise((r) => (release = () => r({}))))
    render(wrap(<AgentPanel host={host} agentId="w1" />))
    await userEvent.type(screen.getByLabelText('Message', { exact: true }), 'once')
    const input = screen.getByLabelText('Message', { exact: true })
    fireEvent.submit(input.closest('form')!)
    fireEvent.submit(input.closest('form')!)
    release()
    await screen.findByText('Sent.')
    expect(client.sendMessage).toHaveBeenCalledTimes(1)

    let releaseStop!: () => void
    client.stopAgent.mockImplementationOnce(() => new Promise((r) => (releaseStop = () => r({}))))
    await userEvent.click(screen.getByRole('button', { name: 'Stop agent' }))
    const confirm = screen.getByRole('button', { name: 'Stop app-fix' })
    fireEvent.click(confirm)
    fireEvent.click(confirm)
    releaseStop()
    await waitFor(() => expect(client.stopAgent).toHaveBeenCalledTimes(1))
  })

  it('stops an agent only after a second click', async () => {
    const { host, wrap, client } = fixture()
    render(wrap(<AgentPanel host={host} agentId="w1" />))
    await userEvent.click(screen.getByRole('button', { name: 'Stop agent' }))
    expect(client.stopAgent).not.toHaveBeenCalled()
    await userEvent.click(screen.getByRole('button', { name: 'Stop app-fix' }))
    expect(client.stopAgent).toHaveBeenCalledWith(expect.objectContaining({ agent: 'w1' }))
  })

  it('shows a rejected action\'s reason', async () => {
    const { host, wrap, client } = fixture()
    client.sendMessage.mockRejectedValueOnce(new Error('permission denied'))
    render(wrap(<AgentPanel host={host} agentId="w1" />))
    await userEvent.type(screen.getByLabelText('Message', { exact: true }), 'x')
    await userEvent.click(screen.getByRole('button', { name: 'Send' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('permission denied')
  })

  it('offers no stop or message for an agent that exited', () => {
    const { host, wrap } = fixture()
    host.store.setState((s) => ({ agents: { ...s.agents, w1: agent('w1', { name: 'app-fix', state: AgentState.EXITED, exitCode: 7 }) } }))
    render(wrap(<AgentPanel host={host} agentId="w1" />))
    expect(screen.getByTestId('agent-status')).toHaveTextContent('Exited with code 7')
    expect(screen.queryByRole('button', { name: 'Stop agent' })).toBeNull()
    expect(screen.getByLabelText('Message', { exact: true })).toBeDisabled()
  })
})

describe('AgentPanel without a terminal', () => {
  it('shows the overview, with no tabs', () => {
    const { host, wrap } = fixture()
    render(wrap(<AgentPanel host={host} agentId="w1" />))
    expect(screen.queryByRole('tablist')).toBeNull()
    expect(screen.getByLabelText('Message', { exact: true })).toBeVisible()
  })
})

describe('ConnectionBanner', () => {
  it('is silent while connected', () => {
    const { host, wrap } = fixture()
    render(wrap(<ConnectionBanner host={host} />))
    expect(screen.queryByRole('status')).toBeNull()
  })

  it('says what is wrong and retries on request', async () => {
    const { host, wrap, connection } = fixture({ kind: 'stopped', reason: 'auth', message: 'bad token' })
    render(wrap(<ConnectionBanner host={host} />))
    expect(screen.getByRole('status')).toHaveTextContent("bad token (the host didn't accept the app's token)")
    await userEvent.click(screen.getByRole('button', { name: 'Retry now' }))
    expect(connection.retryNow).toHaveBeenCalled()
  })

  it('shows the countdown of a reconnect', () => {
    const { host, wrap } = fixture({ kind: 'reconnecting', attempt: 2, retryAt: Date.now() + 4000, error: 'ECONNREFUSED' })
    render(wrap(<ConnectionBanner host={host} />))
    expect(screen.getByRole('status')).toHaveTextContent(/home is unreachable \(ECONNREFUSED\)\. Trying again in [34] s/)
  })
})

describe('App', () => {
  it('asks for a host when there is none, and adds one', async () => {
    const add = vi.fn(async () => ({ id: 'x', name: 'home', kind: 'state-dir' as const, target: '/s/state' }))
    const api = { hosts: { list: async () => [], add, remove: vi.fn(), fixed: false }, request: vi.fn(() => () => {}) } as unknown as DesktopApi
    render(<App api={api} />)
    await userEvent.type(await screen.findByLabelText('Name'), 'home')
    fireEvent.change(screen.getByLabelText('State directory'), { target: { value: '/s/state' } })
    await userEvent.click(screen.getByRole('button', { name: 'Add host' }))
    expect(add).toHaveBeenCalledWith({ name: 'home', kind: 'state-dir', path: '/s/state' })
    expect(await screen.findByRole('button', { name: /Host home/ })).toBeInTheDocument()
  })

  it('shows why a host was refused', async () => {
    const add = vi.fn(async () => {
      throw new Error("Error invoking remote method 'hosts:add': Error: the state dir must be an absolute path")
    })
    const api = { hosts: { list: async () => [], add, remove: vi.fn(), fixed: false }, request: vi.fn() } as unknown as DesktopApi
    render(<App api={api} />)
    await userEvent.type(await screen.findByLabelText('Name'), 'home')
    fireEvent.change(screen.getByLabelText('State directory'), { target: { value: 'rel' } })
    await userEvent.click(screen.getByRole('button', { name: 'Add host' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(/^the state dir must be an absolute path$/)
  })
})

describe('SpawnDialog', () => {
  it('defaults to a lead for a run without one and a worker after, and sends what was typed', async () => {
    const { host, wrap, client } = fixture()
    host.store.setState({ agents: {} })
    const { SpawnDialog } = await import('./components/spawn-dialogs')
    const trigger = <button type="button">add</button>
    render(wrap(<SpawnDialog host={host} runId="r1" trigger={trigger} />))
    await userEvent.click(screen.getByRole('button', { name: 'add' }))
    expect(screen.getByLabelText('Role')).toHaveValue(String(AgentRole.ROOT))
    await userEvent.type(screen.getByLabelText('Repository'), '/p/app')
    await userEvent.type(screen.getByLabelText('Slug'), 'lead')
    await userEvent.click(screen.getByRole('button', { name: 'Start agent' }))
    await waitFor(() => expect(client.spawnAgent).toHaveBeenCalledTimes(1))
    expect(client.spawnAgent.mock.calls[0]![0]).toMatchObject({ runId: 'r1', repo: '/p/app', slug: 'lead', role: AgentRole.ROOT, kind: 'claude' })
    // The lead now exists: reopening offers a worker.
    host.store.setState({ agents: { lead: agent('lead', { role: AgentRole.ROOT }) } })
    await userEvent.click(await screen.findByRole('button', { name: 'add' }))
    expect(screen.getByLabelText('Role')).toHaveValue(String(AgentRole.WORKER))
  })
})
