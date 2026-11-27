import { Tooltip as Base } from '@base-ui/react/tooltip'
import * as React from 'react'
import { cn } from '@/lib/utils'

/** One provider at the app's root, so moving between triggers shows the next tooltip at once. */
export const TooltipProvider = Base.Provider

/**
 * A hint on hover or focus, for an icon button whose `aria-label` already names it (the tooltip is for sighted users,
 * as a `title` was). The child is the trigger: it keeps its own element and props.
 */
export function Tip({ label, side = 'bottom', children }: { label: React.ReactNode; side?: 'top' | 'bottom' | 'left' | 'right'; children: React.ReactElement<Record<string, unknown>> }) {
  return (
    <Base.Root>
      <Base.Trigger render={children} />
      <Base.Portal>
        <Base.Positioner side={side} sideOffset={6} className="z-50">
          <Base.Popup
            className={cn(
              'origin-(--transform-origin) rounded-md border bg-popover px-2 py-1 text-xs text-foreground shadow-md',
              'transition-[scale,opacity] duration-100 ease-out motion-reduce:transition-none data-ending-style:scale-95 data-ending-style:opacity-0 data-instant:transition-none data-starting-style:scale-95 data-starting-style:opacity-0',
            )}
          >
            {label}
          </Base.Popup>
        </Base.Positioner>
      </Base.Portal>
    </Base.Root>
  )
}
