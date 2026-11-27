import { Check, ChevronRight, Clock, Copy, ImagePlus, Pencil, Quote, Send, Square, X } from 'lucide-react'
import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { addAttachment, interruptAgent, listPrompts, promptAgent, readTranscript, sendPromptAnyway, withdrawPrompt } from '@/actions'
import { ConfirmDialog } from '@/components/ui/confirm'
import { imageName, imageProblem, imageRef, imagesUnsupported, keepThumb, rewindUnsupported, splitImages, thumbs } from '@/attachments'
import { Button } from '@/components/ui/button'
import { Skeleton, SkeletonLines } from '@/components/ui/skeleton'
import { ConfirmDialog } from '@/components/ui/confirm'
import { MarkdownField } from '@/components/ui/markdown-field'
import { invoked, opensDialog } from '@/composer-commands'
import { useComposerAssist } from './composer-assist'
import { loadDraft, saveDraft } from '@/layout'
import { useApp, useAppState, useHostState } from '@/context'
import { messagesOf, statusSentence } from '@/derive'
import { ActorClass } from '@/gen/agentmesh/v1/actor_pb'
import type { TranscriptEntry } from '@/gen/agentmesh/v1/chat_pb'
import type { QueuedPrompt } from '@/gen/agentmesh/v1/types_pb'
import { matches, parseChord } from '@/keys'
import { chatKeys } from '@/prefs'
import { addPrompt, dropPrompt, setPrompts } from '@/store'
import { AgentState } from '@/gen/agentmesh/v1/types_pb'
import type { HostRuntime } from '@/model'
import { emptyTranscript, fromTail, timeline, toBlocks, toolsSummary, withEarlier, withLater, type Block, type ToolCall, type Transcript } from '@/transcript'
import { MessageCard } from './messages-panel'
import { RewindDialog } from './rewind-dialog'
import { ContextRing } from './agent-work'
import { Markdown } from './markdown'
import { ago } from '@/dock'
import { useNow } from '@/hooks'
import { cn } from '@/lib/utils'
import { hueStyle } from '@/hue'
import { Tip } from '@/components/ui/tooltip'

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
        const from = t.end
        if (!loaded) {
          const res = await readTranscript(host.client, agentId, -1n, true)
          if (mine !== gen.current) return
          setState({ t: fromTail(res), loaded: true })
          cur.current = { t: fromTail(res), loaded: true }
        } else {
          const res = await readTranscript(host.client, agentId, t.end, false)
          if (mine !== gen.current) return
          // What is held now, not what was when the read began: "Load earlier" may have put entries above since.
          const next = withLater(cur.current.t, res, from)
          if (next.kind === 'reload') {
            cur.current = { t: emptyTranscript(), loaded: false }
            setState(cur.current)
            again.current = true
          } else if (next.kind === 'more') {
            cur.current = { t: next.transcript, loaded: true }
            setState(cur.current)
          } else if (from !== cur.current.t.end) {
            // The read began somewhere else than where the transcript ends now: read again from the right place.
            again.current = true
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
    const mine = gen.current
    setEarlierBusy(true)
    try {
      const res = await readTranscript(host.client, agentId, t.start, true)
      // The conversation was reset while this read was out: it is another one's earlier part.
      if (mine !== gen.current) return
      const next = withEarlier(cur.current.t, res)
      if (next === cur.current.t) {
        // The page doesn't join what is held (the record changed while it was out): read the tail again.
        cur.current = { t: emptyTranscript(), loaded: false }
        setState(cur.current)
        void sync()
        return
      }
      cur.current = { t: next, loaded: true }
      setState(cur.current)
    } catch (e) {
      if (mine === gen.current) setError(say(e))
    } finally {
      setEarlierBusy(false)
    }
  }, [host, agentId, earlierBusy, sync])

  return { transcript: state.t, loaded: state.loaded, error, sync, loadEarlier, earlierBusy }
}

/**
 * The chat view of an agent: its conversation as readable blocks (what was asked, the reply as Markdown, tool calls
 * folded) and a composer that types into the same running process, as typing in its terminal does. The terminal is
 * the other view of the same agent.
 */
export function ChatView({ host, agentId, active, onTerminal }: { host: HostRuntime; agentId: string; active: boolean; /** Shows its terminal in the chat's place (the pane's own view). */ onTerminal?: () => void }) {
  const app = useApp()
  const agent = useHostState(host, (s) => s.agents[agentId])
  const readOnly = useHostState(host, (s) => s.daemon?.actor?.class === ActorClass.READ_ONLY_DEVICE)
  const { transcript, loaded, error, sync, loadEarlier, earlierBusy } = useTranscript(host, agentId, agent?.sessionId ?? '', active)
  const blocks = useMemo(() => toBlocks(transcript.entries), [transcript.entries])
  // The agent's mesh messages, as cards where they were sent; read with the run's messages once the chat shows.
  const runId = agent?.runId
  const messageLoad = useHostState(host, (s) => (runId ? s.messageLoad[runId] : undefined))
  const allMessages = useHostState(host, (s) => s.messages)
  const waiting = useHostState(host, (s) => s.waiting)
  const mine = useMemo(() => messagesOf(allMessages, agentId), [allMessages, agentId])
  const open = useMemo(() => new Set(waiting.flatMap((w) => (w.message ? [w.message.id] : []))), [waiting])
  // A plan an agent asked to show (`OpenView`) is shown like one waiting on the user, whatever the window held.
  const focusId = useAppState((s) => (s.focusMessage?.hostId === host.info.id ? s.focusMessage.id : undefined))
  const shown = useMemo(() => (focusId ? new Set([...open, focusId]) : open), [open, focusId])
  useEffect(() => {
    if (active && runId) host.loadMessages(runId)
  }, [active, runId, host])
  const working = agent?.state === AgentState.WORKING
  const lastTools = useMemo(() => [...blocks].reverse().find((b) => b.kind !== 'mesh'), [blocks])
  const items = useMemo(
    () => timeline(blocks, messageLoad === 'loaded' ? mine : undefined, transcript.atStart || !transcript.supported, shown),
    [blocks, messageLoad, mine, transcript.atStart, transcript.supported, shown],
  )
  const queued = useQueue(host, agentId, active)

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
    <section aria-label="Chat" style={hueStyle(agent.repo)} className="flex min-h-0 flex-1 flex-col gap-2" hidden={!active}>
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
            The host reads no record of a {agent.kind} conversation for the chat view. Its terminal shows the whole of it, and what you send here is typed into it.
          </p>
        )}
        {transcript.supported && !loaded && !error && (
          <div role="status" aria-label="Loading the conversation" className="flex flex-col gap-4 py-2"><span className="sr-only">Loading the conversation</span>
            <Skeleton className="h-8 w-1/2 self-end rounded-lg" />
            <SkeletonLines lines={3} className="w-5/6" />
            <Skeleton className="h-8 w-1/3 self-end rounded-lg" />
          </div>
        )}
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
        {loaded && transcript.supported && items.length === 0 && <p className="py-6 text-center text-xs text-muted-foreground">Nothing said yet.</p>}
        {messageLoad === 'failed' && (
          <div role="alert" className="flex items-center gap-2 text-xs text-destructive">
            <span>Couldn't read this run's messages.</span>
            <Button size="sm" variant="outline" onClick={() => runId && host.loadMessages(runId, true)}>Try again</Button>
          </div>
        )}
        {items.length > 0 && (
          <ol aria-label="Conversation" className="flex flex-col gap-3">
            {items.map((b) => (
              <li key={b.id}>
                {b.kind === 'message' ? (
                  <MessageCard host={host} message={b.message} open={open.has(b.message.id)} />
                ) : (
                  <ChatBlock block={b} agentId={agentId} running={b === lastTools && working && b.kind === 'tools' && b.calls.some((c) => !c.result)} />
                )}
              </li>
            ))}
          </ol>
        )}
        {queued.length > 0 && <QueuedList host={host} agentId={agentId} prompts={queued} readOnly={readOnly} />}
      </div>
      <p role="status" data-testid="chat-status" className="truncate text-xs text-muted-foreground">
        {agent.state === AgentState.WORKING && agent.stateDetail ? `Working: ${agent.stateDetail}` : statusSentence(agent)}
      </p>
      <Composer host={host} agentId={agentId} readOnly={readOnly} active={active} onSent={afterSend} openTerminal={onTerminal ?? (() => app.setTab('terminal'))} />
      {!readOnly && <RewindDialog host={host} agentId={agentId} blocks={blocks} messages={mine} />}
    </section>
  )
}

/** An agent's queued prompts: listed when its chat is shown and again when its state changes (what they wait for follows it), kept by the host's events in between. */
function useQueue(host: HostRuntime, agentId: string, active: boolean): QueuedPrompt[] {
  const state = useHostState(host, (s) => s.agents[agentId]?.state)
  const prompts = useHostState(host, (s) => s.prompts[agentId])
  const snapshots = useHostState(host, (s) => s.snapshots)
  useEffect(() => {
    if (!active) return
    let live = true
    // A listing asked before the queue changed here is stale: ask again (a few times at most) until one isn't.
    void (async () => {
      for (let tries = 0; live && tries < 5; tries++) {
        const asked = host.store.getState().promptTouch[agentId] ?? 0
        const res = await listPrompts(host.client, agentId).catch(() => undefined)
        if (!res || !live) return
        if ((host.store.getState().promptTouch[agentId] ?? 0) === asked) return host.store.setState(setPrompts(agentId, res.prompts, asked))
      }
    })()
    return () => {
      live = false
    }
  }, [host, agentId, active, state, snapshots])
  return prompts ?? NO_PROMPTS
}

const NO_PROMPTS: QueuedPrompt[] = []

/** What a queued prompt waits for, as the host says (`QueuedPrompt.held`). */
const HELD: Record<string, string> = {
  working: 'Sent when it finishes its turn',
  starting: 'Sent once it has started',
  at_prompt: 'Sent once the prompt in its terminal is answered',
  limited: 'Sent once it is off its usage limit',
  stopped: 'Sent when it runs again',
  draft: 'Held: its input box holds a draft. Clear it in the terminal, or send this anyway',
  typing: 'Held: someone is typing in its terminal',
  no_box: 'Held: its screen shows no input box (a dialog may be open in the terminal)',
}

/** The prompts that wait to be typed, under the conversation: each can be taken back to edit, or dropped. */
function QueuedList({ host, agentId, prompts, readOnly }: { host: HostRuntime; agentId: string; prompts: QueuedPrompt[]; readOnly: boolean }) {
  const app = useApp()
  const [error, setError] = useState('')
  // A prompt the user is being asked to send over the draft in the agent's box.
  const [over, setOver] = useState<QueuedPrompt>()
  async function take(p: QueuedPrompt, edit: boolean) {
    setError('')
    try {
      const res = await withdrawPrompt(host.client, agentId, p.id)
      host.store.setState(dropPrompt(agentId, p.id))
      if (edit && res.prompt) {
        const back = splitImages(res.prompt.text)
        app.store.setState({ composerFill: { agentId, text: back.text, images: back.paths } })
      }
    } catch (e) {
      setError(say(e))
    }
  }
  return (
    <section aria-label="Queued prompts" className="flex flex-col items-end gap-1.5">
      {prompts.map((p) => (
        <div key={p.id} data-kind="queued" className="group flex max-w-[85%] flex-col items-end gap-0.5">
          <div className="w-fit min-w-0 max-w-full rounded-lg border border-dashed bg-accent/50 px-3 py-2 opacity-80">{(() => {
            const q = splitImages(p.text)
            return (
              <>
                {q.paths.length > 0 && <p className="flex items-center gap-1 text-xs text-muted-foreground"><ImagePlus className="size-3.5" />{q.paths.length === 1 ? 'An image' : `${q.paths.length} images`}</p>}
                {q.text && <Markdown breaks className="text-[13px] break-words">{q.text}</Markdown>}
              </>
            )
          })()}</div>
          <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
            <Clock className="size-3" />
            <span>{HELD[p.held] ?? 'Queued: sent when it is idle'}</span>
            {!readOnly && p.held === 'draft' && p.heldText && (
              <button type="button" onClick={() => setOver(p)} className="rounded px-1 text-xs text-foreground underline underline-offset-2 hover:bg-muted">
                Send anyway
              </button>
            )}
            {!readOnly && (
              <>
                <Tip label="Take it back to edit">
                  <button type="button" aria-label="Edit the queued prompt" onClick={() => void take(p, true)} className="grid size-5 place-items-center rounded hover:bg-muted">
                    <Pencil className="size-3" />
                  </button>
                </Tip>
                <Tip label="Take it back">
                  <button type="button" aria-label="Remove the queued prompt" onClick={() => void take(p, false)} className="grid size-5 place-items-center rounded hover:bg-muted">
                    <X className="size-3" />
                  </button>
                </Tip>
              </>
            )}
          </p>
        </div>
      ))}
      {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
      <ConfirmDialog
        open={over !== undefined}
        onOpenChange={(o) => !o && setOver(undefined)}
        title="Send over the draft?"
        description={`Its input box holds this draft: “${over?.heldText ?? ''}”. Sending clears it and types your prompt instead. The draft is lost.`}
        action="Send anyway"
        run={async () => {
          if (!over) return
          await sendPromptAnyway(host.client, agentId, over.id, over.heldText)
          host.store.setState(dropPrompt(agentId, over.id))
        }}
      />
    </section>
  )
}

function Composer({ host, agentId, readOnly, onSent, openTerminal, active }: { host: HostRuntime; agentId: string; readOnly: boolean; onSent: () => void; openTerminal: () => void; active: boolean }) {
  const app = useApp()
  const agent = useHostState(host, (s) => s.agents[agentId])
  // The app follows its first host's keys, as for the shortcuts (Settings edits them there).
  const first = useAppState((s) => s.hosts[0]) ?? host
  const settings = useHostState(first, (s) => s.settings)
  const keys = useMemo(() => chatKeys(settings), [settings])
  const fill = useAppState((s) => s.composerFill)
  // What was being written to this agent, kept on this device until it is sent.
  const [text, setTextNow] = useState(() => loadDraft(host.info.id, agentId))
  const setText = useCallback(
    (next: string | ((cur: string) => string)) =>
      setTextNow((cur) => {
        const v = typeof next === 'function' ? next(cur) : next
        saveDraft(host.info.id, agentId, v)
        return v
      }),
    [host, agentId],
  )
  useEffect(() => setTextNow(loadDraft(host.info.id, agentId)), [host, agentId])
  // The images are this agent's: another agent shown here starts with none, and an upload that ends after the switch
  // is dropped (`forAgent`).
  const forAgent = useRef(0)
  useEffect(() => {
    // A new generation for each agent shown (even one shown again): what began under an earlier one is dropped.
    forAgent.current++
    setImages([])
    // Unmounted (the tab closed) is leaving the agent too.
    return () => {
      forAgent.current++
    }
  }, [host, agentId])
  const [busy, setBusy] = useState(false)
  const [stopping, setStopping] = useState(false)
  // Refs, not state: a second key press before the first's re-render must see the call in flight.
  const stopRef = useRef(false)
  const recallRef = useRef(false)
  const [error, setError] = useState('')
  // The images going with the next prompt: stored on the host as they are added, named in the prompt when it is sent.
  const [images, setImages] = useState<{ key: string; name: string; url: string; path?: string }[]>([])
  const fileInput = useRef<HTMLInputElement>(null)
  // When the interrupt key last went in at an idle agent: a second one soon after opens Rewind (Esc Esc).
  const lastEsc = useRef(0)
  // A queued prompt taken back to edit lands here.
  useEffect(() => {
    if (fill && fill.agentId === agentId) {
      // Never over what is being written: the taken-back prompt goes after it.
      setText((cur) => (cur.trim() ? `${cur.replace(/\s+$/, '')}\n\n${fill.text}` : fill.text))
      const back = fill.images ?? []
      if (back.length > 0) setImages((cur) => [...cur, ...back.map((p) => ({ key: `back-${p}-${Math.random()}`, name: p.split(/[\\/]/).pop() ?? p, url: thumbs.get(p) ?? '', path: p }))])
      app.store.setState({ composerFill: undefined })
    }
  }, [fill, agentId, app, setText])
  const hasQueue = useHostState(host, (s) => (s.prompts[agentId]?.length ?? 0) > 0)
  const fieldRef = useRef<HTMLTextAreaElement>(null)
  // A command that ends the CLI's process asks first: the text waits here for the answer.
  const [confirming, setConfirming] = useState('')
  const assist = useComposerAssist({
    host, agentId, kind: agent?.kind ?? '', state: agent?.state, text, setText, field: fieldRef,
    disabled: readOnly || !agent || agent.state === AgentState.EXITED || agent.state === AgentState.RETIRED || !agent.terminalId,
  })
  if (!agent) return null
  const gone = agent.state === AgentState.EXITED || agent.state === AgentState.RETIRED || !agent.terminalId
  const atPrompt = agent.state === AgentState.NEEDS_INPUT
  // Quiet for a while, a working agent shows as unknown: it may still be at work, so Stop stays.
  const working = agent.state === AgentState.WORKING || agent.state === AgentState.UNKNOWN || atPrompt
  // Only Claude Code's interrupt key is known to the host (`interrupt-esc`, TODO.md for the other CLIs).
  const canStop = agent.kind === 'claude'
  const noImages = imagesUnsupported(agent.kind)
  const uploading = images.some((i) => !i.path)
  const why = readOnly
    ? 'This device is read-only: it can read the conversation, not type into the agent.'
    : gone
      ? 'The agent has stopped, so there is no terminal to type into. Send it a task from its menu (⋯) to bring it back.'
      : ''
  /** Stores each image on the host (so an agent on another machine can read it), with a thumbnail kept here. */
  async function attach(files: File[]) {
    if (noImages || why) return
    // The agent these files were added to: the loop stops when another is shown, so none lands in its strip or goes
    // up under the wrong agent's folder.
    const owner = forAgent.current
    for (const f of files) {
      if (forAgent.current !== owner) return
      const problem = imageProblem(f)
      if (problem) {
        setError(problem)
        continue
      }
      const key = `${f.name}-${f.size}-${Math.random()}`
      const url = await new Promise<string>((resolve) => {
        const r = new FileReader()
        r.onload = () => resolve(String(r.result))
        r.onerror = () => resolve('')
        r.readAsDataURL(f)
      })
      if (forAgent.current !== owner) return
      setImages((cur) => [...cur, { key, name: f.name, url }])
      try {
        const bytes = new Uint8Array(await f.arrayBuffer())
        if (forAgent.current !== owner) return
        const res = await addAttachment(host.client, agentId, imageName(f), bytes)
        if (url) keepThumb(res.path, url)
        if (forAgent.current !== owner) return
        setImages((cur) => cur.map((i) => (i.key === key ? { ...i, path: res.path } : i)))
      } catch (e) {
        if (forAgent.current !== owner) return
        setImages((cur) => cur.filter((i) => i.key !== key))
        setError(say(e))
      }
    }
  }
  /** Sends the box, or `confirmedText`: the command the user was asked about (the box may have changed since; it has no images). */
  async function send(confirmedText?: string) {
    const typed = confirmedText ?? text.trim()
    const sent = confirmedText === undefined ? images.filter((i) => i.path) : []
    const refs = sent.map((i) => imageRef(i.path!))
    const body = [...refs, typed].filter(Boolean).join('\n')
    if (!body || busy || uploading) return
    setBusy(true)
    setError('')
    try {
      // `/exit` and the like end the session: ask first. The list may not be here yet (the first probe is slow): wait for it, so the question is never skipped.
      let known = assist.commands
      if (body.startsWith('/') && known.length === 0) known = await assist.load(true)
      // `/resume` and the pickers open a dialog the chat can't show.
      const cmd = invoked(body, known)
      // With no list at all (the host can't say, or it failed) the question is still asked for the commands every CLI has that end the session.
      const unlisted = known.length === 0 && /^\/(exit|quit|logout)(\s|$)/i.test(body)
      if ((cmd?.command.danger || unlisted) && confirmedText === undefined) {
        setConfirming(body)
        setBusy(false)
        return
      }
      const res = await promptAgent(host.client, agentId, body)
      if (res.queued) host.store.setState(addPrompt(res.queued))
      // Typed at once, a dialog is on the terminal's screen now: take the user there.
      else if (cmd && opensDialog(cmd.command, cmd.args)) openTerminal()
      // What was sent goes; text written while it was in flight stays. The saved draft too, directly: a composer
      // unmounted meanwhile (another tab opened) runs no updater.
      const left = (cur: string) => {
        const now = cur.trimStart()
        return now.startsWith(typed) ? now.slice(typed.length).trimStart() : cur
      }
      saveDraft(host.info.id, agentId, left(loadDraft(host.info.id, agentId)))
      setText(left)
      // Only the images sent go: one added while the prompt was in flight stays for the next.
      const gone = new Set(sent.map((i) => i.key))
      setImages((cur) => cur.filter((i) => !gone.has(i.key)))
      onSent()
    } catch (e) {
      setError(say(e))
    } finally {
      setBusy(false)
    }
  }
  async function stop() {
    // One at a time: a second Esc at an agent that already stopped would open Claude's rewind menu.
    if (stopRef.current) return
    stopRef.current = true
    setStopping(true)
    setError('')
    try {
      const res = await interruptAgent(host.client, agentId)
      if (!res.stopped) setError("Sent Esc, but it hasn't stopped yet: look at its terminal.")
    } catch (e) {
      setError(say(e))
    } finally {
      stopRef.current = false
      setStopping(false)
    }
  }
  /** Up in an empty box takes back the newest queued prompt to edit, as Claude Code's Up arrow does. */
  async function recall() {
    if (recallRef.current) return
    recallRef.current = true
    setError('')
    try {
      const res = await withdrawPrompt(host.client, agentId)
      const p = res.prompt
      if (p) {
        host.store.setState(dropPrompt(agentId, p.id))
        // Never over what was typed while the call was out.
        setText((cur) => (cur.trim() ? `${cur.replace(/\s+$/, '')}\n\n${p.text}` : p.text))
      }
    } catch (e) {
      setError(say(e))
    } finally {
      recallRef.current = false
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
      onDragOver={(e) => {
        if (!noImages && !why && e.dataTransfer.types.includes('Files')) e.preventDefault()
      }}
      onDrop={(e) => {
        const files = [...e.dataTransfer.files]
        if (files.length === 0) return
        e.preventDefault()
        void attach(files)
      }}
    >
      {atPrompt && !why && (
        <p role="status" className="flex items-center gap-2 text-xs text-state-needs">
          It is waiting at a prompt in its terminal: answer it there. What you send now waits until then.
          <Button type="button" size="sm" variant="outline" onClick={openTerminal}>Open the terminal</Button>
        </p>
      )}
      {images.length > 0 && (
        <ul aria-label="Images" className="flex flex-wrap gap-2">
          {images.map((i) => (
            <li key={i.key} className="relative">
              {i.url ? <img src={i.url} alt={i.name} className={cn('size-16 rounded-md border object-cover', !i.path && 'opacity-50')} /> : <span className="grid size-16 place-items-center rounded-md border text-xs">{i.name}</span>}
              <button type="button" aria-label={`Remove ${i.name}`} onClick={() => setImages((cur) => cur.filter((x) => x.key !== i.key))} className="absolute -top-1.5 -right-1.5 grid size-5 place-items-center rounded-full border bg-popover outline-none hover:bg-muted focus-visible:ring-2 focus-visible:ring-ring">
                <X className="size-3" />
              </button>
            </li>
          ))}
        </ul>
      )}
      <div className="flex items-end gap-2">
        <MarkdownField
          aria-label="Prompt"
          className="flex-1 text-sm focus-visible:ring-identity"
          value={text}
          disabled={!!why}
          ref={fieldRef}
          {...assist.field}
          title={assist.suggestion ? 'Tab takes the suggestion, Esc drops it' : undefined}
          placeholder={assist.suggestion ? assist.suggestion : why ? '' : working ? `Write to the agent: it is sent when it finishes its turn.${canStop ? ' Esc stops it.' : ''}` : 'Write to the agent. Enter sends, Shift+Enter adds a line.'}
          onValueChange={setText}
          onSend={() => void send()}
          onPaste={(e) => {
            const files = [...e.clipboardData.files].filter((f) => f.type.startsWith('image/'))
            if (files.length === 0 || noImages) return
            e.preventDefault()
            void attach(files)
          }}
          onKeyDown={(e) => {
            if (e.nativeEvent.isComposing) return
            if (assist.onKeyDown(e)) return
            const interrupt = parseChord(keys.interrupt)
            if (interrupt && working && canStop && matches(e.nativeEvent, interrupt)) {
              e.preventDefault()
              if (!e.repeat) void stop() // a held key fires once
              return
            }
            // At an idle agent with nothing written, the interrupt key twice opens Rewind, as Esc Esc does in Claude Code.
            if (interrupt && agent.state === AgentState.IDLE && !text && !rewindUnsupported(agent.kind) && matches(e.nativeEvent, interrupt) && !e.repeat) {
              const now = Date.now()
              if (now - lastEsc.current < 600) {
                e.preventDefault()
                lastEsc.current = 0
                app.openRewind({ hostId: host.info.id, agentId })
              } else lastEsc.current = now
              return
            }
            const back = parseChord(keys.recall)
            if (back && !text && hasQueue && matches(e.nativeEvent, back)) {
              e.preventDefault()
              if (!e.repeat) void recall()
            }
          }}
        />
        <input
          ref={fileInput}
          type="file"
          accept="image/png,image/jpeg,image/gif,image/webp"
          multiple
          hidden
          onChange={(e) => {
            const files = [...(e.target.files ?? [])]
            e.target.value = ''
            void attach(files)
          }}
        />
        <Tip label={noImages || 'Add an image (or paste or drop one)'}>
          <Button type="button" size="icon" variant="ghost" aria-label="Add an image" disabled={!!why || !!noImages} onClick={() => fileInput.current?.click()}>
            <ImagePlus />
          </Button>
        </Tip>
        <ContextRing host={host} agent={agent} active={active} />
        {working && canStop && !why && (
          <Button type="button" variant="outline" disabled={stopping} aria-label="Stop the agent" title={`Interrupt its turn${keys.interrupt ? ` (${keys.interrupt})` : ''}`} onClick={() => void stop()}>
            <Square /> Stop
          </Button>
        )}
        <Button type="submit" disabled={!!why || busy || uploading || (!text.trim() && !images.some((i) => i.path))}>
          <Send /> {working ? 'Queue' : 'Send'}
        </Button>
      </div>
      {assist.popup}
      <ConfirmDialog
        open={!!confirming}
        onOpenChange={(o) => !o && setConfirming('')}
        title="Send this command?"
        description={`${confirming.split(/\s/)[0]} ends or signs out of ${agent.name}'s session. Its terminal closes; you can resume it from its menu.`}
        action="Send"
        run={async () => {
          await send(confirming)
        }}
      />
      {why && <p className="text-xs text-muted-foreground">{why}</p>}
      {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
    </form>
  )
}

/** Copy, quote into the composer, and when it was said: shown on hover or focus of a prompt or a reply. */
function BlockActions({ agentId, text, at, align }: { agentId: string; text: string; at: number; align: 'start' | 'end' }) {
  const app = useApp()
  const now = useNow(30_000)
  const [copied, setCopied] = useState(false)
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined)
  useEffect(() => () => clearTimeout(timer.current), [])
  // Copied only once the write took (it fails when the window lost the focus, or without a clipboard).
  const copy = () =>
    navigator.clipboard?.writeText(text).then(
      () => {
        setCopied(true)
        clearTimeout(timer.current)
        timer.current = setTimeout(() => setCopied(false), 1500)
      },
      () => setCopied(false),
    )
  const quote = () => app.store.setState({ composerFill: { agentId, text: `${text.split('\n').map((l) => `> ${l}`).join('\n')}\n\n` } })
  return (
    <div className={cn('flex h-6 items-center gap-0.5 text-xs text-muted-foreground opacity-0 transition-opacity group-hover:opacity-100 group-focus-within:opacity-100 motion-reduce:transition-none', align === 'end' && 'justify-end')}>
      {at > 0 && <time dateTime={new Date(at).toISOString()} title={new Date(at).toLocaleString()} className="px-1">{ago(at, now)}</time>}
      <Tip label={copied ? 'Copied' : 'Copy'}>
        <button
          type="button"
          aria-label="Copy"
          onClick={() => void copy()}
          className="grid size-6 place-items-center rounded outline-none hover:bg-muted hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
        >
          {copied ? <Check className="size-3.5" /> : <Copy className="size-3.5" />}
        </button>
      </Tip>
      <Tip label="Quote in a reply">
        <button type="button" aria-label="Quote in a reply" onClick={quote} className="grid size-6 place-items-center rounded outline-none hover:bg-muted hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring">
          <Quote className="size-3.5" />
        </button>
      </Tip>
    </div>
  )
}

function ChatBlock({ block, agentId, running }: { block: Block; agentId: string; /** The agent is at work on this block's tools. */ running: boolean }) {
  switch (block.kind) {
    case 'user': {
      // Its images: a thumbnail where this device sent it, else the file's name (the host's file isn't fetched).
      const { paths, text } = splitImages(block.entry.text)
      return (
        <div className="group ml-auto flex w-fit max-w-[min(85%,72ch)] flex-col items-end" data-kind="user">
          {paths.length > 0 && (
            <ul aria-label="Images" className="mb-1 flex flex-wrap justify-end gap-1.5">
              {paths.map((p, n) => {
                const name = p.split(/[\\/]/).pop() ?? p
                const url = thumbs.get(p)
                return (
                  <li key={`${n}-${p}`} title={p}>
                    {url ? <img src={url} alt={name} className="max-h-40 max-w-60 rounded-md border object-contain" /> : <span className="inline-flex items-center gap-1 rounded-md border px-2 py-1 text-xs text-muted-foreground"><ImagePlus className="size-3.5" />{name}</span>}
                  </li>
                )
              })}
            </ul>
          )}
          {text && (
            <div className="rounded-lg border border-identity/25 bg-identity/12 px-3 py-2">
              <Markdown breaks className="text-[15px] leading-relaxed">{text}</Markdown>
            </div>
          )}
          <BlockActions agentId={agentId} text={block.entry.text} at={block.at} align="end" />
        </div>
      )
    }
    case 'mesh':
      return (
        <p className="text-center text-xs text-muted-foreground" data-kind="mesh">
          From the mesh: {block.entry.text.replace(/^📬\s*\[mesh\]\s*/, '')}
        </p>
      )
    case 'assistant':
      return (
        <div className="group" data-kind="assistant">
          <Markdown className="max-w-[72ch] text-[15px] leading-[1.6]">{block.entry.text}</Markdown>
          {block.entry.truncated && <p className="text-xs text-muted-foreground">… cut here</p>}
          <BlockActions agentId={agentId} text={block.entry.text} at={block.at} align="start" />
        </div>
      )
    case 'output':
      // What a command the user typed printed (`/context`, `/usage`): the CLI's own words, not the model's.
      return (
        <pre
          className={cn('max-h-64 max-w-[72ch] overflow-auto rounded-md border bg-surface px-3 py-2 font-mono text-xs whitespace-pre-wrap', block.entry.isError ? 'border-destructive text-destructive' : 'text-muted-foreground')}
          data-kind="output"
        >
          {block.entry.text}
        </pre>
      )
    case 'tools':
      return (
        <details className="rounded-md border bg-card text-xs" data-kind="tools">
          <summary className="flex cursor-pointer list-none items-center gap-1.5 px-2.5 py-1.5 text-muted-foreground [&::-webkit-details-marker]:hidden">
            <ChevronRight className="size-3.5 transition-transform [details[open]>summary>&]:rotate-90" />
            <span
              data-running={running || undefined}
              className={cn(running && 'animate-shimmer bg-linear-to-r from-muted-foreground via-foreground to-muted-foreground bg-size-[200%_100%] bg-clip-text text-transparent motion-reduce:animate-none motion-reduce:bg-none motion-reduce:text-muted-foreground')}
            >
              {toolsSummary(block.calls)}
            </span>
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
          <pre className={`max-h-48 overflow-auto rounded border bg-surface p-2 font-mono text-xs whitespace-pre-wrap ${r.isError ? 'border-destructive text-destructive' : ''}`}>
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
