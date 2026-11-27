import { Server, Settings } from 'lucide-react'
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
import { quotaSections, resetText, type QuotaLine } from '@/usage'
import { VendorIcon } from './vendor-icon'
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
  // One way to each thing: the gear opens the settings and the quota opens Usage and cost; a new lead starts from a tab's
  // + or a project's menu. The Hosts menu is for the hosts themselves (and each one's settings when there are several).
  const hostEntries: MenuEntry[] = [
    ...(many ? hosts.map((h) => ({ label: `${h.info.name}: settings`, onSelect: () => { pickHost(app, h.info.id); app.openSettings(true) } })) : []),
    ...(fixed ? [] : ([...(many ? ['separator' as const] : []), { label: 'Connect a host…', onSelect: () => setAdding(true) }] as MenuEntry[])),
    { label: 'Pair a device', onSelect: () => app.openPairing(true), disabled: !mayPair },
    ...(fixed ? [] : (['separator', ...hosts.map((h) => ({ label: `Remove ${h.info.name}…`, onSelect: () => setRemoving(h), destructive: true }))] as MenuEntry[])),
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

const toneOf = (r: QuotaLine): MeterTone => (r.used >= 100 ? 'over' : r.atReserve ? 'warn' : 'ok')

/** What a quota bar says in its tip: the window, how full, when it resets and where the host stops starting work. */
const lineTip = (l: QuotaLine, reserve: number) =>
  [`${l.title}: ${Math.round(l.used)}% used`, resetWords(l), reserve < 100 ? `the host starts nothing new from ${reserve}%` : ''].filter(Boolean).join(', ')

/** When a window resets: a time as a span ("resets in 2 h"), agy's own text as it gave it ("resets in 1h" for a span, else "resets 12:00"). */
function resetWords(l: QuotaLine): string {
  if (l.resetsAt) return resetText(l.resetsAt)
  const t = l.resetsIn?.trim()
  if (!t) return ''
  const at = /^\d{4}-\d\d-\d\dT/.test(t) ? new Date(t) : undefined
  if (at && !Number.isNaN(at.getTime())) return resetText(at)
  return /^\d+\s*(h|m|d|min|hr)\b/i.test(t) ? `resets in ${t}` : `resets ${t}`
}

/**
 * One host's quota, a section per provider: its mark and name, then a bar per window labelled "5h" or "7d" with its percent
 * and a line at the reserve. Words are in the tips. Green, amber from the reserve on, red when used up; a click opens Usage and cost.
 */
function HostQuota({ host, many }: { host: HostRuntime; many: boolean }) {
  const app = useApp()
  const up = useHostState(host, (s) => s.conn.kind === 'ready')
  const usage = usePolled(() => getUsage(host.client), host.info.id, 60_000, up)
  const q = useMemo(() => (usage.value ? quotaSections(usage.value) : undefined), [usage.value])
  if (!q) return null
  // A host with no reading (no Claude account, no agy) still has its Usage and cost screen: the way in stays.
  if (q.sections.length === 0) {
    return (
      <Button type="button" size="sm" variant="ghost" className="mb-1 h-6 w-full justify-start px-1.5 text-xs text-muted-foreground" aria-label={`Quota${many ? ` of ${host.info.name}` : ''}: open usage and cost`} onClick={() => toggleUsage(app, host.info.id)}>
        Usage and cost
      </Button>
    )
  }
  return (
    <button
      type="button"
      aria-label={`Quota${many ? ` of ${host.info.name}` : ''}: open usage and cost`}
      onClick={() => toggleUsage(app, host.info.id)}
      className="mb-1 flex w-full flex-col gap-1.5 rounded-md px-1.5 py-1 text-left outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring"
    >
      {many && <span className="text-xs font-medium text-muted-foreground">{host.info.name}</span>}
      {q.sections.map((s) => (
        <section key={s.name} aria-label={`${s.name} quota`} className="flex flex-col gap-1">
          <span className="flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
            <VendorIcon vendor={s.vendor} /> {s.name}
          </span>
          <ul aria-label="Quota" className="flex flex-col gap-1">
            {s.lines.map((l) => (
              <Tip key={l.title} label={lineTip(l, q.reserve)}>
                <li className="grid grid-cols-[minmax(0,3.25rem)_1fr_2.5rem] items-center gap-2 text-xs">
                  <span className="truncate text-muted-foreground">{l.label}</span>
                  <Meter label={`${s.name} ${l.label}`} valueText={lineTip(l, q.reserve)} value={l.used} tone={toneOf(l)} mark={q.reserve} />
                  <span className={toneOf(l) === 'ok' ? 'text-right tabular-nums text-muted-foreground' : 'text-right font-semibold tabular-nums text-state-needs'}>{Math.round(l.used)}%</span>
                </li>
              </Tip>
            ))}
          </ul>
        </section>
      ))}
    </button>
  )
}
