import { useState } from 'react'
import { sendMessage } from '@/actions'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent } from '@/components/ui/dialog'
import { Select } from '@/components/ui/input'
import { MarkdownField } from '@/components/ui/markdown-field'
import { useApp, useAppState, useHostState } from '@/context'
import { AgentRole, MessageType } from '@/gen/agentmesh/v1/types_pb'
import type { AgentTab, HostRuntime } from '@/model'
import { ChatView } from './chat-view'
import { TerminalView } from './terminal-view'
import { ShellPanel } from './shell-panel'
import { viewShown } from './view-switch'
import { isShellTab } from '@/shells'

/**
 * One agent: its chat, or its own CLI in its place (the Chat | CLI switch of the tab bar, or of a split pane's title row,
 * kept for the prompts and dialogs only the CLI shows). It has no header of its own: its name and state are on its tab,
 * its status under the conversation, its menu on the tab; what the overview listed is the side panel's Details.
 */
export function AgentPanel(props: { host: HostRuntime; agentId: string; view?: { tab: AgentTab; setTab(t: AgentTab): void } }) {
  // A project shell's tab is shown by the same panes as an agent's.
  return isShellTab(props.agentId) ? <ShellPanel host={props.host} agentId={props.agentId} /> : <AgentBody {...props} />
}

function AgentBody({ host, agentId, view }: { host: HostRuntime; agentId: string; /** The view and how to change it, for a pane beside the main one (the main pane's is the app's). */ view?: { tab: AgentTab; setTab(t: AgentTab): void } }) {
  const agent = useHostState(host, (s) => s.agents[agentId])
  const synced = useHostState(host, (s) => s.synced)
  const canTerminal = useHostState(host, (s) => !!s.daemon?.capabilities.includes('terminals'))
  const app = useApp()
  const mainTab = useAppState((s) => s.tab)
  const tab = view?.tab ?? mainTab
  const setTab = (t: AgentTab) => (view ? view.setTab(t) : app.setTab(t))
  // A restored tab shows before the host's first snapshot: its agent is not missing, only not here yet.
  if (!agent) return <Empty>{synced ? "That agent isn't on this host any more." : 'Connecting to the host…'}</Empty>
  const hasTerminal = canTerminal && !!agent.terminalId
  // The terminal, when the agent has one; else the chat (its terminal went away).
  const shown = viewShown(tab, hasTerminal)
  return (
    <article aria-label={agent.name} className="flex min-h-0 flex-1 flex-col gap-2 p-3">
      <ChatView host={host} agentId={agent.id} active={shown === 'chat'} onTerminal={() => setTab('terminal')} />
      {hasTerminal && <TerminalView host={host} agentId={agent.id} active={shown === 'terminal'} />}
    </article>
  )
}

function Empty({ children }: { children: React.ReactNode }) {
  return <div className="flex flex-1 items-center justify-center p-8 text-sm text-muted-foreground">{children}</div>
}

/** The tiers a re-plan can name (`tiers.<cli>.<tier>`). */
const PLAN_TIERS = ['light', 'mid', 'strong', 'top']

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
  const [plan, setPlan] = useState(false)
  const [tier, setTier] = useState('')
  // A root, an asker and a reviewer have no plan phase to go back to.
  const canPlan = type === MessageType.TASK && agent?.role === AgentRole.WORKER
  const planning = plan && canPlan
  async function submit() {
    // Each send has its own mutation id, so a double Enter would send twice: one at a time.
    if (busy || !body.trim()) return
    setBusy(true)
    setError('')
    try {
      const res = await sendMessage(host.client, agentId, type, body.trim(), planning ? { tier } : undefined)
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
    <DialogContent title={`Message ${agent?.name ?? agentId}`} description="A mesh message, as an agent sends one: it reaches the agent at its next turn. A task or an answer brings a stopped one back; an info waits. A task can send the agent back to planning first.">
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
        {canPlan && (
          <div className="flex items-center gap-3">
            <label className="flex items-center gap-1.5 text-xs whitespace-nowrap">
              <input type="checkbox" checked={plan} onChange={(e) => setPlan(e.target.checked)} /> Plan first
            </label>
            {plan && (
              <Select aria-label="Planning tier" className="h-7 w-40 text-xs" value={tier} onChange={(e) => setTier(e.target.value)}>
                <option value="">The tier it planned on</option>
                {PLAN_TIERS.map((t) => <option key={t} value={t}>{t}</option>)}
              </Select>
            )}
          </div>
        )}
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
