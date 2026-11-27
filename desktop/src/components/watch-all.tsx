import { Columns2, Eye } from 'lucide-react'
import { useEffect, useMemo, useState, useSyncExternalStore } from 'react'
import { readTranscript } from '@/actions'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Tip } from '@/components/ui/tooltip'
import { bucketOf, BUCKET_LABEL, waitingIds } from '@/buckets'
import { useApp, useAppState, useHostState } from '@/context'
import { ago, activityMs, dockTiles, type Tile } from '@/dock'
import { joinWaiting, repoName, ROLE_LABEL, statusSentence } from '@/derive'
import { AgentState } from '@/gen/agentmesh/v1/types_pb'
import { useHostViews, useNow } from '@/hooks'
import { cn } from '@/lib/utils'
import { hueStyle } from '@/hue'
import { Chip, Chips } from '@/components/ui/chip'
import type { HostRuntime } from '@/model'
import type { HostState } from '@/store'
import { fromTail } from '@/transcript'
import { tailLines, type TailLine } from '@/watch'
import { AnswerForm } from './messages-panel'
import { StateIcon } from './state-icon'
import { VendorIcon } from './vendor-icon'

/**
 * "Watch all": every live agent of the open runs on every host as a tile with its recent conversation, the ones that
 * need the user first (herdr's grid, `docs/apps.md`, "A live dock"). A click opens the agent as a tab.
 */
export function WatchAll() {
  const app = useApp()
  const hosts = useAppState((s) => s.hosts)
  const views = useHostViews(hosts)
  const now = useNow(5_000)
  // Every tile, a dismissed one included: here the user asked to see everything.
  const tiles = useMemo(() => dockTiles(views, { newTiles: {}, dismissed: {} }, now), [views, now])
  return (
    <section aria-label="Watch all" className="flex min-h-0 flex-1 flex-col">
      <header className="flex items-center gap-2 border-b px-4 py-2.5">
        <Eye className="size-4 text-muted-foreground" />
        <h1 className="flex-1 text-sm font-semibold">Watch all <span className="font-normal text-muted-foreground">{tiles.length} {tiles.length === 1 ? 'agent' : 'agents'}</span></h1>
        <Button size="sm" variant="ghost" onClick={() => app.openWatch(false)}>Close</Button>
      </header>
      {tiles.length === 0 ? (
        <p className="p-8 text-center text-sm text-muted-foreground">No live agents in the open runs.</p>
      ) : (
        <ul aria-label="Agents" className="grid min-h-0 flex-1 auto-rows-min grid-cols-[repeat(auto-fill,minmax(20rem,1fr))] gap-3 overflow-y-auto p-4">
          {tiles.map((t) => (
            <GridTile key={t.key} tile={t} host={hosts.find((h) => h.info.id === t.hostId)} now={now} many={hosts.length > 1} />
          ))}
        </ul>
      )}
    </section>
  )
}

/** The recent lines of an agent's conversation: read when the tile appears, and again while the agent works or needs the user. */
function useTail(host: HostRuntime | undefined, agentId: string, state: AgentState): { lines: TailLine[]; error: string; loaded: boolean } {
  const [res, setRes] = useState<{ lines: TailLine[]; error: string; loaded: boolean }>({ lines: [], error: '', loaded: false })
  const busy = state === AgentState.WORKING || state === AgentState.NEEDS_INPUT || state === AgentState.STARTING
  useEffect(() => {
    if (!host) return
    let alive = true
    const load = () =>
      readTranscript(host.client, agentId, -1n, true).then(
        (r) => alive && setRes({ lines: tailLines(fromTail(r).entries), error: r.supported ? '' : 'No conversation record for this CLI.', loaded: true }),
        (e: unknown) => alive && setRes((s) => ({ ...s, error: e instanceof Error ? e.message : String(e), loaded: true })),
      )
    void load()
    const t = busy ? setInterval(() => void load(), 8_000) : undefined
    return () => {
      alive = false
      clearInterval(t)
    }
  }, [host, agentId, busy])
  return res
}

const WHO: Record<TailLine['who'], string> = { you: 'you', mesh: 'mesh', agent: '', tools: '' }

/** A value of a host's store, or undefined while the host is not there. */
function useStoreOf<T>(host: HostRuntime | undefined, select: (s: HostState) => T): T | undefined {
  return useSyncExternalStore(
    (notify) => host?.store.subscribe(notify) ?? (() => {}),
    () => (host ? select(host.store.getState()) : undefined),
  )
}

function GridTile({ tile, host, now, many }: { tile: Tile; host?: HostRuntime; now: number; many: boolean }) {
  const app = useApp()
  const a = tile.agent
  const tail = useTail(host, a.id, a.state)
  // Read from the host's store, so a plan that arrives or a PR that merges moves the tile at once.
  const rows = useStoreOf(host, (s) => s.waiting)
  const runs = useStoreOf(host, (s) => s.runs)
  const agents = useStoreOf(host, (s) => s.agents)
  const pullRequests = useStoreOf(host, (s) => s.pullRequests)
  const joined = useMemo(() => joinWaiting(rows ?? [], runs ?? {}, agents ?? {}), [rows, runs, agents])
  const waiting = useMemo(() => waitingIds(joined), [joined])
  // The plans and questions of this agent that nobody has answered: answered here, in the tile.
  const asks = useMemo(() => joined.filter((w) => w.agent.id === a.id && w.message), [joined, a.id])
  const bucket = bucketOf(a, { waiting, pullRequests: pullRequests ?? {} })
  const last = activityMs(a)
  const needs = a.state === AgentState.NEEDS_INPUT
  return (
    <li aria-label={`${a.name} in the grid`} style={hueStyle(a.repo)} className={cn('relative flex min-h-48 flex-col overflow-hidden rounded-lg border bg-card', (needs || bucket === 'needs') && 'border-glow shadow-[0_0_0_3px] shadow-glow/25', bucket === 'failed' && 'border-destructive')}>
      {/* The project's hue along the top, as on a dock tile; the border stays free for waiting and failed. */}
      <span aria-hidden className="absolute inset-x-0 top-0 h-0.5 bg-identity" />
      <header className="flex items-center gap-1.5 border-b px-3 py-2">
        <StateIcon agent={a} />
        <span className="min-w-0 flex-1 truncate text-[13px] font-medium">{a.name}</span>
        <VendorIcon kind={a.kind} model={a.model} />
        <Badge>{ROLE_LABEL[a.role]}</Badge>
        <Tip label="Show beside the open agent">
          <Button size="icon" variant="ghost" className="size-6" aria-label={`Pin ${a.name} as a split`} onClick={() => app.pinSplit(tile.hostId, a.id)}>
            <Columns2 className="size-3.5" />
          </Button>
        </Tip>
      </header>
      <button
        type="button"
        aria-label={`Open ${a.name}`}
        onClick={() => app.selectAgent(tile.hostId, a.id, a.state === AgentState.NEEDS_INPUT && a.needsInput > 0 && !waiting.has(a.id) ? 'terminal' : bucket === 'needs' && waiting.has(a.id) ? 'chat' : undefined)}
        className="flex min-h-0 flex-1 flex-col gap-1 p-3 text-left outline-none hover:bg-accent/40 focus-visible:ring-2 focus-visible:ring-ring"
      >
        <Chips>
          <Chip>{repoName(a.repo)}</Chip>
          {many && <Chip>{tile.hostName}</Chip>}
          {tile.run && <Chip>{tile.run.title || tile.run.id}</Chip>}
          <Chip>{BUCKET_LABEL[bucket]}</Chip>
        </Chips>
        <span className="truncate text-xs">{tile.activity ? `Working: ${tile.activity}` : statusSentence(a)}</span>
        <ul aria-label="Recent" className="mt-1 flex flex-col gap-1 text-xs leading-snug">
          {tail.lines.map((l) => (
            <li key={l.id} className={cn('line-clamp-2', l.who === 'tools' && 'text-muted-foreground', l.who === 'you' && 'text-primary', l.who === 'mesh' && 'text-muted-foreground italic')}>
              {WHO[l.who] && <span className="mr-1 font-semibold">{WHO[l.who]}:</span>}
              {l.text}
            </li>
          ))}
          {tail.loaded && tail.lines.length === 0 && !tail.error && <li className="text-muted-foreground">Nothing said yet.</li>}
          {tail.error && <li className="text-muted-foreground">{tail.error}</li>}
        </ul>
        <span className="mt-auto pt-1 text-xs text-muted-foreground">{last ? ago(last, now + tile.clockSkewMs) : ''}</span>
      </button>
      {host && asks.map((w) => (
        <section key={w.message!.id} aria-label={`${w.kind === 'plan' ? 'Plan' : 'Question'} from ${a.name}`} className="flex flex-col gap-2 border-t border-state-needs p-3">
          <p className="line-clamp-6 text-xs leading-snug whitespace-pre-line">{w.message!.body}</p>
          <AnswerForm host={host} message={w.message!} />
        </section>
      ))}
    </li>
  )
}
