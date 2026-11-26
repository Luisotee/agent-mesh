import { Dialog as Base } from '@base-ui/react/dialog'
import { Search } from 'lucide-react'
import { useEffect, useMemo, useRef, useState } from 'react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { useApp, useAppState } from '@/context'
import { buildCommands, chordOf, filterCommands, type Command } from '@/commands'
import { displayChord } from '@/keys'
import { cn } from '@/lib/utils'

/**
 * The command palette: every agent, run, host and waiting item to go to, and every action the registry has for
 * them, filtered as you type. A command that needs a line of text asks for it, and one that stops something asks to
 * be confirmed.
 */
export function CommandPalette() {
  const app = useApp()
  const open = useAppState((s) => s.paletteOpen)
  return (
    <Base.Root open={open} onOpenChange={(o) => app.openPalette(o)}>
      <Base.Portal>
        <Base.Backdrop className="fixed inset-0 z-40 bg-black/50" />
        <Base.Popup
          aria-label="Command palette"
          className="fixed top-[12vh] left-1/2 z-50 flex max-h-[70vh] w-[min(40rem,calc(100vw-2rem))] -translate-x-1/2 flex-col overflow-hidden rounded-lg border bg-popover shadow-xl outline-none"
        >
          <Base.Title className="sr-only">Command palette</Base.Title>
          <Base.Description className="sr-only">Type to filter; Enter runs the highlighted command; Escape closes.</Base.Description>
          <Palette onDone={() => app.openPalette(false)} />
        </Base.Popup>
      </Base.Portal>
    </Base.Root>
  )
}

type Step = { kind: 'search' } | { kind: 'ask'; cmd: Command } | { kind: 'confirm'; cmd: Command }

function Palette({ onDone }: { onDone: () => void }) {
  const app = useApp()
  // Built once when the palette opens: the list doesn't shift under the cursor while the host changes.
  const cmds = useMemo(() => buildCommands(app), [app])
  const [query, setQuery] = useState('')
  const [at, setAt] = useState(0)
  const [step, setStep] = useState<Step>({ kind: 'search' })
  const [text, setText] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const shown = useMemo(() => filterCommands(cmds, query), [cmds, query])
  const list = useRef<HTMLUListElement>(null)
  useEffect(() => setAt(0), [query])
  useEffect(() => {
    list.current?.querySelector('[aria-selected="true"]')?.scrollIntoView?.({ block: 'nearest' })
  }, [at])

  async function run(cmd: Command, input?: string) {
    if (busy) return
    setBusy(true)
    setError('')
    try {
      await cmd.run(input)
      onDone()
    } catch (e) {
      setError(e instanceof Error ? e.message.replace(/^\[[a-z_]+\]\s*/, '') : String(e))
    } finally {
      setBusy(false)
    }
  }
  function choose(cmd: Command) {
    setError('')
    if (cmd.ask) {
      setText('')
      setStep({ kind: 'ask', cmd })
    } else if (cmd.confirm) setStep({ kind: 'confirm', cmd })
    else void run(cmd)
  }
  const back = () => {
    setStep({ kind: 'search' })
    setError('')
  }

  if (step.kind === 'confirm') {
    return (
      <div className="flex flex-col gap-3 p-4" onKeyDown={(e) => e.key === 'Escape' && (e.stopPropagation(), back())}>
        <p className="text-sm">{step.cmd.confirm}</p>
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={back}>Cancel</Button>
          <Button variant="destructive" autoFocus disabled={busy} onClick={() => void run(step.cmd)}>{step.cmd.title}</Button>
        </div>
        {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
      </div>
    )
  }
  if (step.kind === 'ask') {
    return (
      <form
        className="flex flex-col gap-2 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          void run(step.cmd, text)
        }}
        onKeyDown={(e) => e.key === 'Escape' && (e.stopPropagation(), back())}
      >
        <label className="text-xs text-muted-foreground" htmlFor="palette-ask">{step.cmd.ask!.prompt}</label>
        <Input id="palette-ask" autoFocus value={text} placeholder={step.cmd.ask!.placeholder} onChange={(e) => setText(e.target.value)} />
        <div className="flex justify-end gap-2">
          <Button type="button" variant="ghost" onClick={back}>Back</Button>
          <Button type="submit" disabled={busy || !text.trim()}>{step.cmd.title.replace(/…$/, '')}</Button>
        </div>
        {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
      </form>
    )
  }
  return (
    <>
      <div className="flex items-center gap-2 border-b px-3">
        <Search className="size-4 shrink-0 text-muted-foreground" />
        <input
          autoFocus
          role="combobox"
          aria-label="Search commands"
          aria-expanded="true"
          aria-controls="palette-list"
          aria-activedescendant={shown[at] ? `palette-${at}` : undefined}
          value={query}
          placeholder="Go to an agent, run a command…"
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'ArrowDown') {
              e.preventDefault()
              setAt((i) => Math.min(shown.length - 1, i + 1))
            } else if (e.key === 'ArrowUp') {
              e.preventDefault()
              setAt((i) => Math.max(0, i - 1))
            } else if (e.key === 'Enter' && shown[at]) {
              e.preventDefault()
              choose(shown[at]!)
            }
          }}
          className="h-11 min-w-0 flex-1 bg-transparent text-sm outline-none placeholder:text-muted-foreground"
        />
      </div>
      <ul ref={list} id="palette-list" role="listbox" aria-label="Commands" className="min-h-0 flex-1 overflow-y-auto py-1">
        {shown.length === 0 && <li className="px-3 py-6 text-center text-xs text-muted-foreground">Nothing matches.</li>}
        {shown.map((c, i) => {
          const chord = chordOf(app, c)
          return (
            <li
              key={c.id}
              id={`palette-${i}`}
              role="option"
              aria-selected={i === at}
              onMouseMove={() => setAt(i)}
              onClick={() => choose(c)}
              className={cn('flex cursor-pointer items-baseline gap-2 px-3 py-1.5 text-[13px]', i === at && 'bg-accent text-accent-foreground')}
            >
              <span className="w-24 shrink-0 text-[11px] tracking-wide text-muted-foreground uppercase">{c.group}</span>
              <span className="min-w-0 flex-1 truncate">
                {c.title}
                {c.detail && <span className="ml-2 text-xs text-muted-foreground">{c.detail}</span>}
              </span>
              {chord && <kbd className="shrink-0 rounded border px-1 font-mono text-[11px] text-muted-foreground">{displayChord(chord)}</kbd>}
            </li>
          )
        })}
      </ul>
      {error && <p role="alert" className="border-t px-3 py-2 text-xs text-destructive">{error}</p>}
    </>
  )
}
