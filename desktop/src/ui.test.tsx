import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import type { DesktopApi } from '../shared/api'
import { App } from './App'
import { AgentPanel } from './components/agent-panel'
import { ConnectionBanner } from './components/connection-banner'
import { ChatView } from './components/chat-view'
import { CommandPalette } from './components/command-palette'
import { Dock } from './components/dock'
import { ShortcutField } from './components/shortcut-field'
import { useShortcuts } from './shortcuts'
import { TabBar } from './components/tab-bar'
import { MessagesPanel } from './components/messages-panel'
import { RunSidebar } from './components/run-sidebar'
import { WaitingList } from './components/waiting-list'
import type { ConnState } from './connection'
import { AppContext } from './context'
import { SettingSchema$ as SettingDescriptor, SettingSchemaSchema, AgentRole, AgentSchema, AgentState, MessageSchema, MessageType, RunSchema, RunStatus } from './gen/agentmesh/v1/types_pb'
import { fromJson } from '@bufbuild/protobuf'
import { ValueSchema } from '@bufbuild/protobuf/wkt'
import { createAppModel, type HostRuntime } from './model'
import { WaitingKind, WaitingSchema } from './gen/agentmesh/v1/mesh_pb'
import { ReadTranscriptResponseSchema, TranscriptEntrySchema, TranscriptKind } from './gen/agentmesh/v1/chat_pb'
import { ActorClass } from './gen/agentmesh/v1/actor_pb'
import { createHostStore } from './store'

const noLinks = { take: async () => undefined, onOpen: vi.fn(() => () => {}) }
const noLocal = { detect: async () => [], connect: vi.fn(), fix: vi.fn() }
const manually = async () => userEvent.click(await screen.findByRole('button', { name: 'Connect another way' }))

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
    messageLoad: { r1: 'loaded' },
  })
  const client = { startRun: vi.fn(async () => ({})), spawnAgent: vi.fn(async (_req: unknown) => ({})), stopAgent: vi.fn(async () => ({})), sendMessage: vi.fn(async (_req: unknown) => ({})) }
  const connection = { retryNow: vi.fn(), stop: vi.fn() }
  const host = { info: { id: 'h1', name: 'home', kind: 'state-dir', target: '/s' }, store, client, connection, loadMessages: vi.fn() } as unknown as HostRuntime
  const api = { hosts: { list: async () => [], add: vi.fn(), remove: vi.fn(), fixed: false }, request: vi.fn(), links: noLinks, shell: { notify: vi.fn(), setWaiting: vi.fn(), configure: vi.fn(), onNotificationClick: vi.fn(() => () => {}) } } as unknown as DesktopApi
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
    const lead = within(region).getByRole('button', { name: /app-lead/ })
    const fix = within(region).getByRole('button', { name: /app-fix/ })
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
  it('shows the overview, messages and chat tabs, and no terminal tab', () => {
    const { host, wrap } = fixture()
    render(wrap(<AgentPanel host={host} agentId="w1" />))
    expect(screen.getAllByRole('tab').map((t) => t.textContent)).toEqual(['overview', 'messages', 'chat'])
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
    const api = { hosts: { list: async () => [], add, remove: vi.fn(), fixed: false }, request: vi.fn(() => () => {}), links: noLinks, local: noLocal, shell: { notify: vi.fn(), setWaiting: vi.fn(), configure: vi.fn(), onNotificationClick: vi.fn(() => () => {}) } } as unknown as DesktopApi
    render(<App api={api} />)
    await manually()
    await userEvent.type(await screen.findByLabelText('Name'), 'home')
    fireEvent.change(screen.getByLabelText('State directory'), { target: { value: '/s/state' } })
    await userEvent.click(screen.getByRole('button', { name: 'Add host' }))
    expect(add).toHaveBeenCalledWith({ name: 'home', kind: 'state-dir', path: '/s/state' })
    expect(await screen.findByRole('button', { name: /Host home/ })).toBeInTheDocument()
  })

  it('adds a host by pairing link, with the name this device gives itself', async () => {
    const add = vi.fn(async () => ({ id: 'x', name: 'Home', kind: 'paired' as const, target: '10.0.0.2:9443' }))
    const api = { hosts: { list: async () => [], add, remove: vi.fn(), fixed: false }, request: vi.fn(() => () => {}), links: noLinks, local: noLocal, shell: { notify: vi.fn(), setWaiting: vi.fn(), configure: vi.fn(), onNotificationClick: vi.fn(() => () => {}) } } as unknown as DesktopApi
    render(<App api={api} />)
    await manually()
    await userEvent.selectOptions(await screen.findByLabelText('How to reach it'), 'pair')
    // The host's own name is optional here, and there is no field for a pasted token.
    expect(screen.queryByLabelText(/token/i)).toBeNull()
    fireEvent.change(screen.getByLabelText('Pairing link'), { target: { value: ' agentmesh://pair?v=1&h=h_1 ' } })
    await userEvent.clear(screen.getByLabelText("This device's name"))
    await userEvent.type(screen.getByLabelText("This device's name"), 'Work laptop')
    await userEvent.click(screen.getByRole('button', { name: 'Add host' }))
    expect(add).toHaveBeenCalledWith({ name: '', kind: 'pair', link: 'agentmesh://pair?v=1&h=h_1', deviceName: 'Work laptop' })
    expect(await screen.findByRole('button', { name: /Host Home/ })).toBeInTheDocument()
  })

  const OPENED = { link: 'agentmesh://pair?v=1&h=h_1&t=amp_one', hostName: 'Home server' }

  it('opened by a pairing link with no host yet, fills the form in and adds nothing until the user does', async () => {
    const add = vi.fn(async () => ({ id: 'x', name: 'Home server', kind: 'paired' as const, target: '10.0.0.2:9443' }))
    const api = { hosts: { list: async () => [], add, remove: vi.fn(), fixed: false }, request: vi.fn(() => () => {}), links: { take: async () => OPENED, onOpen: vi.fn(() => () => {}) }, local: noLocal, shell: { notify: vi.fn(), setWaiting: vi.fn(), configure: vi.fn(), onNotificationClick: vi.fn(() => () => {}) } } as unknown as DesktopApi
    render(<App api={api} />)
    expect(await screen.findByLabelText('Pairing link')).toHaveValue(OPENED.link)
    expect(screen.getByLabelText('Name')).toHaveValue('Home server')
    expect(screen.getByRole('note')).toHaveTextContent('opened by a pairing link for Home server')
    expect(add).not.toHaveBeenCalled()
    await userEvent.click(screen.getByRole('button', { name: 'Add host' }))
    expect(add).toHaveBeenCalledWith({ name: 'Home server', kind: 'pair', link: OPENED.link, deviceName: 'Desktop' })
  })

  it('opened by a pairing link while running, opens the add-a-host dialog filled in; closing it drops the link', async () => {
    let open!: (l: typeof OPENED) => void
    const host = { id: 'h', name: 'home', kind: 'state-dir' as const, target: '/s' }
    const api = { hosts: { list: async () => [host], add: vi.fn(), remove: vi.fn(), fixed: false }, request: vi.fn(() => () => {}), openSocket: vi.fn(), links: { take: async () => undefined, onOpen: vi.fn((cb: typeof open) => ((open = cb), () => {})) }, shell: { notify: vi.fn(), setWaiting: vi.fn(), configure: vi.fn(), onNotificationClick: vi.fn(() => () => {}) } } as unknown as DesktopApi
    render(<App api={api} />)
    await screen.findByRole('button', { name: /Host home/ })
    expect(screen.queryByRole('dialog')).toBeNull()
    act(() => open(OPENED))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByLabelText('Pairing link')).toHaveValue(OPENED.link)
    await userEvent.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    await userEvent.click(screen.getByRole('button', { name: 'Add a host' }))
    const fresh = within(await screen.findByRole('dialog'))
    expect(fresh.queryByLabelText('Pairing link')).toBeNull() // a fresh form: a state dir, not the dropped link
    expect(fresh.getByLabelText('Name')).toHaveValue('')
  })

  it('ignores a link when the host list is fixed by the environment', async () => {
    const take = vi.fn(async () => OPENED)
    const host = { id: 'h', name: 'home', kind: 'state-dir' as const, target: '/s' }
    const api = { hosts: { list: async () => [host], add: vi.fn(), remove: vi.fn(), fixed: true }, request: vi.fn(() => () => {}), openSocket: vi.fn(), links: { take, onOpen: vi.fn(() => () => {}) }, shell: { notify: vi.fn(), setWaiting: vi.fn(), configure: vi.fn(), onNotificationClick: vi.fn(() => () => {}) } } as unknown as DesktopApi
    render(<App api={api} />)
    await screen.findByRole('button', { name: /Host home/ })
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(take).not.toHaveBeenCalled() // the link stays where main keeps it: nothing here could use it
  })

  it('shows why a host was refused', async () => {
    const add = vi.fn(async () => {
      throw new Error("Error invoking remote method 'hosts:add': Error: the state dir must be an absolute path")
    })
    const api = { hosts: { list: async () => [], add, remove: vi.fn(), fixed: false }, request: vi.fn(), links: noLinks, local: noLocal, shell: { notify: vi.fn(), setWaiting: vi.fn(), configure: vi.fn(), onNotificationClick: vi.fn(() => () => {}) } } as unknown as DesktopApi
    render(<App api={api} />)
    await manually()
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
    // A lead may take the CLI's default: no tier, no model.
    expect(client.spawnAgent.mock.calls[0]![0]).toMatchObject({ runId: 'r1', repo: '/p/app', slug: 'lead', role: AgentRole.ROOT, kind: 'claude', tier: '', model: '' })
    // The lead now exists: reopening offers a worker, which the host only starts on a tier or a model.
    host.store.setState({ agents: { lead: agent('lead', { role: AgentRole.ROOT }) } })
    await userEvent.click(await screen.findByRole('button', { name: 'add' }))
    expect(screen.getByLabelText('Role')).toHaveValue(String(AgentRole.WORKER))
    expect(screen.getByLabelText('Tier')).toHaveValue('mid')
    expect(within(screen.getByLabelText('Tier')).queryByRole('option', { name: "The CLI's default" })).toBeNull()
    await userEvent.type(screen.getByLabelText('Repository'), '/p/app')
    await userEvent.type(screen.getByLabelText('Slug'), 'fix')
    await userEvent.selectOptions(screen.getByLabelText('Tier'), 'strong')
    await userEvent.click(screen.getByRole('button', { name: 'Start agent' }))
    await waitFor(() => expect(client.spawnAgent).toHaveBeenCalledTimes(2))
    expect(client.spawnAgent.mock.calls[1]![0]).toMatchObject({ role: AgentRole.WORKER, tier: 'strong' })
  })

  it('offers the tiers the host defines for the CLI, and never leaves a worker with none', async () => {
    const { host, wrap } = fixture()
    host.store.setState({ agents: {}, settings: Object.fromEntries(['tiers.claude.light.model', 'tiers.claude.deep.effort', 'tiers.agy.fast.model'].map((k) => [k, {} as never])) })
    const { SpawnDialog, tiersOf } = await import('./components/spawn-dialogs')
    expect(tiersOf(host.store.getState().settings, 'claude')).toEqual(['light', 'deep'])
    expect(tiersOf({}, 'claude')).toEqual(['light', 'mid', 'strong', 'top'])
    render(wrap(<SpawnDialog host={host} runId="r1" trigger={<button type="button">add</button>} />))
    await userEvent.click(screen.getByRole('button', { name: 'add' }))
    expect(screen.getByLabelText('Tier')).toHaveValue('')
    // Switching the lead to a worker picks a tier (this host has no mid, so its first); back to a lead, the default.
    await userEvent.selectOptions(screen.getByLabelText('Role'), String(AgentRole.WORKER))
    expect(screen.getByLabelText('Tier')).toHaveValue('light')
    await userEvent.selectOptions(screen.getByLabelText('Role'), String(AgentRole.ROOT))
    expect(screen.getByLabelText('Tier')).toHaveValue('')
  })
})


const msg = (id: string, type: MessageType, from: string, to: string, ms: number, body = id) =>
  create(MessageSchema, { id, runId: 'r1', type, body, toId: to, toName: to, createdAt: timestampFromMs(ms), from: { $typeName: 'agentmesh.v1.Actor', id: from, name: from, class: 0, runId: 'r1' } })
const putMessages = (host: HostRuntime, ...ms: ReturnType<typeof msg>[]) => host.store.setState((s) => ({ messages: { ...s.messages, ...Object.fromEntries(ms.map((m) => [m.id, m])) } }))

/** What the host says waits: a row per plan or question, as ListWaiting returns them. */
const waitOn = (host: HostRuntime, ...ms: ReturnType<typeof msg>[]) =>
  host.store.setState({ waiting: ms.map((m) => create(WaitingSchema, { kind: m.type === MessageType.PLAN ? WaitingKind.PLAN : WaitingKind.QUESTION, runId: 'r1', agentId: m.from!.id, message: m })) })

describe('MessagesPanel', () => {
  it('shows a plan awaiting the user and approves it with an answer to its author', async () => {
    const { host, wrap, client } = fixture()
    const plan = msg('p1', MessageType.PLAN, 'w1', 'lead', 1000, '# The plan\n\n- change a\n- change b')
    putMessages(host, plan)
    waitOn(host, plan)
    render(wrap(<MessagesPanel host={host} agentId="w1" />))
    const card = screen.getByRole('article', { name: 'Plan from w1' })
    expect(within(card).getByRole('heading', { name: 'The plan' })).toBeInTheDocument()
    expect(within(card).getAllByRole('listitem')).toHaveLength(2)
    expect(card).toHaveTextContent('waiting for you')
    await userEvent.click(within(card).getByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(client.sendMessage).toHaveBeenCalledTimes(1))
    expect(client.sendMessage.mock.calls[0]![0]).toMatchObject({ to: 'w1', type: MessageType.ANSWER, body: 'approved' })
    // The answer arrives as an event, the host is asked again and says nothing waits: the item closes by itself.
    putMessages(host, msg('a1', MessageType.ANSWER, 'lead', 'w1', 2000, 'approved'))
    host.store.setState({ waiting: [] })
    await waitFor(() => expect(screen.queryByRole('form', { name: 'Answer the plan' })).toBeNull())
    expect(screen.getByRole('article', { name: 'Plan from w1' })).not.toHaveTextContent('waiting for you')
  })

  it('asks for changes only with some text, and answers a question', async () => {
    const { host, wrap, client } = fixture()
    const p1 = msg('p1', MessageType.PLAN, 'w1', 'lead', 1000)
    const q1 = msg('q1', MessageType.QUESTION, 'w1', 'lead', 1500, 'Which branch?')
    putMessages(host, p1, q1)
    waitOn(host, p1, q1)
    render(wrap(<MessagesPanel host={host} agentId="w1" />))
    const plan = screen.getByRole('form', { name: 'Answer the plan' })
    expect(within(plan).getByRole('button', { name: 'Request changes' })).toBeDisabled()
    await userEvent.type(within(plan).getByLabelText('Changes to ask for'), 'split it in two')
    await userEvent.click(within(plan).getByRole('button', { name: 'Request changes' }))
    await waitFor(() => expect(client.sendMessage).toHaveBeenCalledTimes(1))
    expect(client.sendMessage.mock.calls[0]![0]).toMatchObject({ to: 'w1', type: MessageType.ANSWER, body: 'split it in two' })

    const q = screen.getByRole('form', { name: 'Answer the question' })
    expect(within(q).queryByRole('button', { name: 'Approve' })).toBeNull()
    await userEvent.type(within(q).getByLabelText('Your answer'), 'main')
    await userEvent.click(within(q).getByRole('button', { name: 'Send answer' }))
    await waitFor(() => expect(client.sendMessage).toHaveBeenCalledTimes(2))
    expect(client.sendMessage.mock.calls[1]![0]).toMatchObject({ to: 'w1', body: 'main' })
  })

  it('does not approve over text typed for changes', async () => {
    const { host, wrap, client } = fixture()
    const p1 = msg('p1', MessageType.PLAN, 'w1', 'lead', 1000)
    putMessages(host, p1)
    waitOn(host, p1)
    render(wrap(<MessagesPanel host={host} agentId="w1" />))
    await userEvent.type(screen.getByLabelText('Changes to ask for'), 'also add tests')
    expect(screen.getByRole('button', { name: 'Approve' })).toBeDisabled()
    expect(client.sendMessage).not.toHaveBeenCalled()
  })

  it('answers once on a double click, and shows a rejection', async () => {
    const { host, wrap, client } = fixture()
    const p1 = msg('p1', MessageType.PLAN, 'w1', 'lead', 1000)
    putMessages(host, p1)
    waitOn(host, p1)
    let release!: () => void
    client.sendMessage.mockImplementationOnce(() => new Promise((r) => (release = () => r({}))))
    render(wrap(<MessagesPanel host={host} agentId="w1" />))
    const approve = screen.getByRole('button', { name: 'Approve' })
    fireEvent.click(approve)
    fireEvent.click(approve)
    release()
    await waitFor(() => expect(approve).not.toBeDisabled())
    expect(client.sendMessage).toHaveBeenCalledTimes(1)
    client.sendMessage.mockRejectedValueOnce(new Error('permission denied'))
    await userEvent.click(approve)
    expect(await screen.findByRole('alert')).toHaveTextContent('permission denied')
  })

  it('shows reports and failures without a form, a long one collapsed', async () => {
    const { host, wrap } = fixture()
    putMessages(host, msg('d1', MessageType.DONE, 'w1', 'lead', 1000, 'All **done**.'), msg('f1', MessageType.FAILED, 'w1', 'lead', 2000, 'x'.repeat(2000)))
    render(wrap(<MessagesPanel host={host} agentId="w1" />))
    expect(screen.getByRole('article', { name: 'Report from w1' })).toHaveTextContent('All done.')
    expect(screen.queryByRole('form')).toBeNull()
    const failed = screen.getByRole('article', { name: 'Failed from w1' })
    await userEvent.click(within(failed).getByRole('button', { name: /Show all/ }))
    expect(within(failed).getByRole('button', { name: /Show less/ })).toBeInTheDocument()
  })

  it('asks for the run\'s messages when shown, and shows Loading until they are there, then a retry if they failed', async () => {
    const { host, wrap } = fixture()
    host.store.setState({ messageLoad: {} })
    const { rerender } = render(wrap(<MessagesPanel host={host} agentId="w1" active={false} />))
    expect(host.loadMessages).not.toHaveBeenCalled() // a tab nobody opened loads nothing
    expect(screen.getByRole('status')).toHaveTextContent('Loading messages')
    rerender(wrap(<MessagesPanel host={host} agentId="w1" active />))
    expect(host.loadMessages).toHaveBeenCalledWith('r1')
    putMessages(host, msg('p1', MessageType.DONE, 'w1', 'lead', 1000, 'a report an event brought'))
    expect(screen.queryByRole('article')).toBeNull() // some of the history isn't all of it
    host.store.setState({ messageLoad: { r1: 'failed' } })
    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't load")
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }))
    expect(host.loadMessages).toHaveBeenLastCalledWith('r1', true)
    host.store.setState({ messageLoad: { r1: 'loaded' } })
    expect(await screen.findByRole('article', { name: 'Report from w1' })).toBeInTheDocument()
  })

  it('says so when there are no messages', () => {
    const { host, wrap } = fixture()
    render(wrap(<MessagesPanel host={host} agentId="w1" />))
    expect(screen.getByText(/No messages yet/)).toBeInTheDocument()
  })
})

describe('Markdown in messages', () => {
  it('opens web links in the browser through main, drops raw html and images, and ignores other schemes', async () => {
    const openExternal = vi.fn()
    ;(window as unknown as { desktop: unknown }).desktop = { openExternal }
    const { host, wrap } = fixture()
    putMessages(host, msg('d1', MessageType.DONE, 'w1', 'lead', 1000, 'See [docs](https://example.com/a) and [bad](javascript:alert(1)).\n\n<script>window.x=1</script><img src="https://evil.example/x.png">![alt](https://evil.example/y.png)\n\n| a | b |\n|---|---|\n| 1 | 2 |'))
    const { container } = render(wrap(<MessagesPanel host={host} agentId="w1" />))
    await userEvent.click(screen.getByRole('link', { name: 'docs' }))
    expect(openExternal).toHaveBeenCalledWith('https://example.com/a')
    const bad = screen.queryByRole('link', { name: 'bad' })
    if (bad) await userEvent.click(bad)
    expect(openExternal).toHaveBeenCalledTimes(1)
    expect(container.querySelector('script')).toBeNull()
    expect(container.querySelector('img')).toBeNull()
    expect(screen.getByRole('table')).toBeInTheDocument()
  })
})

describe('WaitingList', () => {
  it('lists what waits on the user and opens the agent on its messages (or terminal)', async () => {
    const { host, wrap, model } = fixture()
    const p1 = msg('p1', MessageType.PLAN, 'w1', 'lead', 1000)
    putMessages(host, p1)
    host.store.setState((s) => ({
      agents: { ...s.agents, w1: agent('w1', { role: AgentRole.WORKER, name: 'app-fix', parentId: 'lead', state: AgentState.IDLE }), w2: agent('w2', { role: AgentRole.WORKER, name: 'app-two', parentId: 'lead', state: AgentState.NEEDS_INPUT, needsInput: 1 }) },
      waiting: [create(WaitingSchema, { kind: WaitingKind.PLAN, runId: 'r1', agentId: 'w1', message: p1 }), create(WaitingSchema, { kind: WaitingKind.INPUT, runId: 'r1', agentId: 'w2' })],
    }))
    render(wrap(<WaitingList host={host} />))
    const list = screen.getByRole('region', { name: 'Waiting on you' })
    expect(list).toHaveTextContent('app-fix sent a plan')
    expect(list).toHaveTextContent('app-two needs you in its terminal')
    await userEvent.click(within(list).getByRole('button', { name: /app-fix/ }))
    expect(model.store.getState()).toMatchObject({ agentId: 'w1', tab: 'messages' })
    await userEvent.click(within(list).getByRole('button', { name: /app-two/ }))
    expect(model.store.getState()).toMatchObject({ agentId: 'w2', tab: 'terminal' })
  })

  it('is absent when nothing waits', () => {
    const { host, wrap } = fixture()
    host.store.setState((s) => ({ agents: { ...s.agents, w1: agent('w1', { role: AgentRole.WORKER, parentId: 'lead' }) } }))
    render(wrap(<WaitingList host={host} />))
    expect(screen.queryByRole('region', { name: 'Waiting on you' })).toBeNull()
  })
})

describe('Dock and TabBar', () => {
  it('shows a tile per live agent with what it does, opens one as a tab on click, and closes tabs', async () => {
    const { host, wrap, model } = fixture()
    host.store.setState((s) => ({ agents: { ...s.agents, lead: { ...s.agents.lead!, state: AgentState.WORKING, stateDetail: 'Edit' } } }))
    render(wrap(<><TabBar /><Dock /></>))
    const dock = screen.getByRole('region', { name: 'Live dock' })
    expect(dock).toHaveTextContent('2 agents')
    expect(dock).toHaveTextContent('1 need you')
    const tiles = within(dock).getAllByRole('listitem')
    // The one that needs the user comes first; the other shows its tool.
    expect(tiles[0]).toHaveTextContent('app-fix')
    expect(tiles[1]).toHaveTextContent('Working: Edit')
    expect(screen.queryByRole('tablist', { name: 'Open agents' })).toBeNull()

    await userEvent.click(within(tiles[1]!).getByRole('button', { name: /Working: Edit/ }))
    expect(model.store.getState()).toMatchObject({ hostId: 'h1', agentId: 'lead', tabs: [{ agentId: 'lead' }] })
    const tabs = screen.getByRole('tablist', { name: 'Open agents' })
    expect(within(tabs).getByRole('tab', { name: /app-lead/ })).toHaveAttribute('aria-selected', 'true')

    await userEvent.click(within(dock).getByRole('button', { name: /Needs you/ }))
    expect(within(tabs).getAllByRole('tab')).toHaveLength(2)
    await userEvent.click(within(tabs).getByRole('button', { name: 'Close app-fix' }))
    expect(model.store.getState()).toMatchObject({ agentId: 'lead', tabs: [{ agentId: 'lead' }] })
  })

  it('marks a new worker, hides a dismissed tile, and folds away', async () => {
    const { host, wrap, model } = fixture()
    model.store.setState({ newTiles: { 'h1/w1': Date.now() } })
    render(wrap(<Dock />))
    const dock = screen.getByRole('region', { name: 'Live dock' })
    expect(dock).toHaveTextContent('1 new')
    expect(within(screen.getByRole('listitem', { name: 'app-fix in the dock' })).getByText('new')).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Dismiss app-fix from the dock' }))
    expect(screen.queryByRole('listitem', { name: 'app-fix in the dock' })).toBeNull()
    // It comes back when the agent's state changes.
    act(() => host.store.setState((s) => ({ agents: { ...s.agents, w1: { ...s.agents.w1!, state: AgentState.WORKING } } })))
    expect(screen.getByRole('listitem', { name: 'app-fix in the dock' })).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Hide the live dock' }))
    expect(screen.queryByRole('listitem', { name: /in the dock/ })).toBeNull()
    act(() => model.store.setState({ dockBadge: 2 }))
    expect(screen.getByRole('status')).toHaveTextContent('2 started')
    await userEvent.click(screen.getByRole('button', { name: 'Show the live dock' }))
    expect(model.store.getState().dockBadge).toBe(0)
  })
})

describe('the command palette and shortcuts', () => {
  const Keys = () => {
    useShortcuts()
    return null
  }

  it('opens on its shortcut, filters, runs a command, and closes on Escape', async () => {
    const { wrap, model } = fixture()
    render(wrap(<><Keys /><CommandPalette /></>))
    expect(screen.queryByRole('dialog')).toBeNull()
    fireEvent.keyDown(window, { key: 'P', ctrlKey: true, shiftKey: true })
    const box = await screen.findByRole('combobox', { name: 'Search commands' })
    await userEvent.type(box, 'fix')
    const options = within(screen.getByRole('listbox', { name: 'Commands' })).getAllByRole('option')
    expect(options[0]).toHaveTextContent('Go to app-fix')
    await userEvent.keyboard('{Enter}')
    expect(model.store.getState()).toMatchObject({ agentId: 'w1', paletteOpen: false })
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())

    model.openPalette(true)
    await screen.findByRole('combobox', { name: 'Search commands' })
    await userEvent.keyboard('{Escape}')
    await waitFor(() => expect(model.store.getState().paletteOpen).toBe(false))
  })

  it('asks for a title to start a run, and confirms before it stops an agent', async () => {
    const { wrap, model, client } = fixture()
    render(wrap(<CommandPalette />))
    act(() => model.openPalette(true))
    await userEvent.type(await screen.findByRole('combobox', { name: 'Search commands' }), 'start a run')
    await userEvent.keyboard('{Enter}')
    await userEvent.type(await screen.findByLabelText(/Title of the new run/), 'my run{Enter}')
    await waitFor(() => expect(client.startRun).toHaveBeenCalledWith(expect.objectContaining({ title: 'my run' })))
    await waitFor(() => expect(model.store.getState().paletteOpen).toBe(false))

    act(() => model.openPalette(true))
    await userEvent.type(await screen.findByRole('combobox', { name: 'Search commands' }), 'stop app-fix')
    await userEvent.keyboard('{Enter}')
    expect(client.stopAgent).not.toHaveBeenCalled()
    expect(await screen.findByText(/Its process ends/)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    await userEvent.keyboard('{Enter}') // the filter is still there
    await userEvent.click(await screen.findByRole('button', { name: 'Stop app-fix' }))
    await waitFor(() => expect(client.stopAgent).toHaveBeenCalledWith(expect.objectContaining({ agent: 'w1' })))
  })

  it('follows the host\'s own bindings, and an unbound command does nothing', () => {
    const { host, wrap, model } = fixture()
    const bind = (key: string, value: string) =>
      host.store.setState((s) => ({ settings: { ...s.settings, [key]: create(SettingDescriptor, { key, value: fromJson(ValueSchema, value), isSet: true }) } }))
    bind('app.keys.dock', 'Mod+K')
    bind('app.keys.palette', '')
    render(wrap(<Keys />))
    fireEvent.keyDown(window, { key: 'D', ctrlKey: true, shiftKey: true })
    expect(model.store.getState().dockOpen).toBe(true) // the old chord is free now
    fireEvent.keyDown(window, { key: 'k', ctrlKey: true })
    expect(model.store.getState().dockOpen).toBe(false)
    fireEvent.keyDown(window, { key: 'P', ctrlKey: true, shiftKey: true })
    expect(model.store.getState().paletteOpen).toBe(false)
  })
})

describe('ShortcutField', () => {
  const setting = (key: string, value: string) => create(SettingDescriptor, { key, value: fromJson(ValueSchema, value), isSet: true, schema: create(SettingSchemaSchema, { type: 'shortcut', summary: `shortcut: ${key}` }) })

  it('records the pressed chord, unbinds on Backspace, cancels on Escape, and refuses a chord another command has', async () => {
    const { host, wrap } = fixture()
    host.store.setState({ settings: { 'app.keys.dock': setting('app.keys.dock', 'Mod+Shift+D'), 'app.keys.settings': setting('app.keys.settings', 'Mod+,') } })
    const save = vi.fn(async (_v: unknown) => {})
    render(wrap(<ShortcutField host={host} id="app.keys.dock" value="Mod+Shift+D" busy={false} save={save} />))
    const rec = screen.getByRole('button', { name: 'Record a shortcut for app.keys.dock' })
    await userEvent.click(rec)
    expect(rec).toHaveTextContent('Press the keys')
    // Only modifiers: still waiting. Another command's chord: refused. Then a free one.
    fireEvent.keyDown(rec, { key: 'Control', ctrlKey: true })
    expect(save).not.toHaveBeenCalled()
    fireEvent.keyDown(rec, { key: ',', ctrlKey: true, code: 'Comma' })
    expect(await screen.findByRole('alert')).toHaveTextContent('already does')
    expect(save).not.toHaveBeenCalled()
    fireEvent.keyDown(rec, { key: 'J', ctrlKey: true, altKey: true })
    expect(save).toHaveBeenCalledWith('Mod+Alt+J')
    expect(rec).toHaveTextContent('Change')

    await userEvent.click(rec)
    fireEvent.keyDown(rec, { key: 'Backspace' })
    expect(save).toHaveBeenLastCalledWith('')
    await userEvent.click(rec)
    fireEvent.keyDown(rec, { key: 'Escape' })
    expect(save).toHaveBeenCalledTimes(2)
    expect(rec).toHaveTextContent('Change')
  })
})

describe('ChatView', () => {
  const entry = (id: string, kind: TranscriptKind, o: Partial<{ text: string; tool: string; toolInput: string; toolUseId: string; isError: boolean }> = {}) => create(TranscriptEntrySchema, { id, kind, ...o })
  const res = (entries: ReturnType<typeof entry>[], o: Partial<{ startOffset: bigint; endOffset: bigint; atStart: boolean; supported: boolean; sessionId: string }> = {}) =>
    create(ReadTranscriptResponseSchema, { supported: true, sessionId: 's1', startOffset: 10n, endOffset: 100n, atStart: false, entries, ...o })
  const tail = [
    entry('m', TranscriptKind.MESH, { text: '📬 [mesh] task from lead (ref a1)' }),
    entry('u', TranscriptKind.USER, { text: 'fix the typo' }),
    entry('a', TranscriptKind.ASSISTANT, { text: 'Looking at **README.md**.' }),
    entry('c1', TranscriptKind.TOOL_CALL, { tool: 'Bash', toolInput: 'grep -rn typo .', toolUseId: 't1' }),
    entry('r1', TranscriptKind.TOOL_RESULT, { text: 'README.md:3: typo', toolUseId: 't1' }),
    entry('c2', TranscriptKind.TOOL_CALL, { tool: 'Read', toolInput: '/w/README.md', toolUseId: 't2' }),
    entry('r2', TranscriptKind.TOOL_RESULT, { text: 'no such file', toolUseId: 't2', isError: true }),
  ]
  function chat(o: { agent?: Partial<Parameters<typeof create<typeof AgentSchema>>[1]>; readOnly?: boolean; read?: ReturnType<typeof vi.fn> } = {}) {
    const f = fixture()
    f.host.store.setState((s) => ({
      agents: { ...s.agents, w1: { ...s.agents.w1!, state: AgentState.IDLE, needsInput: 0, stateDetail: '', terminalId: 't', sessionId: 's1', ...o.agent } as typeof s.agents.w1 & object },
      daemon: { capabilities: ['terminals'], actor: { class: o.readOnly ? ActorClass.READ_ONLY_DEVICE : ActorClass.DEVICE } } as never,
    }))
    const readTranscript = o.read ?? vi.fn(async (_req: { offset: bigint; backward: boolean }) => res(tail))
    const promptAgent = vi.fn(async (_req: unknown) => ({}))
    Object.assign(f.client, { readTranscript, promptAgent })
    const ui = f.wrap(<ChatView host={f.host} agentId="w1" active panelId="p" tabId="t" />)
    return { ...f, readTranscript, promptAgent, ui }
  }

  it('shows the conversation as blocks, with tool calls folded and the reply as Markdown', async () => {
    const { ui, readTranscript } = chat()
    render(ui)
    const list = await screen.findByRole('list', { name: 'Conversation' })
    expect(readTranscript).toHaveBeenCalledWith(expect.objectContaining({ agent: 'w1', offset: -1n, backward: true }))
    expect(within(list).getByText(/From the mesh: task from lead/)).toBeInTheDocument()
    expect(within(list).getByText('fix the typo')).toBeInTheDocument()
    expect(within(list).getByText('README.md').tagName).toBe('STRONG')
    const tools = within(list).getByText('Ran 2 tools: Bash, Read').closest('details')!
    expect(tools).not.toHaveAttribute('open')
    expect(tools).toHaveTextContent('README.md:3: typo')
    expect(tools).toHaveTextContent('no such file')
    expect(screen.getByRole('button', { name: 'Load earlier' })).toBeInTheDocument()
  })

  it('reads earlier entries on request and puts them first', async () => {
    const read = vi.fn(async (req: { offset: bigint; backward: boolean }) => (req.backward && req.offset === 10n ? res([entry('old', TranscriptKind.USER, { text: 'the very first prompt' })], { startOffset: 0n, endOffset: 10n, atStart: true }) : res(tail)))
    const { ui } = chat({ read })
    render(ui)
    await userEvent.click(await screen.findByRole('button', { name: 'Load earlier' }))
    const first = (await screen.findAllByRole('listitem'))[0]!
    expect(first).toHaveTextContent('the very first prompt')
    expect(read).toHaveBeenLastCalledWith(expect.objectContaining({ offset: 10n, backward: true }))
    expect(screen.queryByRole('button', { name: 'Load earlier' })).toBeNull()
  })

  it('follows the conversation when the agent does something, from where it stopped', async () => {
    const read = vi.fn(async (req: { offset: bigint; backward: boolean }) => (req.backward ? res(tail) : res([entry('n', TranscriptKind.ASSISTANT, { text: 'a new reply' })], { startOffset: 100n, endOffset: 150n })))
    const { ui, host } = chat({ read })
    render(ui)
    await screen.findByRole('list', { name: 'Conversation' })
    expect(screen.queryByText('a new reply')).toBeNull()
    act(() => host.store.setState((s) => ({ agents: { ...s.agents, w1: { ...s.agents.w1!, state: AgentState.WORKING, stateDetail: 'Bash' } } })))
    expect(await screen.findByText('a new reply')).toBeInTheDocument()
    expect(read).toHaveBeenLastCalledWith(expect.objectContaining({ offset: 100n, backward: false }))
    expect(screen.getByTestId('chat-status')).toHaveTextContent('Working: Bash')
  })

  it('starts over when the conversation changes', async () => {
    const read = vi.fn(async (_req: { offset: bigint; backward: boolean }) => res(tail))
    const { ui, host } = chat({ read })
    render(ui)
    await screen.findByRole('list', { name: 'Conversation' })
    read.mockImplementation(async () => res([entry('x', TranscriptKind.USER, { text: 'after /clear' })], { sessionId: 's2' }))
    act(() => host.store.setState((s) => ({ agents: { ...s.agents, w1: { ...s.agents.w1!, sessionId: 's2' } } })))
    expect(await screen.findByText('after /clear')).toBeInTheDocument()
    expect(screen.queryByText('fix the typo')).toBeNull()
  })

  it('a read of the conversation it left does not free the way for a second read of the new one', async () => {
    const pending: Array<(r: ReturnType<typeof res>) => void> = []
    const read = vi.fn(() => new Promise<ReturnType<typeof res>>((r) => pending.push(r)))
    const { ui, host } = chat({ read })
    render(ui)
    await waitFor(() => expect(read).toHaveBeenCalledTimes(1))
    // The conversation changes while the first read is in flight: the new one's read starts, and is in flight too.
    act(() => host.store.setState((s) => ({ agents: { ...s.agents, w1: { ...s.agents.w1!, sessionId: 's2' } } })))
    await waitFor(() => expect(read).toHaveBeenCalledTimes(2))
    // The old read comes back, then an event asks for another read: it waits for the one in flight.
    await act(async () => pending[0]!(res([entry('old', TranscriptKind.USER, { text: 'stale' })])))
    act(() => host.store.setState((s) => ({ agents: { ...s.agents, w1: { ...s.agents.w1!, stateDetail: 'a' } } })))
    await new Promise((r) => setTimeout(r, 600))
    expect(read).toHaveBeenCalledTimes(2)
    await act(async () => pending[1]!(res(tail, { sessionId: 's2' })))
    expect(await screen.findByText('fix the typo')).toBeInTheDocument()
    expect(screen.queryByText('stale')).toBeNull()
  })

  it('types the prompt into the agent: Enter sends, Shift+Enter adds a line, the host\'s refusal is shown', async () => {
    const { ui, promptAgent } = chat()
    render(ui)
    const box = await screen.findByRole('textbox', { name: 'Prompt' })
    await userEvent.type(box, 'first{Shift>}{Enter}{/Shift}second')
    expect(box).toHaveValue('first\nsecond')
    await userEvent.keyboard('{Enter}')
    await waitFor(() => expect(promptAgent).toHaveBeenCalledWith(expect.objectContaining({ agent: 'w1', text: 'first\nsecond' })))
    await waitFor(() => expect(box).toHaveValue(''))

    promptAgent.mockRejectedValueOnce(new Error("[failed_precondition] app-fix's input box holds a draft"))
    await userEvent.type(box, 'again{Enter}')
    expect(await screen.findByRole('alert')).toHaveTextContent("input box holds a draft")
    expect(box).toHaveValue('again') // kept to send again
  })

  it('says why it cannot be typed into: read-only device, a stopped agent, a prompt waiting in the terminal', async () => {
    const ro = chat({ readOnly: true })
    const { unmount } = render(ro.ui)
    expect(await screen.findByText(/This device is read-only/)).toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: 'Prompt' })).toBeDisabled()
    unmount()

    const gone = chat({ agent: { state: AgentState.RETIRED } })
    const second = render(gone.ui)
    expect(await screen.findByText(/The agent has stopped/)).toBeInTheDocument()
    expect(screen.getByRole('list', { name: 'Conversation' })).toBeInTheDocument() // its conversation can still be read
    second.unmount()

    const asking = chat({ agent: { state: AgentState.NEEDS_INPUT, needsInput: 1 } })
    render(asking.ui)
    await userEvent.click(await screen.findByRole('button', { name: 'Open the terminal' }))
    expect(asking.model.store.getState().tab).toBe('terminal')
  })

  it('tells the user a CLI has no record to read, and still takes prompts', async () => {
    const { ui } = chat({ read: vi.fn(async () => res([], { supported: false })) })
    render(ui)
    expect(await screen.findByText(/has none we read yet/)).toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: 'Prompt' })).toBeEnabled()
  })
})
