import { ContextMenu } from '@base-ui/react/context-menu'
import { Menu as Base } from '@base-ui/react/menu'
import { Check, MoreHorizontal } from 'lucide-react'
import * as React from 'react'
import { cn } from '@/lib/utils'

/** One entry of a menu: an action, or a line between groups. */
export type MenuEntry =
  | { label: string; onSelect: () => void; destructive?: boolean; disabled?: boolean; hint?: string; /** Marks the entry that is current (a radio in a picker). */ checked?: boolean }
  | { heading: string }
  | 'separator'

const POPUP =
  'z-50 min-w-48 origin-(--transform-origin) rounded-md border bg-popover p-1 text-[13px] text-foreground shadow-lg outline-none transition-[scale,opacity] duration-100 ease-out motion-reduce:transition-none data-ending-style:scale-95 data-ending-style:opacity-0 data-starting-style:scale-95 data-starting-style:opacity-0'
const ITEM =
  'flex cursor-default items-center gap-3 rounded-sm px-2 py-1.5 outline-none select-none data-disabled:opacity-50 data-highlighted:bg-accent data-highlighted:text-accent-foreground'

function Items({ entries }: { entries: MenuEntry[] }) {
  // A heading and the entries after it (to the next heading or line) are one labelled group.
  const out: React.ReactNode[] = []
  let group: React.ReactNode[] | undefined
  let label = ''
  const flush = (key: number) => {
    if (group) out.push(<Base.Group key={`g${key}`}><Base.GroupLabel className="px-2 pt-1.5 pb-0.5 text-xs font-medium text-muted-foreground">{label}</Base.GroupLabel>{group}</Base.Group>)
    group = undefined
  }
  entries.forEach((e, i) => {
    if (e === 'separator') {
      flush(i)
      out.push(<Base.Separator key={i} className="my-1 h-px bg-border" />)
    } else if ('heading' in e) {
      flush(i)
      group = []
      label = e.heading
    } else {
      const radio = e.checked !== undefined
      const item = (
        <Base.Item key={i} disabled={e.disabled} onClick={e.onSelect} {...(radio ? { role: 'menuitemradio', 'aria-checked': e.checked } : {})} className={cn(ITEM, e.destructive && 'text-destructive')}>
          <span className="flex-1">{e.label}</span>
          {e.hint && <span className="text-xs text-muted-foreground">{e.hint}</span>}
          {e.checked && <Check aria-hidden className="size-3.5" />}
        </Base.Item>
      )
      if (group) group.push(item)
      else out.push(item)
    }
  })
  flush(entries.length)
  return out
}

/** A menu behind a button (the ⋯ on a row, the footer's + and Hosts). */
export function ActionMenu({ entries, trigger, side = 'bottom', align = 'start' }: { entries: MenuEntry[]; trigger: React.ReactElement<Record<string, unknown>>; side?: 'top' | 'bottom' | 'left' | 'right'; align?: 'start' | 'center' | 'end' }) {
  return (
    <Base.Root>
      <Base.Trigger render={trigger} />
      <Base.Portal>
        <Base.Positioner side={side} align={align} sideOffset={4} className="z-50">
          <Base.Popup className={POPUP}>
            <Items entries={entries} />
          </Base.Popup>
        </Base.Positioner>
      </Base.Portal>
    </Base.Root>
  )
}

/** The ⋯ button that opens a row's menu: shown on hover and focus of the row (`group`), and always while open. */
export function MoreButton({ label, entries, className }: { label: string; entries: MenuEntry[]; className?: string }) {
  return (
    <ActionMenu
      entries={entries}
      trigger={
        <button
          type="button"
          aria-label={label}
          className={cn('grid size-6 shrink-0 place-items-center rounded text-muted-foreground opacity-0 outline-none group-hover:opacity-100 group-focus-within:opacity-100 hover:bg-muted hover:text-foreground focus-visible:opacity-100 focus-visible:ring-2 focus-visible:ring-ring data-popup-open:opacity-100', className)}
        >
          <MoreHorizontal className="size-4" />
        </button>
      }
    />
  )
}

/**
 * Right-click (or Shift+F10, or the menu key) anywhere in the children opens the same entries as the row's ⋯ button.
 * The children keep their own element: the trigger renders as a `div` around them.
 */
export function RowContextMenu({ entries, children, className }: { entries: MenuEntry[]; children: React.ReactNode; className?: string }) {
  return (
    <ContextMenu.Root>
      <ContextMenu.Trigger className={className}>{children}</ContextMenu.Trigger>
      <ContextMenu.Portal>
        <ContextMenu.Positioner className="z-50">
          <ContextMenu.Popup className={POPUP}>
            <Items entries={entries} />
          </ContextMenu.Popup>
        </ContextMenu.Positioner>
      </ContextMenu.Portal>
    </ContextMenu.Root>
  )
}
