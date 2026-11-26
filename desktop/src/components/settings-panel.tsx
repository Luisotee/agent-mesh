import { toJson } from '@bufbuild/protobuf'
import { ValueSchema } from '@bufbuild/protobuf/wkt'
import { RotateCcw } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import { resetSetting, setSetting } from '@/actions'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input, Select, Textarea } from '@/components/ui/input'
import { useApp, useAppState, useHostState } from '@/context'
import type { Setting } from '@/gen/agentmesh/v1/types_pb'
import type { HostRuntime } from '@/model'
import { ShortcutField } from './shortcut-field'
import { controlFor, fromText, groupOf, toText, type Control } from '@/settings-form'

const json = (v: Setting['value']): unknown => (v ? toJson(ValueSchema, v) : undefined)
const show = (v: unknown): string => (typeof v === 'string' ? v : JSON.stringify(v))

/**
 * The host's settings, generated from the schema the host sends: a control per type, the host's own check on
 * every value, a reset to the default. The same settings an agent reads and sets with `mesh settings`.
 */
export function SettingsPanel({ host }: { host: HostRuntime }) {
  const settings = useHostState(host, (s) => s.settings)
  const app = useApp()
  // The filter lives in the app's state, so the palette can filter this screen while it is already open
  // ("Keyboard shortcuts…"); closing it clears it.
  const query = useAppState((s) => s.settingsQuery)
  const setQuery = (q: string) => app.store.setState({ settingsQuery: q })
  const [changed, setChanged] = useState(false)
  const groups = useMemo(() => {
    const q = query.trim().toLowerCase()
    const by = new Map<string, Setting[]>()
    for (const s of Object.values(settings).sort((a, b) => a.key.localeCompare(b.key))) {
      if (changed && !s.isSet) continue
      if (q && !s.key.toLowerCase().includes(q) && !(s.schema?.summary ?? '').toLowerCase().includes(q)) continue
      by.set(groupOf(s.key), [...(by.get(groupOf(s.key)) ?? []), s])
    }
    return [...by.entries()]
  }, [settings, query, changed])
  const total = Object.keys(settings).length
  return (
    <section aria-label="Settings" className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-6">
      <header className="flex flex-col gap-1">
        <h1 className="text-base font-semibold">Settings of {host.info.name}</h1>
        <p className="text-xs text-muted-foreground">Kept on the host, so every device and every agent sees the same. A value you haven't set is the host's default.</p>
      </header>
      <div className="flex items-center gap-3">
        <Input aria-label="Filter settings" className="max-w-xs" placeholder="Filter by name or description" value={query} onChange={(e) => setQuery(e.target.value)} />
        <label className="flex items-center gap-2 text-xs">
          <input type="checkbox" checked={changed} onChange={(e) => setChanged(e.target.checked)} /> Only the ones I set
        </label>
      </div>
      {total === 0 && <p className="text-xs text-muted-foreground">The host hasn't sent its settings yet.</p>}
      {total > 0 && groups.length === 0 && <p className="text-xs text-muted-foreground">No setting matches.</p>}
      {groups.map(([group, list]) => (
        <details key={group} open={group === 'app' || !!query || changed} className="rounded-lg border bg-card">
          <summary className="cursor-pointer px-3 py-2 text-[13px] font-semibold">
            {group} <span className="font-normal text-muted-foreground">({list.length})</span>
          </summary>
          <ul className="divide-y">
            {list.map((s) => (
              <li key={s.key}>
                <SettingRow host={host} setting={s} />
              </li>
            ))}
          </ul>
        </details>
      ))}
    </section>
  )
}

function SettingRow({ host, setting: s }: { host: HostRuntime; setting: Setting }) {
  const type = s.schema?.type ?? 'json'
  const control = controlFor(type, s.schema?.choices ?? [])
  const current = json(s.value)
  const [draft, setDraft] = useState(() => toText(control, current))
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  // A change from elsewhere (an agent, another device, a reset) replaces what is being edited.
  const sig = JSON.stringify(current)
  useEffect(() => {
    setDraft(toText(control, JSON.parse(sig ?? 'null') as unknown))
    setError('')
    // eslint-disable-next-line react-hooks/exhaustive-deps -- `control` is derived from the schema, which doesn't change with the value
  }, [sig])

  const say = (e: unknown) => setError(e instanceof Error ? e.message.replace(/^\[[a-z_]+\]\s*/, '') : String(e))
  /** One change at a time; a refused one puts the field back to what the host has. */
  async function change(act: () => Promise<unknown>) {
    if (busy) return
    setBusy(true)
    setError('')
    try {
      await act()
    } catch (e) {
      say(e)
      setDraft(toText(control, current))
    } finally {
      setBusy(false)
    }
  }
  const save = (value: unknown) => change(() => setSetting(host.client, s.key, value as never))
  async function commit(text: string) {
    const p = fromText(type, control, text)
    if (!p.ok) return setError(p.error)
    if (JSON.stringify(p.value) === sig) return setError('')
    await save(p.value)
  }
  const reset = () => change(() => resetSetting(host.client, s.key))
  return (
    <div className="flex flex-col gap-2 px-3 py-2.5" data-setting={s.key}>
      <div className="flex items-center gap-2">
        <code className="font-mono text-xs">{s.key}</code>
        {s.schema?.restart && <Badge>needs a restart</Badge>}
        {s.isSet && <Badge className="border-primary text-primary">changed</Badge>}
        <span className="flex-1" />
        {s.isSet && (
          <Button size="sm" variant="ghost" disabled={busy} onClick={() => void reset()} aria-label={`Reset ${s.key}`}>
            <RotateCcw /> Reset
          </Button>
        )}
      </div>
      <p className="text-xs text-muted-foreground">{s.schema?.summary}</p>
      <Field host={host} control={control} id={s.key} value={current} draft={draft} setDraft={setDraft} commit={commit} save={save} busy={busy} />
      {!s.isSet && <p className="text-[11px] text-muted-foreground">Default</p>}
      {s.isSet && <p className="text-[11px] text-muted-foreground">Default: <code className="font-mono">{show(json(s.default))}</code></p>}
      {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
    </div>
  )
}

function Field({ host, control, id, value, draft, setDraft, commit, save, busy }: { host: HostRuntime; control: Control; id: string; value: unknown; draft: string; setDraft: (t: string) => void; commit: (t: string) => Promise<void>; save: (v: unknown) => Promise<void>; busy: boolean }) {
  const label = `Value of ${id}`
  switch (control.kind) {
    case 'bool':
      return (
        <label className="flex items-center gap-2 text-[13px]">
          <input type="checkbox" aria-label={label} checked={value === true} disabled={busy} onChange={(e) => void save(e.target.checked)} />
          {value === true ? 'On' : 'Off'}
        </label>
      )
    case 'shortcut':
      return <ShortcutField host={host} id={id} value={typeof value === 'string' ? value : ''} busy={busy} save={save} />
    case 'select':
      return (
        <Select aria-label={label} className="max-w-xs" value={draft} disabled={busy} onChange={(e) => void commit(e.target.value)}>
          {control.choices.map((c) => <option key={c} value={c}>{c}</option>)}
        </Select>
      )
    case 'multi': {
      const on = new Set(Array.isArray(value) ? (value as string[]) : [])
      return (
        <div role="group" aria-label={label} className="flex flex-wrap gap-x-4 gap-y-1">
          {control.choices.map((c) => (
            <label key={c} className="flex items-center gap-1.5 text-[13px]">
              <input
                type="checkbox"
                checked={on.has(c)}
                disabled={busy}
                onChange={(e) => {
                  const next = new Set(on)
                  if (e.target.checked) next.add(c)
                  else next.delete(c)
                  void save(control.choices.filter((x) => next.has(x)))
                }}
              />
              {c}
            </label>
          ))}
        </div>
      )
    }
    case 'lines':
    case 'json':
      return (
        <Textarea
          aria-label={label}
          className={control.kind === 'json' ? 'font-mono text-xs' : ''}
          rows={Math.min(8, Math.max(2, draft.split('\n').length))}
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onBlur={() => void commit(draft)}
        />
      )
    default:
      return (
        <Input
          aria-label={label}
          className="max-w-xs"
          type={control.kind === 'number' ? 'number' : 'text'}
          min={control.kind === 'number' ? control.min : undefined}
          max={control.kind === 'number' ? control.max : undefined}
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onBlur={() => void commit(draft)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') void commit(draft)
          }}
        />
      )
  }
}
