import { Plus, Server, Settings } from 'lucide-react'
import { useMemo, useState } from 'react'
import { getUsage, listProjects } from '@/actions'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent } from '@/components/ui/dialog'
import { Field } from '@/components/ui/label'
import { Input } from '@/components/ui/input'
import { ActionMenu, type MenuEntry } from '@/components/ui/menu'
import { Meter, type MeterTone } from '@/components/ui/meter'
import { Tip } from '@/components/ui/tooltip'
import { useApp, useAppState, useHostState, useSelectedHost } from '@/context'
import type { Project } from '@/gen/agentmesh/v1/mesh_pb'
import { displayChord } from '@/keys'
import type { HostRuntime } from '@/model'
import { usePolled } from '@/polled'
import { useKeys } from '@/hooks'
import { rankProjects } from '@/projects'
import { quotaRows, resetText, type QuotaRow } from '@/usage'
import { AddHostDialog, RemoveHostDialog } from './host-dialogs'
import { ActivityBell } from './activity-list'
import { canPair } from './pair-panel'

/** The sidebar's foot: the quota of each host as small meters, then Settings, the hosts and the + menu. */
export function SidebarFooter() {
  const app = useApp()
  const hosts = useAppState((s) => s.hosts)
  const host = useSelectedHost()
  const settingsOpen = useAppState((s) => s.settingsOpen)
  const keys = useKeys(hosts)
  const mayPair = useHostState(host ?? hosts[0]!, (s) => !!host && s.conn.kind === 'ready' && canPair(host.store))
  const [adding, setAdding] = useState(false)
  const [project, setProject] = useState(false)
  const [removing, setRemoving] = useState<HostRuntime>()
  const fixed = app.api.hosts.fixed
  const many = hosts.length > 1
  const plus: MenuEntry[] = [
    { label: 'New lead', onSelect: () => app.openWorkspace(true), disabled: !host },
    { label: 'Add a project…', onSelect: () => setProject(true), disabled: !host },
    ...(fixed ? [] : [{ label: 'Connect a host…', onSelect: () => setAdding(true) }]),
    { label: 'Pair a device', onSelect: () => app.openPairing(true), disabled: !mayPair },
  ]
  const hostEntries: MenuEntry[] = [
    ...hosts.flatMap((h) => [
      { label: many ? `${h.info.name}: settings` : 'Host settings', onSelect: () => { pickHost(app, h.info.id); app.openSettings(true) } },
      { label: many ? `${h.info.name}: usage and cost` : 'Usage and cost', onSelect: () => toggleUsage(app, h.info.id) },
    ]),
    ...(fixed ? [] : (['separator', ...hosts.map((h) => ({ label: `Remove ${h.info.name}…`, onSelect: () => setRemoving(h), destructive: true })), { label: 'Connect a host…', onSelect: () => setAdding(true) }] as MenuEntry[])),
  ]
  return (
    <footer className="border-t px-2 pt-2 pb-2.5">
      {hosts.map((h) => <HostQuota key={h.info.id} host={h} many={many} />)}
      <div className="mt-1 flex items-center gap-1">
        <Tip label={`Settings${host && many ? ` of ${host.info.name}` : ''} (${displayChord(keys.settings)})`}>
          <Button size="icon" variant={settingsOpen ? 'secondary' : 'ghost'} aria-label="Settings" aria-pressed={settingsOpen} disabled={!host} onClick={() => app.openSettings(!settingsOpen)}>
            <Settings />
          </Button>
        </Tip>
        {host && <ActivityBell host={host} />}
        <ActionMenu side="top" entries={hostEntries} trigger={<Button size="icon" variant="ghost" aria-label="Hosts"><Server /></Button>} />
        <span className="min-w-0 flex-1 truncate px-1 text-xs text-muted-foreground">{host && <HostLine host={host} />}</span>
        <ActionMenu side="top" align="end" entries={plus} trigger={<Button size="icon" variant="ghost" aria-label="New"><Plus /></Button>} />
      </div>
      {!fixed && <AddHostDialog open={adding} onOpenChange={setAdding} />}
      {removing && <RemoveHostDialog host={removing} open onOpenChange={(o) => !o && setRemoving(undefined)} />}
      {host && project && <AddProjectDialog host={host} onClose={() => setProject(false)} />}
    </footer>
  )
}

/** "wsl, daemon 0.9.1, you are Desktop": which host the settings, usage and pairing screens are of. */
function HostLine({ host }: { host: HostRuntime }) {
  const c = useHostState(host, (s) => s.conn)
  return (
    <span role="status" aria-label={`Host ${host.info.name}, ${c.kind}`}>
      {c.kind === 'ready' ? `${host.info.name}, daemon ${c.hello.version}${c.hello.actor?.name ? `, you are ${c.hello.actor.name}` : ''}` : `${host.info.name}, ${c.kind}`}
    </span>
  )
}

/** Opens a host's Usage and cost, or closes it when it is the one showing. */
function toggleUsage(app: ReturnType<typeof useApp>, hostId: string) {
  const st = app.store.getState()
  if (st.usageOpen && st.hostId === hostId) return app.openUsage(false)
  pickHost(app, hostId)
  app.openUsage(true)
}

/** Makes a host the one the host screens are of; the open agent stays open when it is that host's. */
function pickHost(app: ReturnType<typeof useApp>, hostId: string) {
  if (app.store.getState().hostId !== hostId) app.selectHost(hostId)
}

const toneOf = (r: QuotaRow): MeterTone => (r.used >= 100 ? 'over' : r.atReserve ? 'warn' : 'ok')

/** One host's quota: a line per window, green, amber from the reserve on, red when used up; a click opens Usage and cost. */
function HostQuota({ host, many }: { host: HostRuntime; many: boolean }) {
  const app = useApp()
  const up = useHostState(host, (s) => s.conn.kind === 'ready')
  const usage = usePolled(() => getUsage(host.client), host.info.id, 60_000, up)
  const q = useMemo(() => (usage.value ? quotaRows(usage.value) : undefined), [usage.value])
  if (!q || q.rows.length === 0) return null
  return (
    <button
      type="button"
      aria-label={`Quota${many ? ` of ${host.info.name}` : ''}: open usage and cost`}
      onClick={() => toggleUsage(app, host.info.id)}
      className="mb-1 flex w-full flex-col gap-1 rounded-md px-1.5 py-1 text-left outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring"
    >
      {many && <span className="text-xs font-medium text-muted-foreground">{host.info.name}</span>}
      <ul aria-label="Quota" className="flex flex-col gap-1">
        {q.rows.map((r) => (
          <li key={r.label} className="grid grid-cols-[minmax(0,8rem)_1fr_2.5rem] items-center gap-2 text-xs">
            <span className="truncate text-muted-foreground" title={r.resetsAt ? resetText(r.resetsAt) : undefined}>{r.label}</span>
            <Meter label={`${r.label}${r.resetsAt ? `, ${resetText(r.resetsAt)}` : ''}`} value={r.used} tone={toneOf(r)} />
            <span className={toneOf(r) === 'ok' ? 'text-right tabular-nums text-muted-foreground' : 'text-right font-semibold tabular-nums text-state-needs'}>{Math.round(r.used)}%</span>
          </li>
        ))}
      </ul>
    </button>
  )
}

/** Adds a project to the sidebar with no run in it yet: it is pinned there (device-local) until unpinned. */
function AddProjectDialog({ host, onClose }: { host: HostRuntime; onClose: () => void }) {
  const app = useApp()
  const [query, setQuery] = useState('')
  const pinned = useAppState((s) => s.pins[host.info.id]) ?? []
  const list = usePolled(async () => (await listProjects(host.client)).projects, `projects/${host.info.id}`, 60_000)
  const shown = rankProjects(list.value ?? [], query, pinned).filter((p) => !pinned.includes(p.path)).slice(0, 8)
  const add = (p: Pick<Project, 'path'>) => {
    app.togglePin(host.info.id, p.path, true)
    onClose()
  }
  /** Enter: the path typed when it is one (a project of the list by that exact path, or a new one), else the best match. */
  const pick = (): Pick<Project, 'path'> | undefined => {
    const typed = query.trim()
    const exact = (list.value ?? []).find((p) => p.path === typed)
    if (exact) return exact
    if (typed.startsWith('/')) return { path: typed }
    return shown[0]
  }
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent title="Add a project" description={`A project on ${host.info.name} to keep in the sidebar, pinned, before any run works in it.`}>
        <form className="flex flex-col gap-3" onSubmit={(e) => { e.preventDefault(); const p = pick(); if (p) add(p) }}>
          <Field label="Project">
            <Input autoFocus value={query} onChange={(e) => setQuery(e.target.value)} placeholder="A name, or a path on the host" />
          </Field>
          {list.error && !list.value && <p role="alert" className="text-xs text-destructive">Couldn't list the host's projects: {list.error}</p>}
          <ul aria-label="Projects" className="flex flex-col">
            {shown.map((p) => (
              <li key={p.path}>
                <button type="button" onClick={() => add(p)} className="flex w-full flex-col rounded-md px-2 py-1.5 text-left hover:bg-accent">
                  <span className="text-[13px] font-medium">{p.name}</span>
                  <span className="truncate font-mono text-xs text-muted-foreground">{p.path}</span>
                </button>
              </li>
            ))}
          </ul>
        </form>
      </DialogContent>
    </Dialog>
  )
}
