import { FitAddon } from '@xterm/addon-fit'
import { SearchAddon } from '@xterm/addon-search'
import { TERMINAL_SEARCH } from '@/keys'
import { WebLinksAddon } from '@xterm/addon-web-links'
import { Terminal } from '@xterm/xterm'
import '@xterm/xterm/css/xterm.css'
import { ChevronDown, ChevronUp, Search, X } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { addAttachment, openShellTerminal, openTerminal } from '@/actions'
import { attachName, attachProblem, terminalAttachText } from '@/attachments'
import { useApp, useHostState } from '@/context'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import type { HostRuntime } from '@/model'
import { TerminalSession, type TermState } from '@/terminal/session'
import { activeTheme, monoStack, onThemeChange } from '@/theme'

const encoder = new TextEncoder()

/** What the host's terminal size says when another client made it smaller than the size this window asked for: "" when they agree or it is larger. */
/** The sizes the terminal host accepts (daemon/internal/termhost/ws.go): it clamps what a client asks for. */
export function clampSize(cols: number, rows: number): { cols: number; rows: number } {
  return { cols: Math.min(Math.max(cols, 20), 500), rows: Math.min(Math.max(rows, 5), 300) }
}

export function sharedSize(host: { cols: number; rows: number } | undefined, local: { cols: number; rows: number }): string {
  if (!host || (host.cols >= local.cols && host.rows >= local.rows)) return ''
  return `${host.cols}×${host.rows}, as another client has it`
}

export function termStatus(s: TermState): string {
  switch (s.kind) {
    case 'connecting':
      return 'Connecting…'
    case 'replaying':
      return 'Catching up…'
    case 'live':
      return s.input ? 'Live' : 'Live, read only'
    case 'reconnecting':
      return `Reconnecting (${s.error})`
    case 'exited':
      return s.signal ? `Exited on signal ${s.signal}` : `Exited with code ${s.exitCode}`
    case 'closed':
      return `Closed: ${s.message}`
  }
}

/**
 * An agent's terminal, or a project shell's (`shellId`: the same stream, another ticket): xterm.js on the terminal stream. The session (src/terminal/session.ts) speaks the protocol;
 * this wires it to xterm, sizes the terminal to its box, and adds search and links.
 */
export function TerminalView({ host, agentId = '', shellId = '', active }: { host: HostRuntime; agentId?: string; shellId?: string; active: boolean }) {
  const box = useRef<HTMLDivElement>(null)
  const term = useRef<Terminal>(null)
  const search = useRef<SearchAddon>(null)
  // Fits the terminal to its box and, the first time it has one, attaches it.
  const refitRef = useRef<() => void>(undefined)
  const [state, setState] = useState<TermState>({ kind: 'connecting' })
  const [shared, setShared] = useState('')
  const [searching, setSearching] = useState(false)
  const [query, setQuery] = useState('')
  const root = useRef<HTMLDivElement>(null)
  const app = useApp()
  // An agent's terminal takes a pasted or dropped file: it is stored on the host and its path typed, as its CLI takes a file
  // (`terminalAttachText`). A project shell has no agent to store it for.
  const kind = useHostState(host, (s) => s.agents[agentId]?.kind ?? '')
  const [sending, setSending] = useState(false)
  const files = useRef<(files: File[]) => void>(undefined)
  files.current = (list) => {
    if (!agentId) return
    if (!(state.kind === 'live' && state.input)) return app.notify('This terminal takes no typing, so a file can\'t be pasted into it.', 'error')
    void (async () => {
      setSending(true)
      try {
        for (const f of list) {
          const problem = attachProblem(f)
          if (problem) {
            app.notify(`${f.name || 'The file'}: ${problem}`, 'error')
            continue
          }
          const res = await addAttachment(host.client, agentId, attachName(f), new Uint8Array(await f.arrayBuffer()))
          term.current?.paste(terminalAttachText(kind, res.path))
        }
      } catch (e) {
        app.notify(e instanceof Error ? e.message.replace(/^\[[a-z_]+\]\s*/, '') : String(e), 'error')
      } finally {
        setSending(false)
      }
    })()
  }
  useEffect(() => {
    const el = root.current
    const open = () => setSearching(true)
    el?.addEventListener(TERMINAL_SEARCH, open)
    return () => el?.removeEventListener(TERMINAL_SEARCH, open)
  }, [])

  useEffect(() => {
    const el = box.current
    if (!el) return
    const t = new Terminal({
      fontFamily: monoStack(activeTheme),
      fontSize: 13,
      cursorBlink: true,
      scrollback: 10_000,
      theme: activeTheme.terminal,
      allowProposedApi: true,
    })
    const f = new FitAddon()
    const s = new SearchAddon()
    t.loadAddon(f)
    t.loadAddon(s)
    t.loadAddon(new WebLinksAddon((_e, url) => window.desktop.openExternal(url)))
    t.open(el)
    term.current = t
    // Before xterm's own handlers (a capture listener on its parent): a clipboard or drop that holds files is not text for it.
    const onPaste = (e: ClipboardEvent) => {
      const list = [...(e.clipboardData?.files ?? [])]
      if (list.length === 0 || !agentId) return
      e.preventDefault()
      e.stopPropagation()
      files.current?.(list)
    }
    const hasFiles = (e: DragEvent) => !!agentId && !!e.dataTransfer?.types.includes('Files')
    const onDragOver = (e: DragEvent) => {
      if (hasFiles(e)) e.preventDefault()
    }
    const onDrop = (e: DragEvent) => {
      if (!hasFiles(e)) return
      e.preventDefault()
      files.current?.([...e.dataTransfer!.files])
    }
    el.addEventListener('paste', onPaste, true)
    el.addEventListener('dragover', onDragOver)
    el.addEventListener('drop', onDrop)
    search.current = s
    // The size the host was told for this window (it clamps what it is told, and takes a resize only from a client with input rights),
    // so another client is blamed only when the host's size is below that.
    let sent = { cols: 0, rows: 0 }
    let canType = false
    const tell = (cols: number, rows: number) => (sent = clampSize(cols, rows))
    const session = new TerminalSession({
      ticket: async () => {
        const r = shellId ? await openShellTerminal(host.client, shellId, true) : await openTerminal(host.client, agentId, true)
        return { path: r.path, input: r.input }
      },
      socket: (path, onEvent) => host.openSocket(path, onEvent),
      sink: {
        write: (d) => t.write(d),
        reset: () => t.reset(),
        size: () => {
          tell(t.cols, t.rows)
          return { cols: t.cols, rows: t.rows }
        },
      },
      onState: (s) => {
        canType = s.kind === 'live' && s.input
        setState(s)
      },
      // Looked at when the host says its size, not when this window changes: until the host answers a resize it still has the old size.
      onHostSize: (cols, rows) => setShared(sharedSize({ cols, rows }, sent)),
    })
    t.onData((d) => session.input(encoder.encode(d)))
    t.onBinary((d) => session.input(Uint8Array.from(d, (ch) => ch.charCodeAt(0))))
    t.onResize(({ cols, rows }) => {
      session.resize(cols, rows)
      if (canType) tell(cols, rows)
    })
    let started = false
    const refit = () => {
      // A box that is hidden or not laid out yet has no size to fit to.
      // Nor does one whose cells xterm couldn't measure yet (fit would keep 80x24).
      if (el.clientWidth === 0 || el.clientHeight === 0 || !f.proposeDimensions()) return
      f.fit()
      // Attached only once it has its real size: a repaint drawn at xterm's default 80x24 and then reflowed to the
      // box's width loses lines (the trailing blanks of a row wrap), and the host would shrink the program to 80x24.
      if (!started) {
        started = true
        session.start()
      }
    }
    refitRef.current = refit
    refit()
    // A box that is being animated (the side panel sliding) changes size at every frame: the program is told once it settles.
    let settle: ReturnType<typeof setTimeout> | undefined
    const ro = new ResizeObserver(() => {
      // The first fit is not waited for.
      if (!started) return refit()
      clearTimeout(settle)
      settle = setTimeout(refit, 120)
    })
    ro.observe(el)
    // Another theme repaints this terminal in its colours and font.
    const offTheme = onThemeChange((th) => {
      t.options.theme = th.terminal
      t.options.fontFamily = monoStack(th)
      refit()
    })
    return () => {
      el.removeEventListener('paste', onPaste, true)
      el.removeEventListener('dragover', onDragOver)
      el.removeEventListener('drop', onDrop)
      offTheme()
      clearTimeout(settle)
      ro.disconnect()
      session.dispose()
      t.dispose()
      term.current = null
      search.current = null
      refitRef.current = undefined
    }
  }, [host, agentId, shellId])

  useEffect(() => {
    if (active) {
      refitRef.current?.()
      term.current?.focus()
    }
  }, [active])

  const find = (back: boolean) => {
    if (!query) return
    if (back) search.current?.findPrevious(query)
    else search.current?.findNext(query)
  }

  return (
    <section ref={root} aria-label="Terminal" className="flex min-h-0 flex-1 flex-col gap-2" hidden={!active}>
      <div className="flex items-center gap-2 text-xs">
        <span role="status" data-testid="terminal-status" className="text-muted-foreground">{termStatus(state)}{shared && state.kind === 'live' ? ` (${shared})` : ''}{sending ? ', storing a file…' : ''}</span>
        <span className="flex-1" />
        {searching ? (
          <form
            className="flex items-center gap-1"
            onSubmit={(e) => {
              e.preventDefault()
              find(false)
            }}
          >
            <Input autoFocus aria-label="Search the terminal" className="h-7 w-48" value={query} onChange={(e) => setQuery(e.target.value)} />
            <Button size="icon" variant="ghost" className="size-7" aria-label="Previous match" onClick={() => find(true)}><ChevronUp /></Button>
            <Button size="icon" variant="ghost" className="size-7" aria-label="Next match" type="submit"><ChevronDown /></Button>
            <Button
              size="icon"
              variant="ghost"
              className="size-7"
              aria-label="Close search"
              onClick={() => {
                setSearching(false)
                search.current?.clearDecorations()
                term.current?.focus()
              }}
            >
              <X />
            </Button>
          </form>
        ) : (
          <Button size="sm" variant="ghost" onClick={() => setSearching(true)}><Search /> Search</Button>
        )}
      </div>
      <div ref={box} data-testid="terminal" className="min-h-0 flex-1 overflow-hidden rounded-md border bg-surface p-2" />
    </section>
  )
}
