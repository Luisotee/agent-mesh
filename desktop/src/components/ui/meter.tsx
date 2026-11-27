import { Meter as Base } from '@base-ui/react/meter'
import { cn } from '@/lib/utils'

export type MeterTone = 'ok' | 'warn' | 'over'

const TONE: Record<MeterTone, string> = { ok: 'bg-state-done', warn: 'bg-state-needs', over: 'bg-destructive' }

/** A small bar for a share of a quota (0 to 100): green, amber from the reserve on, red when used up. */
export function Meter({ label, value, tone, className }: { label: string; value: number; tone: MeterTone; className?: string }) {
  const v = Math.min(100, Math.max(0, value))
  return (
    <Base.Root value={v} aria-label={label} className={cn('min-w-0', className)}>
      <Base.Track className="h-1.5 overflow-hidden rounded-full bg-muted">
        <Base.Indicator className={cn('h-full rounded-full', TONE[tone])} />
      </Base.Track>
    </Base.Root>
  )
}
