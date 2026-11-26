import { toJson } from '@bufbuild/protobuf'
import { ValueSchema } from '@bufbuild/protobuf/wkt'
import { RotateCcw } from 'lucide-react'
import { useEffect, useMemo, useRef, useState } from 'react'
import { resetSetting, setSetting } from '@/actions'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input, Select, Textarea } from '@/components/ui/input'
import { useHostState } from '@/context'
import type { Setting } from '@/gen/agentmesh/v1/types_pb'
import type { HostRuntime } from '@/model'
import { cleanPath, knownRepos, repoName, sameRepo } from '@/derive'
import { controlFor, fromText, groupOf, toText, type Control } from '@/settings-form'
import { themeNames } from '@/theme'

const json = (v: Setting['value']): unknown => (v ? toJson(ValueSchema, v) : undefined)
const show = (v: unknown): string => (typeof v === 'string' ? v : JSON.stringify(v))

/**
 * The host's settings, generated from the schema the host sends: a control per type, the host's own check on
 * every value, a reset to the default. The same settings an agent reads and sets with `mesh settings`.
 */
export function SettingsPanel({ host }: { host: HostRuntime }) {
  const settings = useHostState(host, (s) => s.settings)
  const [query, setQuery] = useState('')
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
  const agents = useHostState(host, (s) => s.agents)
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
      {settings[FREE_KEY] && <FreeModels host={host} setting={settings[FREE_KEY]} repos={knownRepos(agents)} />}
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

const FREE_KEY = 'repos.no_free_models'
/** How long a written list is built on while the host's change event has not arrived (then the host's list is). */
const WRITE_GRACE_MS = 5000

const sameList = (a: string[], b: string[]) => a.length === b.length && a.every((x, i) => x === b[i])

/**
 * Which repos' code may go to OpenCode's free models (some of their providers keep prompts or train on them). The
 * setting is a list of the repos kept away from them; here each repo the host knows is a switch on that list, and
 * a listed folder no agent works in is shown too, so it can be taken off.
 */
function FreeModels({ host, setting, repos }: { host: HostRuntime; setting: Setting; repos: string[] }) {
  const listed = useMemo(() => {
    const v = json(setting.value)
    return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : []
  }, [setting.value])
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  // The list as last written: the host's change event reaches `listed` a moment after the write answers, and a toggle
  // in between must build on what was written, not on the older list (which would undo the first toggle).
  const written = useRef<{ list: string[]; at: number } | undefined>(undefined)
  useEffect(() => {
    const w = written.current
    if (w && sameList(w.list, listed)) written.current = undefined
  }, [listed])
  // Each repo once, then the listed folders no repo here is: a `~` or trailing-slash spelling is the repo it names.
  const all = [...repos]
  for (const l of listed) if (!all.some((r) => sameRepo(l, r))) all.push(cleanPath(l))
  async function toggle(repo: string, mayUse: boolean) {
    if (busy) return
    setBusy(true)
    setError('')
    const w = written.current
    const base = w && Date.now() - w.at < WRITE_GRACE_MS ? w.list : listed
    const next = mayUse ? base.filter((l) => !sameRepo(l, repo)) : [...base, repo]
    try {
      await setSetting(host.client, FREE_KEY, next as never)
      written.current = { list: next, at: Date.now() }
    } catch (e) {
      setError(e instanceof Error ? e.message.replace(/^\[[a-z_]+\]\s*/, '') : String(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <section aria-label="Free models by repo" className="flex flex-col gap-2 rounded-lg border bg-card px-3 py-2.5">
      <h2 className="text-[13px] font-semibold">Free models by repo</h2>
      <p className="text-xs text-muted-foreground">
        Some of OpenCode's free models are served by providers that keep prompts or train on them. Switch a repo off to keep its code from them: nothing there is started on, switched to or picked as a reviewer on a zero-cost model. A paid OpenCode model stays allowed, and agents already running keep their model. Every repo may by default.
      </p>
      {all.length === 0 && <p className="text-xs text-muted-foreground">No repo yet: the ones agents work in show up here.</p>}
      <ul className="flex flex-col divide-y">
        {all.map((repo) => {
          const mayUse = !listed.some((l) => sameRepo(l, repo))
          return (
            <li key={repo} className="flex items-center gap-3 py-1.5">
              <label className="flex flex-1 items-center gap-2 text-[13px]" title={repo}>
                <input type="checkbox" aria-label={`May use free models: ${repoName(repo)}`} checked={mayUse} disabled={busy} onChange={(e) => void toggle(repo, e.target.checked)} />
                <span className="font-medium">{repoName(repo)}</span>
                <span className="truncate font-mono text-[11px] text-muted-foreground">{repo}</span>
              </label>
              <span className="text-xs text-muted-foreground">{mayUse ? 'free models allowed' : 'kept from free models'}</span>
            </li>
          )
        })}
      </ul>
      {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
    </section>
  )
}

/**
 * `app.theme` is a plain string on the host (a host can name a theme this build lacks, and themes may come from
 * files later), so the desktop offers the themes it has, plus whatever the host has set.
 */
function controlOf(key: string, type: string, choices: string[], current: unknown): Control {
  if (key !== 'app.theme') return controlFor(type, choices)
  const names = themeNames()
  return { kind: 'select', choices: typeof current === 'string' && !names.includes(current) ? [current, ...names] : names }
}

function SettingRow({ host, setting: s }: { host: HostRuntime; setting: Setting }) {
  const type = s.schema?.type ?? 'json'
  const current = json(s.value)
  const control = controlOf(s.key, type, s.schema?.choices ?? [], current)
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
      <Field control={control} id={s.key} value={current} draft={draft} setDraft={setDraft} commit={commit} save={save} busy={busy} />
      {!s.isSet && <p className="text-[11px] text-muted-foreground">Default</p>}
      {s.isSet && <p className="text-[11px] text-muted-foreground">Default: <code className="font-mono">{show(json(s.default))}</code></p>}
      {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
    </div>
  )
}

function Field({ control, id, value, draft, setDraft, commit, save, busy }: { control: Control; id: string; value: unknown; draft: string; setDraft: (t: string) => void; commit: (t: string) => Promise<void>; save: (v: unknown) => Promise<void>; busy: boolean }) {
  const label = `Value of ${id}`
  switch (control.kind) {
    case 'bool':
      return (
        <label className="flex items-center gap-2 text-[13px]">
          <input type="checkbox" aria-label={label} checked={value === true} disabled={busy} onChange={(e) => void save(e.target.checked)} />
          {value === true ? 'On' : 'Off'}
        </label>
      )
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
