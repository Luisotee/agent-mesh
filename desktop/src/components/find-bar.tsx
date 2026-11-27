import { ChevronDown, ChevronUp, X } from 'lucide-react'
import { useEffect, useState } from 'react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { useApp, useAppState } from '@/context'

/**
 * Find in the window (Mod+F): Chromium's find in page, run by main (`webContents.findInPage`). Enter goes to the next
 * match, Shift+Enter to the one before, Escape closes and keeps the selection. A terminal keeps xterm's own search.
 */
export function FindBar() {
  const app = useApp()
  const open = useAppState((s) => s.findOpen)
  const [text, setText] = useState('')
  const [result, setResult] = useState<{ active: number; total: number }>()
  const find = app.api.find
  useEffect(() => (open ? find?.onResult(setResult) : undefined), [open, find])
  if (!open) return null
  const close = () => {
    find?.stop()
    setResult(undefined)
    app.openFind(false)
  }
  const go = (forward: boolean, again: boolean, t = text) => {
    if (t) find?.start(t, forward, again)
    else {
      find?.stop()
      setResult(undefined)
    }
  }
  return (
    <div role="search" aria-label="Find in the window" className="absolute top-2 right-3 z-30 flex items-center gap-1 rounded-md border bg-popover p-1 shadow-lg">
      <Input
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
          } else if (e.key === 'Escape') {
            e.preventDefault()
            close()
          }
        }}
      />
      <span role="status" className="w-16 text-center text-xs text-muted-foreground tabular-nums">{text && result ? (result.total ? `${result.active} of ${result.total}` : 'No match') : ''}</span>
      <Button size="icon" variant="ghost" className="size-7" aria-label="Previous match" disabled={!text} onClick={() => go(false, true)}><ChevronUp /></Button>
      <Button size="icon" variant="ghost" className="size-7" aria-label="Next match" disabled={!text} onClick={() => go(true, true)}><ChevronDown /></Button>
      <Button size="icon" variant="ghost" className="size-7" aria-label="Close find" onClick={close}><X /></Button>
    </div>
  )
}
