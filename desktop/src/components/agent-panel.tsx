import { useState } from 'react'
import { sendMessage, stopAgent } from '@/actions'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input, Select } from '@/components/ui/input'
import { useApp, useAppState, useHostState } from '@/context'
import { ROLE_LABEL, statusSentence } from '@/derive'
import { AgentState, MessageType, type Agent } from '@/gen/agentmesh/v1/types_pb'
import type { AgentTab, HostRuntime } from '@/model'
import { StateIcon } from './state-icon'
import { ModelControl } from './model-control'
import { ChatView } from './chat-view'
import { MessagesPanel } from './messages-panel'
import { TerminalView } from './terminal-view'

const SEND_TYPES: Array<[MessageType, string]> = [
  [MessageType.TASK, 'Task'],
  [MessageType.ANSWER, 'Answer'],
  [MessageType.INFO, 'Info'],
]

function when(t: Agent['lastActivity']): string {
  return t ? new Date(Number(t.seconds) * 1000).toLocaleString() : 'never'
}

/** One agent: where it is, what it is doing, and the actions a person has on it (message it, stop it). */
export function AgentPanel({ host, agentId, view }: { host: HostRuntime; agentId: string; /** The view and how to change it, for a pane beside the main one (the main pane's is the app's). */ view?: { tab: AgentTab; setTab(t: AgentTab): void } }) {
  const agent = useHostState(host, (s) => s.agents[agentId])
  const synced = useHostState(host, (s) => s.synced)
  const canTerminal = useHostState(host, (s) => !!s.daemon?.capabilities.includes('terminals'))
  const app = useApp()
  const mainTab = useAppState((s) => s.tab)
  const tab = view?.tab ?? mainTab
  const open = useHostState(host, (s) => s.waiting.filter((w) => w.message && (w.agentId === agentId || w.message.toId === agentId)).length)
  const parent = useHostState(host, (s) => (agent?.parentId ? s.agents[agent.parentId] : undefined))
  // A restored tab shows before the host's first snapshot: its agent is not missing, only not here yet.
  if (!agent) return <Empty>{synced ? "That agent isn't on this host any more." : 'Connecting to the host…'}</Empty>
  const facts: Array<[string, string]> = [
    ['Repository', agent.repo],
    ['Worktree', agent.worktree],
    ['Branch', agent.branch],
    ['CLI', [agent.kind, agent.model, agent.effort].filter(Boolean).join(' · ')],
    ['Parent', parent ? parent.name : agent.parentId ? agent.parentId : 'started by you'],
    ['Last activity', when(agent.lastActivity)],
    ['Session', agent.sessionId || 'none yet'],
    ['Id', agent.id],
  ]
  const hasTerminal = canTerminal && !!agent.terminalId
  // The chat reads the CLI's record, so it is there for a stopped agent too; typing needs the terminal.
  const tabs: AgentTab[] = hasTerminal ? ['overview', 'messages', 'chat', 'terminal'] : ['overview', 'messages', 'chat']
  // A tab the agent doesn't have (its terminal went away) falls back to the overview.
  const shown = tabs.includes(tab) ? tab : 'overview'
  return (
    <article aria-label={agent.name} className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-6">
      <header className="flex items-start gap-3">
        <span className="mt-1"><StateIcon agent={agent} className="size-5" /></span>
        <div className="min-w-0 flex-1">
          <h1 className="flex items-center gap-2 text-base font-semibold">
            {agent.name} <Badge>{ROLE_LABEL[agent.role]}</Badge>
          </h1>
          <p className="text-sm text-muted-foreground" data-testid="agent-status">{statusSentence(agent)}</p>
          <ModelControl host={host} agent={agent} className="mt-0.5" />
        </div>
        <StopButton host={host} agent={agent} />
      </header>
      <div role="tablist" aria-label="Agent views" className="flex gap-1 border-b">
        {tabs.map((t) => (
          <button
            key={t}
            id={`${agent.id}-tab-${t}`}
            type="button"
            role="tab"
            aria-selected={shown === t}
            aria-controls={`${agent.id}-panel-${t}`}
            onClick={() => (view ? view.setTab(t) : app.setTab(t))}
            className={`-mb-px flex items-center gap-1.5 border-b-2 px-3 py-1.5 text-[13px] capitalize ${shown === t ? 'border-primary text-foreground' : 'border-transparent text-muted-foreground hover:text-foreground'}`}
          >
            {t}
            {t === 'messages' && open > 0 && <span className="rounded-full bg-state-needs px-1.5 text-[10px] leading-4 font-bold text-primary-foreground">{open}</span>}
          </button>
        ))}
      </div>
      <div id={`${agent.id}-panel-overview`} role="tabpanel" aria-labelledby={`${agent.id}-tab-overview`} className="flex flex-col gap-5" hidden={shown !== 'overview'}>
        <dl className="grid grid-cols-[8rem_1fr] gap-x-4 gap-y-1.5 text-[13px]">
          {facts.map(([k, v]) => (
            <div key={k} className="contents">
              <dt className="text-muted-foreground">{k}</dt>
              <dd className="font-mono text-xs break-all">{v}</dd>
            </div>
          ))}
        </dl>
        <SendForm host={host} agent={agent} />
      </div>
      <div id={`${agent.id}-panel-messages`} role="tabpanel" aria-labelledby={`${agent.id}-tab-messages`} hidden={shown !== 'messages'}>
        <MessagesPanel host={host} agentId={agent.id} active={shown === 'messages'} />
      </div>
      <ChatView host={host} agentId={agent.id} active={shown === 'chat'} panelId={`${agent.id}-panel-chat`} tabId={`${agent.id}-tab-chat`} />
      {hasTerminal && <TerminalView host={host} agentId={agent.id} active={shown === 'terminal'} panelId={`${agent.id}-panel-terminal`} tabId={`${agent.id}-tab-terminal`} />}
    </article>
  )
}

function Empty({ children }: { children: React.ReactNode }) {
  return <div className="flex flex-1 items-center justify-center p-8 text-sm text-muted-foreground">{children}</div>
}

function StopButton({ host, agent }: { host: HostRuntime; agent: Agent }) {
  const [confirm, setConfirm] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  if (agent.state === AgentState.EXITED || agent.state === AgentState.RETIRED) return null
  async function stop() {
    if (busy) return
    setBusy(true)
    setError('')
    try {
      await stopAgent(host.client, agent.id)
      setConfirm(false)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="flex flex-col items-end gap-1">
      {confirm ? (
        <div className="flex gap-2">
          <Button size="sm" variant="ghost" onClick={() => setConfirm(false)}>Keep running</Button>
          <Button size="sm" variant="destructive" disabled={busy} onClick={stop}>Stop {agent.name}</Button>
        </div>
      ) : (
        <Button size="sm" variant="outline" onClick={() => setConfirm(true)}>Stop agent</Button>
      )}
      {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
    </div>
  )
}

function SendForm({ host, agent }: { host: HostRuntime; agent: Agent }) {
  const [type, setType] = useState<MessageType>(MessageType.TASK)
  const [body, setBody] = useState('')
  const [status, setStatus] = useState<{ ok: boolean; text: string }>()
  const [busy, setBusy] = useState(false)
  const dead = agent.state === AgentState.EXITED
  async function submit(e: React.FormEvent) {
    e.preventDefault()
    // Each send has its own mutation id, so a double Enter would send twice: one at a time.
    if (busy) return
    setBusy(true)
    try {
      await sendMessage(host.client, agent.id, type, body.trim())
      setBody('')
      setStatus({ ok: true, text: 'Sent.' })
    } catch (err) {
      setStatus({ ok: false, text: err instanceof Error ? err.message : String(err) })
    } finally {
      setBusy(false)
    }
  }
  return (
    <form onSubmit={submit} className="flex flex-col gap-2 rounded-lg border bg-card p-3" aria-label="Send a message">
      <h2 className="text-xs font-semibold tracking-wide text-muted-foreground uppercase">Message {agent.name}</h2>
      <div className="flex gap-2">
        <Select aria-label="Message type" className="w-28" value={type} onChange={(e) => setType(Number(e.target.value))}>
          {SEND_TYPES.map(([v, l]) => <option key={v} value={v}>{l}</option>)}
        </Select>
        <Input aria-label="Message" value={body} onChange={(e) => setBody(e.target.value)} placeholder={dead ? 'The agent has exited' : 'Write to the agent'} disabled={dead} />
        <Button type="submit" disabled={dead || busy || !body.trim()}>Send</Button>
      </div>
      {status && <p role={status.ok ? 'status' : 'alert'} className={`text-xs ${status.ok ? 'text-muted-foreground' : 'text-destructive'}`}>{status.text}</p>}
    </form>
  )
}
