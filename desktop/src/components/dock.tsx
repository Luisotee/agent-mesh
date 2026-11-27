import { ChevronDown, ChevronUp, Columns2, Eye, X } from 'lucide-react'
import { AnimatePresence, m } from 'motion/react'
import { useMemo } from 'react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Chip, Chips } from '@/components/ui/chip'
import { useApp, useAppState } from '@/context'
import { activityMs, ago, dockTiles, sentLabel, type Tile } from '@/dock'
import { ROLE_LABEL, repoName, statusSentence } from '@/derive'
import { useHostViews, useNow } from '@/hooks'
import { cn } from '@/lib/utils'
import { hueStyle } from '@/hue'
import { AgentState } from '@/gen/agentmesh/v1/types_pb'
import { StateIcon } from './state-icon'
import { VendorIcon } from './vendor-icon'

/**
 * The live dock: a tile per live agent of the open runs, on every host, with what it is doing now. A worker that
 * starts pops in marked new, and nothing here takes focus; a click opens the agent as a tab.
 */
export function Dock() {
  const app = useApp()
  const hosts = useAppState((s) => s.hosts)
  const open = useAppState((s) => s.dockOpen)
  const badge = useAppState((s) => s.dockBadge)
  const newTiles = useAppState((s) => s.newTiles)
  const dismissed = useAppState((s) => s.dismissed)
  const hostId = useAppState((s) => s.hostId)
  const agentId = useAppState((s) => s.agentId)
  const views = useHostViews(hosts)
  const now = useNow()
  const tiles = useMemo(() => dockTiles(views, { newTiles, dismissed }, now), [views, newTiles, dismissed, now])
  const fresh = tiles.filter((t) => t.isNew).length
  const needs = tiles.filter((t) => t.agent.needsInput > 0).length
  return (
    <section aria-label="Live dock" className="shrink-0 border-t bg-surface">
      <header className="flex items-center gap-2 px-3 py-1">
        <Button size="sm" variant="ghost" className="h-6 gap-1 px-1.5" aria-expanded={open} aria-label={open ? 'Hide the live dock' : 'Show the live dock'} onClick={() => app.setDock(!open)}>
          {open ? <ChevronDown /> : <ChevronUp />}
          <span className="text-[13px] font-semibold">Live</span>
        </Button>
        <span className="text-xs text-muted-foreground">{tiles.length} {tiles.length === 1 ? 'agent' : 'agents'}</span>
        {needs > 0 && <Badge className="border-state-needs text-state-needs">{needs} need you</Badge>}
        {fresh > 0 && <Badge className="border-primary text-primary">{fresh} new</Badge>}
        {!open && badge > 0 && <Badge role="status" className="border-primary text-primary">{badge} started</Badge>}
        <Button size="sm" variant="ghost" className="ml-auto h-6 gap-1 px-1.5 text-xs" onClick={() => app.openWatch(true)}>
          <Eye className="size-3.5" /> Watch all
        </Button>
      </header>
      {open && (
        <ul aria-label="Agents in the dock" className="flex gap-2 overflow-x-auto px-3 pb-2.5">
          {tiles.length === 0 && <li className="list-none py-3 text-xs text-muted-foreground">No live agents in the open runs.</li>}
          <AnimatePresence initial={false}>
            {tiles.map((t) => (
              <DockTile key={t.key} tile={t} now={now} many={hosts.length > 1} selected={t.hostId === hostId && t.agent.id === agentId} />
            ))}
          </AnimatePresence>
        </ul>
      )}
    </section>
  )
}

function DockTile({ tile, now, many, selected }: { tile: Tile; now: number; many: boolean; selected: boolean }) {
  const app = useApp()
  const a = tile.agent
  const last = activityMs(a)
  const when = last ? ago(last, now + tile.clockSkewMs) : ''
  return (
    <m.li
      layout
      initial={{ opacity: 0, scale: 0.96 }}
      animate={{ opacity: 1, scale: 1 }}
      exit={{ opacity: 0, scale: 0.96 }}
      transition={{ duration: 0.16 }}
      aria-label={`${a.name} in the dock`}
      style={hueStyle(a.repo)}
      className={cn(
        'group relative flex w-56 shrink-0 flex-col overflow-hidden rounded-lg border bg-card text-left',
        tile.isNew && 'border-primary ring-1 ring-primary',
        a.needsInput > 0 && 'border-glow shadow-[0_0_0_3px] shadow-glow/25',
        selected && 'bg-accent',
      )}
    >
      {/* The project's hue along the top, breathing while the agent works. */}
      <span aria-hidden className={cn('absolute inset-x-0 top-0 h-0.5 bg-identity', a.state === AgentState.WORKING && 'animate-working motion-reduce:animate-none')} />
      <button
        type="button"
        onClick={() => app.selectAgent(tile.hostId, a.id)}
        className="flex min-w-0 flex-1 flex-col gap-0.5 rounded-lg p-2.5 text-left outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring"
      >
        <span className="flex items-center gap-1.5 pr-5">
          <StateIcon agent={a} />
          <span className="min-w-0 flex-1 truncate text-[13px] font-medium">{a.name}</span>
          <VendorIcon kind={a.kind} model={a.model} />
          {tile.isNew && <Badge className="border-primary text-primary">new</Badge>}
        </span>
        <Chips>
          <Chip>{ROLE_LABEL[a.role]}</Chip>
          <Chip>{repoName(a.repo)}</Chip>
          {many && <Chip>{tile.hostName}</Chip>}
        </Chips>
        <span className="truncate text-xs">{tile.activity ? `Working: ${tile.activity}` : statusSentence(a)}</span>
        <span className="flex min-w-0 gap-2 truncate text-xs text-muted-foreground">
          {tile.lastSent && <span className="truncate">{sentLabel(tile.lastSent)}</span>}
          {when && <span className="ml-auto shrink-0 tabular-nums">{when}</span>}
          {!tile.lastSent && !when && ' '}
        </span>
      </button>
      <button
        type="button"
        aria-label={`Pin ${a.name} as a split`}
        title="Show beside the open agent"
        onClick={() => app.pinSplit(tile.hostId, a.id)}
        className="absolute top-1 right-7 hidden size-5 place-items-center rounded hover:bg-muted group-hover:grid group-focus-within:grid"
      >
        <Columns2 className="size-3" />
      </button>
      <button
        type="button"
        aria-label={`Dismiss ${a.name} from the dock`}
        title="Hide until its state changes"
        onClick={() => app.dismissTile(tile.hostId, a.id)}
        className="absolute top-1 right-1 hidden size-5 place-items-center rounded hover:bg-muted group-hover:grid group-focus-within:grid"
      >
        <X className="size-3" />
      </button>
    </m.li>
  )
}
