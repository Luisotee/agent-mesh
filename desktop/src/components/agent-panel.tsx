import { MessageSquare, SquareTerminal } from 'lucide-react'
import { useState } from 'react'
import { sendMessage, stopAgent } from '@/actions'
import { Button } from '@/components/ui/button'
import { Chip } from '@/components/ui/chip'
import { ConfirmDialog } from '@/components/ui/confirm'
import { Dialog, DialogContent } from '@/components/ui/dialog'
import { Select } from '@/components/ui/input'
import { MarkdownField } from '@/components/ui/markdown-field'
import { MoreButton, type MenuEntry } from '@/components/ui/menu'
import { Tip } from '@/components/ui/tooltip'
import { useApp, useAppState, useHostState } from '@/context'
import { ROLE_LABEL, statusSentence } from '@/derive'
import { AgentState, MessageType, type Agent } from '@/gen/agentmesh/v1/types_pb'
import { useKeys } from '@/hooks'
import { displayChord } from '@/keys'
import type { AgentTab, HostRuntime } from '@/model'
import { StateIcon } from './state-icon'
import { VendorIcon } from './vendor-icon'
import { ChatView } from './chat-view'
import { TerminalView } from './terminal-view'
import { rewindUnsupported } from '@/attachments'

const live = (a: Agent) => a.state !== AgentState.EXITED && a.state !== AgentState.RETIRED

/**
 * One agent: its chat, or its own terminal in its place (the CLI toggle, kept for its prompts and dialogs). The header
 * is one line: its state, name and model, the toggle, and a menu (send a message, details, stop). What the overview
 * listed is the side panel's Details now.
 */
export function AgentPanel({ host, agentId, view }: { host: HostRuntime; agentId: string; /** The view and how to change it, for a pane beside the main one (the main pane's is the app's). */ view?: { tab: AgentTab; setTab(t: AgentTab): void } }) {
  const agent = useHostState(host, (s) => s.agents[agentId])
  const synced = useHostState(host, (s) => s.synced)
  const canTerminal = useHostState(host, (s) => !!s.daemon?.capabilities.includes('terminals'))
  const app = useApp()
  const mainTab = useAppState((s) => s.tab)
  const hosts = useAppState((s) => s.hosts)
  const keys = useKeys(hosts)
  const [stopping, setStopping] = useState(false)
  const tab = view?.tab ?? mainTab
  const setTab = (t: AgentTab) => (view ? view.setTab(t) : app.setTab(t))
  // A restored tab shows before the host's first snapshot: its agent is not missing, only not here yet.
  if (!agent) return <Empty>{synced ? "That agent isn't on this host any more." : 'Connecting to the host…'}</Empty>
  const hasTerminal = canTerminal && !!agent.terminalId
  // The terminal, when the agent has one; else the chat (its terminal went away).
  const shown: AgentTab = tab === 'terminal' && hasTerminal ? 'terminal' : 'chat'
  const toggleKey = keys.chat_toggle ? ` (${displayChord(keys.chat_toggle)})` : ''
  // The side panel shows the main pane's agent: a pane beside it takes the main pane first.
  const toMain = () => view && app.focusPane({ hostId: host.info.id, agentId: agent.id })
  const showModel = () => {
    toMain()
    app.showModel()
  }
  const entries: MenuEntry[] = [
    { label: 'Send a message…', onSelect: () => app.openMessage({ hostId: host.info.id, agentId: agent.id }) },
    {
      label: 'Details',
      onSelect: () => {
        toMain()
        app.setPanel(true)
      },
    },
    { label: 'Model…', onSelect: showModel },
    {
      label: 'Rewind…',
      // Back to before one of its prompts: Claude Code only, the conversation only.
      onSelect: () => {
        app.setTab('chat')
        if (view) view.setTab('chat')
        app.openRewind({ hostId: host.info.id, agentId: agent.id })
      },
      disabled: !!rewindUnsupported(agent.kind) || !synced,
      hint: 'Esc Esc',
    },
    { label: 'Stop…', onSelect: () => setStopping(true), disabled: !live(agent), destructive: true },
  ]
  return (
    <article aria-label={agent.name} className="flex min-h-0 flex-1 flex-col gap-2 p-3 pt-2">
      <header className="flex min-w-0 items-center gap-2">
        <StateIcon agent={agent} className="size-4 shrink-0" />
        <h1 className="truncate text-[14px] font-semibold">{agent.name}</h1>
        <Chip>{ROLE_LABEL[agent.role]}</Chip>
        <Tip label="Change its model">
          <button type="button" aria-label={`Model: ${agent.model || agent.kind}`} onClick={showModel} className="flex min-w-0 items-center gap-1 rounded-md border px-1.5 py-0.5 text-xs text-muted-foreground outline-none hover:bg-muted hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring">
            <VendorIcon kind={agent.kind} model={agent.model} className="size-3.5" />
            <span className="truncate">{[agent.model || agent.kind, agent.effort].filter(Boolean).join(' ')}</span>
          </button>
        </Tip>
        <p className="min-w-0 flex-1 truncate text-xs text-muted-foreground" data-testid="agent-status">{statusSentence(agent)}</p>
        {hasTerminal && (
          <Tip label={shown === 'chat' ? `Show its terminal${toggleKey}` : `Back to the chat${toggleKey}`}>
            <Button size="sm" variant="outline" aria-pressed={shown === 'terminal'} aria-label="Terminal" onClick={() => setTab(shown === 'chat' ? 'terminal' : 'chat')}>
              {shown === 'chat' ? <SquareTerminal /> : <MessageSquare />} {shown === 'chat' ? 'Terminal' : 'Chat'}
            </Button>
          </Tip>
        )}
        <MoreButton label={`Menu of ${agent.name}`} entries={entries} />
      </header>
      <ChatView host={host} agentId={agent.id} active={shown === 'chat'} onTerminal={() => setTab('terminal')} />
      {hasTerminal && <TerminalView host={host} agentId={agent.id} active={shown === 'terminal'} />}
      <ConfirmDialog
        open={stopping}
        onOpenChange={setStopping}
        title={`Stop ${agent.name}?`}
        description="Its CLI exits. A task or an answer to it later brings it back in the same conversation."
        action={`Stop ${agent.name}`}
        run={() => stopAgent(host.client, agent.id)}
      />
    </article>
  )
}

function Empty({ children }: { children: React.ReactNode }) {
  return <div className="flex flex-1 items-center justify-center p-8 text-sm text-muted-foreground">{children}</div>
}

const SEND_TYPES: Array<[MessageType, string]> = [
  [MessageType.TASK, 'Task'],
  [MessageType.ANSWER, 'Answer'],
  [MessageType.INFO, 'Info'],
]

/**
 * A mesh message to an agent (a task, an answer, an info), as another agent would send it: unlike the chat's prompt it
 * reaches a stopped agent too, which it brings back. Opened from an agent's menu.
 */
export function SendMessageDialog() {
  const app = useApp()
  const to = useAppState((s) => s.messageTo)
  const host = useAppState((s) => s.hosts.find((h) => h.info.id === to?.hostId))
  const close = () => app.openMessage(undefined)
  return (
    <Dialog open={!!to && !!host} onOpenChange={(o) => !o && close()}>
      {to && host && <SendForm key={`${to.hostId}/${to.agentId}`} host={host} agentId={to.agentId} onDone={close} />}
    </Dialog>
  )
}

function SendForm({ host, agentId, onDone }: { host: HostRuntime; agentId: string; onDone: () => void }) {
  const agent = useHostState(host, (s) => s.agents[agentId])
  const [type, setType] = useState<MessageType>(MessageType.TASK)
  const [body, setBody] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [sent, setSent] = useState(false)
  async function submit() {
    // Each send has its own mutation id, so a double Enter would send twice: one at a time.
    if (busy || !body.trim()) return
    setBusy(true)
    setError('')
    try {
      const res = await sendMessage(host.client, agentId, type, body.trim())
      // Stored either way; a stopped agent the host couldn't start again (a cap, a closed run) says why, and the dialog stays.
      if (res.resumeError) {
        // It is stored: the body goes, so a second Send can't store it twice.
        setBody('')
        setSent(true)
        setError(`Sent, but it couldn't be brought back: ${res.resumeError}`)
      } else onDone()
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <DialogContent title={`Message ${agent?.name ?? agentId}`} description="A mesh message, as an agent sends one: it reaches the agent at its next turn. A task or an answer brings a stopped one back; an info waits.">
      <form
        aria-label="Send a message"
        className="flex flex-col gap-2"
        onSubmit={(e) => {
          e.preventDefault()
          void submit()
        }}
      >
        <Select aria-label="Message type" className="w-28" value={type} onChange={(e) => setType(Number(e.target.value))}>
          {SEND_TYPES.map(([v, l]) => <option key={v} value={v}>{l}</option>)}
        </Select>
        <MarkdownField aria-label="Message" autoFocus className="min-h-20" value={body} onValueChange={setBody} placeholder="Write to the agent" />
        {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={onDone}>{sent ? 'Close' : 'Cancel'}</Button>
          <Button type="submit" disabled={busy || !body.trim()}>Send</Button>
        </div>
      </form>
    </DialogContent>
  )
}
