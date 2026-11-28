import { Profiler, type ReactNode } from 'react'
import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterAll, beforeEach, describe, expect, it, vi } from 'vitest'
import type { DesktopApi } from '../shared/api'
import { ChatView } from './components/chat-view'
import { AgentPanel } from './components/agent-panel'
import { ProjectSidebar } from './components/project-sidebar'
import { SidePanel } from './components/side-panel'
import { TabBar } from './components/tab-bar'
import { AppContext, useAppState } from './context'
import { ReadTranscriptResponseSchema, TranscriptEntrySchema, TranscriptKind } from './gen/agentmesh/v1/chat_pb'
import { ActorClass } from './gen/agentmesh/v1/actor_pb'
import { GetUsageResponseSchema, ListProjectsResponseSchema } from './gen/agentmesh/v1/mesh_pb'
import { GetCostResponseSchema } from './gen/agentmesh/v1/report_pb'
import { AgentRole, AgentSchema, AgentState, MessageSchema, MessageType, RunSchema, RunStatus } from './gen/agentmesh/v1/types_pb'
import { createAppModel, type HostRuntime } from './model'
import { createHostStore } from './store'

/**
 * What the speed work is measured by: how many components a change re-renders, and how long React spends on it, for a
 * host with 40 agents, a 300-entry chat and a dozen tabs. Counts are exact and asserted; times (jsdom, React's
 * development build: only good for comparing runs) are printed when PERF=1 (`PERF=1 pnpm exec vitest run src/perf.test.tsx`).
 */
const calls = vi.hoisted(() => ({ icon: 0, markdown: 0, cards: new Set<string>() }))
vi.mock('./components/state-icon', async (importOriginal) => {
  const m = await importOriginal<typeof import('./components/state-icon')>()
  return { ...m, StateIcon: (p: Parameters<typeof m.StateIcon>[0]) => (calls.icon++, m.StateIcon(p)) }
})
vi.mock('./components/markdown', async (importOriginal) => {
  const m = await importOriginal<typeof import('./components/markdown')>()
  return { ...m, Markdown: (p: Parameters<typeof m.Markdown>[0]) => (calls.markdown++, m.Markdown(p)) }
})

vi.mock('./components/messages-panel', async (importOriginal) => {
  const m = await importOriginal<typeof import('./components/messages-panel')>()
  return { ...m, MessageCard: (p: Parameters<typeof m.MessageCard>[0]) => (calls.cards.add(p.message.id), m.MessageCard(p)) }
})

// xterm needs a real canvas and measured fonts: the terminal is left out here (the e2e measures it).
vi.mock('./components/terminal-view', () => ({ TerminalView: () => null }))

const noLinks = { take: async () => undefined, onOpen: vi.fn(() => () => {}) }
const REPOS = ['/p/app', '/p/web', '/p/api', '/p/docs', '/p/infra']

/** 5 projects × 2 runs × (a lead and three workers) = 40 agents, each in its own record, as the store holds them. */
function seed() {
  const store = createHostStore()
  const agents = []
  const runs: Record<string, ReturnType<typeof create<typeof RunSchema>>> = {}
  for (const [i, repo] of REPOS.entries())
    for (const j of [0, 1]) {
      const runId = `r${i}${j}`
      runs[runId] = create(RunSchema, { id: runId, title: `run ${i}.${j}`, status: RunStatus.OPEN })
      const a = (id: string, o: object) => create(AgentSchema, { id, name: id, runId, repo, kind: 'claude', model: 'sonnet', createdAt: timestampFromMs(1000), state: AgentState.WORKING, stateDetail: 'Bash', ...o })
      agents.push(a(`l${i}${j}`, { role: AgentRole.ROOT, name: `lead-${i}${j}` }))
      for (const k of [0, 1, 2]) agents.push(a(`w${i}${j}${k}`, { role: AgentRole.WORKER, name: `worker-${i}${j}${k}`, parentId: `l${i}${j}`, state: k === 0 ? AgentState.IDLE : AgentState.WORKING }))
    }
  store.setState({
    conn: { kind: 'ready', hello: { daemonId: 'd', instanceId: 'i', version: 'v', capabilities: [], protocolVersion: 1 } as never },
    synced: true,
    runs,
    agents: Object.fromEntries(agents.map((a) => [a.id, a])),
    daemon: { capabilities: ['terminals'], actor: { class: ActorClass.DEVICE } } as never,
  })
  const entries = Array.from({ length: 300 }, (_, n) =>
    create(TranscriptEntrySchema, n % 3 === 0 ? { id: `e${n}`, kind: TranscriptKind.USER, text: `question ${n}` } : n % 3 === 1 ? { id: `e${n}`, kind: TranscriptKind.ASSISTANT, text: `Answer **${n}** with \`code\`.` } : { id: `e${n}`, kind: TranscriptKind.TOOL_CALL, tool: 'Bash', toolInput: `ls ${n}`, toolUseId: `t${n}` }),
  )
  let tail = entries
  const client = {
    // The host's own reading: the whole tail first, then only what is after the offset the caller holds.
    readTranscript: vi.fn(async (req: { offset: bigint; backward: boolean }) => {
      const from = req.backward || req.offset === 0n ? 0 : Number(req.offset)
      return create(ReadTranscriptResponseSchema, { supported: true, sessionId: 's', startOffset: BigInt(from), endOffset: BigInt(tail.length), atStart: from === 0, entries: tail.slice(from) })
    }),
    listPrompts: vi.fn(async () => ({ prompts: [] })),
    listProjects: vi.fn(async () => create(ListProjectsResponseSchema)),
    getUsage: vi.fn(async () => create(GetUsageResponseSchema)),
    getCost: vi.fn(async () => create(GetCostResponseSchema)),
    getAgentProgress: vi.fn(async () => ({})),
    listAgentCommands: vi.fn(async () => ({ commands: [] })),
    getPromptSuggestion: vi.fn(async () => ({ text: '' })),
  }
  const host = { info: { id: 'h1', name: 'home', kind: 'state-dir', target: '/s' }, store, client, connection: { retryNow: vi.fn(), stop: vi.fn() }, loadMessages: vi.fn() } as unknown as HostRuntime
  const api = { hosts: { list: async () => [], add: vi.fn(), remove: vi.fn(), fixed: false }, request: vi.fn(), links: noLinks, shell: { notify: vi.fn(), setWaiting: vi.fn(), configure: vi.fn(), onNotificationClick: vi.fn(() => () => {}) } } as unknown as DesktopApi
  const model = createAppModel(api)
  model.store.setState({ loaded: true, hosts: [host], hostId: 'h1', agentId: 'l00' })
  const wrap = (ui: ReactNode) => <AppContext.Provider value={model}>{ui}</AppContext.Provider>
  return { host, model, wrap, client, grow: () => (tail = [...tail, create(TranscriptEntrySchema, { id: `e${tail.length}`, kind: TranscriptKind.ASSISTANT, text: 'One more reply.' })]) }
}

/** The cost React reports for the commits after the first. */
function recorder() {
  const rec = { commits: 0, ms: 0, mount: 0 }
  const onRender = (_id: string, phase: string, actual: number) => {
    if (phase === 'mount') rec.mount += actual
    else {
      rec.commits++
      rec.ms += actual
    }
  }
  return { rec, wrap: (ui: ReactNode) => <Profiler id="p" onRender={onRender}>{ui}</Profiler> }
}

/** The main pane as `App` draws it for one open agent. */
function Shown({ host }: { host: HostRuntime }) {
  const id = useAppState((s) => s.agentId)
  return id ? <AgentPanel key={id} host={host} agentId={id} /> : null
}

const report: string[] = []
const note = (name: string, v: Record<string, number | string>) => report.push(`${name.padEnd(46)} ${Object.entries(v).map(([k, x]) => `${k} ${typeof x === 'number' ? Number.isInteger(x) ? x : x.toFixed(2) : x}`).join('  ')}`)
afterAll(() => {
  if (process.env.PERF) process.stdout.write(`\n${report.join('\n')}\n`)
})
beforeEach(() => {
  calls.icon = 0
  calls.markdown = 0
  calls.cards.clear()
})

describe('speed: what a change re-renders', () => {
  it('sidebar: one agent of 40 changing state redraws its own row, not the other 39', async () => {
    const { host, wrap } = seed()
    const p = recorder()
    render(wrap(p.wrap(<ProjectSidebar />)))
    // Every lead unfolded, so all 40 rows are on screen.
    for (const b of screen.getAllByRole('button', { name: /^Show the workers of/ })) fireEvent.click(b)
    expect(document.querySelectorAll('[data-sidebar-row="agent"]')).toHaveLength(40)
    const N = 30
    calls.icon = 0
    p.rec.commits = p.rec.ms = 0
    for (let n = 0; n < N; n++) {
      const id = `w${n % 5}${n % 2}${n % 3}`
      act(() => host.store.setState((s) => ({ agents: { ...s.agents, [id]: { ...s.agents[id]!, state: n % 2 ? AgentState.IDLE : AgentState.WORKING, stateDetail: `step ${n}` } } })))
    }
    note('sidebar, one of 40 agents changes', { 'rows/change': calls.icon / N, 'ms/change': p.rec.ms / N })
    expect.soft(calls.icon / N).toBeLessThanOrEqual(2)
  })

  it('sidebar: a change that is none of the sidebar\'s (the host\'s cursor) redraws nothing', () => {
    const { host, wrap } = seed()
    render(wrap(<ProjectSidebar />))
    calls.icon = 0
    for (let n = 0; n < 10; n++) act(() => host.store.setState({ synced: true, pullRequests: {} }))
    expect.soft(calls.icon).toBe(0)
  })

  it('tab bar: 12 tabs, an agent changing redraws the open project\'s four tabs, about 3 ms, so no memo there', () => {
    const { host, model, wrap } = seed()
    const ids = ['l00', 'l01', 'l10', 'l11', 'l20', 'l21', 'l30', 'l31', 'l40', 'l41', 'w000', 'w001']
    model.store.setState({ tabs: ids.map((agentId) => ({ hostId: 'h1', agentId })) })
    const p = recorder()
    render(wrap(p.wrap(<TabBar />)))
    const N = 30
    calls.icon = 0
    p.rec.commits = p.rec.ms = 0
    for (let n = 0; n < N; n++) act(() => host.store.setState((s) => ({ agents: { ...s.agents, w001: { ...s.agents.w001!, stateDetail: `step ${n}` } } })))
    note('tab bar, one of 12 tabs changes', { 'tabs/change': calls.icon / N, 'ms/change': p.rec.ms / N })
    expect.soft(calls.icon / N).toBeLessThanOrEqual(4)
    calls.icon = 0
    p.rec.commits = p.rec.ms = 0
    for (let n = 0; n < N; n++) act(() => model.selectAgent('h1', ids[n % ids.length]!))
    note('tab bar, a tab switch', { 'tabs/switch': calls.icon / N, 'ms/switch': p.rec.ms / N })
    expect.soft(calls.icon / N).toBeLessThanOrEqual(4)
  })

  it('side panel: the open run\'s tree redraws for its own agent changing, not for another run\'s', () => {
    const { host, wrap } = seed()
    const p = recorder()
    render(wrap(p.wrap(<SidePanel host={host} agentId="l00" />)))
    const change = (id: string, n: number) => act(() => host.store.setState((s) => ({ agents: { ...s.agents, [id]: { ...s.agents[id]!, stateDetail: `step ${n}` } } })))
    const N = 30
    calls.icon = 0
    p.rec.commits = p.rec.ms = 0
    for (let n = 0; n < N; n++) change('w001', n) // the open run's
    note('side panel, an agent of the open run changes', { 'rows/change': calls.icon / N, 'ms/change': p.rec.ms / N })
    expect.soft(calls.icon / N).toBeLessThanOrEqual(1)
    calls.icon = 0
    p.rec.commits = p.rec.ms = 0
    for (let n = 0; n < N; n++) change('w401', n) // another run's
    note('side panel, an agent of another run changes', { 'rows/change': calls.icon / N, 'ms/change': p.rec.ms / N, commits: p.rec.commits })
    expect.soft(calls.icon).toBe(0)
  })

  it('chat: a reply arriving in a 300-entry conversation draws the new block, not the 300', async () => {
    const { host, wrap, grow } = seed()
    const p = recorder()
    host.store.setState((s) => ({ agents: { ...s.agents, l00: { ...s.agents.l00!, state: AgentState.IDLE, terminalId: 't', sessionId: 's' } } }))
    render(wrap(p.wrap(<ChatView host={host} agentId="l00" active />)))
    await screen.findByRole('textbox', { name: 'Prompt' })
    await waitFor(() => expect(calls.markdown).toBeGreaterThan(20))
    const first = calls.markdown
    calls.markdown = 0
    p.rec.commits = p.rec.ms = 0
    grow()
    act(() => host.store.setState((s) => ({ agents: { ...s.agents, l00: { ...s.agents.l00!, stateDetail: 'x' } } })))
    await waitFor(() => expect(screen.getAllByText('One more reply.').length).toBeGreaterThan(0), { timeout: 3000 })
    note('chat, 300 entries, one more arrives', { 'first draw (markdown)': first, 'markdown drawn': calls.markdown, ms: p.rec.ms, 'mount ms': p.rec.mount })
    expect.soft(first).toBeLessThanOrEqual(80) // the newest 60 blocks, not the whole page
    expect.soft(calls.markdown).toBeLessThanOrEqual(3)
  })

  it('a tab switch to a 300-entry chat: what drawing it costs', async () => {
    const { host, model, wrap } = seed()
    host.store.setState((s) => ({ agents: { ...s.agents, l00: { ...s.agents.l00!, terminalId: 't', sessionId: 's' }, l01: { ...s.agents.l01!, terminalId: 't', sessionId: 's' } } }))
    const p = recorder()
    render(wrap(p.wrap(<><TabBar /><Shown host={host} /></>)))
    model.store.setState({ tabs: [{ hostId: 'h1', agentId: 'l00' }, { hostId: 'h1', agentId: 'l01' }], side: [], agentId: undefined })
    const t0 = performance.now()
    act(() => model.selectAgent('h1', 'l00'))
    await waitFor(() => expect(calls.markdown).toBeGreaterThan(20))
    note('tab switch to a 300-entry chat', { 'wall ms': performance.now() - t0, 'render ms': p.rec.ms + p.rec.mount, markdown: calls.markdown })
    expect.soft(calls.markdown).toBeLessThanOrEqual(80)
    expect(screen.getByRole('textbox', { name: 'Prompt' })).toBeInTheDocument()
  })

  it('opening a big run\'s lead: its thousand mesh cards are windowed before its transcript arrives, not drawn whole', async () => {
    const { host, wrap, client } = seed()
    const messages = Object.fromEntries(Array.from({ length: 1000 }, (_, n) => {
      const m = create(MessageSchema, { id: `m${n}`, runId: 'r00', type: MessageType.INFO, body: `note ${n}`, toId: n % 2 ? 'l00' : 'w000', from: { id: n % 2 ? 'w000' : 'l00' }, createdAt: timestampFromMs(1000 + n) })
      return [m.id, m]
    }))
    host.store.setState((s) => ({ messages, messageLoad: { ...s.messageLoad, r00: 'loaded' }, agents: { ...s.agents, l00: { ...s.agents.l00!, terminalId: 't', sessionId: 's' } } }))
    // The transcript is still on its way: the cards are all there is to draw.
    client.readTranscript.mockImplementation(() => new Promise(() => {}))
    const t0 = performance.now()
    render(wrap(<ChatView host={host} agentId="l00" active />))
    await screen.findByRole('textbox', { name: 'Prompt' })
    note('open a lead with 1000 cards, transcript pending', { cards: calls.cards.size, 'wall ms': performance.now() - t0 })
    // The newest 60 only; it was all 1000.
    expect(calls.cards.size).toBe(60)
    expect(calls.cards.has('m999')).toBe(true)
  })
})
