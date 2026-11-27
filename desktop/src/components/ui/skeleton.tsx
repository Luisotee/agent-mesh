import * as React from 'react'
import { cn } from '@/lib/utils'

/**
 * A placeholder bar in the shape of what is loading, with a slow shimmer (still when the user prefers reduced motion).
 * It replaces "Loading…" texts; the region around it carries the `status` role and its label.
 */
export function Skeleton({ className, ...props }: React.HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      aria-hidden
      className={cn('h-3 rounded-md bg-linear-to-r from-muted via-accent to-muted bg-size-[200%_100%] animate-shimmer motion-reduce:animate-none', className)}
      {...props}
    />
  )
}

/** Rows of skeleton lines for a list or a block of text, `label` read out as what is loading (none inside a labelled region). */
export function SkeletonLines({ label, lines = 3, className }: { label?: string; lines?: number; className?: string }) {
  return (
    <div role={label ? 'status' : undefined} aria-label={label} className={cn('flex flex-col gap-2 py-1', className)}>
      {label && <span className="sr-only">{label}</span>}
      {Array.from({ length: lines }, (_, i) => (
        <Skeleton key={i} className={i === lines - 1 ? 'w-2/3' : i % 2 ? 'w-5/6' : 'w-full'} />
      ))}
    </div>
  )
}
