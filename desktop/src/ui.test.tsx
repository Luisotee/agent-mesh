import { useState } from 'react'
import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import type { DesktopApi } from '../shared/api'
import { App } from './App'
import { AgentPanel, SendMessageDialog } from './components/agent-panel'
import { Markdown } from './components/markdown'
import { MarkdownField } from './components/ui/markdown-field'
import { ConnectionBanner } from './components/connection-banner'
import { addPrompt } from './store'
import { ChatView } from './components/chat-view'
import { SettingsPanel } from './components/settings-panel'
import { CommandPalette } from './components/command-palette'
import { Dock } from './components/dock'
import { Panes } from './components/panes'
import { SidePanel } from './components/side-panel'
import { WatchAll } from './components/watch-all'
import { ModelControl } from './components/model-control'
import { VendorIcon } from './components/vendor-icon'
import { NewWorkspaceDialog } from './components/workspace-dialog'
import { ShortcutField } from './components/shortcut-field'
import { useShortcuts } from './shortcuts'
import { TabBar } from './components/tab-bar'
import { ShellCloseDialog } from './components/shell-panel'
import { keyedCommands } from './commands'
import { Code, ConnectError } from '@connectrpc/connect'
import { pruneHost } from './layout'
import { MessageCard } from './components/messages-panel'
import { ProjectSidebar, stepSidebar } from './components/project-sidebar'
import { ProjectSettingsDialog } from './components/project-settings'
import { SidebarFooter } from './components/sidebar-footer'
import { FindBar } from './components/find-bar'
import { WaitingList } from './components/waiting-list'
import type { ConnState } from './connection'
import { AppContext, useHostState } from './context'
import { messagesOf } from './derive'
import { SettingSchema$ as SettingDescriptor, SettingSchemaSchema, AgentRole, AgentSchema, AgentState, ReopenSchema, MessageSchema, MessageType, RunSchema, RunStatus, QueuedPromptSchema, type QueuedPrompt } from './gen/agentmesh/v1/types_pb'
import { fromJson } from '@bufbuild/protobuf'
import { ValueSchema } from '@bufbuild/protobuf/wkt'
import { createAppModel, type HostRuntime } from './model'
import { WaitingKind, WaitingSchema } from './gen/agentmesh/v1/mesh_pb'
import { ReadTranscriptResponseSchema, TranscriptEntrySchema, TranscriptKind } from './gen/agentmesh/v1/chat_pb'
import { ActorClass } from './gen/agentmesh/v1/actor_pb'
import { createHostStore } from './store'
import { AgentPullRequestsSchema, PullRequestSchema } from './gen/agentmesh/v1/pr_pb'
import { AgentCostSchema, GetCostResponseSchema } from './gen/agentmesh/v1/report_pb'
import { CheckModelsResponseSchema, KindModelsSchema, ModelFamilyEntrySchema } from './gen/agentmesh/v1/report_pb'
import { ShellSchema, ListShellsResponseSchema, OpenShellResponseSchema, CloseShellResponseSchema } from './gen/agentmesh/v1/mesh_pb'
import { ClaudeUsageSchema, ClaudeUsageWindowSchema, GetUsageResponseSchema, ListProjectsResponseSchema, ProjectPullRequestSchema, ProjectSchema } from './gen/agentmesh/v1/mesh_pb'

const noLinks = { take: async () => undefined, onOpen: vi.fn(() => () => {}) }
const noLocal = { detect: async () => [], connect: vi.fn(), fix: vi.fn() }
const manually = async () => userEvent.click(await screen.findByRole('button', { name: 'Connect another way' }))

type AgentInit = Parameters<typeof create<typeof AgentSchema>>[1]
const agent = (id: string, o: AgentInit = {}) => create(AgentSchema, { id, name: id, runId: 'r1', repo: '/p/app', kind: 'claude', createdAt: timestampFromMs(1000), state: AgentState.IDLE, ...o })

const noTiers = vi.hoisted(() => ({ on: false }))
vi.mock('./components/spawn-dialogs', async (importOriginal) => {
  const m = await importOriginal<typeof import('./components/spawn-dialogs')>()
  return { ...m, tiersOf: (settings: Record<string, unknown>, kind: string) => (noTiers.on ? [] : m.tiersOf(settings, kind)) }
})

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
  const client = { startRun: vi.fn(async (_req: unknown): Promise<unknown> => ({})), setSetting: vi.fn(async (_req: unknown) => ({})), holdPullRequest: vi.fn(async (_req: unknown) => ({})), listProjects: vi.fn(async (_req: unknown) => create(ListProjectsResponseSchema)), spawnAgent: vi.fn(async (_req: unknown) => ({})), stopAgent: vi.fn(async () => ({})), sendMessage: vi.fn(async (_req: unknown) => ({})), getCost: vi.fn(async (_req: unknown) => create(GetCostResponseSchema)), getUsage: vi.fn(async (_req: unknown) => create(GetUsageResponseSchema)), listPrompts: vi.fn(async (_req: unknown) => ({ prompts: [] })) }
  const connection = { retryNow: vi.fn(), stop: vi.fn() }
  const host = { info: { id: 'h1', name: 'home', kind: 'state-dir', target: '/s' }, store, client, connection, loadMessages: vi.fn() } as unknown as HostRuntime
  const api = { hosts: { list: async () => [], add: vi.fn(), remove: vi.fn(), fixed: false }, request: vi.fn(), links: noLinks, shell: { notify: vi.fn(), setWaiting: vi.fn(), configure: vi.fn(), onNotificationClick: vi.fn(() => () => {}) } } as unknown as DesktopApi
  const model = createAppModel(api)
  model.store.setState({ loaded: true, hosts: [host], hostId: 'h1' })
  const wrap = (ui: React.ReactNode) => <AppContext.Provider value={model}>{ui}</AppContext.Provider>
  return { host, client, connection, model, wrap, api }
}

describe('ProjectSidebar', () => {
  it('shows skeleton rows until the first snapshot, and the empty text for a host that is down', () => {
    const { host, wrap } = fixture({ kind: 'connecting' })
    host.store.setState({ synced: false, runs: {}, agents: {} })
    const { unmount } = render(wrap(<ProjectSidebar />))
    expect(screen.getByRole('status', { name: 'Loading the projects' })).toBeInTheDocument()
    unmount()
    host.store.setState({ conn: { kind: 'stopped', reason: 'auth', message: 'no' } })
    render(wrap(<ProjectSidebar />))
    expect(screen.queryByRole('status', { name: 'Loading the projects' })).toBeNull()
    expect(screen.getByText(/No projects yet/)).toBeInTheDocument()
  })

  it('shows a project with its run as the lead, the workers folded under it, and opens an agent on click', async () => {
    const { wrap, model } = fixture()
    render(wrap(<ProjectSidebar />))
    const project = screen.getByRole('listitem', { name: 'app' })
    expect(within(project).getByLabelText('1 waiting on you')).toBeInTheDocument()
    const lead = within(project).getByRole('button', { name: 'ship it, lead app-lead' })
    expect(lead).toHaveTextContent('Idle, waiting for a message')
    // The workers are folded until the lead's chevron opens them.
    expect(within(project).queryByRole('button', { name: 'app-fix' })).toBeNull()
    await userEvent.click(within(project).getByRole('button', { name: 'Show the workers of ship it' }))
    const fix = within(project).getByRole('button', { name: 'app-fix' })
    expect(fix).toHaveTextContent('Needs you: Bash')
    expect(within(fix).getByText('sonnet')).toBeInTheDocument()
    await userEvent.click(fix)
    expect(model.store.getState()).toMatchObject({ hostId: 'h1', agentId: 'w1' })
  })

  it('folds reviewers and askers into a chip on their worker, and tags a worker in another repo', async () => {
    const { host, wrap } = fixture()
    host.store.setState((s) => ({
      agents: {
        ...s.agents,
        r1: agent('r1', { role: AgentRole.REVIEWER, name: 'rev-1', parentId: 'w1' }),
        r2: agent('r2', { role: AgentRole.REVIEWER, name: 'rev-2', parentId: 'w1' }),
        w2: agent('w2', { role: AgentRole.WORKER, name: 'web-docs', repo: '/p/web', parentId: 'lead' }),
      },
    }))
    render(wrap(<ProjectSidebar />))
    await userEvent.click(screen.getByRole('button', { name: 'Show the workers of ship it' }))
    expect(screen.queryByRole('button', { name: 'rev-1' })).toBeNull()
    expect(within(screen.getByRole('button', { name: 'app-fix' })).getByText('2 reviews')).toBeInTheDocument()
    expect(within(screen.getByRole('button', { name: 'web-docs' })).getByText('web')).toBeInTheDocument()
    // The other repo lists the run under "Also working here", folded.
    const web = screen.getByRole('listitem', { name: 'web' })
    expect(within(web).getByRole('button', { name: /Also working here/ })).toHaveAttribute('aria-expanded', 'false')
  })

  it('folds closed runs under Earlier and names the CLI in a chip when its vendor has no mark', async () => {
    const { host, wrap } = fixture()
    host.store.setState((s) => ({
      runs: { ...s.runs, r2: create(RunSchema, { id: 'r2', title: 'old work', status: RunStatus.CLOSED }) },
      agents: { ...s.agents, o: agent('o', { role: AgentRole.ROOT, name: 'old-lead', runId: 'r2', kind: 'zed' }) },
    }))
    render(wrap(<ProjectSidebar />))
    expect(screen.queryByRole('button', { name: 'old work, lead old-lead' })).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: /Earlier/ }))
    expect(within(screen.getByRole('button', { name: 'old work, lead old-lead' })).getByText('zed')).toBeInTheDocument()
  })

  it('offers the project menu by right click: pin, remove from the sidebar and back', async () => {
    const { wrap, model } = fixture()
    render(wrap(<ProjectSidebar />))
    fireEvent.contextMenu(screen.getByRole('button', { name: 'app' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'Pin' }))
    expect(model.store.getState().pins).toEqual({ h1: ['/p/app'] })
    // A project with something waiting on the user stays even when removed: take the wait away first.
    model.store.getState().hosts[0]!.store.setState((s) => ({ agents: { ...s.agents, w1: { ...s.agents.w1!, state: AgentState.IDLE, needsInput: 0 } } }))
    fireEvent.contextMenu(screen.getByRole('button', { name: 'app' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'Remove from the sidebar' }))
    await waitFor(() => expect(screen.queryByRole('listitem', { name: 'app' })).toBeNull())
    await userEvent.click(screen.getByRole('button', { name: /Removed from the sidebar/ }))
    await userEvent.click(screen.getByRole('button', { name: 'Show' }))
    expect(screen.getByRole('listitem', { name: 'app' })).toBeInTheDocument()
  })

  it('renames a run from its menu, and stops an agent only after a confirmation', async () => {
    const { wrap, client } = fixture()
    ;(client as unknown as { renameRun: unknown }).renameRun = vi.fn(async () => ({}))
    render(wrap(<ProjectSidebar />))
    await userEvent.click(screen.getByRole('button', { name: 'Menu of ship it' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: /Rename/ }))
    const field = screen.getByRole('textbox', { name: 'Run title' })
    await userEvent.clear(field)
    await userEvent.type(field, 'ship it now{Enter}')
    await waitFor(() => expect((client as unknown as { renameRun: ReturnType<typeof vi.fn> }).renameRun).toHaveBeenCalledWith(expect.objectContaining({ runId: 'r1', title: 'ship it now' })))
    await userEvent.click(screen.getByRole('button', { name: 'Menu of ship it' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'Stop' }))
    expect(client.stopAgent).not.toHaveBeenCalled()
    await userEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Stop' }))
    await waitFor(() => expect(client.stopAgent).toHaveBeenCalledTimes(1))
  })

  it('stops a whole run from its menu, after a confirmation', async () => {
    const { client, wrap } = fixture()
    ;(client as unknown as { stopRun: unknown }).stopRun = vi.fn(async () => ({}))
    const stopRunMock = (client as unknown as { stopRun: ReturnType<typeof vi.fn> }).stopRun
    render(wrap(<ProjectSidebar />))
    await userEvent.click(screen.getByRole('button', { name: 'Menu of ship it' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'Stop run…' }))
    expect(stopRunMock).not.toHaveBeenCalled()
    await userEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Stop the run' }))
    await waitFor(() => expect(stopRunMock).toHaveBeenCalledWith(expect.objectContaining({ runId: 'r1' })))
  })

  it('offers Archive only for an agent the host would archive: a worker with a worktree, not the lead', async () => {
    const { host, wrap } = fixture()
    host.store.setState((s) => ({ agents: { ...s.agents, w1: { ...s.agents.w1!, worktree: '/w/fix' } } }))
    render(wrap(<ProjectSidebar />))
    await userEvent.click(screen.getByRole('button', { name: 'Menu of ship it' }))
    expect((await screen.findAllByRole('menuitem')).map((m) => m.textContent)).not.toContain('Archive…')
    await userEvent.keyboard('{Escape}')
    await userEvent.click(screen.getByRole('button', { name: 'Show the workers of ship it' }))
    await userEvent.click(screen.getByRole('button', { name: 'Menu of app-fix' }))
    expect((await screen.findAllByRole('menuitem')).map((m) => m.textContent)).toContain('Archive…')
  })

  it('unfolds Earlier to rename a closed run, and drops a rename that has no row to edit on', async () => {
    const { host, wrap, model } = fixture()
    host.store.setState((s) => ({
      runs: { ...s.runs, r2: create(RunSchema, { id: 'r2', title: 'old work', status: RunStatus.CLOSED }), r3: create(RunSchema, { id: 'r3', title: 'elsewhere', status: RunStatus.OPEN }) },
      agents: { ...s.agents, o: agent('o', { role: AgentRole.ROOT, name: 'old-lead', runId: 'r2' }), x: agent('x', { role: AgentRole.ROOT, name: 'web-lead', runId: 'r3', repo: '/p/web' }) },
    }))
    render(wrap(<ProjectSidebar />))
    act(() => model.renameRun({ hostId: 'h1', runId: 'r2' }))
    expect(await screen.findByRole('textbox', { name: 'Run title' })).toHaveValue('old work')
    await userEvent.keyboard('{Escape}')
    // A project taken off the sidebar has no row for its run: the rename is dropped, not left to pop up later.
    act(() => model.toggleHidden('h1', '/p/web', true))
    act(() => model.renameRun({ hostId: 'h1', runId: 'r3' }))
    await waitFor(() => expect(model.store.getState().renaming).toBeUndefined())
  })

  it('keeps a hidden project that came back hidden when its menu says so, and shows it again by Keep', async () => {
    const { wrap, model } = fixture()
    model.toggleHidden('h1', '/p/app', true)
    model.toggleHidden('h1', '/p/app', true)
    expect(model.store.getState().hidden).toEqual({ h1: ['/p/app'] })
    render(wrap(<ProjectSidebar />))
    // Back while app-fix waits on the user; its menu puts it back for good rather than flipping the mark.
    fireEvent.contextMenu(screen.getByRole('button', { name: 'app' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'Keep in the sidebar' }))
    expect(model.store.getState().hidden).toEqual({})
  })

  it('steps the agent rows with Alt+Down past the copies under "Also working here"', async () => {
    const { host, wrap, model } = fixture()
    host.store.setState((s) => ({ agents: { ...s.agents, w2: agent('w2', { role: AgentRole.WORKER, name: 'web-docs', repo: '/p/web', parentId: 'lead' }) } }))
    // Pinned, web comes first: its copy of the open lead's row is above the lead's own.
    model.togglePin('h1', '/p/web')
    render(wrap(<ProjectSidebar />))
    await userEvent.click(within(screen.getByRole('listitem', { name: 'web' })).getByRole('button', { name: /Also working here/ }))
    await userEvent.click(within(screen.getByRole('listitem', { name: 'app' })).getByRole('button', { name: 'Show the workers of ship it' }))
    act(() => model.selectAgent('h1', 'lead'))
    act(() => stepSidebar(1))
    expect(model.store.getState().agentId).toBe('w1')
    act(() => stepSidebar(1))
    expect(model.store.getState().agentId).toBe('w2')
    act(() => stepSidebar(-1))
    expect(model.store.getState().agentId).toBe('w1')
  })

  it('opens a project\'s dialogs on its host without moving the open agent to another host', () => {
    const { model } = fixture()
    model.selectAgent('h1', 'w1')
    model.openWorkspace(true, undefined, { hostId: 'h2', repo: '/p/web' })
    expect(model.store.getState()).toMatchObject({ hostId: 'h1', agentId: 'w1', workspaceHost: 'h2', workspaceRepo: '/p/web' })
    model.openProjectSettings({ hostId: 'h2', repo: '/p/web' })
    expect(model.store.getState()).toMatchObject({ hostId: 'h1', agentId: 'w1' })
  })

  it('folds to icons and back', async () => {
    const { wrap, model } = fixture()
    render(wrap(<ProjectSidebar />))
    await userEvent.click(screen.getByRole('button', { name: 'Fold the sidebar' }))
    expect(model.store.getState().sidebarCollapsed).toBe(true)
    expect(screen.getByRole('button', { name: 'app' })).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Show the sidebar' }))
    expect(model.store.getState().sidebarCollapsed).toBe(false)
  })
})

describe('AgentPanel', () => {
  const panel = (wrap: (n: React.ReactNode) => React.ReactElement) => render(wrap(<><AgentPanel host={fixtureHost!} agentId="w1" /><SendMessageDialog /></>))
  let fixtureHost: HostRuntime | undefined
  async function openSend() {
    await userEvent.click(screen.getByRole('button', { name: 'Menu of app-fix' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'Send a message…' }))
    return screen.findByRole('dialog', { name: 'Message app-fix' })
  }

  it('messages the agent from its menu, with a mutation id and an expiry, and closes the dialog', async () => {
    const { host, wrap, client } = fixture()
    fixtureHost = host
    panel(wrap)
    const dialog = await openSend()
    await userEvent.type(within(dialog).getByLabelText('Message', { exact: true }), 'look at the typo')
    await userEvent.click(within(dialog).getByRole('button', { name: 'Send' }))
    await waitFor(() => expect(client.sendMessage).toHaveBeenCalledTimes(1))
    const req = (client.sendMessage.mock.calls[0] as unknown as [{ to: string; type: MessageType; body: string; meta: { mutationId: string; expiresAt: unknown } }])[0]
    expect(req).toMatchObject({ to: 'w1', type: MessageType.TASK, body: 'look at the typo' })
    expect(req.meta.mutationId).toMatch(/^[0-9a-f-]{36}$/)
    expect(req.meta.expiresAt).toBeTruthy()
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
  })

  it("keeps the dialog open and says why when the host couldn't bring a stopped agent back", async () => {
    const { host, wrap, client } = fixture()
    fixtureHost = host
    client.sendMessage.mockResolvedValueOnce({ resumeError: 'the host is at its agent cap' } as never)
    panel(wrap)
    const dialog = await openSend()
    await userEvent.type(within(dialog).getByLabelText('Message', { exact: true }), 'come back')
    await userEvent.click(within(dialog).getByRole('button', { name: 'Send' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent("Sent, but it couldn't be brought back: the host is at its agent cap")
    expect(screen.getByRole('dialog', { name: 'Message app-fix' })).toBeInTheDocument()
    // Stored already: nothing is left to send twice.
    expect(within(dialog).getByLabelText('Message', { exact: true })).toHaveValue('')
    expect(within(dialog).getByRole('button', { name: 'Send' })).toBeDisabled()
    expect(within(dialog).getByText('Close', { selector: 'button' })).toBeInTheDocument()
    expect(within(dialog).queryByText('Cancel', { selector: 'button' })).toBeNull()
  })

  it('sends once on a double submit, and stops once on a double click, after a confirmation', async () => {
    const { host, wrap, client } = fixture()
    fixtureHost = host
    let release!: () => void
    client.sendMessage.mockImplementationOnce(() => new Promise((r) => (release = () => r({}))))
    panel(wrap)
    const dialog = await openSend()
    const input = within(dialog).getByLabelText('Message', { exact: true })
    await userEvent.type(input, 'once')
    fireEvent.submit(input.closest('form')!)
    fireEvent.submit(input.closest('form')!)
    release()
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(client.sendMessage).toHaveBeenCalledTimes(1)

    let releaseStop!: () => void
    client.stopAgent.mockImplementationOnce(() => new Promise((r) => (releaseStop = () => r({}))))
    await userEvent.click(screen.getByRole('button', { name: 'Menu of app-fix' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'Stop…' }))
    expect(client.stopAgent).not.toHaveBeenCalled()
    const confirm = await screen.findByRole('button', { name: 'Stop app-fix' })
    fireEvent.click(confirm)
    fireEvent.click(confirm)
    releaseStop()
    await waitFor(() => expect(client.stopAgent).toHaveBeenCalledTimes(1))
    expect(client.stopAgent).toHaveBeenCalledWith(expect.objectContaining({ agent: 'w1' }))
  })

  it('shows a rejected message\'s reason in the dialog', async () => {
    const { host, wrap, client } = fixture()
    fixtureHost = host
    client.sendMessage.mockRejectedValueOnce(new Error('permission denied'))
    panel(wrap)
    const dialog = await openSend()
    await userEvent.type(within(dialog).getByLabelText('Message', { exact: true }), 'x')
    await userEvent.click(within(dialog).getByRole('button', { name: 'Send' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('permission denied')
  })

  it('offers no stop for an agent that exited, and says why its chat takes no prompt', async () => {
    const { host, wrap } = fixture()
    fixtureHost = host
    host.store.setState((s) => ({ agents: { ...s.agents, w1: agent('w1', { name: 'app-fix', state: AgentState.EXITED, exitCode: 7 }) } }))
    panel(wrap)
    expect(screen.getByTestId('agent-status')).toHaveTextContent('Exited with code 7')
    await userEvent.click(screen.getByRole('button', { name: 'Menu of app-fix' }))
    expect(await screen.findByRole('menuitem', { name: 'Stop…' })).toHaveAttribute('aria-disabled', 'true')
    expect(screen.getByRole('textbox', { name: 'Prompt' })).toBeDisabled()
  })
})

describe('AgentPanel without a terminal', () => {
  it('shows the chat, with no terminal toggle', () => {
    const { host, wrap } = fixture()
    render(wrap(<AgentPanel host={host} agentId="w1" />))
    expect(screen.queryByRole('button', { name: 'Terminal' })).toBeNull()
    expect(screen.getByRole('region', { name: 'Chat' })).toBeVisible()
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
    expect(await screen.findByText(/^home, /)).toBeInTheDocument()
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
    expect(await screen.findByText(/^Home, /)).toBeInTheDocument()
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
    await screen.findByText(/^home, /)
    expect(screen.queryByRole('dialog')).toBeNull()
    act(() => open(OPENED))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByLabelText('Pairing link')).toHaveValue(OPENED.link)
    await userEvent.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    await userEvent.click(screen.getByRole('button', { name: 'New' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'Connect a host…' }))
    const fresh = within(await screen.findByRole('dialog'))
    expect(fresh.queryByLabelText('Pairing link')).toBeNull() // a fresh form: a state dir, not the dropped link
    expect(fresh.getByLabelText('Name')).toHaveValue('')
  })

  it('ignores a link when the host list is fixed by the environment', async () => {
    const take = vi.fn(async () => OPENED)
    const host = { id: 'h', name: 'home', kind: 'state-dir' as const, target: '/s' }
    const api = { hosts: { list: async () => [host], add: vi.fn(), remove: vi.fn(), fixed: true }, request: vi.fn(() => () => {}), openSocket: vi.fn(), links: { take, onOpen: vi.fn(() => () => {}) }, shell: { notify: vi.fn(), setWaiting: vi.fn(), configure: vi.fn(), onNotificationClick: vi.fn(() => () => {}) } } as unknown as DesktopApi
    render(<App api={api} />)
    await screen.findByText(/^home, /)
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

/** The agent's message cards, as its chat shows them (without the conversation around them). */
function Cards({ host, agentId }: { host: HostRuntime; agentId: string }) {
  const messages = useHostState(host, (s) => s.messages)
  const waiting = useHostState(host, (s) => s.waiting)
  return <ol>{messagesOf(messages, agentId).map((m) => <li key={m.id}><MessageCard host={host} message={m} open={waiting.some((w) => w.message?.id === m.id)} /></li>)}</ol>
}

describe('MessageCard', () => {
  it('shows a plan awaiting the user and approves it with an answer to its author', async () => {
    const { host, wrap, client } = fixture()
    const plan = msg('p1', MessageType.PLAN, 'w1', 'lead', 1000, '# The plan\n\n- change a\n- change b')
    putMessages(host, plan)
    waitOn(host, plan)
    render(wrap(<Cards host={host} agentId="w1" />))
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
    render(wrap(<Cards host={host} agentId="w1" />))
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
    render(wrap(<Cards host={host} agentId="w1" />))
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
    render(wrap(<Cards host={host} agentId="w1" />))
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

  it("marks the plan an OpenView named, expanded and in view, and the mark goes after a few seconds", async () => {
    const { host, wrap, model } = fixture()
    putMessages(host, msg('p1', MessageType.PLAN, 'w1', 'lead', 1000), msg('p2', MessageType.PLAN, 'w1', 'lead', 2000, 'x'.repeat(2000)))
    vi.useFakeTimers({ shouldAdvanceTime: true })
    try {
      render(wrap(<Cards host={host} agentId="w1" />))
      // The card was shown, collapsed, before the request came: the request expands it too.
      expect(within(screen.getAllByRole('article')[1]!).getByRole('button', { name: /Show all/ })).toBeInTheDocument()
      act(() => model.store.setState({ focusMessage: { hostId: host.info.id, id: 'p2' } }))
      const [first, second] = screen.getAllByRole('article')
      expect(first).not.toHaveAttribute('aria-current')
      expect(second).toHaveAttribute('aria-current', 'true')
      expect(within(second!).getByRole('button', { name: /Show less/ })).toBeInTheDocument()
      await act(async () => { await vi.advanceTimersByTimeAsync(3100) })
      expect(model.store.getState().focusMessage).toBeUndefined()
      expect(screen.getAllByRole('article')[1]).not.toHaveAttribute('aria-current')
    } finally {
      vi.useRealTimers()
    }
  })

  it('shows reports and failures without a form, a long one collapsed', async () => {
    const { host, wrap } = fixture()
    putMessages(host, msg('d1', MessageType.DONE, 'w1', 'lead', 1000, 'All **done**.'), msg('f1', MessageType.FAILED, 'w1', 'lead', 2000, 'x'.repeat(2000)))
    render(wrap(<Cards host={host} agentId="w1" />))
    expect(screen.getByRole('article', { name: 'Report from w1' })).toHaveTextContent('All done.')
    expect(screen.queryByRole('form')).toBeNull()
    const failed = screen.getByRole('article', { name: 'Failed from w1' })
    await userEvent.click(within(failed).getByRole('button', { name: /Show all/ }))
    expect(within(failed).getByRole('button', { name: /Show less/ })).toBeInTheDocument()
  })

})

describe('Markdown in messages', () => {
  it('opens web links in the browser through main, drops raw html and images, and ignores other schemes', async () => {
    const openExternal = vi.fn()
    ;(window as unknown as { desktop: unknown }).desktop = { openExternal }
    const { host, wrap } = fixture()
    putMessages(host, msg('d1', MessageType.DONE, 'w1', 'lead', 1000, 'See [docs](https://example.com/a) and [bad](javascript:alert(1)).\n\n<script>window.x=1</script><details><summary>x</summary>secret</details><img src="https://evil.example/x.png">![alt](https://evil.example/y.png)\n\n| a | b |\n|---|---|\n| 1 | 2 |'))
    const { container } = render(wrap(<Cards host={host} agentId="w1" />))
    await userEvent.click(screen.getByRole('link', { name: 'docs' }))
    expect(openExternal).toHaveBeenCalledWith('https://example.com/a')
    const bad = screen.queryByRole('link', { name: 'bad' })
    if (bad) await userEvent.click(bad)
    expect(openExternal).toHaveBeenCalledTimes(1)
    expect(container.querySelector('script')).toBeNull()
    expect(container.querySelector('img')).toBeNull()
    expect(container.querySelector('details')).toBeNull()
    // Dropped, not escaped into the page as text.
    expect(container.textContent).not.toMatch(/<script|<details|secret|<img/)
    expect(screen.getByRole('table')).toBeInTheDocument()
  })

  it("keeps a prompt's line breaks, and joins a reply's lines as markdown does", () => {
    const { container: prompt } = render(<Markdown breaks>{'step 1: build\nstep 2: test'}</Markdown>)
    expect(prompt.querySelectorAll('br')).toHaveLength(1)
    const { container: reply } = render(<Markdown>{'one\ntwo'}</Markdown>)
    expect(reply.querySelector('br')).toBeNull()
    const { container: inline } = render(<Markdown>{'a <b>bold</b> word <!-- note -->'}</Markdown>)
    expect(inline.textContent).toBe('a bold word ')
    // A placeholder in angle brackets is no HTML element: it stays, as text.
    const { container: words } = render(<Markdown>{'replace <name> in mesh spawn <repo>, a List<String>'}</Markdown>)
    expect(words.textContent).toBe('replace <name> in mesh spawn <repo>, a List<String>')
  })
})

describe('MarkdownField', () => {
  it('takes Ctrl+B by the physical key, on a layout that types another letter there', () => {
    function Field() {
      const [v, setV] = useState('')
      return <MarkdownField aria-label="Body" value={v} onValueChange={setV} />
    }
    render(<Field />)
    const field = screen.getByRole('textbox', { name: 'Body' }) as HTMLTextAreaElement
    field.focus()
    fireEvent.keyDown(field, { key: 'и', code: 'KeyB', ctrlKey: true })
    expect(field.value).toBe('****')
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
    expect(model.store.getState()).toMatchObject({ agentId: 'w1', tab: 'chat' })
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
  it('shows the open project\'s tabs only, each project keeping its own, and starts a new lead in that project from the +', async () => {
    const { host, wrap, model } = fixture()
    host.store.setState((s) => ({
      runs: { ...s.runs, r2: create(RunSchema, { id: 'r2', title: 'web', status: RunStatus.OPEN }) },
      agents: { ...s.agents, o1: agent('o1', { runId: 'r2', role: AgentRole.ROOT, repo: '/p/web', name: 'web-lead' }) },
    }))
    model.selectAgent('h1', 'o1')
    model.selectAgent('h1', 'w1')
    model.selectAgent('h1', 'lead')
    render(wrap(<TabBar />))
    const tabs = screen.getByRole('tablist', { name: 'Open agents' })
    expect(within(tabs).getAllByRole('tab').map((t) => t.textContent)).toEqual(['app-fix', 'app-lead'])
    act(() => model.selectAgent('h1', 'o1'))
    expect(within(tabs).getAllByRole('tab').map((t) => t.textContent)).toEqual(['web-lead'])
    await userEvent.click(screen.getByRole('button', { name: 'New in web' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'New lead' }))
    expect(model.store.getState()).toMatchObject({ workspaceOpen: true, workspaceRepo: '/p/web', workspaceHost: 'h1' })
  })

  it('shows the live leads with what they do until an agent is open, then that agent\'s tree; opens one as a tab and closes tabs', async () => {
    const { host, wrap, model } = fixture()
    host.store.setState((s) => ({ agents: { ...s.agents, lead: { ...s.agents.lead!, state: AgentState.WORKING, stateDetail: 'Edit' } } }))
    render(wrap(<><TabBar /><Dock /></>))
    const dock = screen.getByRole('region', { name: 'Live dock' })
    expect(dock).toHaveTextContent('1 lead')
    expect(within(dock).getAllByRole('listitem')).toHaveLength(1)
    expect(screen.getByText('No agent open')).toBeInTheDocument()

    await userEvent.click(within(dock).getByRole('button', { name: /Working: Edit/ }))
    expect(model.store.getState()).toMatchObject({ hostId: 'h1', agentId: 'lead', tabs: [{ agentId: 'lead' }] })
    const tabs = screen.getByRole('tablist', { name: 'Open agents' })
    expect(within(tabs).getByRole('tab', { name: /app-lead/ })).toHaveAttribute('aria-selected', 'true')
    // Now the dock is the lead's run: the one that needs the user first.
    expect(dock).toHaveTextContent('2 in this run')
    expect(dock).toHaveTextContent('1 need you')
    const tiles = within(dock).getAllByRole('listitem')
    expect(tiles[0]).toHaveTextContent('app-fix')

    await userEvent.click(within(dock).getByRole('button', { name: /Needs you/ }))
    expect(within(tabs).getAllByRole('tab')).toHaveLength(2)
    await userEvent.click(within(tabs).getByRole('button', { name: 'Close app-fix' }))
    expect(model.store.getState()).toMatchObject({ agentId: 'lead', tabs: [{ agentId: 'lead' }] })
    // The closed tab comes back with Ctrl+Shift+T.
    model.reopenTab()
    expect(model.store.getState()).toMatchObject({ agentId: 'w1', tabs: [{ agentId: 'lead' }, { agentId: 'w1' }] })
  })

  it('closes a tab with a middle click and moves one by dragging it onto another', () => {
    const { wrap, model } = fixture()
    model.selectAgent('h1', 'lead')
    model.selectAgent('h1', 'w1')
    render(wrap(<TabBar />))
    const tab = (name: RegExp) => screen.getByRole('tab', { name }).parentElement!
    fireEvent.dragStart(tab(/app-fix/), { dataTransfer: { setData: vi.fn(), effectAllowed: '' } })
    fireEvent.dragOver(tab(/app-lead/))
    fireEvent.drop(tab(/app-lead/))
    expect(model.store.getState().tabs.map((t) => t.agentId)).toEqual(['w1', 'lead'])
    fireEvent(tab(/app-lead/), new MouseEvent('auxclick', { bubbles: true, button: 1 }))
    expect(model.store.getState().tabs.map((t) => t.agentId)).toEqual(['w1'])
  })

  it('marks a new worker, hides a dismissed tile, and folds away', async () => {
    const { host, wrap, model } = fixture()
    model.selectAgent('h1', 'lead')
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

describe('Splits', () => {
  it('shows an agent beside the open one from its tab or the dock, stacks and unstacks, swaps and closes a pane', async () => {
    const { host, wrap, model } = fixture()
    host.store.setState({ daemon: { capabilities: [] } as never })
    model.store.setState({ hostId: 'h1' })
    model.selectAgent('h1', 'lead')
    model.selectAgent('h1', 'w1')
    model.selectAgent('h1', 'lead')
    render(wrap(<><TabBar /><Panes host={host} agentId="lead" /><Dock /></>))
    const tabs = screen.getByRole('tablist', { name: 'Open agents' })
    await userEvent.click(within(tabs).getByRole('button', { name: 'Show app-fix beside' }))
    expect(model.store.getState().side).toEqual([{ hostId: 'h1', agentId: 'w1', tab: 'chat' }])
    // The tab says it is beside, and has no split button now.
    expect(within(tabs).queryByRole('button', { name: 'Show app-fix beside' })).toBeNull()
    const split = screen.getByRole('region', { name: 'Split pane' })
    expect(within(split).getByRole('article', { name: 'app-fix' })).toBeInTheDocument()
    expect(within(screen.getByRole('region', { name: 'Main pane' })).getByRole('article', { name: 'app-lead' })).toBeInTheDocument()
    expect(screen.getByRole('group', { name: 'Panes' }).className).toContain('flex-row')

    await userEvent.click(screen.getByRole('button', { name: 'Stack the panes' }))
    expect(screen.getByRole('group', { name: 'Panes' }).className).toContain('flex-col')

    // A pane has its own view (its CLI toggle sets it; jsdom has no terminal to show).
    act(() => model.setPaneTab({ hostId: 'h1', agentId: 'w1' }, 'terminal'))
    expect(model.store.getState().side[0]!.tab).toBe('terminal')
    expect(model.store.getState().tab).toBe('chat')

    await userEvent.click(within(split).getByRole('button', { name: 'Make this the main pane' }))
    expect(model.store.getState()).toMatchObject({ agentId: 'w1', tab: 'terminal', side: [{ agentId: 'lead', tab: 'chat' }] })

    // The side panel is the main pane's: a pane's model chip makes its agent the main pane's first.
    await userEvent.click(within(screen.getByRole('region', { name: 'Split pane' })).getByRole('button', { name: /^Model: / }))
    expect(model.store.getState()).toMatchObject({ agentId: 'lead', panelOpen: true, side: [{ agentId: 'w1' }] })
    act(() => {
      model.focusPane({ hostId: 'h1', agentId: 'w1' })
      model.setPanel(false)
    })

    await userEvent.click(within(screen.getByRole('region', { name: 'Split pane' })).getByRole('button', { name: 'Close this pane' }))
    expect(model.store.getState().side).toEqual([])
    expect(model.store.getState().tabs).toHaveLength(2)

    // From the dock.
    await userEvent.click(screen.getByRole('button', { name: 'Pin app-lead as a split' }))
    expect(model.store.getState().side.map((p) => p.agentId)).toEqual(['lead'])
  })
})

describe('WatchAll tiles follow the host\'s store', () => {
  it('moves a tile to needing the user when a plan arrives for it', async () => {
    const { host, client, wrap } = fixture()
    Object.assign(client, { readTranscript: vi.fn(async () => create(ReadTranscriptResponseSchema, { supported: true })) })
    host.store.setState((s) => ({ agents: { ...s.agents, w2: agent('w2', { name: 'app-w2', role: AgentRole.WORKER, parentId: 'lead', state: AgentState.IDLE }) } }))
    render(wrap(<WatchAll />))
    const tile = await screen.findByRole('listitem', { name: 'app-w2 in the grid' })
    expect(tile).toHaveTextContent(/idle/i)
    act(() => host.store.setState({ waiting: [create(WaitingSchema, { kind: WaitingKind.PLAN, runId: 'r1', agentId: 'w2', message: create(MessageSchema, { id: 'm9', type: MessageType.PLAN, runId: 'r1' }) })] }))
    expect(screen.getByRole('listitem', { name: 'app-w2 in the grid' })).toHaveTextContent(/needs you/i)
  })
})

describe('SidePanel', () => {
  const costResp = create(GetCostResponseSchema, { usd: 7.5, agents: [create(AgentCostSchema, { name: 'app-fix', usd: 2 }), create(AgentCostSchema, { name: 'app-lead', usd: 5.5 })] })
  const usageResp = create(GetUsageResponseSchema, { quotaStop: 90, minQuota: 10, claude: create(ClaudeUsageSchema, { windows: [create(ClaudeUsageWindowSchema, { label: 'weekly window', used: 95 })], atReserve: ['weekly window'] }) })

  function panelFixture() {
    const f = fixture()
    Object.assign(f.client, { getCost: vi.fn(async () => costResp), getUsage: vi.fn(async () => usageResp) })
    f.host.store.setState((s) => ({
      waiting: [create(WaitingSchema, { kind: WaitingKind.PLAN, runId: 'r1', agentId: 'w1', message: create(MessageSchema, { id: 'm1', type: MessageType.PLAN, runId: 'r1', toId: 'lead' }) })],
      pullRequests: { w1: create(AgentPullRequestsSchema, { agentId: 'w1', pullRequests: [create(PullRequestSchema, { agentId: 'w1', number: 12, url: 'https://github.com/o/r/pull/12', state: 'open', checks: 'failing' })] }) },
      agents: { ...s.agents },
    }))
    return f
  }

  it('shows a lead\'s tree first, then its run\'s PRs, and the run total folded to one line', async () => {
    const { host, client, wrap, model } = panelFixture()
    host.store.setState((s) => ({ agents: { ...s.agents, rv: agent('rv', { role: AgentRole.REVIEWER, name: 'rev', parentId: 'w1' }) } }))
    render(wrap(<SidePanel host={host} agentId="lead" />))
    const tree = screen.getByRole('list', { name: 'Agent tree' })
    expect(within(tree).getAllByRole('button').map((b) => b.textContent)).toEqual(['app-leadlead', 'app-fixworker', 'revreviewer'])
    expect(within(tree).getByRole('button', { name: /app-lead/ })).toHaveAttribute('aria-current', 'true')
    const prs = screen.getByRole('region', { name: 'Pull requests' })
    expect(prs).toHaveTextContent('#12')
    expect(prs).toHaveTextContent('checks failing')
    expect(prs).toHaveTextContent('app-fix')
    const cost = screen.getByRole('region', { name: 'Run total' })
    await waitFor(() => expect(cost).toHaveTextContent('$7.50'))
    expect(client.getCost).toHaveBeenCalledWith({ agents: true, runId: 'r1', since: '' })
    await userEvent.click(within(cost).getByRole('button', { name: /Run total/ }))
    const per = screen.getByRole('list', { name: 'Cost per agent' })
    expect(within(per).getAllByRole('listitem').map((l) => l.textContent)).toEqual(['app-lead$5.50', 'app-fix$2.00'])
    // The quota is in the sidebar's footer, the autonomy in the project's settings.
    expect(screen.queryByRole('region', { name: 'Quota' })).toBeNull()
    expect(screen.queryByRole('region', { name: 'Autonomy' })).toBeNull()
    await userEvent.click(within(tree).getByRole('button', { name: /app-fix/ }))
    expect(model.store.getState()).toMatchObject({ agentId: 'w1' })
  })

  it('shows another agent\'s share of the cost, its own PRs, and its details folded', async () => {
    const { host, wrap, model } = panelFixture()
    host.store.setState((s) => ({ agents: { ...s.agents, w1: { ...s.agents.w1!, branch: 'mesh/r1/fix', sessionId: 'sess-1' } } }))
    const open = vi.fn()
    ;(window as unknown as { desktop: unknown }).desktop = { openExternal: open }
    render(wrap(<SidePanel host={host} agentId="w1" />))
    const cost = screen.getByRole('region', { name: 'Cost' })
    await waitFor(() => expect(cost).toHaveTextContent('$2.00'))
    await userEvent.click(within(cost).getByRole('button', { name: /Cost/ }))
    expect(cost).toHaveTextContent('of $7.50 for the run')
    const details = screen.getByRole('region', { name: 'Details' })
    await userEvent.click(within(details).getByRole('button', { name: /Details/ }))
    for (const t of ['app', 'home', 'mesh/r1/fix', 'sess-1', 'w1']) expect(details).toHaveTextContent(t)
    await userEvent.click(within(screen.getByRole('region', { name: 'Pull requests' })).getByRole('button', { name: /^#12/ }))
    expect(open).toHaveBeenCalledWith('https://github.com/o/r/pull/12')
    await userEvent.click(screen.getByRole('button', { name: 'Hide the side panel' }))
    expect(model.store.getState().panelOpen).toBe(false)
    delete (window as unknown as { desktop?: unknown }).desktop
  })

  it('sends a comment on an open PR to the agent that wrote it, as a task, and shows a refusal', async () => {
    const { host, client, wrap } = panelFixture()
    render(wrap(<SidePanel host={host} agentId="lead" />))
    await userEvent.click(await screen.findByRole('button', { name: 'Comment on #12' }))
    const box = screen.getByLabelText('Comment on #12 for app-fix')
    expect(screen.getByRole('button', { name: 'Send to app-fix' })).toBeDisabled()
    await userEvent.type(box, 'rename the helper')
    client.sendMessage.mockRejectedValueOnce(new Error('[failed_precondition] agent has exited'))
    await userEvent.click(screen.getByRole('button', { name: 'Send to app-fix' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('agent has exited')
    await userEvent.click(screen.getByRole('button', { name: 'Send to app-fix' }))
    await waitFor(() => expect(client.sendMessage).toHaveBeenCalledTimes(2))
    expect(client.sendMessage.mock.calls[1]![0]).toMatchObject({ to: 'w1', type: MessageType.TASK, body: 'Comment on https://github.com/o/r/pull/12:\n\nrename the helper' })
    await waitFor(() => expect(screen.queryByLabelText('Comment on #12 for app-fix')).toBeNull())
  })

  it('holds and releases a PR\'s merge, and shows it held', async () => {
    const { host, client, wrap } = panelFixture()
    render(wrap(<SidePanel host={host} agentId="lead" />))
    await userEvent.click(await screen.findByRole('button', { name: 'Hold the merge of #12' }))
    expect(client.holdPullRequest.mock.calls[0]![0]).toMatchObject({ url: 'https://github.com/o/r/pull/12', release: false })
    // The host's event says it is held: the button offers the release and a badge says so.
    act(() => host.store.setState((s) => ({ pullRequests: { ...s.pullRequests, w1: create(AgentPullRequestsSchema, { agentId: 'w1', pullRequests: [create(PullRequestSchema, { agentId: 'w1', number: 12, url: 'https://github.com/o/r/pull/12', state: 'open', held: true })] }) } })))
    expect(within(screen.getByRole('region', { name: 'Pull requests' })).getByText('held')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Release the merge of #12' }))
    expect(client.holdPullRequest.mock.calls[1]![0]).toMatchObject({ release: true })
  })

  it('says so when the cost can\'t be read, and when no agent is open', async () => {
    const { host, client, wrap } = panelFixture()
    client.getCost.mockRejectedValue(new Error('no cost data'))
    const { unmount } = render(wrap(<SidePanel host={host} agentId="lead" />))
    const cost = screen.getByRole('region', { name: 'Run total' })
    await waitFor(() => expect(cost).toHaveTextContent('unread'))
    await userEvent.click(within(cost).getByRole('button', { name: /Run total/ }))
    expect(await screen.findByText(/Couldn't read the cost: no cost data/)).toBeInTheDocument()
    unmount()
    render(wrap(<SidePanel host={host} />))
    expect(screen.getByText(/Open an agent to see/)).toBeInTheDocument()
  })
})

describe('FindBar', () => {
  it('finds as you type, steps with Enter and Shift+Enter, and closes on Escape wherever the focus is', async () => {
    const { wrap, model } = fixture()
    const find = { start: vi.fn(), stop: vi.fn(), onResult: vi.fn((cb: (r: { active: number; total: number }) => void) => ((find.cb = cb), () => {})), cb: undefined as undefined | ((r: { active: number; total: number }) => void) }
    ;(model.api as unknown as { find: unknown }).find = find
    model.openFind(true)
    render(wrap(<FindBar />))
    await userEvent.type(screen.getByRole('textbox', { name: 'Find' }), 'ab')
    expect(find.start).toHaveBeenLastCalledWith('ab', true, false)
    // Match 1 is the bar's own field: the count leaves it out, and landing on it steps on.
    act(() => find.cb!({ active: 3, total: 6 }))
    expect(screen.getByRole('status')).toHaveTextContent('2 of 5')
    find.start.mockClear()
    act(() => find.cb!({ active: 1, total: 6 }))
    expect(find.start).toHaveBeenLastCalledWith('ab', true, true)
    act(() => find.cb!({ active: 1, total: 1 }))
    expect(screen.getByRole('status')).toHaveTextContent('No match')
    await userEvent.keyboard('{Shift>}{Enter}{/Shift}')
    expect(find.start).toHaveBeenLastCalledWith('ab', false, true)
    act(() => (document.activeElement as HTMLElement).blur())
    // An Escape something else took (a dialog, the composer) leaves it open.
    const taken = new KeyboardEvent('keydown', { key: 'Escape', cancelable: true })
    taken.preventDefault()
    window.dispatchEvent(taken)
    expect(model.store.getState().findOpen).toBe(true)
    fireEvent.keyDown(window, { key: 'Escape' })
    expect(find.stop).toHaveBeenCalled()
    expect(model.store.getState().findOpen).toBe(false)
  })
})

describe('ProjectSettingsDialog', () => {
  const list = (key: string, v: string[]) => create(SettingDescriptor, { key, value: fromJson(ValueSchema, v) })

  it('sets the autonomy level of a project in the host\'s two lists, only when the host has them, and its free models', async () => {
    const { host, client, wrap, model } = fixture()
    model.openProjectSettings({ hostId: 'h1', repo: '/p/app' })
    const { unmount } = render(wrap(<ProjectSettingsDialog />))
    expect(screen.queryByLabelText('Autonomy')).toBeNull()
    expect(screen.getByText(/no autonomy settings yet/)).toBeInTheDocument()
    unmount()
    host.store.setState({ settings: { 'repos.user_merges': list('repos.user_merges', ['/p/shop']), 'repos.user_approves_plans': list('repos.user_approves_plans', []), 'repos.no_free_models': list('repos.no_free_models', []) } })
    render(wrap(<ProjectSettingsDialog />))
    const select = screen.getByLabelText('Autonomy')
    expect(select).toHaveValue('default')
    await userEvent.selectOptions(select, 'plans')
    await waitFor(() => expect(client.setSetting).toHaveBeenCalledTimes(1))
    expect(client.setSetting.mock.calls[0]![0]).toMatchObject({ key: 'repos.user_approves_plans', value: { kind: { case: 'listValue' } } })
    await userEvent.click(screen.getByRole('checkbox', { name: 'Free models allowed' }))
    await waitFor(() => expect(client.setSetting).toHaveBeenCalledTimes(2))
    expect(client.setSetting.mock.calls[1]![0]).toMatchObject({ key: 'repos.no_free_models' })
    await userEvent.click(screen.getByRole('checkbox', { name: 'Pinned in the sidebar' }))
    expect(model.store.getState().pins).toEqual({ h1: ['/p/app'] })
  })

  it('puts the first autonomy write back when the second fails, so a repo is never left in neither list', async () => {
    const { host, client, wrap, model } = fixture()
    host.store.setState({ settings: { 'repos.user_merges': list('repos.user_merges', ['/p/shop']), 'repos.user_approves_plans': list('repos.user_approves_plans', ['/p/other']) } })
    client.setSetting.mockResolvedValueOnce({}).mockRejectedValueOnce(new Error('[invalid_argument] no'))
    model.openProjectSettings({ hostId: 'h1', repo: '/p/shop' })
    render(wrap(<ProjectSettingsDialog />))
    await userEvent.selectOptions(screen.getByLabelText('Autonomy'), 'plans')
    expect(await screen.findByRole('alert')).toHaveTextContent('no')
    const calls = client.setSetting.mock.calls.map((c) => c[0] as { key: string; value: { kind: { value: { values: Array<{ kind: { value: string } }> } } } })
    // merges written (without /p/shop), plans failed, then merges put back as it was.
    expect(calls.map((c) => c.key)).toEqual(['repos.user_merges', 'repos.user_approves_plans', 'repos.user_merges'])
    expect(calls[2]!.value.kind.value.values.map((v) => v.kind.value)).toEqual(['/p/shop'])
  })
})

describe('SidebarFooter', () => {
  it('shows the quota as meters, amber at the reserve, and opens Usage and cost on a click', async () => {
    const { client, wrap, model } = fixture()
    const usage = create(GetUsageResponseSchema, { quotaStop: 90, minQuota: 10, claude: create(ClaudeUsageSchema, { windows: [create(ClaudeUsageWindowSchema, { label: '5h', used: 42 }), create(ClaudeUsageWindowSchema, { label: '7d', used: 95 })], atReserve: ['7d'] }) })
    Object.assign(client, { getUsage: vi.fn(async () => usage) })
    render(wrap(<SidebarFooter />))
    const meter = await screen.findByRole('meter', { name: 'Claude 7d' })
    expect(meter).toHaveAttribute('aria-valuenow', '95')
    expect(screen.getByRole('meter', { name: 'Claude 5h' })).toHaveAttribute('aria-valuenow', '42')
    await userEvent.click(screen.getByRole('button', { name: /Quota: open usage and cost/ }))
    expect(model.store.getState().usageOpen).toBe(true)
  })

  it('adds a project by Enter on the path typed, and keeps one already pinned pinned', async () => {
    const { wrap, model, client } = fixture()
    client.listProjects.mockResolvedValue(create(ListProjectsResponseSchema, { projects: [create(ProjectSchema, { path: '/p/apple', name: 'apple' }), create(ProjectSchema, { path: '/p/app', name: 'app' })] }))
    model.togglePin('h1', '/p/app')
    render(wrap(<SidebarFooter />))
    await userEvent.click(screen.getByRole('button', { name: 'New' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'Add a project…' }))
    await userEvent.type(await screen.findByRole('textbox'), '  /p/app {Enter}')
    expect(model.store.getState().pins).toEqual({ h1: ['/p/app'] })
  })

  it('offers New lead, a project, a host and pairing from +, and the host\'s settings from Hosts', async () => {
    const { wrap, model } = fixture()
    render(wrap(<SidebarFooter />))
    await userEvent.click(screen.getByRole('button', { name: 'New' }))
    expect((await screen.findAllByRole('menuitem')).map((m) => m.textContent)).toEqual(['New lead', 'Add a project…', 'Connect a host…', 'Pair a device'])
    await userEvent.click(screen.getByRole('menuitem', { name: 'New lead' }))
    expect(model.store.getState().workspaceOpen).toBe(true)
    await userEvent.click(screen.getByRole('button', { name: 'Hosts' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'Host settings' }))
    expect(model.store.getState().settingsOpen).toBe(true)
  })
})

describe('WatchAll', () => {
  it('shows a tile per live agent with its recent conversation, needing-you first, and a click opens it', async () => {
    const { host, client, wrap, model } = fixture()
    Object.assign(client, {
      readTranscript: vi.fn(async (req: { agent: string }) =>
        create(ReadTranscriptResponseSchema, { supported: true, sessionId: 's', entries: [create(TranscriptEntrySchema, { id: `${req.agent}-1`, kind: TranscriptKind.ASSISTANT, text: `hello from ${req.agent}` })] }),
      ),
    })
    model.store.setState({ watchOpen: true })
    render(wrap(<WatchAll />))
    const grid = screen.getByRole('list', { name: 'Agents' })
    const tiles = within(grid).getAllByRole('listitem', { name: /in the grid/ })
    // The worker at a prompt comes before the idle lead.
    expect(tiles.map((t) => t.getAttribute('aria-label'))).toEqual(['app-fix in the grid', 'app-lead in the grid'])
    expect(await within(tiles[0]!).findByText(/hello from w1/)).toBeInTheDocument()
    expect(tiles[0]).toHaveTextContent('Needs you: Bash')
    await userEvent.click(within(tiles[0]!).getByRole('button', { name: 'Open app-fix' }))
    expect(model.store.getState()).toMatchObject({ agentId: 'w1', watchOpen: false })
  })

  it('pins a tile as a split, and says so when there is nobody to watch', async () => {
    const { host, client, wrap, model } = fixture()
    Object.assign(client, { readTranscript: vi.fn(async () => create(ReadTranscriptResponseSchema, { supported: false })) })
    model.selectAgent('h1', 'lead')
    model.store.setState({ watchOpen: true })
    render(wrap(<WatchAll />))
    await userEvent.click(await screen.findByRole('button', { name: 'Pin app-fix as a split' }))
    expect(model.store.getState().side.map((p) => p.agentId)).toEqual(['w1'])
    expect(await screen.findAllByText('No conversation record for this CLI.')).toHaveLength(2)
    act(() => host.store.setState({ agents: {} }))
    expect(screen.getByText('No live agents in the open runs.')).toBeInTheDocument()
  })
})

describe('NewWorkspaceDialog', () => {
  const projects = [create(ProjectSchema, { path: '/p/shop', name: 'shop', defaultBranch: 'main' }), create(ProjectSchema, { path: '/p/api', name: 'api' })]
  const detail = create(ProjectSchema, {
    path: '/p/shop',
    name: 'shop',
    defaultBranch: 'main',
    branches: ['main', 'origin/main', 'feature-x'],
    pullRequests: [create(ProjectPullRequestSchema, { number: 7, title: 'Fix the cart', headRef: 'mesh/r/fix-cart', url: 'u7' })],
  })

  function dialog() {
    const f = fixture()
    f.client.listProjects.mockImplementation(async (req: unknown) => create(ListProjectsResponseSchema, { projects: (req as { repo: string }).repo ? [detail] : projects }))
    f.client.startRun.mockImplementation(async () => ({ run: { id: 'r9' } }))
    f.client.spawnAgent.mockImplementation(async () => ({ agent: { id: 'new1' } }))
    f.model.store.setState({ workspaceOpen: true })
    return f
  }

  it('starts a new run with a lead in a worktree of the project, from a pull request\'s branch, and opens its terminal', async () => {
    const { host, client, wrap, model } = dialog()
    render(wrap(<NewWorkspaceDialog host={host} trigger={<button>New workspace</button>} />))
    const project = await screen.findByLabelText('Project')
    await userEvent.type(project, '/p/shop')
    await userEvent.tab()
    expect(client.listProjects).toHaveBeenLastCalledWith({ repo: '/p/shop' })
    expect(await screen.findByText('Default branch: main')).toBeInTheDocument()
    await userEvent.selectOptions(screen.getByLabelText('Start from', { selector: 'select' }), 'pr')
    await userEvent.selectOptions(screen.getByLabelText('Pull request'), 'mesh/r/fix-cart')
    await userEvent.type(screen.getByLabelText('Name'), 'fix-cart')
    await userEvent.type(screen.getByLabelText('First message'), 'start with the cart')
    await userEvent.click(screen.getByRole('button', { name: 'Start workspace' }))
    await waitFor(() => expect(client.spawnAgent).toHaveBeenCalledTimes(1))
    expect(client.startRun.mock.calls[0]![0]).toMatchObject({ title: 'fix-cart' })
    expect(client.spawnAgent.mock.calls[0]![0]).toMatchObject({ runId: 'r9', repo: '/p/shop', slug: 'fix-cart', role: AgentRole.ROOT, kind: 'claude', base: 'origin/mesh/r/fix-cart', task: 'start with the cart' })
    await waitFor(() => expect(model.store.getState()).toMatchObject({ workspaceOpen: false, agentId: 'new1', tab: 'chat' }))
  })

  it('starts from a branch, joins an open run as a worker, and needs a choice before it starts', async () => {
    const { host, client, wrap } = dialog()
    render(wrap(<NewWorkspaceDialog host={host} trigger={<button>New workspace</button>} />))
    await userEvent.type(await screen.findByLabelText('Project'), '/p/shop')
    await userEvent.tab()
    await screen.findByText('Default branch: main')
    await userEvent.selectOptions(screen.getByLabelText('Start from', { selector: 'select' }), 'branch')
    await userEvent.type(screen.getByLabelText('Name'), 'try-it')
    expect(screen.getByRole('button', { name: 'Start workspace' })).toBeDisabled()
    await userEvent.selectOptions(screen.getByLabelText('Branch'), 'feature-x')
    await userEvent.selectOptions(screen.getByLabelText('Run'), 'r1')
    await userEvent.click(screen.getByRole('button', { name: 'Start workspace' }))
    await waitFor(() => expect(client.spawnAgent).toHaveBeenCalledTimes(1))
    expect(client.startRun).not.toHaveBeenCalled()
    expect(client.spawnAgent.mock.calls[0]![0]).toMatchObject({ runId: 'r1', role: AgentRole.WORKER, base: 'feature-x' })
  })

  it('joins the run it started when a retry follows a failed start, and ignores a start choice made for another project', async () => {
    const { host, client, wrap } = dialog()
    client.spawnAgent.mockRejectedValueOnce(new Error('[failed_precondition] no such ref')).mockResolvedValue({ agent: { id: 'new1' } })
    render(wrap(<NewWorkspaceDialog host={host} trigger={<button>New workspace</button>} />))
    await userEvent.type(await screen.findByLabelText('Project'), '/p/shop')
    await userEvent.tab()
    await screen.findByText('Default branch: main')
    await userEvent.selectOptions(screen.getByLabelText('Start from', { selector: 'select' }), 'branch')
    await userEvent.selectOptions(screen.getByLabelText('Branch'), 'feature-x')
    await userEvent.type(screen.getByLabelText('Name'), 'a')
    await userEvent.click(screen.getByRole('button', { name: 'Start workspace' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('no such ref')
    // Another project typed over it and sent: the old branch is not carried over, and the run is not started twice.
    await userEvent.clear(screen.getByLabelText('Project'))
    await userEvent.type(screen.getByLabelText('Project'), '/p/api')
    await userEvent.click(screen.getByRole('button', { name: 'Start workspace' }))
    await waitFor(() => expect(client.spawnAgent).toHaveBeenCalledTimes(2))
    expect(client.startRun).toHaveBeenCalledTimes(1)
    expect(client.spawnAgent.mock.calls[1]![0]).toMatchObject({ runId: 'r9', repo: '/p/api', base: '' })
  })

  it('reads a project again after a failed read of the same path, and offers a fork\'s pull request only as unavailable, one option for each branch', async () => {
    const { host, client, wrap } = dialog()
    const withForks = create(ProjectSchema, {
      path: '/p/shop',
      name: 'shop',
      defaultBranch: 'main',
      branches: ['main'],
      pullRequests: [
        create(ProjectPullRequestSchema, { number: 7, title: 'Fix', headRef: 'mesh/r/fix', url: 'u7' }),
        create(ProjectPullRequestSchema, { number: 8, title: 'Fix again', headRef: 'mesh/r/fix', url: 'u8' }),
        create(ProjectPullRequestSchema, { number: 9, title: 'From a fork', headRef: 'main', url: 'u9', crossRepository: true }),
      ],
    })
    let reads = 0
    client.listProjects.mockImplementation(async (req: unknown) => {
      if (!(req as { repo: string }).repo) return create(ListProjectsResponseSchema, { projects })
      if (++reads === 1) throw new Error('[unavailable] fetch timed out')
      return create(ListProjectsResponseSchema, { projects: [withForks] })
    })
    render(wrap(<NewWorkspaceDialog host={host} trigger={<button>New workspace</button>} />))
    const project = await screen.findByLabelText('Project')
    await userEvent.type(project, '/p/new') // not listed: only leaving the field reads it
    await userEvent.tab()
    expect(await screen.findByText('fetch timed out')).toBeInTheDocument()
    expect(screen.getByLabelText('Start from', { selector: 'select' })).toBeDisabled()
    // Leaving the field again asks again.
    await userEvent.click(project)
    await userEvent.tab()
    expect(await screen.findByText('Default branch: main')).toBeInTheDocument()
    expect(reads).toBe(2)
    await userEvent.selectOptions(screen.getByLabelText('Start from', { selector: 'select' }), 'pr')
    const options = within(screen.getByLabelText('Pull request')).getAllByRole('option').slice(1)
    expect(options.map((o) => [o.textContent, (o as HTMLOptionElement).value, (o as HTMLOptionElement).disabled])).toEqual([
      ['#7 Fix', 'mesh/r/fix', false],
      ['#9 From a fork (from a fork: not available)', '', true],
    ])
  })

  it('does not invite a choice when every open pull request is from a fork', async () => {
    const { host, client, wrap } = dialog()
    const forks = create(ProjectSchema, { path: '/p/shop', name: 'shop', defaultBranch: 'main', pullRequests: [create(ProjectPullRequestSchema, { number: 9, title: 'From a fork', headRef: 'main', url: 'u9', crossRepository: true })] })
    client.listProjects.mockImplementation(async (req: unknown) => create(ListProjectsResponseSchema, { projects: (req as { repo: string }).repo ? [forks] : projects }))
    render(wrap(<NewWorkspaceDialog host={host} trigger={<button>New workspace</button>} />))
    await userEvent.type(await screen.findByLabelText('Project'), '/p/shop')
    await userEvent.tab()
    await screen.findByText('Default branch: main')
    await userEvent.selectOptions(screen.getByLabelText('Start from', { selector: 'select' }), 'pr')
    expect(within(screen.getByLabelText('Pull request')).getAllByRole('option')[0]).toHaveTextContent('No open pull requests')
  })

  it('does not start a worker on no tier when the host lists none for the CLI', async () => {
    const { host, client, wrap } = dialog()
    noTiers.on = true
    try {
      render(wrap(<NewWorkspaceDialog host={host} trigger={<button>New workspace</button>} />))
      await userEvent.type(await screen.findByLabelText('Name'), 'w')
      await userEvent.type(screen.getByLabelText('Project'), '/p/shop')
      await userEvent.selectOptions(screen.getByLabelText('Run'), 'r1') // r1 has a lead: this one is a worker
      expect(screen.getByRole('button', { name: 'Start workspace' })).toBeDisabled()
      expect(client.spawnAgent).not.toHaveBeenCalled()
    } finally {
      noTiers.on = false
    }
  })

  it('offers a path to type when the host lists no projects, and shows why a start failed', async () => {
    const { host, client, wrap } = dialog()
    client.listProjects.mockRejectedValue(new Error('[unimplemented] no such action'))
    client.spawnAgent.mockRejectedValue(new Error('[invalid_argument] /nope is not a git repository'))
    render(wrap(<NewWorkspaceDialog host={host} trigger={<button>New workspace</button>} />))
    expect(await screen.findByText(/project list isn't available \(no such action\)/)).toBeInTheDocument()
    await userEvent.type(screen.getByLabelText('Project'), '/nope')
    await userEvent.type(screen.getByLabelText('Name'), 'x')
    await userEvent.click(screen.getByRole('button', { name: 'Start workspace' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('/nope is not a git repository')
  })
})

describe('ModelControl and vendor icons', () => {
  const kinds = create(CheckModelsResponseSchema, { kinds: [create(KindModelsSchema, { kind: 'claude', listed: [create(ModelFamilyEntrySchema, { family: 'opus' }), create(ModelFamilyEntrySchema, { family: 'sonnet' })] })] })

  function control(o: Parameters<typeof create<typeof AgentSchema>>[1] = {}) {
    const f = fixture()
    Object.assign(f.client, { switchAgent: vi.fn(async (_req: unknown) => ({})), checkModels: vi.fn(async (_req: unknown) => kinds) })
    const a = agent('c1', { name: 'app-c', kind: 'claude', model: 'sonnet', effort: 'high', tier: 'mid', ...o })
    f.host.store.setState((s) => ({ agents: { ...s.agents, c1: a } }))
    return { ...f, a, client: f.client as typeof f.client & { switchAgent: ReturnType<typeof vi.fn>; checkModels: ReturnType<typeof vi.fn> } }
  }

  it('shows the CLI, model and effort with the vendor\'s mark, and a reopen under way', () => {
    const { host, a, wrap } = control({ pendingReopen: create(ReopenSchema, { model: 'opus', effort: 'high', why: 'moved' }) })
    render(wrap(<ModelControl host={host} agent={a} />))
    expect(screen.getByTestId('runs-on')).toHaveTextContent('claude · sonnet · high')
    expect(screen.getByRole('img', { name: 'Anthropic Claude' })).toBeInTheDocument()
    expect(screen.getByRole('status')).toHaveTextContent('Switching to opus (moved)…')
  })

  it('does not switch to a tier when the host lists none for the CLI', async () => {
    const { host, a, wrap, client } = control()
    noTiers.on = true
    try {
      render(wrap(<ModelControl host={host} agent={a} />))
      await userEvent.click(screen.getByRole('button', { name: 'Change the model of app-c' }))
      expect(screen.getByLabelText('Tier')).toBeDisabled()
      expect(screen.getByRole('button', { name: 'Switch' })).toBeDisabled()
      expect(client.switchAgent).not.toHaveBeenCalled()
    } finally {
      noTiers.on = false
    }
  })

  it('switches to a tier, to a model of the host\'s list with an effort, or the effort alone', async () => {
    const { host, a, wrap, client } = control()
    render(wrap(<ModelControl host={host} agent={a} />))
    await userEvent.click(screen.getByRole('button', { name: 'Change the model of app-c' }))
    // To a tier (the host's tiers for the kind, or the usual four).
    await userEvent.selectOptions(screen.getByLabelText('Tier'), 'strong')
    await userEvent.click(screen.getByRole('button', { name: 'Switch' }))
    await waitFor(() => expect(client.switchAgent).toHaveBeenCalledTimes(1))
    expect(client.switchAgent.mock.calls[0]![0]).toMatchObject({ agent: 'c1', tier: 'strong', model: '', effort: '', next: false })
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())

    await userEvent.click(screen.getByRole('button', { name: 'Change the model of app-c' }))
    await userEvent.selectOptions(screen.getByLabelText('Change by'), 'model')
    expect(screen.getByRole('button', { name: 'Switch' })).toBeDisabled()
    await waitFor(() => expect(client.checkModels).toHaveBeenCalled())
    await userEvent.type(screen.getByLabelText('Model'), 'opus')
    await userEvent.selectOptions(screen.getByLabelText('Effort'), 'max')
    await userEvent.click(screen.getByRole('button', { name: 'Switch' }))
    await waitFor(() => expect(client.switchAgent).toHaveBeenCalledTimes(2))
    expect(client.switchAgent.mock.calls[1]![0]).toMatchObject({ tier: '', model: 'opus', effort: 'max' })
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())

    await userEvent.click(screen.getByRole('button', { name: 'Change the model of app-c' }))
    await userEvent.selectOptions(screen.getByLabelText('Change by'), 'effort')
    await userEvent.selectOptions(screen.getByLabelText('Effort'), 'low')
    await userEvent.click(screen.getByRole('button', { name: 'Switch' }))
    await waitFor(() => expect(client.switchAgent).toHaveBeenCalledTimes(3))
    expect(client.switchAgent.mock.calls[2]![0]).toMatchObject({ tier: '', model: '', effort: 'low' })
  })

  it('shows the host\'s refusal in its own words and keeps the dialog open', async () => {
    const { host, a, wrap, client } = control()
    client.switchAgent.mockRejectedValue(new Error('[failed_precondition] app-c already runs on sonnet at high effort'))
    render(wrap(<ModelControl host={host} agent={a} />))
    await userEvent.click(screen.getByRole('button', { name: 'Change the model of app-c' }))
    await userEvent.click(screen.getByRole('button', { name: 'Switch' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('app-c already runs on sonnet at high effort')
    expect(screen.getByRole('dialog')).toBeInTheDocument()
  })

  it('draws a mark for each known vendor and a letter badge for the rest', () => {
    render(<>
      <VendorIcon kind="agy" /><VendorIcon kind="opencode" /><VendorIcon kind="copilot" /><VendorIcon kind="codex" /><VendorIcon kind="zed" />
    </>)
    expect(screen.getByRole('img', { name: 'Google Gemini' }).tagName.toLowerCase()).toBe('svg')
    expect(screen.getByRole('img', { name: 'OpenCode' }).tagName.toLowerCase()).toBe('svg')
    expect(screen.getByRole('img', { name: 'GitHub Copilot' }).tagName.toLowerCase()).toBe('svg')
    expect(screen.getByRole('img', { name: 'OpenAI' })).toHaveTextContent('O')
    expect(screen.getByRole('img', { name: 'zed' })).toHaveTextContent('Z')
    // Each in its vendor's brand colour (a theme token), an unknown CLI in the muted text colour.
    expect(screen.getByRole('img', { name: 'Google Gemini' })).toHaveClass('text-vendor-gemini')
    expect(screen.getByRole('img', { name: 'OpenAI' })).toHaveClass('text-vendor-openai')
    expect(screen.getByRole('img', { name: 'zed' })).toHaveClass('text-muted-foreground')
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
    bind('app.keys.dock', 'Mod+Shift+O')
    bind('app.keys.palette', '')
    render(wrap(<Keys />))
    fireEvent.keyDown(window, { key: 'D', ctrlKey: true, shiftKey: true })
    expect(model.store.getState().dockOpen).toBe(true) // the old chord is free now
    fireEvent.keyDown(window, { key: 'O', ctrlKey: true, shiftKey: true })
    expect(model.store.getState().dockOpen).toBe(false)
    fireEvent.keyDown(window, { key: 'P', ctrlKey: true, shiftKey: true })
    expect(model.store.getState().paletteOpen).toBe(false)
  })
})

describe('SettingsPanel', () => {
  const setting = (key: string, type: string) => create(SettingDescriptor, { key, value: fromJson(ValueSchema, type === 'bool' ? true : 'x'), isSet: false, schema: create(SettingSchemaSchema, { type, summary: `about ${key}` }) })

  it('is filtered by the palette while it is already open, and by what is typed in it', async () => {
    const { host, wrap, model } = fixture()
    host.store.setState({ settings: { 'app.keys.dock': setting('app.keys.dock', 'string'), 'repos.dir': setting('repos.dir', 'string') } })
    render(wrap(<SettingsPanel host={host} />))
    expect(screen.getByText('repos.dir')).toBeInTheDocument()
    act(() => model.openSettings(true, 'app.keys'))
    expect(screen.getByRole('textbox', { name: 'Filter settings' })).toHaveValue('app.keys')
    expect(screen.queryByText('repos.dir')).toBeNull()
    await userEvent.clear(screen.getByRole('textbox', { name: 'Filter settings' }))
    await userEvent.type(screen.getByRole('textbox', { name: 'Filter settings' }), 'repos')
    expect(screen.getByText('repos.dir')).toBeInTheDocument()
    expect(model.store.getState().settingsQuery).toBe('repos')
    // Asked for again after the user changed the filter: it is the palette's again.
    act(() => model.openSettings(true, 'app.keys'))
    expect(screen.getByRole('textbox', { name: 'Filter settings' })).toHaveValue('app.keys')
  })
})

describe('ShortcutField', () => {
  const setting = (key: string, value: string) => create(SettingDescriptor, { key, value: fromJson(ValueSchema, value), isSet: true, schema: create(SettingSchemaSchema, { type: 'shortcut', summary: `shortcut: ${key}` }) })

  it('on a host that is not the first only shows the keys, saying whose the app follows', () => {
    const { host, wrap } = fixture()
    const second = { ...host, info: { ...host.info, id: 'h2', name: 'work' } } as HostRuntime
    render(wrap(<ShortcutField host={second} id="app.keys.dock" value="Mod+Shift+D" busy={false} save={vi.fn()} />))
    expect(screen.queryByRole('button', { name: /Record a shortcut/ })).toBeNull()
    expect(screen.getByText('Ctrl+Shift+D')).toBeInTheDocument()
    expect(screen.getByText(/uses the keys of home, its first host/)).toBeInTheDocument()
  })

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
    const promptAgent = vi.fn(async (_req: unknown): Promise<object> => ({}))
    const listPrompts = vi.fn(async (_req: unknown): Promise<{ prompts: QueuedPrompt[] }> => ({ prompts: [] }))
    const withdrawPrompt = vi.fn(async (_req: unknown): Promise<object> => ({}))
    const interruptAgent = vi.fn(async (_req: unknown) => ({ stopped: true }))
    const sendPromptAnyway = vi.fn(async (_req: unknown): Promise<object> => ({ discarded: '' }))
    Object.assign(f.client, { readTranscript, promptAgent, listPrompts, withdrawPrompt, interruptAgent, sendPromptAnyway })
    const ui = f.wrap(<ChatView host={f.host} agentId="w1" active />)
    return { ...f, readTranscript, promptAgent, listPrompts, withdrawPrompt, interruptAgent, sendPromptAnyway, ui }
  }

  it('shows the conversation as blocks, with tool calls folded, the reply as Markdown, and the doorbell until the messages are read', async () => {
    const { ui, readTranscript, host } = chat()
    host.store.setState({ messageLoad: {} })
    render(ui)
    const list = await screen.findByRole('list', { name: 'Conversation' })
    expect(readTranscript).toHaveBeenCalledWith(expect.objectContaining({ agent: 'w1', offset: -1n, backward: true }))
    expect(host.loadMessages).toHaveBeenCalledWith('r1')
    expect(within(list).getByText(/From the mesh: task from lead/)).toBeInTheDocument()
    expect(within(list).getByText('fix the typo')).toBeInTheDocument()
    expect(within(list).getByText('README.md').tagName).toBe('STRONG')
    const tools = within(list).getByText('Ran 2 tools: Bash, Read').closest('details')!
    expect(tools).not.toHaveAttribute('open')
    expect(tools).toHaveTextContent('README.md:3: typo')
    expect(tools).toHaveTextContent('no such file')
    expect(screen.getByRole('button', { name: 'Load earlier' })).toBeInTheDocument()
    // The messages are read: the doorbell goes, the message is a card where it was sent, a plan answerable in place.
    const plan = msg('p1', MessageType.PLAN, 'w1', 'lead', 1000, 'the plan')
    act(() => {
      putMessages(host, plan)
      waitOn(host, plan)
      host.store.setState({ messageLoad: { r1: 'loaded' } })
    })
    expect(within(list).queryByText(/From the mesh/)).toBeNull()
    const card = await within(list).findByRole('article', { name: 'Plan from w1' })
    expect(within(card).getByRole('button', { name: 'Approve' })).toBeInTheDocument()
  })

  it('quotes a reply into the composer, copies it, and shimmers a tool run the agent is still at', async () => {
    const writeText = vi.fn(async () => {})
    Object.assign(navigator, { clipboard: { writeText } })
    const running = [...tail, entry('c3', TranscriptKind.TOOL_CALL, { tool: 'Edit', toolUseId: 't3' })]
    const { ui } = chat({ agent: { state: AgentState.WORKING }, read: vi.fn(async () => res(running)) })
    render(ui)
    const reply = (await screen.findByText('README.md')).closest('[data-kind="assistant"]')! as HTMLElement
    await userEvent.click(within(reply).getByRole('button', { name: 'Copy' }))
    expect(writeText).toHaveBeenCalledWith('Looking at **README.md**.')
    await userEvent.click(within(reply).getByRole('button', { name: 'Quote in a reply' }))
    await waitFor(() => expect(screen.getByRole('textbox', { name: 'Prompt' })).toHaveValue('> Looking at **README.md**.\n\n'))
    expect(screen.getByText(/^Ran 3 tools/)).toHaveAttribute('data-running', 'true')
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

  it('reads the tail again when an earlier page does not join what is held', async () => {
    const read = vi.fn(async (req: { offset: bigint; backward: boolean }) => (req.backward && req.offset === 10n ? res([entry('x', TranscriptKind.USER, { text: 'from elsewhere' })], { startOffset: 0n, endOffset: 40n, atStart: true }) : res(tail)))
    const { ui } = chat({ read })
    render(ui)
    await userEvent.click(await screen.findByRole('button', { name: 'Load earlier' }))
    await waitFor(() => expect(read.mock.calls.filter(([r]) => r.backward && r.offset === -1n)).toHaveLength(2))
    expect(await screen.findByRole('list', { name: 'Conversation' })).toBeInTheDocument()
    expect(screen.queryByText('from elsewhere')).toBeNull()
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

  it('a forward read that was out while earlier entries came in adds to them instead of undoing them', async () => {
    const pending: Array<{ req: { offset: bigint; backward: boolean }; done: (r: ReturnType<typeof res>) => void }> = []
    let first = true // the tail is answered at once; every read after it waits to be answered
    const read = vi.fn((req: { offset: bigint; backward: boolean }) => {
      if (first) {
        first = false
        return Promise.resolve(res(tail))
      }
      return new Promise<ReturnType<typeof res>>((done) => pending.push({ req, done }))
    })
    const { ui, host } = chat({ read })
    render(ui)
    await screen.findByRole('list', { name: 'Conversation' })
    await userEvent.click(screen.getByRole('button', { name: 'Load earlier' }))
    act(() => host.store.setState((s) => ({ agents: { ...s.agents, w1: { ...s.agents.w1!, state: AgentState.WORKING, stateDetail: 'Bash' } } })))
    await waitFor(() => expect(pending).toHaveLength(2))
    const [earlier, later] = [pending.find((p) => p.req.backward)!, pending.find((p) => !p.req.backward)!]
    // The earlier page lands first, then the forward read that began before it did.
    await act(async () => earlier.done(res([entry('old', TranscriptKind.USER, { text: 'the very first prompt' })], { startOffset: 0n, endOffset: 10n, atStart: true })))
    await act(async () => later.done(res([entry('n', TranscriptKind.ASSISTANT, { text: 'a new reply' })], { startOffset: 100n, endOffset: 150n })))
    expect(await screen.findByText('a new reply')).toBeInTheDocument()
    expect(screen.getByText('the very first prompt')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Load earlier' })).toBeNull()
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

  it('keeps what was typed while a prompt was in flight, and only that', async () => {
    const { ui, promptAgent } = chat()
    let release = () => {}
    promptAgent.mockImplementationOnce(() => new Promise<object>((r) => (release = () => r({}))))
    render(ui)
    const box = await screen.findByRole('textbox', { name: 'Prompt' })
    await userEvent.type(box, 'run tests{Enter}')
    await userEvent.type(box, ' and lint')
    await act(async () => release())
    await waitFor(() => expect(box).toHaveValue('and lint'))
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

  it('queues what is sent while the agent works, shows it waiting, and Up takes the newest back to edit', async () => {
    const c = chat({ agent: { state: AgentState.WORKING } })
    const queued = create(QueuedPromptSchema, { id: 'q1', agentId: 'w1', text: 'after this', held: 'working' })
    c.promptAgent.mockResolvedValueOnce({ queued })
    render(c.ui)
    const box = await screen.findByRole('textbox', { name: 'Prompt' })
    expect(box).toBeEnabled() // never refused for being busy
    await userEvent.type(box, 'after this{Enter}')
    const list = await screen.findByRole('region', { name: 'Queued prompts' })
    expect(within(list).getByText('after this')).toBeInTheDocument()
    expect(within(list).getByText(/Sent when it finishes its turn/)).toBeInTheDocument()
    expect(box).toHaveValue('')

    // Up in the empty box: the newest queued prompt comes back into it, and leaves the queue.
    c.withdrawPrompt.mockResolvedValueOnce({ prompt: queued })
    await userEvent.type(box, '{ArrowUp}')
    await waitFor(() => expect(box).toHaveValue('after this'))
    expect(c.withdrawPrompt).toHaveBeenCalledWith(expect.objectContaining({ agent: 'w1', promptId: '' }))
    expect(screen.queryByRole('region', { name: 'Queued prompts' })).not.toBeInTheDocument()
  })

  it('stops a working agent with Esc in the composer or the Stop button, and says when it did not stop', async () => {
    const c = chat({ agent: { state: AgentState.WORKING } })
    render(c.ui)
    const box = await screen.findByRole('textbox', { name: 'Prompt' })
    await userEvent.type(box, '{Escape}')
    await waitFor(() => expect(c.interruptAgent).toHaveBeenCalledWith(expect.objectContaining({ agent: 'w1' })))
    c.interruptAgent.mockResolvedValueOnce({ stopped: false })
    await userEvent.click(screen.getByRole('button', { name: 'Stop the agent' }))
    expect(await screen.findByText(/hasn't stopped yet/)).toBeInTheDocument()
  })

  it('editing a queued prompt puts it after what is being written, never over it', async () => {
    const c = chat({ agent: { state: AgentState.WORKING } })
    const q = create(QueuedPromptSchema, { id: 'q1', agentId: 'w1', text: 'old idea', held: 'working' })
    c.listPrompts.mockResolvedValue({ prompts: [q] })
    c.withdrawPrompt.mockResolvedValueOnce({ prompt: q })
    render(c.ui)
    const box = await screen.findByRole('textbox', { name: 'Prompt' })
    await userEvent.type(box, 'new thought')
    const list = await screen.findByRole('region', { name: 'Queued prompts' })
    await userEvent.click(within(list).getByRole('button', { name: 'Edit the queued prompt' }))
    await waitFor(() => expect(box).toHaveValue('new thought\n\nold idea'))
  })

  it('offers no Stop for a CLI whose interrupt key the host doesn\'t know, and Up twice takes back one prompt', async () => {
    const c = chat({ agent: { state: AgentState.WORKING, kind: 'agy' } })
    c.listPrompts.mockResolvedValue({ prompts: [create(QueuedPromptSchema, { id: 'q1', agentId: 'w1', text: 'one', held: 'working' }), create(QueuedPromptSchema, { id: 'q2', agentId: 'w1', text: 'two', held: 'working' })] })
    let release: (v: object) => void = () => {}
    c.withdrawPrompt.mockImplementationOnce(() => new Promise((r) => (release = r)))
    render(c.ui)
    const box = await screen.findByRole('textbox', { name: 'Prompt' })
    await screen.findByRole('region', { name: 'Queued prompts' })
    expect(screen.queryByRole('button', { name: 'Stop the agent' })).not.toBeInTheDocument()
    await userEvent.type(box, '{Escape}')
    expect(c.interruptAgent).not.toHaveBeenCalled()
    // Two presses while the first take-back is out: one call.
    await userEvent.type(box, '{ArrowUp}{ArrowUp}')
    expect(c.withdrawPrompt).toHaveBeenCalledTimes(1)
    release({ prompt: create(QueuedPromptSchema, { id: 'q2', agentId: 'w1', text: 'two' }) })
    await waitFor(() => expect(box).toHaveValue('two'))
  })

  it('reads the queue again when it changed while the first listing was out', async () => {
    const c = chat({ agent: { state: AgentState.WORKING } })
    let release: (v: { prompts: QueuedPrompt[] }) => void = () => {}
    c.listPrompts.mockImplementationOnce(() => new Promise((r) => (release = r)))
    c.listPrompts.mockResolvedValueOnce({ prompts: [create(QueuedPromptSchema, { id: 'q1', agentId: 'w1', text: 'old', held: 'working' }), create(QueuedPromptSchema, { id: 'q2', agentId: 'w1', text: 'new', held: 'working' })] })
    render(c.ui)
    await waitFor(() => expect(c.listPrompts).toHaveBeenCalledTimes(1))
    act(() => c.host.store.setState(addPrompt(create(QueuedPromptSchema, { id: 'q2', agentId: 'w1', text: 'new' }))))
    release({ prompts: [create(QueuedPromptSchema, { id: 'q1', agentId: 'w1', text: 'old' })] })
    const list = await screen.findByRole('region', { name: 'Queued prompts' })
    await waitFor(() => expect(within(list).getByText('old')).toBeInTheDocument())
    expect(c.listPrompts).toHaveBeenCalledTimes(2)
  })

  it('a taken-back prompt that was typed meanwhile says so', async () => {
    const c = chat({ agent: { state: AgentState.WORKING } })
    c.listPrompts.mockResolvedValue({ prompts: [create(QueuedPromptSchema, { id: 'q1', agentId: 'w1', text: 'late', held: 'working' })] })
    c.withdrawPrompt.mockRejectedValueOnce(new Error('[failed_precondition] that prompt is no longer waiting: it was typed into app-fix\'s terminal'))
    render(c.ui)
    const list = await screen.findByRole('region', { name: 'Queued prompts' })
    await userEvent.click(within(list).getByRole('button', { name: 'Remove the queued prompt' }))
    expect(await screen.findByText(/no longer waiting/)).toBeInTheDocument()
  })

  it('offers Send anyway on a prompt held by a draft, naming the draft, and drops the prompt once it was sent', async () => {
    const c = chat({ agent: { state: AgentState.IDLE } })
    const q = create(QueuedPromptSchema, { id: 'q1', agentId: 'w1', text: 'my prompt', held: 'draft', heldText: 'half a thou' })
    c.listPrompts.mockResolvedValue({ prompts: [q] })
    render(c.ui)
    const list = await screen.findByRole('region', { name: 'Queued prompts' })
    expect(within(list).getByText(/Clear it in the terminal, or send this anyway/)).toBeInTheDocument()
    await userEvent.click(within(list).getByRole('button', { name: 'Send anyway' }))
    const dialog = await screen.findByRole('dialog', { name: 'Send over the draft?' })
    expect(dialog).toHaveTextContent('half a thou')
    await userEvent.click(within(dialog).getByRole('button', { name: 'Send anyway' }))
    await waitFor(() => expect(c.sendPromptAnyway).toHaveBeenCalledWith(expect.objectContaining({ agent: 'w1', promptId: 'q1', draft: 'half a thou' })))
    await waitFor(() => expect(screen.queryByRole('region', { name: 'Queued prompts' })).not.toBeInTheDocument())
  })

  it('shows the host\'s refusal of Send anyway (the draft changed, someone types) and keeps the prompt queued', async () => {
    const c = chat({ agent: { state: AgentState.IDLE } })
    c.listPrompts.mockResolvedValue({ prompts: [create(QueuedPromptSchema, { id: 'q1', agentId: 'w1', text: 'my prompt', held: 'draft', heldText: 'half a thou' })] })
    c.sendPromptAnyway.mockRejectedValueOnce(new Error('[failed_precondition] someone is typing in app-fix\'s terminal: try again when they stop'))
    render(c.ui)
    const list = await screen.findByRole('region', { name: 'Queued prompts' })
    await userEvent.click(within(list).getByRole('button', { name: 'Send anyway' }))
    const dialog = await screen.findByRole('dialog', { name: 'Send over the draft?' })
    await userEvent.click(within(dialog).getByRole('button', { name: 'Send anyway' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('someone is typing')
    expect(screen.getByRole('region', { name: 'Queued prompts', hidden: true })).toBeInTheDocument()
  })

  it('offers no Send anyway for another hold, without the draft text, or to a read-only device', async () => {
    const c = chat({ agent: { state: AgentState.WORKING } })
    c.listPrompts.mockResolvedValue({ prompts: [create(QueuedPromptSchema, { id: 'q1', agentId: 'w1', text: 'a', held: 'working' }), create(QueuedPromptSchema, { id: 'q2', agentId: 'w1', text: 'b', held: 'draft' })] })
    render(c.ui)
    const list = await screen.findByRole('region', { name: 'Queued prompts' })
    expect(within(list).queryByRole('button', { name: 'Send anyway' })).not.toBeInTheDocument()
  })

  it('tells the user a CLI has no record to read, and still takes prompts', async () => {
    const { ui } = chat({ read: vi.fn(async () => res([], { supported: false })) })
    render(ui)
    expect(await screen.findByText(/reads no record of a/)).toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: 'Prompt' })).toBeEnabled()
  })
})

describe('Project shells', () => {
  const shell = (id: string, o: { repo?: string; exited?: boolean } = {}) => create(ShellSchema, { id, repo: '/p/app', ...o })
  function shells(initial: ReturnType<typeof shell>[] = []) {
    const f = fixture()
    let live = initial
    Object.assign(f.client, {
      listShells: vi.fn(async () => create(ListShellsResponseSchema, { shells: live })),
      openShell: vi.fn(async (req: { repo: string }) => {
        const sh = shell('t1', { repo: req.repo })
        live = [...live, sh]
        return create(OpenShellResponseSchema, { shell: sh })
      }),
      closeShell: vi.fn(async (req: { id: string }) => {
        live = live.filter((x) => x.id !== req.id)
        return create(CloseShellResponseSchema)
      }),
    })
    f.model.selectAgent('h1', 'lead')
    return { ...f, client: f.client as unknown as Record<'openShell' | 'closeShell' | 'listShells', ReturnType<typeof vi.fn>> }
  }

  it('offers New terminal from the +, starts a shell in the project, opens its tab, and keeps its tab across a restore', async () => {
    const { client, wrap, model } = shells()
    render(wrap(<TabBar />))
    await userEvent.click(screen.getByRole('button', { name: 'New in app' }))
    expect((await screen.findAllByRole('menuitem')).map((m) => m.textContent)).toEqual(['New lead', 'New terminal'])
    await userEvent.click(screen.getByRole('menuitem', { name: 'New terminal' }))
    await waitFor(() => expect(model.store.getState().agentId).toBe('shell:t1'))
    expect(client.openShell).toHaveBeenCalledWith(expect.objectContaining({ repo: '/p/app' }))
    // It is a tab of the project's bar, beside the lead's.
    const tabs = screen.getByRole('tablist', { name: 'Open agents' })
    expect(within(tabs).getAllByRole('tab').map((t) => t.textContent)).toEqual(['app-lead', 'Terminal'])
    expect(model.store.getState().tabs).toContainEqual({ hostId: 'h1', agentId: 'shell:t1' })
  })

  it('asks when a shell tab closes: Keep running closes the tab only and the + menu opens it again; End shell stops it', async () => {
    const { client, wrap, model } = shells([shell('t1'), shell('t2')])
    await act(() => model.loadShells('h1'))
    model.selectAgent('h1', 'shell:t1')
    model.selectAgent('h1', 'shell:t2')
    render(wrap(<><TabBar /><ShellCloseDialog /></>))
    const tabs = screen.getByRole('tablist', { name: 'Open agents' })
    expect(within(tabs).getAllByRole('tab').map((t) => t.textContent)).toEqual(['app-lead', 'Terminal', 'Terminal 2'])
    // Keep running: the tab goes, the shell stays on the host.
    await userEvent.click(within(tabs).getByRole('button', { name: 'Close Terminal' }))
    expect(await screen.findByRole('dialog', { name: 'Close this terminal?' })).toBeInTheDocument()
    expect(model.store.getState().tabs).toContainEqual({ hostId: 'h1', agentId: 'shell:t1' })
    await userEvent.click(screen.getByRole('button', { name: 'Keep running' }))
    expect(model.store.getState().tabs).not.toContainEqual({ hostId: 'h1', agentId: 'shell:t1' })
    expect(client.closeShell).not.toHaveBeenCalled()
    await userEvent.click(screen.getByRole('button', { name: 'New in app' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'Open the running terminal' }))
    expect(model.store.getState().agentId).toBe('shell:t1')
    // End shell: the host is told, and the tab and the shell are gone.
    await userEvent.click(within(tabs).getByRole('button', { name: 'Close Terminal' }))
    await userEvent.click(await screen.findByRole('button', { name: 'End shell' }))
    await waitFor(() => expect(client.closeShell).toHaveBeenCalledWith(expect.objectContaining({ id: 't1' })))
    await waitFor(() => expect(model.store.getState().tabs).not.toContainEqual({ hostId: 'h1', agentId: 'shell:t1' }))
    expect(model.store.getState().shells.h1!.map((x) => x.id)).toEqual(['t2'])
  })

  it('ends a shell that ended on its own without asking, closes a kept one that came back, and tells when a new terminal fails', async () => {
    const { client, wrap, model } = shells([shell('t1'), shell('t2')])
    await act(() => model.loadShells('h1'))
    model.selectAgent('h1', 'shell:t1')
    model.selectAgent('h1', 'shell:t2')
    render(wrap(<><TabBar /><ShellCloseDialog /></>))
    const tabs = screen.getByRole('tablist', { name: 'Open agents' })
    // The shell t1 ended by itself (the host forgot it): closing its tab does not offer to keep it running.
    client.listShells.mockImplementation(async () => create(ListShellsResponseSchema, { shells: [shell('t2')] }))
    await userEvent.click(within(tabs).getByRole('button', { name: 'Close Terminal' }))
    await waitFor(() => expect(model.store.getState().tabs).not.toContainEqual({ hostId: 'h1', agentId: 'shell:t1' }))
    expect(screen.queryByRole('dialog', { name: 'Close this terminal?' })).toBeNull()
    // End shell of one the host no longer has (it ended between the list and the click) is done, not an error.
    client.closeShell.mockImplementationOnce(async () => {
      throw new ConnectError('no shell', Code.NotFound)
    })
    await act(() => model.endShell('h1', 't2'))
    expect(model.store.getState().tabs).not.toContainEqual({ hostId: 'h1', agentId: 'shell:t2' })
    // A New terminal the host refuses says why, in the bar.
    model.selectAgent('h1', 'lead')
    client.openShell.mockImplementationOnce(async () => {
      throw new ConnectError('20 shells are running: end one first', Code.FailedPrecondition)
    })
    await userEvent.click(screen.getByRole('button', { name: 'New in app' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'New terminal' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('20 shells are running')
  })

  it('reopens a closed shell tab that is still running, forgets a host\'s shells with the host, and opens New lead on a shell tab\'s project', async () => {
    const { host, model } = shells([shell('t1')])
    Object.assign(host, { stop: vi.fn() })
    await act(() => model.loadShells('h1'))
    model.selectAgent('h1', 'shell:t1')
    model.closeTab({ hostId: 'h1', agentId: 'shell:t1' }, true)
    expect(model.store.getState().tabs).not.toContainEqual({ hostId: 'h1', agentId: 'shell:t1' })
    model.reopenTab()
    expect(model.store.getState().agentId).toBe('shell:t1')
    keyedCommands(model).tab_new()
    expect(model.store.getState()).toMatchObject({ workspaceOpen: true, workspaceRepo: '/p/app', workspaceHost: 'h1' })
    await act(() => model.removeHost('h1'))
    expect(model.store.getState().shells.h1).toBeUndefined()
  })

  it('judges a restored shell tab by the host\'s shells, once they are read, and not before', async () => {
    const { host, model } = shells([shell('t1')])
    model.store.setState({ tabs: [{ hostId: 'h1', agentId: 'lead' }, { hostId: 'h1', agentId: 'shell:t1' }, { hostId: 'h1', agentId: 'shell:gone' }] })
    // A host's first snapshot prunes the agents it lost, not the shells it has not listed yet.
    const patch = pruneHost(model.store.getState(), 'h1', host.store.getState().agents)
    expect(patch).toBeUndefined()
    await act(() => model.loadShells('h1'))
    expect(model.store.getState().tabs).toEqual([{ hostId: 'h1', agentId: 'lead' }, { hostId: 'h1', agentId: 'shell:t1' }])
  })
})
