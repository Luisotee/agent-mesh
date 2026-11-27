import type { Agent } from '@/gen/agentmesh/v1/types_pb'
import { pendingText, runsOn } from '@/models'
import { cn } from '@/lib/utils'
import { VendorIcon } from './vendor-icon'

/**
 * The CLI, model and effort an agent runs on, with the vendor's mark and a reopen under way. Read-only: the model is
 * changed from the composer's picker (`model-picker.tsx`), the one place for it.
 */
export function ModelControl({ agent, className }: { agent: Agent; className?: string }) {
  const pending = pendingText(agent)
  return (
    <div className={cn('flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-muted-foreground', className)}>
      <VendorIcon kind={agent.kind} model={agent.model} />
      <span data-testid="runs-on">{runsOn(agent) || agent.kind}</span>
      {pending && <span role="status" className="text-state-needs">{pending}</span>}
    </div>
  )
}
