import { FitAddon } from '@xterm/addon-fit'
import { SearchAddon } from '@xterm/addon-search'
import { WebLinksAddon } from '@xterm/addon-web-links'
import { Terminal } from '@xterm/xterm'
import '@xterm/xterm/css/xterm.css'
import { ChevronDown, ChevronUp, Search, X } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { openTerminal } from '@/actions'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import type { HostRuntime } from '@/model'
import { TerminalSession, type TermState } from '@/terminal/session'
import { activeTheme, monoStack, onThemeChange } from '@/theme'

const encoder = new TextEncoder()

/** What the host's terminal size says when another client made it smaller than this window's: "" when they agree or it is larger. */
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
 * An agent's terminal: xterm.js on the terminal stream. The session (src/terminal/session.ts) speaks the protocol;
 * this wires it to xterm, sizes the terminal to its box, and adds search and links.
 */
export function TerminalView({ host, agentId, active, panelId, tabId }: { host: HostRuntime; agentId: string; active: boolean; panelId: string; tabId: string }) {
  const box = useRef<HTMLDivElement>(null)
  const term = useRef<Terminal>(null)
  const search = useRef<SearchAddon>(null)
  const fit = useRef<FitAddon>(null)
  const [state, setState] = useState<TermState>({ kind: 'connecting' })
  const [shared, setShared] = useState('')
  const [searching, setSearching] = useState(false)
  const [query, setQuery] = useState('')

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
    search.current = s
    fit.current = f
    let hostSize: { cols: number; rows: number } | undefined
    const noteSize = () => setShared(sharedSize(hostSize, { cols: t.cols, rows: t.rows }))
    const session = new TerminalSession({
      ticket: async () => {
        const r = await openTerminal(host.client, agentId, true)
        return { path: r.path, input: r.input }
      },
      socket: (path, onEvent) => host.openSocket(path, onEvent),
      sink: {
        write: (d) => t.write(d),
        reset: () => t.reset(),
        size: () => ({ cols: t.cols, rows: t.rows }),
      },
      onState: setState,
      onHostSize: (cols, rows) => {
        hostSize = { cols, rows }
        noteSize()
      },
    })
    t.onData((d) => session.input(encoder.encode(d)))
    t.onBinary((d) => session.input(Uint8Array.from(d, (ch) => ch.charCodeAt(0))))
    t.onResize(({ cols, rows }) => {
      session.resize(cols, rows)
      noteSize()
    })
    const refit = () => {
      // A box that is hidden or not laid out yet has no size to fit to.
      if (el.clientWidth > 0 && el.clientHeight > 0) f.fit()
    }
    refit()
    session.start()
    const ro = new ResizeObserver(refit)
    ro.observe(el)
    // Another theme repaints this terminal in its colours and font.
    const offTheme = onThemeChange((th) => {
      t.options.theme = th.terminal
      t.options.fontFamily = monoStack(th)
      refit()
    })
    return () => {
      offTheme()
      ro.disconnect()
      session.dispose()
      t.dispose()
      term.current = null
    }
  }, [host, agentId])

  useEffect(() => {
    if (active) {
      fit.current?.fit()
      term.current?.focus()
    }
  }, [active])

  const find = (back: boolean) => {
    if (!query) return
    if (back) search.current?.findPrevious(query)
    else search.current?.findNext(query)
  }

  return (
    <div id={panelId} role="tabpanel" aria-labelledby={tabId} className="flex min-h-0 flex-1 flex-col gap-2" hidden={!active}>
      <div className="flex items-center gap-2 text-xs">
        <span role="status" data-testid="terminal-status" className="text-muted-foreground">{termStatus(state)}{shared && state.kind === 'live' ? ` (${shared})` : ''}</span>
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
    </div>
  )
}
