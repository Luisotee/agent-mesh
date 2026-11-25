import * as React from 'react'
import { cn } from '@/lib/utils'

export function Badge({ className, ...props }: React.HTMLAttributes<HTMLSpanElement>) {
  return (
    <span
      className={cn('inline-flex items-center rounded border border-border px-1.5 py-px text-[11px] leading-4 text-muted-foreground', className)}
      {...props}
    />
  )
}
