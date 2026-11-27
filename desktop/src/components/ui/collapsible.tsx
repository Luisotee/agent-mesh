import { Collapsible as Base } from '@base-ui/react/collapsible'
import { ChevronRight } from 'lucide-react'
import * as React from 'react'
import { cn } from '@/lib/utils'

/**
 * A section folded to one line until opened: its title, and a summary that shows while it is folded ("Run total $4.12").
 * `aria-label` is the title, so a test or screen reader finds the section open or not. A change of `reveal` (a
 * counter) opens it.
 */
export function Fold({ title, summary, defaultOpen = false, reveal, children, className }: { title: string; summary?: React.ReactNode; defaultOpen?: boolean; reveal?: number; children: React.ReactNode; className?: string }) {
  const [open, setOpen] = React.useState(defaultOpen || !!reveal)
  const [seen, setSeen] = React.useState(reveal)
  if (reveal !== seen) {
    setSeen(reveal)
    if (reveal) setOpen(true)
  }
  return (
    <Base.Root open={open} onOpenChange={setOpen} render={<section aria-label={title} className={cn('border-b px-3 py-2', className)} />}>
      <Base.Trigger className="group flex w-full items-center gap-1.5 rounded text-left text-xs font-semibold text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring">
        <ChevronRight className="size-3.5 transition-transform group-data-panel-open:rotate-90 motion-reduce:transition-none" />
        <span className="flex-1">{title}</span>
        {!open && summary !== undefined && <span className="font-normal text-foreground tabular-nums">{summary}</span>}
      </Base.Trigger>
      <Base.Panel className="pt-1.5">{children}</Base.Panel>
    </Base.Root>
  )
}
