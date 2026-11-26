import { ChevronRight, Send } from 'lucide-react'
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { promptAgent, readTranscript } from '@/actions'
import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/input'
import { useApp, useHostState } from '@/context'
import { statusSentence } from '@/derive'
import { ActorClass } from '@/gen/agentmesh/v1/actor_pb'
import type { TranscriptEntry } from '@/gen/agentmesh/v1/chat_pb'
import { AgentState } from '@/gen/agentmesh/v1/types_pb'
import type { HostRuntime } from '@/model'
import { emptyTranscript, fromTail, toBlocks, toolsSummary, withEarlier, withLater, type Block, type ToolCall, type Transcript } from '@/transcript'
import { Markdown } from './markdown'

const say = (e: unknown) => (e instanceof Error ? e.message.replace(/^\[[a-z_]+\]\s*/, '') : String(e))

/** How often the chat reads on while the agent works (a reply fires no event of its own; a tool call or a state change does). */
const POLL_MS = 2000
/** How long after an event the chat reads on: a burst of events is one read. */
const SETTLE_MS = 300

/**
 * An agent's conversation as its CLI recorded it, kept up to date: the tail first, earlier entries on request, newer
 * ones as the agent's events (and a slow poll while it works) say there is more. Reads only while `active`, and starts
 * over when the agent's conversation changes (a reopen, `/clear`).
 */
function useTranscript(host: HostRuntime, agentId: string, sessionId: string, active: boolean) {
  const [state, setState] = useState<{ t: Transcript; loaded: boolean }>({ t: emptyTranscript(), loaded: false })
  const [error, setError] = useState('')
  const [earlierBusy, setEarlierBusy] = useState(false)
  const cur = useRef(state)
  cur.current = state
  const busy = useRef(false)
  const again = useRef(false)
  const gen = useRef(0)

  const sync = useCallback(async () => {
    if (busy.current) {
      again.current = true
      return
    }
    busy.current = true
    const mine = gen.current
    try {
      do {
        again.current = false
        const { t, loaded } = cur.current
        if (!loaded) {
          const res = await readTranscript(host.client, agentId, -1n, true)
          if (mine !== gen.current) return
          setState({ t: fromTail(res), loaded: true })
          cur.current = { t: fromTail(res), loaded: true }
        } else {
          const res = await readTranscript(host.client, agentId, t.end, false)
          if (mine !== gen.current) return
          const next = withLater(t, res)
          if (next.kind === 'reload') {
            cur.current = { t: emptyTranscript(), loaded: false }
            setState(cur.current)
            again.current = true
          } else if (next.kind === 'more') {
            cur.current = { t: next.transcript, loaded: true }
            setState(cur.current)
          }
        }
        setError('')
      } while (again.current)
    } catch (e) {
      if (mine === gen.current) setError(say(e))
    } finally {
      // A read of an earlier conversation ends without freeing the flag: the reset did, and a newer read may hold it now.
      if (mine === gen.current) busy.current = false
    }
  }, [host, agentId])

  // Another conversation (or another agent): what was held is not it.
  useEffect(() => {
    gen.current++
    busy.current = false
    cur.current = { t: emptyTranscript(), loaded: false }
    setState(cur.current)
    setError('')
  }, [agentId, sessionId])

  useEffect(() => {
    if (active) void sync()
  }, [active, agentId, sessionId, sync])

  // Read on when the agent's events say it did something.
  useEffect(() => {
    if (!active) return
    let timer: ReturnType<typeof setTimeout> | undefined
    const off = host.store.subscribe((s, prev) => {
      if (s.agents[agentId] === prev.agents[agentId]) return
      clearTimeout(timer)
      timer = setTimeout(() => void sync(), SETTLE_MS)
    })
    return () => {
      off()
      clearTimeout(timer)
    }
  }, [active, host, agentId, sync])

  const working = useHostState(host, (s) => s.agents[agentId]?.state === AgentState.WORKING)
  useEffect(() => {
    if (!active || !working) return
    const t = setInterval(() => void sync(), POLL_MS)
    return () => clearInterval(t)
  }, [active, working, sync])

  const loadEarlier = useCallback(async () => {
    const { t } = cur.current
    if (earlierBusy || t.atStart) return
    setEarlierBusy(true)
    try {
      const res = await readTranscript(host.client, agentId, t.start, true)
      const next = withEarlier(cur.current.t, res)
      cur.current = { t: next, loaded: true }
      setState(cur.current)
    } catch (e) {
      setError(say(e))
    } finally {
      setEarlierBusy(false)
    }
  }, [host, agentId, earlierBusy])

  return { transcript: state.t, loaded: state.loaded, error, sync, loadEarlier, earlierBusy }
}

/**
 * The chat view of an agent: its conversation as readable blocks (what was asked, the reply as Markdown, tool calls
 * folded) and a composer that types into the same running process, as typing in its terminal does. The terminal is
 * the other view of the same agent.
 */
export function ChatView({ host, agentId, active, panelId, tabId }: { host: HostRuntime; agentId: string; active: boolean; panelId: string; tabId: string }) {
  const app = useApp()
  const agent = useHostState(host, (s) => s.agents[agentId])
  const readOnly = useHostState(host, (s) => s.daemon?.actor?.class === ActorClass.READ_ONLY_DEVICE)
  const { transcript, loaded, error, sync, loadEarlier, earlierBusy } = useTranscript(host, agentId, agent?.sessionId ?? '', active)
  const blocks = useMemo(() => toBlocks(transcript.entries), [transcript.entries])

  // Scrolling: follow the end while the reader is at it; keep the place when earlier entries come in above.
  const scroller = useRef<HTMLDivElement>(null)
  const stick = useRef(true)
  const anchor = useRef<{ height: number; top: number }>(undefined)
  useLayoutEffect(() => {
    const el = scroller.current
    if (!el) return
    if (anchor.current) {
      el.scrollTop = anchor.current.top + (el.scrollHeight - anchor.current.height)
      anchor.current = undefined
    } else if (stick.current) el.scrollTop = el.scrollHeight
  }, [transcript.entries, active])
  // Read on shortly after a prompt is sent (its turn shows within a second or so); not after the view is gone.
  const sendTimers = useRef<Array<ReturnType<typeof setTimeout>>>([])
  useEffect(() => () => sendTimers.current.forEach(clearTimeout), [])
  const afterSend = () => {
    sendTimers.current = [300, 1500].map((ms) => setTimeout(() => void sync(), ms))
  }
  const earlier = async () => {
    const el = scroller.current
    if (el) anchor.current = { height: el.scrollHeight, top: el.scrollTop }
    await loadEarlier()
  }

  if (!agent) return null
  return (
    <div id={panelId} role="tabpanel" aria-labelledby={tabId} className="flex min-h-0 flex-1 flex-col gap-2" hidden={!active}>
      <div
        ref={scroller}
        data-testid="chat"
        onScroll={(e) => {
          const el = e.currentTarget
          stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 48
        }}
        className="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto rounded-md border bg-surface p-3"
      >
        {!transcript.supported && (
          <p role="status" className="py-2 text-xs text-muted-foreground">
            The chat view reads Claude Code's record of a conversation; {agent.kind} has none we read yet. Its terminal shows the whole of it, and what you send here is typed into it.
          </p>
        )}
        {transcript.supported && !loaded && !error && <p role="status" className="py-6 text-center text-xs text-muted-foreground">Loading the conversation…</p>}
        {error && (
          <div role="alert" className="flex items-center gap-2 text-xs text-destructive">
            <span>Couldn't read the conversation: {error}</span>
            <Button size="sm" variant="outline" onClick={() => void sync()}>Try again</Button>
          </div>
        )}
        {loaded && transcript.supported && !transcript.atStart && (
          <Button size="sm" variant="ghost" className="self-center" disabled={earlierBusy} onClick={() => void earlier()}>
            {earlierBusy ? 'Loading…' : 'Load earlier'}
          </Button>
        )}
        {loaded && transcript.supported && blocks.length === 0 && <p className="py-6 text-center text-xs text-muted-foreground">Nothing said yet.</p>}
        {blocks.length > 0 && (
          <ol aria-label="Conversation" className="flex flex-col gap-3">
            {blocks.map((b) => (
              <li key={b.id}>
                <ChatBlock block={b} />
              </li>
            ))}
          </ol>
        )}
      </div>
      <p role="status" data-testid="chat-status" className="truncate text-xs text-muted-foreground">
        {agent.state === AgentState.WORKING && agent.stateDetail ? `Working: ${agent.stateDetail}` : statusSentence(agent)}
      </p>
      <Composer host={host} agentId={agentId} readOnly={readOnly} onSent={afterSend} openTerminal={() => app.setTab('terminal')} />
    </div>
  )
}

function Composer({ host, agentId, readOnly, onSent, openTerminal }: { host: HostRuntime; agentId: string; readOnly: boolean; onSent: () => void; openTerminal: () => void }) {
  const agent = useHostState(host, (s) => s.agents[agentId])
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  if (!agent) return null
  const gone = agent.state === AgentState.EXITED || agent.state === AgentState.RETIRED || !agent.terminalId
  const atPrompt = agent.state === AgentState.NEEDS_INPUT
  const why = readOnly
    ? 'This device is read-only: it can read the conversation, not type into the agent.'
    : gone
      ? 'The agent has stopped, so there is no terminal to type into. Message it from the Overview tab to bring it back.'
      : ''
  async function send() {
    const body = text.trim()
    if (!body || busy) return
    setBusy(true)
    setError('')
    try {
      await promptAgent(host.client, agentId, body)
      setText('')
      onSent()
    } catch (e) {
      setError(say(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <form
      aria-label="Prompt"
      className="flex flex-col gap-1.5"
      onSubmit={(e) => {
        e.preventDefault()
        void send()
      }}
    >
      {atPrompt && !why && (
        <p role="status" className="flex items-center gap-2 text-xs text-state-needs">
          It is waiting at a prompt in its terminal: answer it there.
          <Button type="button" size="sm" variant="outline" onClick={openTerminal}>Open the terminal</Button>
        </p>
      )}
      <div className="flex items-end gap-2">
        <Textarea
          aria-label="Prompt"
          className="max-h-40 min-h-9 flex-1 resize-none"
          rows={Math.min(6, Math.max(1, text.split('\n').length))}
          value={text}
          disabled={!!why || atPrompt}
          placeholder={why ? '' : 'Write to the agent. Enter sends, Shift+Enter adds a line.'}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
              e.preventDefault()
              void send()
            }
          }}
        />
        <Button type="submit" disabled={!!why || atPrompt || busy || !text.trim()}>
          <Send /> Send
        </Button>
      </div>
      {why && <p className="text-xs text-muted-foreground">{why}</p>}
      {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
    </form>
  )
}

function ChatBlock({ block }: { block: Block }) {
  switch (block.kind) {
    case 'user':
      return (
        <div className="ml-auto w-fit max-w-[85%] rounded-lg bg-accent px-3 py-2 text-[13px] break-words whitespace-pre-wrap" data-kind="user">
          {block.entry.text}
        </div>
      )
    case 'mesh':
      return (
        <p className="text-center text-[11px] text-muted-foreground" data-kind="mesh">
          From the mesh: {block.entry.text.replace(/^📬\s*\[mesh\]\s*/, '')}
        </p>
      )
    case 'assistant':
      return (
        <div data-kind="assistant">
          <Markdown>{block.entry.text}</Markdown>
          {block.entry.truncated && <p className="text-[11px] text-muted-foreground">… cut here</p>}
        </div>
      )
    case 'tools':
      return (
        <details className="rounded-md border bg-card text-xs" data-kind="tools">
          <summary className="flex cursor-pointer list-none items-center gap-1.5 px-2.5 py-1.5 text-muted-foreground [&::-webkit-details-marker]:hidden">
            <ChevronRight className="size-3.5 transition-transform [details[open]>summary>&]:rotate-90" />
            {toolsSummary(block.calls)}
          </summary>
          <ul className="flex flex-col gap-2 border-t p-2.5">
            {block.calls.map((c) => (
              <li key={c.id}>
                <ToolCallRow call={c} />
              </li>
            ))}
          </ul>
        </details>
      )
  }
}

function ToolCallRow({ call }: { call: ToolCall }) {
  const r: TranscriptEntry | undefined = call.result
  return (
    <div className="flex flex-col gap-1">
      <p className="flex items-baseline gap-2">
        <code className="font-mono font-semibold">{call.tool}</code>
        <span className="min-w-0 truncate font-mono text-muted-foreground">{call.input}</span>
      </p>
      {r ? (
        r.text && (
          <pre className={`max-h-48 overflow-auto rounded border bg-surface p-2 font-mono text-[11px] whitespace-pre-wrap ${r.isError ? 'border-destructive text-destructive' : ''}`}>
            {r.text}
            {r.truncated ? '\n… cut here' : ''}
          </pre>
        )
      ) : (
        <p className="text-muted-foreground">Running…</p>
      )}
    </div>
  )
}
