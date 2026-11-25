import { Dialog as Base } from '@base-ui/react/dialog'
import { X } from 'lucide-react'
import * as React from 'react'
import { cn } from '@/lib/utils'

export const Dialog = Base.Root
export const DialogTrigger = Base.Trigger
export const DialogClose = Base.Close

export function DialogContent({ className, children, title, description, ...props }: Omit<React.ComponentProps<typeof Base.Popup>, 'title'> & { title: string; description?: string }) {
  return (
    <Base.Portal>
      <Base.Backdrop className="fixed inset-0 z-40 bg-black/60" />
      <Base.Popup
        className={cn('fixed top-1/2 left-1/2 z-50 flex max-h-[85vh] w-[min(32rem,calc(100vw-2rem))] -translate-x-1/2 -translate-y-1/2 flex-col gap-4 overflow-y-auto rounded-lg border bg-popover p-5 shadow-xl outline-none', className)}
        {...props}
      >
        <div className="flex flex-col gap-1">
          <Base.Title className="text-sm font-semibold">{title}</Base.Title>
          {description && <Base.Description className="text-xs text-muted-foreground">{description}</Base.Description>}
        </div>
        {children}
        <Base.Close aria-label="Close" className="absolute top-3 right-3 rounded p-1 text-muted-foreground hover:bg-accent hover:text-accent-foreground">
          <X className="size-4" />
        </Base.Close>
      </Base.Popup>
    </Base.Portal>
  )
}
