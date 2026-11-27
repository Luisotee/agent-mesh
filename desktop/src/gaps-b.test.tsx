import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { cleanup, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Code, ConnectError } from '@connectrpc/connect'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { DesktopApi } from '../shared/api'
import { Changes, ContextMeter, Progress, WorktreeActions } from './components/agent-work'
import { LOCAL_DEVICE } from './components/pair-panel'
import { TailnetPanel } from './components/tailnet-panel'
import { AppContext } from './context'
import { ChangedFileSchema, ContextMarkSchema, GetAgentChangesResponseSchema, GetAgentProgressResponseSchema, PlanItemSchema, SetTailnetResponseSchema, TailnetStatusResponseSchema } from './gen/agentmesh/v1/mesh_pb'
import { ActorClass } from './gen/agentmesh/v1/actor_pb'
import { AgentRole, AgentSchema, AgentState, RunSchema, RunStatus } from './gen/agentmesh/v1/types_pb'
import { createAppModel, type HostRuntime } from './model'
import { createHostStore } from './store'

afterEach(cleanup)

const agent = (o: Parameters<typeof create<typeof AgentSchema>>[1] = {}) => create(AgentSchema, { id: 'w1', name: 'app-fix', runId: 'r1', repo: '/p/app', branch: 'mesh/r1/fix', worktree: '/w/app-fix', kind: 'claude', role: AgentRole.WORKER, createdAt: timestampFromMs(1000), state: AgentState.IDLE, ...o })

function fixture(actor = LOCAL_DEVICE, a = agent()) {
  const store = createHostStore()
  store.setState({
    conn: { kind: 'ready', hello: { capabilities: [], actor: { id: actor } } as never },
    synced: true,
    runs: { r1: create(RunSchema, { id: 'r1', title: 'ship it', status: RunStatus.OPEN }) },
    agents: { [a.id]: a },
  })
  const client = {
    getAgentProgress: vi.fn(async (_req: unknown) => create(GetAgentProgressResponseSchema)),
    getAgentChanges: vi.fn(async (_req: unknown) => create(GetAgentChangesResponseSchema, { available: true })),
    archiveAgent: vi.fn(async (_req: unknown): Promise<unknown> => ({})),
    resumeAgent: vi.fn(async (_req: unknown): Promise<unknown> => ({})),
    openInEditor: vi.fn(async (_req: unknown): Promise<unknown> => ({})),
    setTailnet: vi.fn(async (_req: unknown) => create(SetTailnetResponseSchema, { enabled: true, restarting: true })),
    tailnetStatus: vi.fn(async (_req: unknown) => create(TailnetStatusResponseSchema, { enabled: false })),
  }
  const host = { info: { id: 'h1', name: 'home', kind: 'state-dir', target: '/s' }, store, client, connection: { retryNow: vi.fn(), stop: vi.fn() }, loadMessages: vi.fn() } as unknown as HostRuntime
  const api = { hosts: { list: async () => [], add: vi.fn(), remove: vi.fn(), fixed: false }, request: vi.fn(), links: { take: async () => undefined, onOpen: vi.fn(() => () => {}) }, shell: { notify: vi.fn(), setWaiting: vi.fn(), configure: vi.fn(), onNotificationClick: vi.fn(() => () => {}) } } as unknown as DesktopApi
  const model = createAppModel(api)
  model.store.setState({ loaded: true, hosts: [host], hostId: 'h1' })
  const wrap = (ui: React.ReactNode) => <AppContext.Provider value={model}>{ui}</AppContext.Provider>
  return { host, client, model, wrap, agent: a }
}

const last = <T,>(fn: { mock: { calls: unknown[][] } }) => fn.mock.calls.at(-1)![0] as T

describe('Progress', () => {
  it('shows the plan with how far it is and the context against the window', async () => {
    const { host, client, wrap, agent: a } = fixture()
    client.getAgentProgress.mockResolvedValue(create(GetAgentProgressResponseSchema, { supported: true, contextTokens: 128_000n, contextWindow: 200_000n, plan: [['Read the code', 'completed'], ['Write the fix', 'in_progress'], ['Run the tests', 'pending']].map(([content, status]) => create(PlanItemSchema, { content, status })) }))
    render(wrap(<Progress host={host} agent={a} />))
    const section = await screen.findByRole('region', { name: 'Progress' })
    expect(section).toHaveTextContent('1 of 3')
    expect(within(section).getByRole('progressbar', { name: 'Plan progress' })).toHaveAttribute('value', '1')
    expect(section).toHaveTextContent('Now: Write the fix')
    expect(within(within(section).getByRole('list', { name: 'Plan' })).getAllByRole('listitem')).toHaveLength(3)
    expect(last(client.getAgentProgress)).toMatchObject({ agent: 'w1' })
  })

  it('shows the context against the whole window as a bar with a tick at each mark, amber from the first one', async () => {
    const { host, client, wrap, agent: a } = fixture()
    client.getAgentProgress.mockResolvedValue(create(GetAgentProgressResponseSchema, {
      supported: true, contextTokens: 160_000n, contextWindow: 200_000n,
      marks: [create(ContextMarkSchema, { tokens: 150_000n, label: 'The mesh resets this lead', setting: 'root_reset.at' }), create(ContextMarkSchema, { tokens: 300_000n, label: 'past the window', setting: 'x' })],
    }))
    render(wrap(<ContextMeter host={host} agent={a} active />))
    const meter = await screen.findByRole('meter', { name: 'Context used' })
    expect(meter).toHaveAttribute('aria-valuenow', '80')
    expect(meter).toHaveAttribute('aria-valuetext', '160k of 200k tokens (80%)')
    expect(meter).toHaveTextContent('160k / 200k')
    // One tick, at 75% of the window (a mark past the window is not drawn), and the bar is amber: past the first mark.
    expect(within(meter).getAllByTestId('context-mark').map((m) => m.style.left)).toEqual(['75%'])
    expect(meter.querySelector('[data-tone]')!.getAttribute('data-tone')).toBe('warn')
  })

  it('is green below the first mark and red from 90% of the window', async () => {
    const { host, client, wrap, agent: a } = fixture()
    const marks = [create(ContextMarkSchema, { tokens: 150_000n, label: 'x', setting: 'root_reset.at' })]
    client.getAgentProgress.mockResolvedValue(create(GetAgentProgressResponseSchema, { supported: true, contextTokens: 100_000n, contextWindow: 200_000n, marks }))
    const { rerender } = render(wrap(<ContextMeter host={host} agent={a} active />))
    expect((await screen.findByRole('meter', { name: 'Context used' })).querySelector('[data-tone]')!.getAttribute('data-tone')).toBe('ok')
    client.getAgentProgress.mockResolvedValue(create(GetAgentProgressResponseSchema, { supported: true, contextTokens: 185_000n, contextWindow: 200_000n, marks }))
    rerender(wrap(<ContextMeter host={host} agent={{ ...a, state: AgentState.WORKING }} active />))
    await waitFor(() => expect(screen.getByRole('meter', { name: 'Context used' }).querySelector('[data-tone]')!.getAttribute('data-tone')).toBe('over'))
  })

  it('shows nothing for a CLI the host cannot read, or an agent with no plan', async () => {
    const { host, client, wrap, agent: a } = fixture()
    render(wrap(<Progress host={host} agent={a} />))
    await waitFor(() => expect(client.getAgentProgress).toHaveBeenCalled())
    expect(screen.queryByRole('region', { name: 'Progress' })).toBeNull()
  })
})

describe('Changes', () => {
  it('lists the files with their marks and counts, and says what is uncommitted or not pushed', async () => {
    const { host, client, wrap, agent: a } = fixture()
    client.getAgentChanges.mockResolvedValue(create(GetAgentChangesResponseSchema, { available: true, uncommitted: 1, unpushed: 2, commits: 2, files: [create(ChangedFileSchema, { path: 'src/a.ts', status: 'modified', additions: 3, deletions: 1 }), create(ChangedFileSchema, { path: 'new.txt', status: 'untracked', uncommitted: true })] }))
    render(wrap(<Changes host={host} agent={a} />))
    expect(await screen.findByTestId('change-summary')).toHaveTextContent('1 file uncommitted · 2 commits not pushed')
    const files = screen.getByRole('list', { name: 'Changed files' })
    expect(within(files).getAllByRole('listitem')).toHaveLength(2)
    expect(files).toHaveTextContent('src/a.ts')
    expect(files).toHaveTextContent('+3 −1')
    expect(within(files).getByLabelText('untracked')).toHaveTextContent('?')
  })

  it('keeps what it shows while it reads again after the agent changed state', async () => {
    const { host, client, wrap, agent: a } = fixture()
    client.getAgentChanges.mockResolvedValue(create(GetAgentChangesResponseSchema, { available: true, uncommitted: 1 }))
    const { rerender } = render(wrap(<Changes host={host} agent={a} />))
    expect(await screen.findByTestId('change-summary')).toHaveTextContent('1 file uncommitted')
    const calls = client.getAgentChanges.mock.calls.length
    rerender(wrap(<Changes host={host} agent={agent({ state: AgentState.WORKING })} />))
    expect(screen.getByTestId('change-summary')).toHaveTextContent('1 file uncommitted')
    expect(screen.queryByText('Loading…')).toBeNull()
    await waitFor(() => expect(client.getAgentChanges.mock.calls.length).toBeGreaterThan(calls))
  })

  it('reads once, not twice, when another agent in another state is shown', async () => {
    const { host, client, wrap, agent: a } = fixture()
    client.getAgentChanges.mockResolvedValue(create(GetAgentChangesResponseSchema, { available: true, uncommitted: 1 }))
    const { rerender } = render(wrap(<Changes host={host} agent={a} />))
    expect(await screen.findByTestId('change-summary')).toHaveTextContent('1 file uncommitted')
    const calls = client.getAgentChanges.mock.calls.length
    rerender(wrap(<Changes host={host} agent={agent({ id: 'w2', name: 'other', state: AgentState.WORKING })} />))
    await waitFor(() => expect(client.getAgentChanges.mock.calls.length).toBeGreaterThan(calls))
    await new Promise((r) => setTimeout(r, 50))
    expect(client.getAgentChanges.mock.calls.length).toBe(calls + 1)
  })

  it('says why when the host could not read the worktree', async () => {
    const { host, client, wrap, agent: a } = fixture()
    client.getAgentChanges.mockResolvedValue(create(GetAgentChangesResponseSchema, { reason: 'its worktree and its branch are gone' }))
    render(wrap(<Changes host={host} agent={a} />))
    expect(await screen.findByTestId('change-summary')).toHaveTextContent('its worktree and its branch are gone')
  })
})

describe('worktree actions', () => {
  it('opens the worktree in the editor, for the host\'s own device only', async () => {
    const own = fixture()
    render(own.wrap(<WorktreeActions host={own.host} agent={own.agent} />))
    await userEvent.click(screen.getByRole('button', { name: 'Open in editor' }))
    expect(last(own.client.openInEditor)).toMatchObject({ agent: 'w1', path: '' })
    await waitFor(() => expect(own.model.store.getState().notices.map((n) => [n.text, n.tone])).toEqual([['Opened in the editor.', 'info']]))
    cleanup()
    const paired = fixture('dev-phone')
    render(paired.wrap(<WorktreeActions host={paired.host} agent={paired.agent} />))
    expect(screen.queryByRole('button', { name: 'Open in editor' })).toBeNull()
    expect(screen.getByRole('button', { name: 'Archive' })).toBeInTheDocument()
  })

  it('shows what the host says when the editor can\'t open', async () => {
    const { host, client, wrap, agent: a, model } = fixture()
    client.openInEditor.mockRejectedValue(new ConnectError('the editor "code" isn\'t on this host\'s PATH', Code.FailedPrecondition))
    render(wrap(<WorktreeActions host={host} agent={a} />))
    await userEvent.click(screen.getByRole('button', { name: 'Open in editor' }))
    await waitFor(() => expect(model.store.getState().notices.map((n) => [n.text, n.tone])).toEqual([[expect.stringContaining("isn't on this host's PATH"), 'error']]))
  })

  it('archives a clean agent after a confirmation', async () => {
    const { host, client, wrap, agent: a } = fixture()
    render(wrap(<WorktreeActions host={host} agent={a} />))
    await userEvent.click(screen.getByRole('button', { name: 'Archive' }))
    const dialog = await screen.findByRole('dialog', { name: 'Archive app-fix?' })
    await userEvent.click(within(dialog).getByRole('button', { name: 'Archive' }))
    await waitFor(() => expect(client.archiveAgent).toHaveBeenCalledTimes(1))
    expect(last(client.archiveAgent)).toMatchObject({ agent: 'w1', force: false })
  })

  it('will not archive uncommitted work, and asks to be sure about commits on no remote', async () => {
    const { host, client, wrap, agent: a } = fixture()
    client.getAgentChanges.mockResolvedValue(create(GetAgentChangesResponseSchema, { available: true, uncommitted: 2 }))
    render(wrap(<WorktreeActions host={host} agent={a} />))
    await userEvent.click(screen.getByRole('button', { name: 'Archive' }))
    let dialog = await screen.findByRole('dialog', { name: 'Archive app-fix?' })
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('2 files have uncommitted changes')
    expect(within(dialog).getByRole('button', { name: 'Archive' })).toBeDisabled()
    await userEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())

    client.getAgentChanges.mockResolvedValue(create(GetAgentChangesResponseSchema, { available: true, unpushed: 2 }))
    await userEvent.click(screen.getByRole('button', { name: 'Archive' }))
    dialog = await screen.findByRole('dialog', { name: 'Archive app-fix?' })
    const sure = await within(dialog).findByRole('checkbox')
    const go = within(dialog).getByRole('button', { name: 'Archive' })
    expect(go).toBeDisabled()
    await userEvent.click(sure)
    expect(go).toBeEnabled()
    await userEvent.click(go)
    await waitFor(() => expect(client.archiveAgent).toHaveBeenCalledTimes(1))
    expect(last(client.archiveAgent)).toMatchObject({ agent: 'w1', force: true })
  })

  it('offers Archive only for a worker, and Open in editor not for a peer; a read-only device gets neither', () => {
    for (const [label, o] of [['a root', { role: AgentRole.ROOT }], ['an asker', { role: AgentRole.ASKER }], ['a reviewer', { role: AgentRole.REVIEWER }], ['an adopted session', { adoptedPid: 42 }], ['a peer', { peer: true }]] as const) {
      const f = fixture(LOCAL_DEVICE, agent(o))
      const { unmount } = render(f.wrap(<WorktreeActions host={f.host} agent={f.agent} />))
      expect(screen.queryByRole('button', { name: 'Archive' }), label).toBeNull()
      unmount()
    }
    const peer = fixture(LOCAL_DEVICE, agent({ peer: true }))
    const { unmount } = render(peer.wrap(<WorktreeActions host={peer.host} agent={peer.agent} />))
    expect(screen.queryByRole('button', { name: 'Open in editor' })).toBeNull()
    unmount()
    const ro = fixture(LOCAL_DEVICE)
    ro.host.store.setState({ conn: { kind: 'ready', hello: { capabilities: [], actor: { id: 'viewer', class: ActorClass.READ_ONLY_DEVICE } } as never }, daemon: { actor: { class: ActorClass.READ_ONLY_DEVICE } } as never })
    render(ro.wrap(<WorktreeActions host={ro.host} agent={ro.agent} />))
    expect(screen.queryByRole('button', { name: 'Archive' })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Open in editor' })).toBeNull()
  })

  it('keeps Archive off until what the worktree holds is read, and forgets a tick when the dialog closes', async () => {
    const { host, client, wrap, agent: a } = fixture()
    let release!: (r: ReturnType<typeof create<typeof GetAgentChangesResponseSchema>>) => void
    client.getAgentChanges.mockImplementationOnce(() => new Promise((res) => (release = res)))
    render(wrap(<WorktreeActions host={host} agent={a} />))
    await userEvent.click(screen.getByRole('button', { name: 'Archive' }))
    const dialog = await screen.findByRole('dialog', { name: 'Archive app-fix?' })
    expect(within(dialog).getByRole('button', { name: 'Archive' })).toBeDisabled()
    release(create(GetAgentChangesResponseSchema, { available: true, unpushed: 1 }))
    await userEvent.click(await within(dialog).findByRole('checkbox'))
    expect(within(dialog).getByRole('button', { name: 'Archive' })).toBeEnabled()
    await userEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    client.getAgentChanges.mockResolvedValue(create(GetAgentChangesResponseSchema, { available: true, unpushed: 1 }))
    await userEvent.click(screen.getByRole('button', { name: 'Archive' }))
    const again = await screen.findByRole('dialog', { name: 'Archive app-fix?' })
    expect(await within(again).findByRole('checkbox')).not.toBeChecked()
    expect(within(again).getByRole('button', { name: 'Archive' })).toBeDisabled()
  })

  it('asks for the ignored files and lists them with their sizes before the user archives', async () => {
    const { host, client, wrap, agent: a } = fixture()
    client.getAgentChanges.mockResolvedValue(create(GetAgentChangesResponseSchema, {
      available: true,
      ignored: [{ path: 'node_modules/', bytes: 222_300_000n, partial: true }, { path: '.env.local', bytes: 11n }],
      ignoredMore: 3,
    }))
    render(wrap(<WorktreeActions host={host} agent={a} />))
    await userEvent.click(screen.getByRole('button', { name: 'Archive' }))
    const dialog = await screen.findByRole('dialog', { name: 'Archive app-fix?' })
    const list = await within(dialog).findByRole('list', { name: 'Ignored files that will be deleted' })
    expect(within(list).getAllByRole('listitem').map((li) => li.textContent)).toEqual(['node_modules/at least 212.0 MB', '.env.local11 B', 'and 3 more'])
    // The walk of the worktree is asked once, by its own call; the plain read of what changed is not asked to do it.
    const asked = client.getAgentChanges.mock.calls.map((c) => (c[0] as { withIgnored?: boolean }).withIgnored ?? false)
    expect(asked.filter((w) => w)).toHaveLength(1)
    expect(asked.some((w) => !w)).toBe(true)
    // The tick is for what git can lose (commits no remote has), not for build output: archiving stays one click.
    await waitFor(() => expect(within(dialog).getByRole('button', { name: 'Archive' })).toBeEnabled())
  })

  it('says so in the dialog when the worktree cannot be read, and keeps Archive off', async () => {
    const { host, client, wrap, agent: a } = fixture()
    client.getAgentChanges.mockRejectedValue(new Error('host is unreachable'))
    render(wrap(<WorktreeActions host={host} agent={a} />))
    await userEvent.click(screen.getByRole('button', { name: 'Archive' }))
    const dialog = await screen.findByRole('dialog', { name: 'Archive app-fix?' })
    expect(await within(dialog).findByRole('alert')).toHaveTextContent("Couldn't read the worktree: host is unreachable")
    expect(within(dialog).getByRole('button', { name: 'Archive' })).toBeDisabled()
  })

  it('says it is archived and offers Restore, which resumes the agent', async () => {
    const f = fixture(LOCAL_DEVICE, agent({ archived: true, state: AgentState.RETIRED }))
    render(f.wrap(<WorktreeActions host={f.host} agent={f.agent} />))
    expect(screen.getByRole('status')).toHaveTextContent('Archived: its worktree was removed')
    expect(screen.queryByRole('button', { name: 'Open in editor' })).toBeNull()
    await userEvent.click(screen.getByRole('button', { name: 'Restore' }))
    expect(last(f.client.resumeAgent)).toMatchObject({ agent: 'w1' })
  })
})

describe('the Tailscale switch', () => {
  it('turns the node on after a confirmation that says meshd restarts, and tells what the host answers', async () => {
    const { host, client, wrap } = fixture()
    render(wrap(<TailnetPanel host={host} />))
    await userEvent.click(await screen.findByRole('button', { name: 'Turn Tailscale on' }))
    const dialog = await screen.findByRole('dialog', { name: 'Turn Tailscale on?' })
    expect(dialog).toHaveTextContent('Agents keep running')
    expect(client.setTailnet).not.toHaveBeenCalled()
    await userEvent.click(within(dialog).getByRole('button', { name: 'Turn on and restart' }))
    expect(last(client.setTailnet)).toMatchObject({ enabled: true })
    expect(await screen.findByText(/meshd is restarting to apply it/)).toBeInTheDocument()
  })

  it('says a restart is needed when the host cannot do it, and shows the host\'s refusal', async () => {
    const { host, client, wrap } = fixture()
    client.setTailnet.mockResolvedValueOnce(create(SetTailnetResponseSchema, { enabled: true, restartNeeded: true }))
    render(wrap(<TailnetPanel host={host} />))
    await userEvent.click(await screen.findByRole('button', { name: 'Turn Tailscale on' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Turn on and restart' }))
    expect(await screen.findByText(/Restart meshd for it to take effect/)).toBeInTheDocument()
    client.setTailnet.mockRejectedValueOnce(new ConnectError("couldn't write the marker", Code.FailedPrecondition))
    await userEvent.click(screen.getByRole('button', { name: 'Turn Tailscale on' }))
    await userEvent.click(await screen.findByRole('button', { name: 'Turn on and restart' }))
    expect(await screen.findByRole('alert')).toHaveTextContent("couldn't write the marker")
  })
})
