import { toJson } from '@bufbuild/protobuf'
import { ValueSchema } from '@bufbuild/protobuf/wkt'
import { Check, Copy, RotateCcw } from 'lucide-react'
import { useEffect, useMemo, useRef, useState } from 'react'
import { resetSetting, setSetting } from '@/actions'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Fold } from '@/components/ui/collapsible'
import { Input, Select, Textarea } from '@/components/ui/input'
import { useApp, useAppState, useHostState } from '@/context'
import type { Setting } from '@/gen/agentmesh/v1/types_pb'
import type { HostRuntime } from '@/model'
import { ShortcutField } from './shortcut-field'
import { cleanPath, knownRepos, repoName, sameRepo } from '@/derive'
import { PER_PROJECT, controlFor, fromText, labelOf, sectionsOf, toText, type Control, type Section } from '@/settings-form'
import { cn } from '@/lib/utils'
import { themeByName, themeNames, themes } from '@/theme'

const json = (v: Setting['value']): unknown => (v ? toJson(ValueSchema, v) : undefined)
const show = (v: unknown): string => (typeof v === 'string' ? v : JSON.stringify(v))

/**
 * The host's settings, generated from the schema the host sends: the groups in a nav on the left, a row per setting
 * with its label, description and a control by type, the host's own check on every value, a reset to the default.
 * The same settings an agent reads and sets with `mesh settings`, by key.
 */
export function SettingsPanel({ host }: { host: HostRuntime }) {
  const settings = useHostState(host, (s) => s.settings)
  const app = useApp()
  // The filter lives in the app's state, so the palette can filter this screen while it is already open
  // ("Keyboard shortcuts…"); closing it clears it.
  const query = useAppState((s) => s.settingsQuery)
  const setQuery = (q: string) => app.store.setState({ settingsQuery: q })
  const [changed, setChanged] = useState(false)
  const [picked, setPicked] = useState('')
  const sections = useMemo(() => sectionsOf(Object.values(settings), query, changed), [settings, query, changed])
  const total = Object.keys(settings).length
  const agents = useHostState(host, (s) => s.agents)
  // A search (or "only the ones I set") lists every group that has a match; otherwise one group at a time.
  const listing = query.trim() !== '' || changed
  const current = sections.find((x) => x.group === picked) ?? sections[0]
  const shown = listing ? sections : current ? [current] : []
  const go = (group: string) => {
    setPicked(group)
    if (listing) document.getElementById(groupId(group))?.scrollIntoView({ block: 'start' })
  }
  return (
    <section aria-label="Settings" className="flex min-h-0 flex-1 flex-col gap-4 p-6">
      <header className="flex flex-col gap-1">
        <h1 className="text-base font-semibold">Settings of {host.info.name}</h1>
        <p className="text-xs text-muted-foreground">Kept on the host, so every device and every agent sees the same. A value you haven't set is the host's default.</p>
      </header>
      <div className="flex items-center gap-3">
        <Input aria-label="Filter settings" className="max-w-xs" placeholder="Search by name, description or key" value={query} onChange={(e) => setQuery(e.target.value)} />
        <label className="flex items-center gap-2 text-xs">
          <input type="checkbox" checked={changed} onChange={(e) => setChanged(e.target.checked)} /> Only the ones I set
        </label>
      </div>
      {total === 0 && <p className="text-xs text-muted-foreground">The host hasn't sent its settings yet.</p>}
      {total > 0 && sections.length === 0 && <p className="text-xs text-muted-foreground">No setting matches.</p>}
      {sections.length > 0 && (
        <div className="flex min-h-0 flex-1 gap-4">
          <nav aria-label="Setting groups" className="flex w-52 shrink-0 flex-col gap-0.5 overflow-y-auto">
            {sections.map((x) => (
              <button
                key={x.group}
                type="button"
                aria-current={!listing && x.group === current?.group ? 'true' : undefined}
                className={cn(
                  'flex items-center justify-between rounded-md px-2.5 py-1.5 text-left text-[13px] outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring',
                  !listing && x.group === current?.group && 'bg-accent font-medium',
                )}
                onClick={() => go(x.group)}
              >
                <span>{x.group}</span>
                <span className="text-xs tabular-nums text-muted-foreground">{x.count}</span>
              </button>
            ))}
          </nav>
          <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto pr-1">
            {shown.map((x) => (
              <GroupSection key={x.group} host={host} section={x} repos={knownRepos(agents)} settings={settings} />
            ))}
          </div>
        </div>
      )}
    </section>
  )
}

const groupId = (group: string) => `settings-group-${group.toLowerCase().replace(/[^a-z0-9]+/g, '-')}`

function GroupSection({ host, section, repos, settings }: { host: HostRuntime; section: Section<Setting>; repos: string[]; settings: Record<string, Setting> }) {
  const rows = (list: Setting[]) => list.filter((s) => !PER_PROJECT.has(s.key))
  const perProject = [...section.shown, ...section.advanced].some((s) => PER_PROJECT.has(s.key))
  const table = section.group === 'Shortcuts'
  const list = (items: Setting[]) => (
    <ul className="divide-y">
      {rows(items).map((s) => (
        <li key={s.key}>{table ? <ShortcutRow host={host} setting={s} /> : <SettingRow host={host} setting={s} />}</li>
      ))}
    </ul>
  )
  return (
    <section id={groupId(section.group)} aria-label={section.group} className="rounded-lg border bg-card">
      <h2 className="border-b px-4 py-2.5 text-[13px] font-semibold">
        {section.group} <span className="font-normal text-muted-foreground">({section.count})</span>
      </h2>
      {perProject && <ProjectLinks host={host} repos={repos} settings={settings} />}
      {list(section.shown)}
      {section.advanced.length > 0 && (
        <Fold title="Advanced" summary={section.advanced.length} className="border-b-0 px-4 py-2.5">
          {list(section.advanced)}
        </Fold>
      )}
    </section>
  )
}

/** The per-repo lists (who merges, who approves plans, who is kept from free models) are set in each project's own settings. */
function ProjectLinks({ host, repos, settings }: { host: HostRuntime; repos: string[]; settings: Record<string, Setting> }) {
  const app = useApp()
  // Each repo once, then the folders the lists name that no agent works in (a `~` or trailing-slash spelling is the repo it names).
  const all = [...repos]
  for (const key of PER_PROJECT) {
    const v = json(settings[key]?.value)
    for (const l of Array.isArray(v) ? v : []) if (typeof l === 'string' && !all.some((r) => sameRepo(l, r))) all.push(cleanPath(l))
  }
  return (
    <div data-setting="per-project" className="flex flex-col gap-2 border-b px-4 py-3">
      <span className="text-[13px] font-medium">Per project</span>
      <p className="text-xs text-muted-foreground">
        How much a project's leads do without asking (who merges, who approves plans) and whether its code may go to OpenCode's free models are set in each project's own settings.
      </p>
      {all.length === 0 && <p className="text-xs text-muted-foreground">No project yet: the ones agents work in show up here.</p>}
      <div className="flex flex-wrap gap-2">
        {all.map((repo) => (
          <Button key={repo} size="sm" variant="outline" title={repo} aria-label={`Project settings: ${repoName(repo)}`} onClick={() => app.openProjectSettings({ hostId: host.info.id, repo })}>
            {repoName(repo)}
          </Button>
        ))}
      </div>
    </div>
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

/** One setting's editing state: the draft text, the host's refusal, one change at a time, a refused one put back. */
function useEdit(host: HostRuntime, s: Setting) {
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
  return { control, current, draft, setDraft, error, busy, save, commit, reset }
}

/** The key as the CLI and an agent spell it, in small mono type; Copy puts it on the clipboard. Shown on hover or focus. */
function KeyLine({ id }: { id: string }) {
  const [copied, setCopied] = useState(false)
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined)
  useEffect(() => () => clearTimeout(timer.current), [])
  return (
    <span className="flex items-center gap-1 opacity-0 transition-opacity group-focus-within/row:opacity-100 group-hover/row:opacity-100 motion-reduce:transition-none">
      <code className="font-mono text-xs text-muted-foreground">{id}</code>
      <Button
        size="icon"
        variant="ghost"
        className="size-6"
        aria-label={`Copy ${id}`}
        onClick={() => {
          // Shown as copied only once the write took (it fails without a clipboard or without the window's focus).
          void navigator.clipboard?.writeText(id).then(() => {
            setCopied(true)
            clearTimeout(timer.current)
            timer.current = setTimeout(() => setCopied(false), 1500)
          }, () => {})
        }}
      >
        {copied ? <Check /> : <Copy />}
      </Button>
    </span>
  )
}

/** A description of a line or two, the rest behind "More". */
function Description({ text }: { text: string }) {
  const [open, setOpen] = useState(false)
  const long = text.length > 150
  return (
    <p className="text-xs text-muted-foreground">
      <span className={cn(long && !open && 'line-clamp-2')}>{text}</span>
      {long && (
        <button type="button" className="rounded text-xs text-foreground underline-offset-2 outline-none hover:underline focus-visible:ring-2 focus-visible:ring-ring" aria-expanded={open} onClick={() => setOpen(!open)}>
          {open ? 'Less' : 'More'}
        </button>
      )}
    </p>
  )
}

function SettingRow({ host, setting: s }: { host: HostRuntime; setting: Setting }) {
  const { control, current, draft, setDraft, error, busy, save, commit, reset } = useEdit(host, s)
  return (
    <div className="group/row grid items-start gap-x-6 gap-y-2 px-4 py-3 md:grid-cols-[minmax(0,1fr)_minmax(0,20rem)]" data-setting={s.key}>
      <div className="flex min-w-0 flex-col gap-1">
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-[13px] font-medium">{labelOf(s)}</span>
          {s.schema?.restart && <Badge>needs a restart</Badge>}
          {s.isSet && <Badge className="border-primary text-primary">changed</Badge>}
        </div>
        <Description text={s.schema?.summary ?? ''} />
        {labelOf(s) !== s.key && <KeyLine id={s.key} />}
        {s.isSet && <p className="text-xs text-muted-foreground">Default: <code className="font-mono">{show(json(s.default))}</code></p>}
      </div>
      <div className="flex min-w-0 flex-col items-start gap-1.5 md:items-end">
        <Field host={host} control={control} id={s.key} value={current} draft={draft} setDraft={setDraft} commit={commit} save={save} busy={busy} />
        {s.isSet && (
          <Button size="sm" variant="ghost" disabled={busy} onClick={() => void reset()} aria-label={`Reset ${s.key}`}>
            <RotateCcw /> Reset to default
          </Button>
        )}
        {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
      </div>
    </div>
  )
}

/** A shortcut as a row of a two-column table: the action, and the keys to press for it. */
function ShortcutRow({ host, setting: s }: { host: HostRuntime; setting: Setting }) {
  const { control, current, draft, setDraft, error, busy, save, commit, reset } = useEdit(host, s)
  return (
    <div className="group/row flex flex-col gap-1 px-4 py-2" data-setting={s.key} title={s.schema?.summary}>
      <div className="grid grid-cols-[minmax(0,1fr)_minmax(0,20rem)] items-center gap-x-6">
        <div className="flex min-w-0 flex-col">
          <span className="text-[13px]">{labelOf(s)}{s.isSet && <Badge className="ml-2 border-primary text-primary">changed</Badge>}</span>
          {labelOf(s) !== s.key && <KeyLine id={s.key} />}
        </div>
        <div className="flex items-center justify-end gap-2">
          <Field host={host} control={control} id={s.key} value={current} draft={draft} setDraft={setDraft} commit={commit} save={save} busy={busy} />
          {s.isSet && (
            <Button size="sm" variant="ghost" disabled={busy} onClick={() => void reset()} aria-label={`Reset ${s.key}`}>
              <RotateCcw /> Reset to default
            </Button>
          )}
        </div>
      </div>
      {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
    </div>
  )
}

/** The themes as cards showing their colours; the host may name one this build lacks, which is listed too, bare. */
function ThemeCards({ id, value, choices, busy, commit }: { id: string; value: string; choices: string[]; busy: boolean; commit: (t: string) => Promise<void> }) {
  return (
    <div role="radiogroup" aria-label={`Value of ${id}`} className="grid grid-cols-2 gap-2">
      {choices.map((name) => {
        const known = themes.some((t) => t.name === name)
        const t = themeByName(name)
        const on = value === name
        return (
          <button
            key={name}
            type="button"
            role="radio"
            aria-checked={on}
            disabled={busy}
            className={cn('flex flex-col gap-1.5 rounded-md border p-2 text-left text-xs outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50', on ? 'border-primary ring-1 ring-primary' : 'hover:bg-accent')}
            onClick={() => void commit(name)}
          >
            {known && (
              <span className="flex h-6 overflow-hidden rounded border" style={{ background: t.tokens.background }} aria-hidden>
                {['primary', 'foreground', 'state-working', 'destructive'].map((k) => <span key={k} className="flex-1" style={{ background: t.tokens[k] ?? t.tokens.primary }} />)}
              </span>
            )}
            <span className="font-medium">{name}</span>
          </button>
        )
      })}
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
      return <ShortcutField host={host} id={id} value={typeof value === 'string' ? value : ''} busy={busy} save={save} bare={control.bare} />
    case 'select':
      if (id === 'app.theme') return <ThemeCards id={id} value={draft} choices={control.choices} busy={busy} commit={commit} />
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
