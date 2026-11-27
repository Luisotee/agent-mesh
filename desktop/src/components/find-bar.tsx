import { ChevronDown, ChevronUp, X } from 'lucide-react'
import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { useApp, useAppState } from '@/context'

/** The node the bar is put in: the first in the page, so the bar's own field is always match 1 of Chromium's find. */
function firstNode(): HTMLElement {
  let n = document.getElementById('find-bar-root')
  if (!n) {
    n = document.createElement('div')
    n.id = 'find-bar-root'
    document.body.prepend(n)
  }
  return n
}

/**
 * Find in the window (Mod+F): Chromium's find in page, run by main (`webContents.findInPage`). Enter goes to the next
 * match, Shift+Enter to the one before, Escape closes and keeps the selection. A terminal keeps xterm's own search.
 * Chromium also matches the text in the bar's own field: the bar sits first in the page, so that is match 1, which
 * the count leaves out and a step skips.
 */
export function FindBar() {
  const app = useApp()
  const open = useAppState((s) => s.findOpen)
  const [text, setText] = useState('')
  const [result, setResult] = useState<{ active: number; total: number }>()
  const find = app.api.find
  const field = useRef<HTMLInputElement>(null)
  const anchor = useRef<HTMLSpanElement>(null)
  const forward = useRef(true)
  const [at, setAt] = useState<{ top: number; right: number }>()
  // Chromium moves the focus to the match it selects: the field takes it back, so typing goes on there.
  useEffect(
    () =>
      open
        ? find?.onResult((r) => {
            field.current?.focus()
            // Match 1 is the field: step past it when there is anything else.
            if (r.active === 1 && r.total > 1 && field.current?.value) find.start(field.current.value, forward.current, true)
            else setResult({ active: Math.max(0, r.active - 1), total: Math.max(0, r.total - 1) })
          })
        : undefined,
    [open, find],
  )
  // Shown over the top right of the content, where the anchor is.
  useLayoutEffect(() => {
    if (!open) return
    const place = () => {
      const r = anchor.current?.parentElement?.getBoundingClientRect()
      if (r) setAt({ top: r.top + 8, right: window.innerWidth - r.right + 12 })
    }
    place()
    window.addEventListener('resize', place)
    return () => window.removeEventListener('resize', place)
  }, [open])
  const close = () => {
    find?.stop()
    setResult(undefined)
    app.openFind(false)
  }
  // Escape closes it wherever the focus went, unless something else (a dialog, a menu, the composer) took the key.
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || e.defaultPrevented) return
      find?.stop()
      setResult(undefined)
      app.openFind(false)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, find, app])
  if (!open) return null
  const go = (fwd: boolean, again: boolean, t = text) => {
    forward.current = fwd
    if (t) find?.start(t, fwd, again)
    else {
      find?.stop()
      setResult(undefined)
    }
  }
  return (
    <span ref={anchor} hidden>
      {createPortal(
    <div role="search" aria-label="Find in the window" style={at} className="fixed z-30 flex items-center gap-1 rounded-md border bg-popover p-1 shadow-lg">
      <Input
        ref={field}
        autoFocus
        aria-label="Find"
        value={text}
        placeholder="Find"
        className="h-7 w-52 text-[13px]"
        onChange={(e) => {
          setText(e.target.value)
          go(true, false, e.target.value)
        }}
        onKeyDown={(e) => {
          if (e.key === 'Enter') {
            e.preventDefault()
            go(!e.shiftKey, true)
          }
        }}
      />
      <span role="status" className="w-16 text-center text-xs text-muted-foreground tabular-nums">{text && result ? (result.total ? `${result.active} of ${result.total}` : 'No match') : ''}</span>
      <Button size="icon" variant="ghost" className="size-7" aria-label="Previous match" disabled={!text} onClick={() => go(false, true)}><ChevronUp /></Button>
      <Button size="icon" variant="ghost" className="size-7" aria-label="Next match" disabled={!text} onClick={() => go(true, true)}><ChevronDown /></Button>
      <Button size="icon" variant="ghost" className="size-7" aria-label="Close find" onClick={close}><X /></Button>
    </div>,
        firstNode(),
      )}
    </span>
  )
}
