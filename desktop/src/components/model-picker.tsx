import { ChevronDown } from 'lucide-react'
import { useMemo, useRef, useState } from 'react'
import { checkModels, switchAgent } from '@/actions'
import { ActionMenu, type MenuEntry } from '@/components/ui/menu'
import { useApp, useHostState } from '@/context'
import type { Agent } from '@/gen/agentmesh/v1/types_pb'
import type { HostRuntime } from '@/model'
import { EFFORTS, familiesOf, pendingText, refusal, runsOn } from '@/models'
import { cn } from '@/lib/utils'
import { tiersOf } from './spawn-dialogs'
import { VendorIcon } from './vendor-icon'

/** "claude-sonnet-5-5" and "sonnet" read the same on the chip: the family, then the effort. */
const chipText = (a: Agent) => [a.model || a.kind, a.effort].filter(Boolean).join(' · ')

/**
 * The model and effort of the agent, by the composer, as claude.ai and Gemini put it: a chip with the vendor's mark that
 * opens the choices, the tiers of the host's settings, the models its list has for this CLI, and the efforts. A pick is
 * one `SwitchAgent`: the host reopens the same conversation (mid-turn it waits for the agent to be idle, and the chip
 * says what it will switch to). The host's refusal (already on it, at the quota reserve) shows as a toast.
 */
export function ModelPicker({ host, agent, disabled }: { host: HostRuntime; agent: Agent; disabled?: boolean }) {
  const app = useApp()
  const settings = useHostState(host, (s) => s.settings)
  const tiers = useMemo(() => tiersOf(settings, agent.kind), [settings, agent.kind])
  // The host's list is read the first time the chip is used, not for every composer drawn.
  const [families, setFamilies] = useState<string[]>([])
  const asked = useRef('')
  const load = () => {
    if (asked.current === `${host.info.id}/${agent.kind}`) return
    asked.current = `${host.info.id}/${agent.kind}`
    checkModels(host.client).then(
      (r) => setFamilies(familiesOf(r.kinds, agent.kind)),
      () => {},
    )
  }
  const pick = async (to: { tier?: string; model?: string; effort?: string }) => {
    try {
      await switchAgent(host.client, agent.id, to)
    } catch (e) {
      app.notify(refusal(e), 'error')
    }
  }
  const pending = pendingText(agent)
  const entries: MenuEntry[] = [
    ...(tiers.length > 0 ? [{ heading: 'Tier' } as const, ...tiers.map((t) => ({ label: t, checked: agent.tier === t, onSelect: () => void pick({ tier: t }) }))] : []),
    ...(families.length > 0 ? ['separator' as const, { heading: 'Model' }, ...families.map((f) => ({ label: f, checked: !!agent.model && agent.model.toLowerCase().includes(f.toLowerCase()), onSelect: () => void pick({ model: f }) }))] : []),
    'separator',
    { heading: 'Effort' },
    ...EFFORTS.map((x) => ({ label: x, checked: agent.effort === x, onSelect: () => void pick({ effort: x }) })),
  ]
  return (
    <ActionMenu
      side="top"
      entries={entries}
      trigger={
        <button
          type="button"
          disabled={disabled}
          aria-label={`Model: ${runsOn(agent) || agent.kind}`}
          title={pending || 'Change the model or effort: the same conversation goes on'}
          onClick={load}
          onPointerEnter={load}
          className={cn('flex h-8 min-w-0 max-w-56 items-center gap-1.5 rounded-md px-2 text-xs text-muted-foreground outline-none hover:bg-muted hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring disabled:pointer-events-none disabled:opacity-50 data-popup-open:bg-muted', pending && 'text-state-needs')}
        >
          <VendorIcon kind={agent.kind} model={agent.model} className="size-3.5" />
          <span className="truncate" data-testid="model-chip">{pending ? pending.replace(/^Switching to /, '→ ').replace(/…$/, ' at next idle') : chipText(agent)}</span>
          <ChevronDown aria-hidden className="size-3 shrink-0" />
        </button>
      }
    />
  )
}
