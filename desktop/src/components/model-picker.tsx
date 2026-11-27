import { ChevronDown } from 'lucide-react'
import { useMemo, useRef, useState } from 'react'
import { checkModels, switchAgent } from '@/actions'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Field } from '@/components/ui/label'
import { ActionMenu, type MenuEntry } from '@/components/ui/menu'
import { useApp, useHostState } from '@/context'
import { AgentPhase, type Agent } from '@/gen/agentmesh/v1/types_pb'
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
  // "Other model…": a model id the host's list doesn't have (or a host that listed none).
  const [other, setOther] = useState<string>()
  const load = () => {
    if (asked.current === `${host.info.id}/${agent.kind}`) return
    const key = `${host.info.id}/${agent.kind}`
    asked.current = key
    checkModels(host.client).then(
      (r) => setFamilies(familiesOf(r.kinds, agent.kind)),
      () => {
        // Asked again at the next hover or click: the host may only have been busy.
        if (asked.current === key) asked.current = ''
      },
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
  // The tier it runs on now: an executing worker's is its execution tier.
  const runTier = agent.phase === AgentPhase.EXECUTING && agent.execTier ? agent.execTier : agent.tier
  // The listed family the agent runs on: the longest name its model contains (`gpt-5-codex` is not also `gpt-5`).
  const current = families.filter((f) => !!agent.model && agent.model.toLowerCase().includes(f.toLowerCase())).sort((a, b) => b.length - a.length)[0]
  const entries: MenuEntry[] = [
    ...(tiers.length > 0 ? [{ heading: 'Tier' } as const, ...tiers.map((t) => ({ label: t, checked: runTier === t, onSelect: () => void pick({ tier: t }) }))] : []),
    ...(tiers.length > 0 ? ['separator' as const] : []),
    { heading: 'Model' },
    ...families.map((f) => ({ label: f, checked: f === current, onSelect: () => void pick({ model: f }) })),
    { label: 'Other model…', onSelect: () => setOther('') },
    'separator' as const,
    { heading: 'Effort' },
    ...EFFORTS.map((x) => ({ label: x, checked: agent.effort === x, onSelect: () => void pick({ effort: x }) })),
  ]
  return (
    <>
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
          <span className="truncate" data-testid="model-chip">{pending ? pending.replace(/^Switching to /, '→ ').replace(/ \(.*\)…$|…$/, '') : chipText(agent)}</span>
          <ChevronDown aria-hidden className="size-3 shrink-0" />
        </button>
      }
    />
    {other !== undefined && (
      <Dialog open onOpenChange={(o) => !o && setOther(undefined)}>
        <DialogContent title="Other model" description={`A model id of ${agent.kind} that the host's list doesn't show. The same conversation goes on, as with any pick.`}>
          <form
            className="flex flex-col gap-3"
            onSubmit={(e) => {
              e.preventDefault()
              // The dialog is a portal inside the composer's form in the React tree: its submit must not send the draft.
              e.stopPropagation()
              const model = other.trim()
              if (!model) return
              setOther(undefined)
              void pick({ model })
            }}
          >
            <Field label="Model">
              <Input autoFocus value={other} onChange={(e) => setOther(e.target.value)} placeholder="claude-opus-5-5" spellCheck={false} />
            </Field>
            <div className="flex justify-end gap-2">
              <Button type="button" variant="ghost" onClick={() => setOther(undefined)}>Cancel</Button>
              <Button type="submit" disabled={!other.trim()}>Switch</Button>
            </div>
          </form>
        </DialogContent>
      </Dialog>
    )}
    </>
  )
}
