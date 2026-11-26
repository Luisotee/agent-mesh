import { useEffect, useMemo, useState } from 'react'
import { checkModels, switchAgent } from '@/actions'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogTrigger } from '@/components/ui/dialog'
import { Field } from '@/components/ui/label'
import { Input, Select } from '@/components/ui/input'
import { useHostState } from '@/context'
import type { Agent } from '@/gen/agentmesh/v1/types_pb'
import type { HostRuntime } from '@/model'
import { EFFORTS, familiesOf, pendingText, refusal, runsOn } from '@/models'
import { cn } from '@/lib/utils'
import { VendorIcon } from './vendor-icon'
import { tiersOf } from './spawn-dialogs'

/**
 * The CLI, model and effort an agent runs on, with the vendor's mark, a reopen under way, and a control to change them: a
 * tier, a model of the host's list for its CLI, or an effort alone. The host reopens the same conversation (`SwitchAgent`).
 */
export function ModelControl({ host, agent, className }: { host: HostRuntime; agent: Agent; className?: string }) {
  const pending = pendingText(agent)
  return (
    <div className={cn('flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-muted-foreground', className)}>
      <VendorIcon kind={agent.kind} model={agent.model} />
      <span data-testid="runs-on">{runsOn(agent) || agent.kind}</span>
      {pending && <span role="status" className="text-state-needs">{pending}</span>}
      <ChangeModel host={host} agent={agent} />
    </div>
  )
}

type By = 'tier' | 'model' | 'effort'

function ChangeModel({ host, agent }: { host: HostRuntime; agent: Agent }) {
  const [open, setOpen] = useState(false)
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" variant="ghost" className="h-5 px-1.5 text-[11px]" aria-label={`Change the model of ${agent.name}`} />}>Change</DialogTrigger>
      <DialogContent title={`Change ${agent.name}'s model`} description="The host reopens the same conversation on the new model: nothing is briefed again.">
        <ChangeForm host={host} agent={agent} onDone={() => setOpen(false)} />
      </DialogContent>
    </Dialog>
  )
}

function ChangeForm({ host, agent, onDone }: { host: HostRuntime; agent: Agent; onDone: () => void }) {
  const settings = useHostState(host, (s) => s.settings)
  const tiers = useMemo(() => tiersOf(settings, agent.kind), [settings, agent.kind])
  const [by, setBy] = useState<By>('tier')
  const [tier, setTier] = useState(agent.tier && tiers.includes(agent.tier) ? agent.tier : (tiers[0] ?? ''))
  const [model, setModel] = useState('')
  const [effort, setEffort] = useState('')
  const [families, setFamilies] = useState<string[]>([])
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  // The host's list for this CLI, read when the dialog opens; without it a model is typed.
  useEffect(() => {
    let alive = true
    checkModels(host.client).then(
      (r) => alive && setFamilies(familiesOf(r.kinds, agent.kind)),
      () => {},
    )
    return () => {
      alive = false
    }
  }, [host, agent.kind])

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    if (busy) return
    setBusy(true)
    setError('')
    try {
      await switchAgent(host.client, agent.id, by === 'tier' ? { tier } : by === 'model' ? { model: model.trim(), effort } : { effort })
      onDone()
    } catch (err) {
      // The host's own words: already on it, at the quota reserve, a free model of a repo that wants none.
      setError(refusal(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <form onSubmit={submit} className="flex flex-col gap-3">
      <p className="text-xs text-muted-foreground">Now: {runsOn(agent) || agent.kind}{agent.tier ? ` (tier ${agent.tier})` : ''}</p>
      <Field label="Change">
        <Select aria-label="Change by" value={by} onChange={(e) => setBy(e.target.value as By)}>
          <option value="tier">To a tier</option>
          <option value="model">To a model</option>
          <option value="effort">The effort only (same model, its cache kept)</option>
        </Select>
      </Field>
      {by === 'tier' && (
        <Field label="Tier" hint="Each tier is a model and effort in the host's settings.">
          <Select aria-label="Tier" value={tier} disabled={tiers.length === 0} onChange={(e) => setTier(e.target.value)}>
            {tiers.length === 0 && <option value="">The host lists no tier for {agent.kind}</option>}
            {tiers.map((t) => <option key={t} value={t}>{t}</option>)}
          </Select>
        </Field>
      )}
      {by === 'model' && (
        <>
          <Field label="Model" hint={families.length ? `The families the host lists for ${agent.kind}, or any model id.` : 'A model name or family.'}>
            <Input aria-label="Model" list={`models-${agent.id}`} required autoFocus value={model} onChange={(e) => setModel(e.target.value)} placeholder="sonnet" />
          </Field>
          <datalist id={`models-${agent.id}`}>{families.map((f) => <option key={f} value={f} />)}</datalist>
        </>
      )}
      {(by === 'effort' || by === 'model') && (
        <Field label="Effort" hint={by === 'model' ? "Optional: the model's own when empty." : undefined}>
          <Select aria-label="Effort" value={effort} required={by === 'effort'} onChange={(e) => setEffort(e.target.value)}>
            {by === 'model' && <option value="">The model's own</option>}
            {by === 'effort' && <option value="">Choose…</option>}
            {EFFORTS.map((x) => <option key={x} value={x}>{x}</option>)}
          </Select>
        </Field>
      )}
      {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
      <Button type="submit" disabled={busy || (by === 'tier' && !tier) || (by === 'model' && !model.trim()) || (by === 'effort' && !effort)}>Switch</Button>
    </form>
  )
}
