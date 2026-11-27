import * as React from 'react'
import { cn } from '@/lib/utils'

/** A small piece of meta (a repo, a CLI, a model, a host) as a quiet pill, in place of "a · b · c" strings. */
export function Chip({ className, children, ...props }: React.HTMLAttributes<HTMLSpanElement>) {
  return (
    <span className={cn('inline-flex max-w-full min-w-0 items-center gap-1 rounded-full bg-muted px-1.5 text-xs leading-5 text-muted-foreground', className)} {...props}>
      {/* The ellipsis needs a block of text: a flex container's own text can't show one. */}
      <span className="truncate">{children}</span>
    </span>
  )
}

/** A row of chips that wraps nothing: what doesn't fit is cut at the end. */
export function Chips({ className, ...props }: React.HTMLAttributes<HTMLSpanElement>) {
  return <span className={cn('flex min-w-0 items-center gap-1 overflow-hidden', className)} {...props} />
}
