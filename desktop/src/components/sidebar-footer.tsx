import { Plus, Server, Settings } from 'lucide-react'
import { useMemo, useState } from 'react'
import { getUsage } from '@/actions'
import { Button } from '@/components/ui/button'
import { ActionMenu, type MenuEntry } from '@/components/ui/menu'
import { Meter, type MeterTone } from '@/components/ui/meter'
import { Tip } from '@/components/ui/tooltip'
import { useApp, useAppState, useHostState, useSelectedHost } from '@/context'
import { displayChord } from '@/keys'
import type { HostRuntime } from '@/model'
import { usePolled } from '@/polled'
import { useKeys } from '@/hooks'
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
  const [removing, setRemoving] = useState<HostRuntime>()
  const fixed = app.api.hosts.fixed
  const many = hosts.length > 1
  const plus: MenuEntry[] = [
    { label: 'New lead', onSelect: () => app.openWorkspace(true), disabled: !host },
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
