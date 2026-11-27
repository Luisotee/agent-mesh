import { X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { useApp, useAppState } from '@/context'
import { cn } from '@/lib/utils'

/** The app's own toasts (`notify`): what failed or finished, at the bottom left so they never cover the composer's buttons. */
export function Notices() {
  const app = useApp()
  const rows = useAppState((s) => s.notices)
  if (rows.length === 0) return null
  return (
    <div role="region" aria-label="Notices" className="pointer-events-none fixed bottom-4 left-4 z-50 flex flex-col gap-2">
      {rows.map((n) => (
        <div key={n.id} role={n.tone === 'error' ? 'alert' : 'status'} className={cn('pointer-events-auto flex w-80 items-start gap-2 rounded-lg border bg-popover p-3 text-[13px] shadow-lg', n.tone === 'error' && 'border-destructive/50')}>
          <p className={cn('min-w-0 flex-1 break-words', n.tone === 'error' && 'text-destructive')}>{n.text}</p>
          <Button size="icon" variant="ghost" className="size-6 shrink-0" aria-label="Dismiss" onClick={() => app.dismissNotice(n.id)}><X className="size-3.5" /></Button>
        </div>
      ))}
    </div>
  )
}
