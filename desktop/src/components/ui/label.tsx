import * as React from 'react'
import { cn } from '@/lib/utils'

export function Label({ className, ...props }: React.LabelHTMLAttributes<HTMLLabelElement>) {
  return <label className={cn('text-xs font-medium text-muted-foreground', className)} {...props} />
}

/** A label above its control, tied to it (and to its hint) for screen readers. */
export function Field({ label, hint, children }: { label: string; hint?: string; children: React.ReactElement<{ id?: string; 'aria-describedby'?: string }> }) {
  const id = React.useId()
  return (
    <div className="flex flex-col gap-1">
      <Label htmlFor={id}>{label}</Label>
      {React.cloneElement(children, { id, 'aria-describedby': hint ? `${id}-hint` : undefined })}
      {hint && (
        <p id={`${id}-hint`} className="text-xs text-muted-foreground">
          {hint}
        </p>
      )}
    </div>
  )
}
