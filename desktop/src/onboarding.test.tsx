import { create } from '@bufbuild/protobuf'
import { Code, ConnectError } from '@connectrpc/connect'
import { act, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import type { DesktopApi, HostInfo, LocalCandidate } from '../shared/api'
import { App } from './App'
import { ConnectionBanner, isLocalHost } from './components/connection-banner'
import { EmptyPane } from './components/empty-pane'
import { LocalSetup } from './components/local-setup'
import { RemoveHostDialog } from './components/host-dialogs'
import { AppContext } from './context'
import { StartSetupResponseSchema } from './gen/agentmesh/v1/mesh_pb'
import { AgentRole, AgentSchema, AgentState, RunSchema, RunStatus } from './gen/agentmesh/v1/types_pb'
import { createAppModel, setupMessage, type HostRuntime } from './model'
import { createHostStore } from './store'

const cand = (o: Partial<LocalCandidate> & { id: string }): LocalCandidate => ({ label: o.id === 'native' ? 'This computer' : 'WSL: Ubuntu', state: 'ready', detail: 'A daemon is running here.', fixes: [], ...o })
const info: HostInfo = { id: 'h1', name: 'laptop', kind: 'state-dir', target: '/s' }

function fakeApi(opts: { detect?: LocalCandidate[] | (() => Promise<LocalCandidate[]>); hosts?: HostInfo[]; fixed?: boolean } = {}) {
  const detect = vi.fn(async () => (typeof opts.detect === 'function' ? opts.detect() : (opts.detect ?? [])))
  const api = {
    hosts: { list: async () => opts.hosts ?? [], add: vi.fn(async () => info), remove: vi.fn(async (_id: string) => {}), fixed: !!opts.fixed },
    request: vi.fn(() => () => {}),
    openSocket: vi.fn(),
    links: { take: async () => undefined, onOpen: vi.fn(() => () => {}) },
    shell: { notify: vi.fn(), setWaiting: vi.fn(), configure: vi.fn(), onNotificationClick: vi.fn(() => () => {}) },
    local: { detect, connect: vi.fn(async (_id: string) => info), fix: vi.fn(async (_id: string, _fix: string): Promise<LocalCandidate[]> => []) },
  }
  return api
}

const startSetupCalled = (api: ReturnType<typeof fakeApi>) => api.request.mock.calls.some((c) => (c as unknown as [string, { path: string }])[1].path.endsWith('/StartSetup'))

describe('the first-run screen', () => {
  it('connects by itself to the one daemon it finds, with nothing pasted, and goes on to the setup assistant', async () => {
    const api = fakeApi({ detect: [cand({ id: 'wsl:Ubuntu' })] })
    render(<App api={api as unknown as DesktopApi} />)
    await waitFor(() => expect(api.local.connect).toHaveBeenCalledWith('wsl:Ubuntu'))
    expect(api.local.connect).toHaveBeenCalledTimes(1)
    // The host is saved and its setup agent asked for, with no click.
    await waitFor(() => expect(startSetupCalled(api)).toBe(true))
    expect(screen.queryByText('Connect to agent-mesh')).toBeNull()
  })

  it('says what is wrong where it can\'t connect, with the commands to copy, and does not connect', async () => {
    const api = fakeApi({ detect: [cand({ id: 'wsl:Ubuntu', state: 'not-installed', detail: "agent-mesh isn't installed in Ubuntu.", commands: ['git clone https://example/agent-mesh', 'cd agent-mesh && ./install.sh'] })] })
    render(<App api={api as unknown as DesktopApi} />)
    expect(await screen.findByText("agent-mesh isn't installed in Ubuntu.")).toBeInTheDocument()
    expect(screen.getByText('Not installed')).toBeInTheDocument()
    expect(screen.getByText('cd agent-mesh && ./install.sh')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Copy cd agent-mesh && ./install.sh' })).toBeInTheDocument()
    expect(api.local.connect).not.toHaveBeenCalled()
  })

  it('offers the fix, says what it will do, runs it on a click, and then lets the user connect', async () => {
    const api = fakeApi({ detect: [cand({ id: 'wsl:Ubuntu', state: 'no-listen', detail: 'It runs, but only inside WSL.', fixes: ['enable-listen'] })] })
    api.local.fix.mockResolvedValue([cand({ id: 'wsl:Ubuntu' })])
    render(<App api={api as unknown as DesktopApi} />)
    expect(await screen.findByText(/makes the daemon listen on 127\.0\.0\.1:9443/)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Let Windows reach it' }))
    await waitFor(() => expect(api.local.fix).toHaveBeenCalledWith('wsl:Ubuntu', 'enable-listen'))
    // Connecting after a fix is the user's click.
    expect(api.local.connect).not.toHaveBeenCalled()
    await userEvent.click(await screen.findByRole('button', { name: 'Connect' }))
    await waitFor(() => expect(api.local.connect).toHaveBeenCalledWith('wsl:Ubuntu'))
  })

  it('shows a failed fix instead of going on', async () => {
    const api = fakeApi({ detect: [cand({ id: 'native', state: 'not-running', detail: "The daemon isn't running.", fixes: ['start'] })] })
    api.local.fix.mockRejectedValue(new Error("Error invoking remote method 'local:fix': Error: systemctl failed"))
    render(<App api={api as unknown as DesktopApi} />)
    await userEvent.click(await screen.findByRole('button', { name: 'Start the daemon' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('systemctl failed')
    expect(screen.getByRole('alert').textContent).not.toContain('Error invoking')
  })

  it('leaves the choice to the user when it finds more than one', async () => {
    const api = fakeApi({ detect: [cand({ id: 'wsl:Ubuntu' }), cand({ id: 'wsl:Debian', label: 'WSL: Debian' })] })
    render(<App api={api as unknown as DesktopApi} />)
    expect(await screen.findAllByRole('button', { name: 'Connect' })).toHaveLength(2)
    expect(api.local.connect).not.toHaveBeenCalled()
  })

  it('keeps the manual form as the fallback', async () => {
    const api = fakeApi({ detect: [] })
    render(<App api={api as unknown as DesktopApi} />)
    await userEvent.click(await screen.findByRole('button', { name: 'Connect another way' }))
    // The old form is there, on its own default (a state dir), with the way to a pairing link beside it.
    expect(screen.getByLabelText('State directory')).toBeInTheDocument()
    expect(screen.getByRole('option', { name: 'Another machine, by a pairing link' })).toBeInTheDocument()
  })

  it('does not reconnect by itself after the user removed the host', async () => {
    const api = fakeApi({ hosts: [info], detect: [cand({ id: 'native' })] })
    render(<App api={api as unknown as DesktopApi} />)
    await userEvent.click(await screen.findByRole('button', { name: 'Hosts' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'Remove laptop…' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Remove host' }))
    await waitFor(() => expect(api.hosts.remove).toHaveBeenCalledWith('h1'))
    expect(await screen.findByRole('button', { name: 'Connect' })).toBeInTheDocument()
    expect(api.local.connect).not.toHaveBeenCalled()
  })

  it('does not look for a daemon here when the app was opened by a pairing link: the link\'s form is what the user came for', async () => {
    const api = fakeApi({ detect: [cand({ id: 'native' })] })
    api.links.take = vi.fn(async () => ({ link: 'agentmesh://pair?v=1&h=h_1&t=amp_one', hostName: 'Home server' })) as never
    render(<App api={api as unknown as DesktopApi} />)
    expect(await screen.findByLabelText('Pairing link')).toHaveValue('agentmesh://pair?v=1&h=h_1&t=amp_one')
    expect(api.local.detect).not.toHaveBeenCalled()
    expect(api.local.connect).not.toHaveBeenCalled()
  })

  it('starts the setup agent for a daemon found here, but not for a host added by hand', async () => {
    const byHand = fakeApi()
    const model = createAppModel(byHand as unknown as DesktopApi)
    await model.load()
    await model.addHost({ name: 'home', kind: 'state-dir', path: '/s' })
    await new Promise((r) => setTimeout(r, 20))
    expect(startSetupCalled(byHand)).toBe(false)
    const found = fakeApi()
    const m2 = createAppModel(found as unknown as DesktopApi)
    await m2.load()
    await m2.connectLocal('native', true)
    await waitFor(() => expect(startSetupCalled(found)).toBe(true))
  })

  it('does not start the setup agent for a daemon the add dialog found (a host that may have weeks of runs)', async () => {
    const api = fakeApi({ detect: [cand({ id: 'native' })] })
    const model = createAppModel(api as unknown as DesktopApi)
    await model.load()
    render(
      <AppContext.Provider value={model}>
        <LocalSetup />
      </AppContext.Provider>,
    )
    await userEvent.click(await screen.findByRole('button', { name: 'Connect' }))
    await waitFor(() => expect(api.local.connect).toHaveBeenCalledWith('native'))
    await waitFor(() => expect(model.store.getState().hosts).toHaveLength(1))
    await new Promise((r) => setTimeout(r, 20))
    expect(startSetupCalled(api)).toBe(false)
  })

  it('has nothing to detect when the host list is fixed by the environment', async () => {
    const api = fakeApi({ fixed: true })
    render(<App api={api as unknown as DesktopApi} />)
    expect(await screen.findByText(/set by the environment/)).toBeInTheDocument()
    expect(api.local.detect).not.toHaveBeenCalled()
  })
})

describe('the setup assistant', () => {
  function hostWith(client: Record<string, unknown>) {
    const store = createHostStore()
    store.setState({ conn: { kind: 'ready', hello: { capabilities: [] } as never }, synced: true })
    const host = { info, store, client, connection: { retryNow: vi.fn(), stop: vi.fn() }, loadMessages: vi.fn(), stop: vi.fn() } as unknown as HostRuntime
    const api = fakeApi()
    const model = createAppModel(api as unknown as DesktopApi)
    model.store.setState({ loaded: true, hosts: [host], hostId: 'h1' })
    return { host, model, api }
  }

  it('opens its chat, with the agent already in the host\'s state', async () => {
    const lead = create(AgentSchema, { id: 'a1', name: 'setup-agent', runId: 'r1', role: AgentRole.ROOT, state: AgentState.STARTING })
    const run = create(RunSchema, { id: 'r1', title: 'Setup', status: RunStatus.OPEN, setup: true })
    const startSetup = vi.fn(async (_req: unknown) => create(StartSetupResponseSchema, { agent: lead, run }))
    const { host, model } = hostWith({ startSetup })
    await model.openSetup('h1')
    expect(model.store.getState()).toMatchObject({ hostId: 'h1', agentId: 'a1', tab: 'chat', setupError: undefined })
    expect(host.store.getState().agents.a1?.name).toBe('setup-agent')
    expect(host.store.getState().runs.r1?.setup).toBe(true)
    expect((startSetup.mock.calls[0]![0] as { meta: { mutationId: string } }).meta.mutationId).toBeTruthy()
  })

  it('says a daemon that is older than the app needs updating, and says any other reason as the host gave it', async () => {
    const old = hostWith({ startSetup: async () => Promise.reject(new ConnectError('not found', Code.Unimplemented)) })
    await old.model.openSetup('h1')
    expect(old.model.store.getState().setupError).toMatchObject({ hostId: 'h1' })
    expect(old.model.store.getState().setupError?.message).toMatch(/older than the app.*install\.sh/)
    expect(setupMessage(new ConnectError('no agent CLI is installed', Code.FailedPrecondition))).toBe('no agent CLI is installed')
  })

  it('is offered on the empty pane of a host with nothing running, and shows why it could not start', async () => {
    const startSetup = vi.fn(async (_req: unknown) => Promise.reject(new ConnectError('no agent CLI is installed', Code.FailedPrecondition)))
    const { host, model } = hostWith({ startSetup })
    render(
      <AppContext.Provider value={model}>
        <EmptyPane host={host} />
      </AppContext.Provider>,
    )
    expect(screen.getByText(/Nothing is running on laptop yet/)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Set up with the assistant' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('no agent CLI is installed')
    expect(startSetup).toHaveBeenCalledTimes(1)
  })

  it('shows a failure only on the host it happened on', async () => {
    const { host, model } = hostWith({ startSetup: async () => Promise.reject(new ConnectError('no agent CLI is installed', Code.FailedPrecondition)) })
    await model.openSetup('h1')
    const other = { ...host, info: { ...info, id: 'h2', name: 'desk' }, store: createHostStore() } as unknown as HostRuntime
    render(
      <AppContext.Provider value={model}>
        <EmptyPane host={other} />
      </AppContext.Provider>,
    )
    expect(screen.getByText(/Nothing is running on desk yet/)).toBeInTheDocument()
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('says only to pick an agent when the host already has some', () => {
    const { host, model } = hostWith({})
    host.store.setState({ agents: { a: create(AgentSchema, { id: 'a', name: 'a' }) } })
    render(
      <AppContext.Provider value={model}>
        <EmptyPane host={host} />
      </AppContext.Provider>,
    )
    expect(screen.getByText('Pick an agent in a run to see it here.')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Set up with the assistant' })).toBeInTheDocument()
  })
})

describe('removing a host and the banner', () => {
  it('removes a host only after a confirmation that says what stays', async () => {
    const api = fakeApi()
    const model = createAppModel(api as unknown as DesktopApi)
    const host = { info, store: createHostStore(), client: {}, connection: { stop: vi.fn() }, stop: vi.fn() } as unknown as HostRuntime
    model.store.setState({ loaded: true, hosts: [host], hostId: 'h1' })
    render(
      <AppContext.Provider value={model}>
        <RemoveHostDialog host={host} open onOpenChange={() => {}} />
      </AppContext.Provider>,
    )
    expect(await screen.findByText(/daemon, its runs and its agents are not touched/)).toBeInTheDocument()
    expect(api.hosts.remove).not.toHaveBeenCalled()
    await act(async () => userEvent.click(screen.getByRole('button', { name: 'Remove host' })))
    await waitFor(() => expect(api.hosts.remove).toHaveBeenCalledWith('h1'))
    expect(model.store.getState().hosts).toEqual([])
  })

  it('takes main\'s word for which host is this machine\'s daemon: a state dir of a test daemon is not', () => {
    expect(isLocalHost({ id: 'a', name: 'a', kind: 'state-dir', target: '/s', here: true })).toBe(true)
    expect(isLocalHost({ id: 'a', name: 'a', kind: 'state-dir', target: '/tmp/test-daemon' })).toBe(false)
    expect(isLocalHost({ id: 'a', name: 'a', kind: 'paired', target: '127.0.0.1:9443', here: false })).toBe(false)
  })

  it('offers "Check this machine" on an unreachable local host and not on a remote one', async () => {
    const make = (i: HostInfo) => {
      const store = createHostStore()
      store.setState({ conn: { kind: 'reconnecting', error: 'refused', retryAt: Date.now() + 5000 } as never })
      return { info: i, store, client: {}, connection: { retryNow: vi.fn(), stop: vi.fn() }, stop: vi.fn() } as unknown as HostRuntime
    }
    const model = createAppModel(fakeApi() as unknown as DesktopApi)
    const local = render(<AppContext.Provider value={model}><ConnectionBanner host={make({ ...info, here: true })} /></AppContext.Provider>)
    expect(screen.getByRole('button', { name: 'Check this machine' })).toBeInTheDocument()
    local.unmount()
    render(<AppContext.Provider value={model}><ConnectionBanner host={make({ ...info, kind: 'paired', target: '10.0.0.2:9443' })} /></AppContext.Provider>)
    expect(screen.queryByRole('button', { name: 'Check this machine' })).toBeNull()
  })

  it('does not offer it on a state-dir host main did not say is this machine\'s', () => {
    const store = createHostStore()
    store.setState({ conn: { kind: 'reconnecting', error: 'refused', retryAt: Date.now() + 5000 } as never })
    const host = { info, store, client: {}, connection: { retryNow: vi.fn(), stop: vi.fn() }, stop: vi.fn() } as unknown as HostRuntime
    render(<AppContext.Provider value={createAppModel(fakeApi() as unknown as DesktopApi)}><ConnectionBanner host={host} /></AppContext.Provider>)
    expect(screen.queryByRole('button', { name: 'Check this machine' })).toBeNull()
  })
})
