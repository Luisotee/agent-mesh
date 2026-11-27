import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Chip } from '@/components/ui/chip'
import { Skeleton } from '@/components/ui/skeleton'
import { CommandDialog, type AgentCommand } from '@/gen/agentmesh/v1/chat_pb'
import { cn } from '@/lib/utils'

/** Where the popup goes: above the field it belongs to, as wide as it, whatever box the field sits in (it is not inside it, so nothing clips it). */
function place(anchor: HTMLElement) {
  const r = anchor.getBoundingClientRect()
  return { left: r.left, width: Math.max(r.width, 360), bottom: window.innerHeight - r.top + 6 }
}

/**
 * The composer's `/` popup: the commands that match what was typed, each with its argument hint and description, the
 * highlighted one taken by Tab or Enter (`composer-commands.ts` `popupKey`) or a click. It is a portal anchored to the
 * field's element, and takes no focus: the field keeps the keys.
 */
export function CommandPopup({ anchor, id, items, loading, active, onActive, onPick }: { anchor: HTMLElement | null; id: string; items: AgentCommand[]; loading: boolean; active: number; onActive: (i: number) => void; onPick: (c: AgentCommand) => void }) {
  const [pos, setPos] = useState(() => (anchor ? place(anchor) : undefined))
  useLayoutEffect(() => {
    if (!anchor) return
    const go = () => setPos(place(anchor))
    go()
    window.addEventListener('resize', go)
    // The field grows as its text does: the popup follows its top edge.
    const ro = typeof ResizeObserver === 'undefined' ? undefined : new ResizeObserver(go)
    ro?.observe(anchor)
    return () => {
      window.removeEventListener('resize', go)
      ro?.disconnect()
    }
  }, [anchor])
  const list = useRef<HTMLUListElement>(null)
  useEffect(() => {
    list.current?.querySelector(`[id="${id}-${active}"]`)?.scrollIntoView?.({ block: 'nearest' })
  }, [active, id])
  if (!pos) return null
  return createPortal(
    <div
      className="fixed z-50 max-h-64 overflow-y-auto rounded-md border bg-popover p-1 text-[13px] text-foreground shadow-lg"
      style={{ left: pos.left, width: pos.width, bottom: pos.bottom }}
      // A click picks without taking the focus from the field.
      onMouseDown={(e) => e.preventDefault()}
    >
      {loading && items.length === 0 ? (
        <div role="status" aria-label="Loading commands" className="flex flex-col gap-2 p-2">
          <Skeleton className="w-2/3" />
          <Skeleton className="w-1/2" />
        </div>
      ) : (
        <ul ref={list} id={id} role="listbox" aria-label="Commands">
          {items.map((c, i) => (
            <li
              key={c.name}
              id={`${id}-${i}`}
              role="option"
              aria-selected={i === active}
              className={cn('flex cursor-default items-baseline gap-2 rounded-sm px-2 py-1.5 select-none', i === active && 'bg-accent text-accent-foreground')}
              onMouseMove={() => i !== active && onActive(i)}
              onClick={() => onPick(c)}
            >
              <span className="shrink-0 font-mono">/{c.name}</span>
              {c.argumentHint && <span className="max-w-[40%] shrink-0 truncate font-mono text-xs text-muted-foreground">{c.argumentHint}</span>}
              <span className="min-w-0 flex-1 truncate text-xs text-muted-foreground">{c.description}</span>
              {c.danger && <Chip className="shrink-0 text-destructive">ends the session</Chip>}
              {!c.danger && c.dialog !== CommandDialog.UNSPECIFIED && <Chip className="shrink-0">terminal</Chip>}
              {c.source && c.source !== 'builtin' && <Chip className="shrink-0">{c.source}</Chip>}
            </li>
          ))}
        </ul>
      )}
    </div>,
    document.body,
  )
}
