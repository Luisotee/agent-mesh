import { create, fromJson } from '@bufbuild/protobuf'
import { ValueSchema, timestampFromMs } from '@bufbuild/protobuf/wkt'
import { Code, ConnectError } from '@connectrpc/connect'
import { act, cleanup, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { DesktopApi } from '../shared/api'
import { RunSidebar } from './components/run-sidebar'
import { NewRunDialog } from './components/spawn-dialogs'
import { NewWorkspaceDialog } from './components/workspace-dialog'
import { SidePanel } from './components/side-panel'
import { DropEdges } from './components/panes'
import { TabBar } from './components/tab-bar'
import { WaitingList } from './components/waiting-list'
import { WatchAll } from './components/watch-all'
import { AppContext } from './context'
import { ReadTranscriptResponseSchema } from './gen/agentmesh/v1/chat_pb'
import { GetAgentResponseSchema, GetCostResponseSchema, GetUsageResponseSchema, ListProjectsResponseSchema, ModelSwapSchema, PendingPermissionSchema, ProjectSchema, ViewRequestSchema, WaitingKind, WaitingSchema } from './gen/agentmesh/v1/mesh_pb'
import { AgentRole, AgentSchema, AgentState, MessageSchema, MessageType, RunSchema, RunStatus, SettingSchema$ as SettingDescriptor, SettingSchemaSchema } from './gen/agentmesh/v1/types_pb'
import { createAppModel, offerView, showView, type HostRuntime } from './model'
import { ViewToasts } from './components/view-toasts'
import { createHostStore } from './store'

afterEach(cleanup)

const agent = (id: string, o: Parameters<typeof create<typeof AgentSchema>>[1] = {}) => create(AgentSchema, { id, name: id, runId: 'r1', repo: '/p/app', kind: 'claude', createdAt: timestampFromMs(1000), state: AgentState.IDLE, ...o })
const list = (key: string, v: string[]) => create(SettingDescriptor, { key, value: fromJson(ValueSchema, v), isSet: true, schema: create(SettingSchemaSchema, { type: 'strings' }) })

function fixture() {
  const store = createHostStore()
  const agents = [agent('lead', { role: AgentRole.ROOT, name: 'app-lead' }), agent('w1', { role: AgentRole.WORKER, name: 'app-fix', parentId: 'lead', state: AgentState.NEEDS_INPUT, needsInput: 1 })]
  store.setState({
    conn: { kind: 'ready', hello: { capabilities: [] } as never },
    synced: true,
    runs: { r1: create(RunSchema, { id: 'r1', title: 'ship it', status: RunStatus.OPEN }) },
    agents: Object.fromEntries(agents.map((a) => [a.id, a])),
    messageLoad: { r1: 'loaded' },
  })
  const client = {
    startRun: vi.fn(async (_req: unknown) => ({ run: { id: 'r9' } })),
    renameRun: vi.fn(async (_req: unknown): Promise<unknown> => ({})),
    decidePermission: vi.fn(async (_req: unknown): Promise<unknown> => ({})),
    sendMessage: vi.fn(async (_req: unknown) => ({})),
    setSetting: vi.fn(async (_req: unknown) => ({})),
    listProjects: vi.fn(async (_req: unknown) => create(ListProjectsResponseSchema)),
    spawnAgent: vi.fn(async (_req: unknown) => ({ agent: { id: 'new' } })),
    getAgent: vi.fn(async (_req: unknown) => create(GetAgentResponseSchema)),
    getCost: vi.fn(async (_req: unknown) => create(GetCostResponseSchema)),
    getUsage: vi.fn(async (_req: unknown) => create(GetUsageResponseSchema)),
    readTranscript: vi.fn(async (_req: unknown) => create(ReadTranscriptResponseSchema, { supported: true })),
  }
  const host = { info: { id: 'h1', name: 'home', kind: 'state-dir', target: '/s' }, store, client, connection: { retryNow: vi.fn(), stop: vi.fn() }, loadMessages: vi.fn() } as unknown as HostRuntime
  const api = { hosts: { list: async () => [], add: vi.fn(), remove: vi.fn(), fixed: false }, request: vi.fn(), links: { take: async () => undefined, onOpen: vi.fn(() => () => {}) }, shell: { notify: vi.fn(), setWaiting: vi.fn(), configure: vi.fn(), onNotificationClick: vi.fn(() => () => {}) } } as unknown as DesktopApi
  const model = createAppModel(api)
  model.store.setState({ loaded: true, hosts: [host], hostId: 'h1' })
  const wrap = (ui: React.ReactNode) => <AppContext.Provider value={model}>{ui}</AppContext.Provider>
  return { host, client, model, wrap }
}

const lastReq = <T,>(fn: { mock: { calls: unknown[][] } }) => fn.mock.calls.at(-1)![0] as T

describe('permission prompts under "Waiting on you"', () => {
  const prompt = create(PendingPermissionSchema, { id: 'p1', runId: 'r1', agentId: 'w1', agentName: 'app-fix', parentId: 'lead', tool: 'Bash', summary: 'rm -rf build' })
  const held = (host: HostRuntime) =>
    host.store.setState({ waiting: [create(WaitingSchema, { kind: WaitingKind.INPUT, runId: 'r1', agentId: 'w1' })], permissions: [prompt] })

  it('replaces the "needs you in its terminal" row of the agent with the tool, what it touches and who it is held for', () => {
    const { host, wrap } = fixture()
    held(host)
    render(wrap(<WaitingList host={host} />))
    const list = screen.getByRole('region', { name: 'Waiting on you' })
    expect(list).toHaveTextContent('app-fix asks to use Bash')
    expect(list).toHaveTextContent('rm -rf build')
    expect(list).toHaveTextContent('held for app-lead')
    expect(list).not.toHaveTextContent('needs you in its terminal')
    expect(within(list).getByText('1')).toBeInTheDocument()
  })

  it('allows it, and drops the row', async () => {
    const { host, client, wrap } = fixture()
    held(host)
    render(wrap(<WaitingList host={host} />))
    await userEvent.click(screen.getByRole('button', { name: 'Allow' }))
    expect(lastReq<{ id: string; decision: string; meta: { mutationId: string } }>(client.decidePermission)).toMatchObject({ id: 'p1', decision: 'allow' })
    expect(host.store.getState().permissions).toEqual([])
  })

  it('denies only with a reason, which the agent reads', async () => {
    const { host, client, wrap } = fixture()
    held(host)
    render(wrap(<WaitingList host={host} />))
    await userEvent.click(screen.getByRole('button', { name: 'Deny…' }))
    const deny = screen.getByRole('button', { name: 'Deny' })
    expect(deny).toBeDisabled()
    await userEvent.type(screen.getByLabelText('Why app-fix may not'), 'use make clean instead')
    await userEvent.click(deny)
    expect(lastReq(client.decidePermission)).toMatchObject({ id: 'p1', decision: 'deny', why: 'use make clean instead' })
  })

  it('says a prompt someone answered first is already answered, and takes the buttons away until the host lists again', async () => {
    const { host, client, wrap } = fixture()
    held(host)
    client.decidePermission.mockRejectedValue(new ConnectError('no pending permission', Code.NotFound))
    render(wrap(<WaitingList host={host} />))
    await userEvent.click(screen.getByRole('button', { name: 'Allow' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('Already answered')
    expect(screen.queryByRole('button', { name: 'Allow' })).toBeNull()
  })
})

describe('Watch all answers a plan or question in the tile', () => {
  it('approves a plan, and sends an answer to a question, to the agent that asked', async () => {
    const { host, client, wrap, model } = fixture()
    const plan = create(MessageSchema, { id: 'm1', runId: 'r1', type: MessageType.PLAN, body: 'Add a retry', from: { id: 'w1', name: 'app-fix' } as never, toId: 'lead' })
    host.store.setState({ waiting: [create(WaitingSchema, { kind: WaitingKind.PLAN, runId: 'r1', agentId: 'w1', message: plan })] })
    model.store.setState({ watchOpen: true })
    render(wrap(<WatchAll />))
    const tile = screen.getByRole('listitem', { name: 'app-fix in the grid' })
    expect(within(tile).getByRole('region', { name: 'Plan from app-fix' })).toHaveTextContent('Add a retry')
    await userEvent.click(within(tile).getByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(client.sendMessage).toHaveBeenCalledTimes(1))
    expect(lastReq(client.sendMessage)).toMatchObject({ to: 'w1', type: MessageType.ANSWER, body: 'approved' })
    // The other tile has nothing to answer.
    expect(within(screen.getByRole('listitem', { name: 'app-lead in the grid' })).queryByRole('region')).toBeNull()
  })
})

describe('renaming a run', () => {
  it('edits the title in place: Enter saves it through the registry, and the pencil is there for every run', async () => {
    const { host, client, wrap } = fixture()
    render(wrap(<RunSidebar host={host} />))
    await userEvent.click(screen.getByRole('button', { name: 'Rename ship it' }))
    const box = screen.getByLabelText('Run title')
    expect(box).toHaveValue('ship it')
    await userEvent.clear(box)
    await userEvent.type(box, 'the checkout{Enter}')
    expect(lastReq(client.renameRun)).toMatchObject({ runId: 'r1', title: 'the checkout' })
    await waitFor(() => expect(screen.queryByLabelText('Run title')).toBeNull())
  })

  it('keeps the form open with the host\'s reason when the rename fails', async () => {
    const { host, client, wrap } = fixture()
    client.renameRun.mockRejectedValue(new ConnectError('give the run a title', Code.InvalidArgument))
    render(wrap(<RunSidebar host={host} />))
    await userEvent.click(screen.getByRole('button', { name: 'Rename ship it' }))
    await userEvent.type(screen.getByLabelText('Run title'), ' now{Enter}')
    expect(await screen.findByRole('alert')).toHaveTextContent('give the run a title')
  })
})

describe('why there is nothing ready to review', () => {
  it('says the GitHub CLI is missing or not logged in, and nothing when it works', () => {
    const { host, wrap } = fixture()
    host.store.setState({ githubProblem: 'no_gh' })
    render(wrap(<RunSidebar host={host} />))
    expect(screen.getByRole('note')).toHaveTextContent("isn't installed on the host")
    act(() => host.store.setState({ githubProblem: 'not_logged_in' }))
    expect(screen.getByRole('note')).toHaveTextContent('gh auth login')
    act(() => host.store.setState({ githubProblem: '' }))
    expect(screen.queryByRole('note')).toBeNull()
  })
})

describe('a new run with autonomy of its own', () => {
  it('starts with the level and the ask-before lines chosen, and with none when left on the host\'s settings', async () => {
    const { host, client, wrap } = fixture()
    host.store.setState({ settings: { 'autonomy.ask_before': list('autonomy.ask_before', ['migrations']) } })
    render(wrap(<NewRunDialog host={host} />))
    await userEvent.click(screen.getByRole('button', { name: /New run/ }))
    expect(screen.getByText(/Always asked, from the host's settings: migrations/)).toBeInTheDocument()
    await userEvent.type(screen.getByLabelText('Title'), 'careful')
    await userEvent.click(screen.getByRole('button', { name: 'Start run' }))
    await waitFor(() => expect(client.startRun).toHaveBeenCalledTimes(1))
    expect((lastReq(client.startRun) as { autonomy?: unknown }).autonomy).toBeUndefined()

    await userEvent.click(screen.getByRole('button', { name: /New run/ }))
    await userEvent.type(screen.getByLabelText('Title'), 'careful')
    await userEvent.selectOptions(screen.getByLabelText('What the lead may do on its own'), 'plans')
    await userEvent.type(screen.getByLabelText('Ask me before (one per line)'), 'deploys{Enter}{Enter}migrations')
    await userEvent.click(screen.getByRole('button', { name: 'Start run' }))
    await waitFor(() => expect(client.startRun).toHaveBeenCalledTimes(2))
    expect(lastReq(client.startRun)).toMatchObject({ autonomy: { userApprovesPlans: true, userMerges: true, askBefore: ['deploys', 'migrations'], keepRepoLists: false } })
  })
})

describe('the New workspace dialog\'s project list', () => {
  const projects = [create(ProjectSchema, { path: '/p/shop', name: 'shop', inReposDir: true }), create(ProjectSchema, { path: '/p/api', name: 'api', inReposDir: true }), create(ProjectSchema, { path: '/p/web', name: 'web', inReposDir: true })]
  function open() {
    const f = fixture()
    f.client.listProjects.mockImplementation(async () => create(ListProjectsResponseSchema, { projects }))
    f.model.store.setState({ workspaceOpen: true })
    render(f.wrap(<NewWorkspaceDialog host={f.host} trigger={<button>open</button>} />))
    return f
  }

  it('narrows to what is typed, and a click fills the path in', async () => {
    const { model } = open()
    const list = await screen.findByRole('list', { name: 'Repos on this host' })
    // The host's three, and the repo an agent already works in.
    expect(within(list).getAllByRole('listitem')).toHaveLength(4)
    await userEvent.type(screen.getByLabelText('Project'), 'SHO')
    expect(within(screen.getByRole('list', { name: 'Repos on this host' })).getAllByRole('listitem').map((li) => li.textContent)).toEqual([expect.stringContaining('shop')])
    await userEvent.click(within(screen.getByRole('list', { name: 'Repos on this host' })).getByRole('button', { name: 'shop /p/shop' }))
    expect(screen.getByLabelText('Project')).toHaveValue('/p/shop')
    expect(model.store.getState().pins).toEqual({})
  })

  it('pins a project, lists it first, and keeps the pin for the host', async () => {
    const { model } = open()
    await screen.findByRole('list', { name: 'Repos on this host' })
    await userEvent.click(screen.getByRole('button', { name: 'Pin web' }))
    expect(model.store.getState().pins).toEqual({ h1: ['/p/web'] })
    expect(within(screen.getByRole('list', { name: 'Repos on this host' })).getAllByRole('listitem')[0]).toHaveTextContent('web')
    expect(screen.getByRole('button', { name: 'Unpin web' })).toHaveAttribute('aria-pressed', 'true')
    await userEvent.click(screen.getByRole('button', { name: 'Unpin web' }))
    expect(model.store.getState().pins).toEqual({})
  })

  it('opens on the run the palette named', async () => {
    const f = fixture()
    f.model.openWorkspace(true, 'r1')
    render(f.wrap(<NewWorkspaceDialog host={f.host} trigger={<button>open</button>} />))
    expect(await screen.findByLabelText('Run')).toHaveValue('r1')
  })
})

describe('the side panel', () => {
  it('lists the models the agent ran on, once it ran on more than one', async () => {
    const { host, client, wrap } = fixture()
    client.getAgent.mockResolvedValue(create(GetAgentResponseSchema, { modelHistory: [create(ModelSwapSchema, { model: 'opus', effort: 'high', why: 'spawned' }), create(ModelSwapSchema, { model: 'sonnet', effort: 'medium', why: 'plan approved' })] }))
    render(wrap(<SidePanel host={host} agentId="w1" />))
    const history = await screen.findByRole('list', { name: 'Model history' })
    expect(within(history).getAllByRole('listitem').map((li) => li.textContent)).toEqual(['opus · highspawned', 'sonnet · mediumplan approved'])
    expect(lastReq(client.getAgent)).toMatchObject({ agent: 'w1' })
  })

  it('shows no history for a single model', async () => {
    const { host, client, wrap } = fixture()
    client.getAgent.mockResolvedValue(create(GetAgentResponseSchema, { modelHistory: [create(ModelSwapSchema, { model: 'opus', why: 'spawned' })] }))
    render(wrap(<SidePanel host={host} agentId="w1" />))
    await waitFor(() => expect(client.getAgent).toHaveBeenCalled())
    expect(screen.queryByRole('list', { name: 'Model history' })).toBeNull()
  })

  it('shows the host\'s ask-before list one to a line and saves an edit as a whole list', async () => {
    const { host, client, wrap } = fixture()
    host.store.setState({ settings: { 'autonomy.ask_before': list('autonomy.ask_before', ['deploys']) } })
    render(wrap(<SidePanel host={host} agentId="w1" />))
    const box = screen.getByLabelText('Always ask me before')
    expect(box).toHaveValue('deploys')
    await userEvent.type(box, '{Enter}migrations{Enter}deploys')
    await userEvent.tab()
    await waitFor(() => expect(client.setSetting).toHaveBeenCalledTimes(1))
    expect(lastReq<{ key: string; value: unknown }>(client.setSetting).key).toBe('autonomy.ask_before')
  })
})

describe('OpenView', () => {
  it('shows an agent\'s chat or terminal, a run\'s lead, or the settings, and nothing for what the host lacks', () => {
    const { model } = fixture()
    const view = (v: string, target: string) => showView(model, 'h1', create(ViewRequestSchema, { view: v, target }))
    view('agent', 'w1')
    expect(model.store.getState()).toMatchObject({ agentId: 'w1', tab: 'chat' })
    view('terminal', 'lead')
    expect(model.store.getState()).toMatchObject({ agentId: 'lead', tab: 'terminal' })
    view('run', 'r1')
    expect(model.store.getState().agentId).toBe('lead')
    view('settings', 'tiers.')
    expect(model.store.getState()).toMatchObject({ settingsOpen: true, settingsQuery: 'tiers.' })
    const before = model.store.getState()
    view('agent', 'nobody')
    view('run', 'nope')
    expect(model.store.getState()).toBe(before)
  })
})

describe('an agent asking to show a view', () => {
  const req = (view: string, target: string, from = 'agent-mesh-lead') => create(ViewRequestSchema, { view, target, from })

  it('is a toast naming who asks and what, which the user opens or dismisses; nothing switches by itself', async () => {
    const { model, wrap } = fixture()
    render(wrap(<ViewToasts />))
    offerView(model, 'h1', req('agent', 'w1'), false)
    expect(model.store.getState().agentId).toBeUndefined()
    const toast = await screen.findByRole('status')
    expect(toast).toHaveTextContent("agent-mesh-lead wants to show you app-fix's chat.")
    await userEvent.click(within(toast).getByRole('button', { name: 'Open' }))
    expect(model.store.getState()).toMatchObject({ agentId: 'w1', tab: 'chat', viewRequests: [] })
    offerView(model, 'h1', req('settings', ''), false)
    await userEvent.click(await screen.findByRole('button', { name: 'Dismiss' }))
    expect(model.store.getState().viewRequests).toEqual([])
    expect(model.store.getState().settingsOpen).toBe(false)
  })

  it('switches at once only when the user allowed it, and never while they type', () => {
    const { model } = fixture()
    offerView(model, 'h1', req('agent', 'w1'), true)
    expect(model.store.getState()).toMatchObject({ agentId: 'w1', viewRequests: [] })
    const box = document.createElement('input')
    document.body.append(box)
    box.focus()
    offerView(model, 'h1', req('agent', 'lead'), true)
    expect(model.store.getState().agentId).toBe('w1')
    expect(model.store.getState().viewRequests).toHaveLength(1)
    box.remove()
  })

  it('keeps at most three toasts, and one for the same thing asked twice', () => {
    const { model } = fixture()
    for (const t of ['a', 'b', 'c', 'd']) offerView(model, 'h1', req('agent', t), false)
    offerView(model, 'h1', req('agent', 'd'), false)
    expect(model.store.getState().viewRequests.map((r) => r.request.target)).toEqual(['b', 'c', 'd'])
  })
})

describe('dragging a tab to an edge of the main pane', () => {
  it('shows the edges only during a drag, and a drop on one opens the tab as a split', async () => {
    const { host, wrap, model } = fixture()
    model.selectAgent('h1', 'lead')
    model.selectAgent('h1', 'w1')
    model.selectAgent('h1', 'lead')
    render(wrap(<><TabBar /><div className="relative"><DropEdges /></div></>))
    expect(document.querySelector('[data-edge]')).toBeNull()
    const tab = screen.getByRole('tab', { name: /app-fix/ })
    const draggable = tab.parentElement!
    expect(draggable).toHaveAttribute('draggable', 'true')
    const { fireEvent } = await import('@testing-library/react')
    fireEvent.dragStart(draggable, { dataTransfer: { setData: vi.fn(), effectAllowed: '' } })
    expect(model.store.getState().dragTab).toEqual({ hostId: 'h1', agentId: 'w1' })
    const bottom = document.querySelector('[data-edge="bottom"]')!
    fireEvent.dragOver(bottom, { dataTransfer: { dropEffect: '' } })
    fireEvent.drop(bottom, { dataTransfer: {} })
    expect(model.store.getState()).toMatchObject({ splitDir: 'column', dragTab: undefined, agentId: 'lead' })
    expect(model.store.getState().side.map((p) => p.agentId)).toEqual(['w1'])
    void host
  })

  it('takes the main pane when dropped on the left, and clears the edges when the drag ends without a drop', async () => {
    const { wrap, model } = fixture()
    model.selectAgent('h1', 'lead')
    model.selectAgent('h1', 'w1')
    model.selectAgent('h1', 'lead')
    render(wrap(<><TabBar /><div className="relative"><DropEdges /></div></>))
    const { fireEvent } = await import('@testing-library/react')
    const draggable = screen.getByRole('tab', { name: /app-fix/ }).parentElement!
    fireEvent.dragStart(draggable, { dataTransfer: { setData: vi.fn(), effectAllowed: '' } })
    fireEvent.drop(document.querySelector('[data-edge="left"]')!, { dataTransfer: {} })
    expect(model.store.getState()).toMatchObject({ splitDir: 'row', agentId: 'w1' })
    expect(model.store.getState().side.map((p) => p.agentId)).toEqual(['lead'])
    fireEvent.dragStart(draggable, { dataTransfer: { setData: vi.fn(), effectAllowed: '' } })
    fireEvent.dragEnd(draggable)
    expect(model.store.getState().dragTab).toBeUndefined()
  })
})
