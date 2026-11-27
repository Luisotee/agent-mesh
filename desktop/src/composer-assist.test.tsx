import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { DesktopApi } from '../shared/api'
import { ChatView } from './components/chat-view'
import { forgetCommands, suggestionTiming } from './components/composer-assist'
import { AppContext } from './context'
import { ActorClass } from './gen/agentmesh/v1/actor_pb'
import { AgentCommandSchema, CommandDialog, ListAgentCommandsResponseSchema, ReadTranscriptResponseSchema } from './gen/agentmesh/v1/chat_pb'
import { GetPromptSuggestionResponseSchema } from './gen/agentmesh/v1/chat_pb'
import { AgentRole, AgentSchema, AgentState } from './gen/agentmesh/v1/types_pb'
import { createAppModel, type HostRuntime } from './model'
import { createHostStore } from './store'

const cmd = (name: string, o: Partial<{ aliases: string[]; description: string; dialog: CommandDialog; danger: boolean; argumentHint: string; source: string }> = {}) => create(AgentCommandSchema, { name, source: 'builtin', ...o })
const COMMANDS = [
  cmd('compact', { description: 'Free up context by summarizing the conversation so far', argumentHint: '<focus>' }),
  cmd('config', { description: 'Open settings', dialog: CommandDialog.BARE }),
  cmd('context', { description: 'Show what fills the context' }),
  cmd('resume', { description: 'Resume a previous session', dialog: CommandDialog.ALWAYS }),
  cmd('exit', { aliases: ['quit'], description: 'Exit the CLI', danger: true }),
  cmd('deploy', { description: 'Ship it', source: 'project', argumentHint: '[env]' }),
]

function chat(o: { kind?: string; suggestion?: string; commands?: typeof COMMANDS; state?: AgentState } = {}) {
  const store = createHostStore()
  const agent = create(AgentSchema, { id: 'w1', name: 'fix', runId: 'r1', repo: '/p/app', kind: o.kind ?? 'claude', role: AgentRole.WORKER, createdAt: timestampFromMs(1000), state: o.state ?? AgentState.IDLE, terminalId: 't', sessionId: 's1' })
  store.setState({
    synced: true,
    agents: { w1: agent },
    daemon: { capabilities: ['terminals'], actor: { class: ActorClass.DEVICE } } as never,
    conn: { kind: 'ready', hello: {} as never },
  })
  const promptAgent = vi.fn(async (_req: unknown): Promise<object> => ({}))
  const client = {
    readTranscript: vi.fn(async () => create(ReadTranscriptResponseSchema, { supported: true, sessionId: 's1', atStart: true })),
    listPrompts: vi.fn(async () => ({ prompts: [] })),
    listAgentCommands: vi.fn(async (_req: unknown) => create(ListAgentCommandsResponseSchema, { supported: true, commands: o.commands ?? COMMANDS })),
    getPromptSuggestion: vi.fn(async (_req: unknown) => create(GetPromptSuggestionResponseSchema, { text: o.suggestion ?? '' })),
    promptAgent,
  }
  const host = { info: { id: 'h1', name: 'home', kind: 'state-dir', target: '/s' }, store, client, connection: { retryNow: vi.fn(), stop: vi.fn() }, loadMessages: vi.fn() } as unknown as HostRuntime
  const api = { hosts: { list: async () => [], add: vi.fn(), remove: vi.fn(), fixed: false }, request: vi.fn(), links: { take: async () => undefined, onOpen: vi.fn(() => () => {}) }, shell: { notify: vi.fn(), setWaiting: vi.fn(), configure: vi.fn(), onNotificationClick: vi.fn(() => () => {}) } } as unknown as DesktopApi
  const model = createAppModel(api)
  model.store.setState({ loaded: true, hosts: [host], hostId: 'h1' })
  const onTerminal = vi.fn()
  const ui = (
    <AppContext.Provider value={model}>
      <ChatView host={host} agentId="w1" active onTerminal={onTerminal} />
    </AppContext.Provider>
  )
  return { ui, client, promptAgent, onTerminal, store }
}

const field = () => screen.getByRole('textbox', { name: 'Prompt' }) as HTMLTextAreaElement
const popup = () => screen.queryByRole('listbox', { name: 'Commands' })
const names = () => within(popup()!).getAllByRole('option').map((o) => o.textContent?.match(/^\/([a-z:-]+)/)?.[1])

beforeEach(() => {
  forgetCommands()
  localStorage.clear()
  suggestionTiming.readsMs = [0]
})

describe('the composer\'s / popup', () => {
  it('opens on a slash with each command\'s hint and description, filters as the user types and closes on Escape', async () => {
    const { ui, client } = chat()
    render(ui)
    await waitFor(() => expect(client.listAgentCommands).toHaveBeenCalledWith({ agent: 'w1' }))
    await userEvent.type(field(), '/')
    await waitFor(() => expect(popup()).not.toBeNull())
    expect(names()).toEqual(['compact', 'config', 'context', 'deploy', 'exit', 'resume'])
    const compact = within(popup()!).getAllByRole('option')[0]!
    expect(compact).toHaveTextContent('<focus>')
    expect(compact).toHaveTextContent('Free up context by summarizing')
    expect(within(popup()!).getAllByRole('option')[3]).toHaveTextContent('project')
    await userEvent.type(field(), 'con')
    expect(names()).toEqual(['config', 'context'])
    await userEvent.keyboard('{Escape}')
    expect(popup()).toBeNull()
    expect(field()).toHaveValue('/con')
  })

  it('completes the highlighted command with Tab, or with Enter before the name is whole, and sends it on the second Enter', async () => {
    const { ui, promptAgent } = chat()
    render(ui)
    await userEvent.type(field(), '/con')
    await waitFor(() => expect(popup()).not.toBeNull())
    await userEvent.keyboard('{ArrowDown}')
    expect(within(popup()!).getAllByRole('option')[1]).toHaveAttribute('aria-selected', 'true')
    await userEvent.keyboard('{Enter}')
    expect(field()).toHaveValue('/context ')
    expect(promptAgent).not.toHaveBeenCalled()
    expect(popup()).toBeNull()
    await userEvent.keyboard('{Enter}')
    await waitFor(() => expect(promptAgent).toHaveBeenCalledWith(expect.objectContaining({ agent: 'w1', text: '/context' })))
  })

  it('picks with a click and keeps the arguments typed after the name', async () => {
    const { ui } = chat()
    render(ui)
    await userEvent.type(field(), '/comp')
    await waitFor(() => expect(popup()).not.toBeNull())
    await userEvent.click(within(popup()!).getByRole('option', { name: /compact/ }))
    expect(field()).toHaveValue('/compact ')
    await userEvent.type(field(), 'keep the plan')
    expect(popup()).toBeNull() // arguments: the popup is shut
  })

  it('is no command for a path, an unknown word or a slash mid-text: no popup, sent as typed', async () => {
    const { ui, promptAgent } = chat()
    render(ui)
    await userEvent.type(field(), '/etc/hosts is wrong')
    expect(popup()).toBeNull()
    await userEvent.keyboard('{Enter}')
    await waitFor(() => expect(promptAgent).toHaveBeenCalledWith(expect.objectContaining({ text: '/etc/hosts is wrong' })))
  })

  it('opens the terminal after a command typed at once that opens a dialog there, not after one that runs', async () => {
    const { ui, promptAgent, onTerminal } = chat()
    render(ui)
    await userEvent.type(field(), '/context{Enter}')
    await waitFor(() => expect(promptAgent).toHaveBeenCalledTimes(1))
    expect(onTerminal).not.toHaveBeenCalled()
    await userEvent.type(field(), '/config{Enter}') // bare: a picker
    await waitFor(() => expect(onTerminal).toHaveBeenCalledTimes(1))
    await userEvent.type(field(), '/config foo.bar=1{Enter}') // with an argument it just runs
    await waitFor(() => expect(promptAgent).toHaveBeenCalledTimes(3))
    expect(onTerminal).toHaveBeenCalledTimes(1)
    await userEvent.type(field(), '/resume{Enter}')
    await waitFor(() => expect(onTerminal).toHaveBeenCalledTimes(2))
  })

  it('does not take the user to the terminal when the command was queued behind a turn', async () => {
    const { ui, promptAgent, onTerminal } = chat({ state: AgentState.WORKING })
    promptAgent.mockResolvedValue({ queued: { id: 'q1', text: '/resume', createdAt: timestampFromMs(1), held: 'working' } })
    render(ui)
    await userEvent.type(field(), '/resume{Enter}')
    await waitFor(() => expect(promptAgent).toHaveBeenCalled())
    expect(onTerminal).not.toHaveBeenCalled()
  })

  it('asks before a command that ends the session, and sends nothing on Cancel', async () => {
    const { ui, promptAgent } = chat()
    render(ui)
    await userEvent.type(field(), '/quit{Enter}')
    const dialog = await screen.findByRole('dialog')
    expect(dialog).toHaveTextContent(/\/quit ends or signs out/)
    expect(promptAgent).not.toHaveBeenCalled()
    await userEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    expect(promptAgent).not.toHaveBeenCalled()
    await userEvent.keyboard('{Enter}')
    await userEvent.click(await screen.findByRole('button', { name: 'Send' }))
    await waitFor(() => expect(promptAgent).toHaveBeenCalledWith(expect.objectContaining({ text: '/quit' })))
  })

  it('stays out of the way while the host has no commands for this CLI', async () => {
    const { ui, client } = chat({ kind: 'agy' })
    client.listAgentCommands.mockResolvedValue(create(ListAgentCommandsResponseSchema, { supported: false }))
    render(ui)
    await userEvent.type(field(), '/co')
    expect(popup()).toBeNull()
  })
})

describe('the composer\'s suggested prompt', () => {
  it('shows what Claude suggests as the box\'s grey text, and Tab takes it', async () => {
    const { ui, client } = chat({ suggestion: 'run it' })
    render(ui)
    await waitFor(() => expect(field()).toHaveAttribute('placeholder', 'run it'))
    expect(client.getPromptSuggestion).toHaveBeenCalledWith({ agent: 'w1' })
    await userEvent.click(field())
    await userEvent.keyboard('{Tab}')
    expect(field()).toHaveValue('run it')
  })

  it('drops with Escape, and with the first thing typed', async () => {
    const { ui } = chat({ suggestion: 'run it' })
    render(ui)
    await waitFor(() => expect(field()).toHaveAttribute('placeholder', 'run it'))
    await userEvent.click(field())
    await userEvent.keyboard('{Escape}')
    expect(field().getAttribute('placeholder')).not.toBe('run it')
  })

  it('shows the same words again once the agent has worked and is idle again', async () => {
    const { ui, store } = chat({ suggestion: 'run it' })
    render(ui)
    await waitFor(() => expect(field()).toHaveAttribute('placeholder', 'run it'))
    await userEvent.click(field())
    await userEvent.keyboard('{Escape}')
    expect(field().getAttribute('placeholder')).not.toBe('run it')
    act(() => store.setState((s) => ({ agents: { ...s.agents, w1: { ...s.agents.w1!, state: AgentState.WORKING } as never } })))
    act(() => store.setState((s) => ({ agents: { ...s.agents, w1: { ...s.agents.w1!, state: AgentState.IDLE } as never } })))
    await waitFor(() => expect(field()).toHaveAttribute('placeholder', 'run it'))
  })

  it('is asked of Claude only, and only while the agent is at its prompt', async () => {
    const other = chat({ kind: 'agy', suggestion: 'x' })
    const { unmount } = render(other.ui)
    await new Promise((r) => setTimeout(r, 30))
    expect(other.client.getPromptSuggestion).not.toHaveBeenCalled()
    unmount()
    const busy = chat({ state: AgentState.WORKING, suggestion: 'x' })
    render(busy.ui)
    await new Promise((r) => setTimeout(r, 30))
    expect(busy.client.getPromptSuggestion).not.toHaveBeenCalled()
  })

  it('reads again after a miss, as the CLI makes its suggestion a moment after the turn', async () => {
    suggestionTiming.readsMs = [0, 10]
    const { ui, client } = chat()
    client.getPromptSuggestion.mockResolvedValueOnce(create(GetPromptSuggestionResponseSchema, { text: '' })).mockResolvedValue(create(GetPromptSuggestionResponseSchema, { text: 'ship it' }))
    render(ui)
    await waitFor(() => expect(field()).toHaveAttribute('placeholder', 'ship it'))
    expect(client.getPromptSuggestion).toHaveBeenCalledTimes(2)
  })
})
