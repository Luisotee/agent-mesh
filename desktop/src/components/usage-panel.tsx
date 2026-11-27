import { RefreshCw, TriangleAlert } from 'lucide-react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { getCost, getUsage } from '@/actions'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Select } from '@/components/ui/input'
import { useHostState } from '@/context'
import type { GetUsageResponse } from '@/gen/agentmesh/v1/mesh_pb'
import type { GetCostResponse } from '@/gen/agentmesh/v1/report_pb'
import type { HostRuntime } from '@/model'
import { cn } from '@/lib/utils'
import { WINDOWS, costView, resetText, usageView, type CostTable, type QuotaBar } from '@/usage'

/** How often the open screen asks the host again (the host caches readings itself). */
const POLL_MS = 60_000

type Load<T> = { kind: 'loading' } | { kind: 'ready'; value: T } | { kind: 'failed'; error: string }

const say = (e: unknown) => (e instanceof Error ? e.message.replace(/^\[[a-z_]+\]\s*/, '') : String(e))

/**
 * What `mesh usage` and `mesh cost` show, from the same two actions: each quota window with its reset and the
 * reserve line, what agy tiers would start on, and what the sessions cost for a run or a span of days.
 */
export function UsagePanel({ host }: { host: HostRuntime }) {
  // Everything it holds (a reading, the cost's scope, a run id) belongs to one host: another host starts afresh.
  return <UsageOf key={host.info.id} host={host} />
}

function UsageOf({ host }: { host: HostRuntime }) {
  const runs = useHostState(host, (s) => s.runs)
  const [usage, setUsage] = useState<Load<GetUsageResponse>>({ kind: 'loading' })
  // The last refresh's error, kept beside the reading it did not replace.
  const [usageError, setUsageError] = useState('')
  const [cost, setCost] = useState<Load<GetCostResponse>>({ kind: 'loading' })
  // The cost's scope: a span back from now, or one run's id.
  const [scope, setScope] = useState('7d')
  const [agents, setAgents] = useState(false)
  const [now, setNow] = useState(() => new Date())
  // A late answer to an earlier request must not replace a newer one.
  const seq = useRef({ usage: 0, cost: 0 })

  const loadUsage = useCallback(
    async (refresh: boolean) => {
      const n = ++seq.current.usage
      try {
        const value = await getUsage(host.client, refresh)
        if (n === seq.current.usage) {
          setUsage({ kind: 'ready', value })
          setUsageError('')
        }
      } catch (e) {
        // A reading we already have stays on screen, with the error beside it, rather than vanishing on one failed poll.
        if (n === seq.current.usage) {
          setUsageError(say(e))
          setUsage((u) => (u.kind === 'ready' ? u : { kind: 'failed', error: say(e) }))
        }
      }
      setNow(new Date())
    },
    [host],
  )
  const loadCost = useCallback(async () => {
    const n = ++seq.current.cost
    setCost((c) => (c.kind === 'ready' ? c : { kind: 'loading' }))
    try {
      const value = await getCost(host.client, scope.startsWith('run:') ? { runId: scope.slice(4) } : { since: scope }, agents)
      if (n === seq.current.cost) setCost({ kind: 'ready', value })
    } catch (e) {
      if (n === seq.current.cost) setCost({ kind: 'failed', error: say(e) })
    }
  }, [host, scope, agents])

  useEffect(() => {
    void loadUsage(false)
    const t = setInterval(() => void loadUsage(false), POLL_MS)
    return () => clearInterval(t)
  }, [loadUsage])
  useEffect(() => {
    void loadCost()
  }, [loadCost])

  const runList = useMemo(() => Object.values(runs).sort((a, b) => Number((b.createdAt?.seconds ?? 0n) - (a.createdAt?.seconds ?? 0n))), [runs])
  const view = usage.kind === 'ready' ? usageView(usage.value) : undefined
  return (
    <section aria-label="Usage" className="flex min-h-0 flex-1 flex-col gap-5 overflow-y-auto p-6">
      <header className="flex items-start gap-3">
        <div className="flex-1">
          <h1 className="text-base font-semibold">Usage and cost of {host.info.name}</h1>
          <p className="text-xs text-muted-foreground">What mesh usage and mesh cost print: how much of each quota is used, and how many tokens the agents' sessions used (dollars at API prices beside them).</p>
        </div>
        <Button
          variant="outline"
          size="sm"
          aria-label="Refresh"
          onClick={() => {
            void loadUsage(true)
            void loadCost()
          }}
        >
          <RefreshCw /> Refresh
        </Button>
      </header>

      {view && view.warnings.length > 0 && (
        <div role="alert" className="flex items-start gap-2 rounded-lg border border-state-needs bg-card p-3 text-[13px]">
          <TriangleAlert className="mt-0.5 size-4 shrink-0 text-state-needs" />
          <p>
            At the reserve ({view.reserve}% of a quota used): <strong>{view.warnings.join(', ')}</strong>. The host starts nothing new on {view.warnings.length === 1 ? 'it' : 'them'} until it resets, or you run <code className="font-mono text-xs">mesh reserve off</code>.
          </p>
        </div>
      )}

      <Card title="Quota">
        {usage.kind === 'loading' && <p className="text-xs text-muted-foreground">Reading the quotas…</p>}
        {usage.kind === 'failed' && <p role="alert" className="text-xs text-destructive">{usage.error}</p>}
        {usage.kind === 'ready' && usageError && <p role="alert" className="text-xs text-destructive">Last refresh failed: {usageError}. The reading below is from before.</p>}
        {view && (
          <div className="flex flex-col gap-4">
            <div className="flex flex-col gap-2.5">
              <h3 className="text-xs font-semibold">Claude</h3>
              {view.claude.length === 0 && <p className="text-xs text-muted-foreground">No reading (no Claude account is signed in on the host, or it gave no usage).</p>}
              {view.claude.map((w) => (
                <Bar key={w.key} bar={w} reserve={view.reserve} now={now} />
              ))}
              {view.claude.length > 0 && view.claudeSource && view.claudeSource !== 'live' && <p className="text-xs text-muted-foreground">From the {view.claudeSource}{view.claudeFetchedAt ? `, read ${view.claudeFetchedAt.toLocaleTimeString()}` : ''}.</p>}
            </div>
            <div className="flex flex-col gap-2">
              <h3 className="text-xs font-semibold">agy</h3>
              {view.agy.length === 0 && <p className="text-xs text-muted-foreground">No quota rows (agy gave none, or is not installed).</p>}
              {view.agy.length > 0 && (
                <table className="w-full text-[13px]">
                  <tbody>
                    {view.agy.map((r) => (
                      <tr key={`${r.group}/${r.label}`} className="border-b last:border-0">
                        <td className="py-1 pr-3">{r.group}</td>
                        <td className="py-1 pr-3 text-muted-foreground">{r.label}</td>
                        <td className={cn('py-1 pr-3 text-right tabular-nums', r.atReserve && 'font-semibold text-state-needs')}>{r.left}% left</td>
                        <td className="py-1 text-xs text-muted-foreground">{r.resets && `resets ${r.resets}`}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
              {view.tiers.map((t) => (
                <p key={t.tier} className="text-xs">
                  <span className="font-medium">{t.tier}</span> would start on{' '}
                  {t.model ? <code className="font-mono">{t.model}</code> : <span className="text-state-needs">nothing (every model of it is at the reserve)</span>}
                  {t.left !== undefined && <span className="text-muted-foreground"> ({t.left}% left)</span>}
                  {t.notes.map((n) => (
                    <span key={n} className="block text-muted-foreground">{n}</span>
                  ))}
                </p>
              ))}
            </div>
            <p className="text-xs text-muted-foreground">Reserve: the host starts nothing new on a quota once {view.reserve}% of it is used (<code className="font-mono">quota_stop</code>).</p>
          </div>
        )}
      </Card>

      <Card
        title="Cost"
        action={
          <div className="flex items-center gap-3">
            <label className="flex items-center gap-1.5 text-xs whitespace-nowrap">
              <input type="checkbox" checked={agents} onChange={(e) => setAgents(e.target.checked)} /> Per agent
            </label>
            <Select aria-label="Cost of" value={scope} onChange={(e) => setScope(e.target.value)} className="h-7 text-xs">
              {WINDOWS.map((w) => (
                <option key={w.since} value={w.since}>{w.label}</option>
              ))}
              {runList.length > 0 && (
                <optgroup label="One run">
                  {runList.map((r) => (
                    <option key={r.id} value={`run:${r.id}`}>{r.title || r.id}</option>
                  ))}
                </optgroup>
              )}
            </Select>
          </div>
        }
      >
        {cost.kind === 'loading' && <p className="text-xs text-muted-foreground">Adding up the sessions…</p>}
        {cost.kind === 'failed' && <p role="alert" className="text-xs text-destructive">{cost.error}</p>}
        {cost.kind === 'ready' && <CostBody cost={cost.value} />}
      </Card>
    </section>
  )
}

function Card({ title, action, children }: { title: string; action?: React.ReactNode; children: React.ReactNode }) {
  return (
    <div className="rounded-lg border bg-card p-4">
      <div className="mb-3 flex items-center gap-3">
        <h2 className="flex-1 text-[13px] font-semibold">{title}</h2>
        {action}
      </div>
      {children}
    </div>
  )
}

/** One quota window: how much is used against the reserve line, and when it resets. */
function Bar({ bar, reserve, now }: { bar: QuotaBar; reserve: number; now: Date }) {
  const used = Math.min(100, Math.max(0, bar.used))
  return (
    <div className="flex flex-col gap-1" data-window={bar.key}>
      <div className="flex items-baseline gap-2 text-[13px]">
        <span className="flex-1">{bar.label}</span>
        {bar.atReserve && <Badge className="border-state-needs text-state-needs">at the reserve</Badge>}
        <span className={cn('tabular-nums', bar.atReserve && 'font-semibold text-state-needs')}>{Math.round(bar.used)}% used</span>
      </div>
      <div
        role="progressbar"
        aria-label={`${bar.label} used`}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={Math.round(used)}
        className="relative h-2 rounded-full bg-muted"
      >
        <div className={cn('h-full rounded-full', bar.atReserve ? 'bg-state-needs' : 'bg-primary')} style={{ width: `${used}%` }} />
        {reserve > 0 && reserve < 100 && <div title={`reserve: ${reserve}%`} className="absolute -top-0.5 h-3 w-0.5 bg-foreground/70" style={{ left: `${reserve}%` }} />}
      </div>
      <p className="text-xs text-muted-foreground">{resetText(bar.resetsAt, now)}</p>
    </div>
  )
}

function CostBody({ cost }: { cost: GetCostResponse }) {
  const v = useMemo(() => costView(cost), [cost])
  const empty = v.tables.length === 0
  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-col gap-0.5 text-[13px]">
        <p>
          <span className="text-lg font-semibold tabular-nums" data-testid="cost-total">{v.tokens}</span>{' '}
          <span className="text-xs text-muted-foreground" data-testid="cost-scope">{v.scope}</span>
        </p>
        <p className="text-xs text-muted-foreground">
          Total = input + cache write + output. {v.breakdown}.
        </p>
        <p className="text-xs text-muted-foreground" data-testid="cost-usd">
          {v.usd} at API prices for Claude{v.pricesAsOf ? ` (prices as of ${v.pricesAsOf})` : ''}: a plan is billed otherwise, so the tokens are the measure.
        </p>
      </div>
      {empty && <p className="text-xs text-muted-foreground">No sessions in that scope yet.</p>}
      {v.tables.map((t) => (
        <Table key={t.title} table={t} />
      ))}
      {v.bases.map((b) => (
        <p key={b.cli} className="text-xs text-muted-foreground">{b.cli}: {b.basis}</p>
      ))}
      {v.notes.map((n) => (
        <p key={n} className="text-xs text-muted-foreground">{n}</p>
      ))}
    </div>
  )
}

function Table({ table }: { table: CostTable }) {
  return (
    <div className="flex flex-col gap-1">
      <h3 className="text-xs font-semibold">{table.title}</h3>
      <div className="overflow-x-auto">
        <table className="w-full text-xs" aria-label={table.title}>
          <thead>
            <tr className="border-b text-left text-muted-foreground">
              {table.headers.map((h, i) => (
                <th key={h} scope="col" className={cn('py-1 pr-3 font-medium', i > 0 && 'text-right')}>{h}</th>
              ))}
            </tr>
          </thead>
          <tbody>
            {table.rows.map((r, i) => (
              <tr key={i} className="border-b last:border-0">
                {r.map((c, j) => (
                  <td key={j} className={cn('py-1 pr-3', j > 0 && 'text-right tabular-nums', j === 0 && 'font-mono')}>{c}</td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}
