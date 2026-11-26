import { Copy, ExternalLink, MessageSquarePlus, PanelRightClose, Terminal } from 'lucide-react'
import { useMemo, useState } from 'react'
import { getCost, getUsage, sendMessage } from '@/actions'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/input'
import { MessageType } from '@/gen/agentmesh/v1/types_pb'
import { useApp, useAppState, useHostState } from '@/context'
import { joinWaiting, repoName, ROLE_LABEL } from '@/derive'
import { usePolled } from '@/polled'
import { cn } from '@/lib/utils'
import type { HostRuntime } from '@/model'
import { costOf, costRows, dollars, isLead, prRows, quotaRows, resetsIn, type PrRow } from '@/side-panel'

/**
 * Beside the open agent: for a run's lead the whole run (what it cost in total and per agent, what waits on the user,
 * the PRs, quota); for any other agent where it is (project, host, branch, with a terminal button), its model, its cost
 * and its PRs. What the host derives is read from the host (`GetCost`, `ListWaiting`, `ListPullRequests`, `GetUsage`).
 */
export function SidePanel({ host, agentId }: { host: HostRuntime; agentId?: string }) {
  const app = useApp()
  const agent = useHostState(host, (s) => (agentId ? s.agents[agentId] : undefined))
  return (
    <aside aria-label="Side panel" className="flex w-72 shrink-0 flex-col overflow-y-auto border-l bg-surface">
      <header className="flex items-center gap-2 border-b px-3 py-2">
        <h2 className="min-w-0 flex-1 truncate text-xs font-semibold tracking-wide text-muted-foreground uppercase">{agent ? (isLead(agent) ? 'Run' : 'Agent') : 'Details'}</h2>
        <Button size="icon" variant="ghost" className="size-6" aria-label="Hide the side panel" title="Hide the side panel" onClick={() => app.setPanel(false)}>
          <PanelRightClose className="size-3.5" />
        </Button>
      </header>
      {agent ? <ForAgent host={host} agentId={agent.id} /> : <p className="p-3 text-xs text-muted-foreground">Open an agent to see its run, cost and pull requests here.</p>}
      <Quota host={host} />
    </aside>
  )
}

function Section({ title, children, count }: { title: string; children: React.ReactNode; count?: number }) {
  return (
    <section aria-label={title} className="border-b px-3 py-2.5">
      <h3 className="mb-1.5 flex items-center gap-1.5 text-[11px] font-semibold tracking-wide text-muted-foreground uppercase">
        {title}
        {count !== undefined && <span className="font-normal">{count}</span>}
      </h3>
      {children}
    </section>
  )
}

function ForAgent({ host, agentId }: { host: HostRuntime; agentId: string }) {
  const app = useApp()
  const agent = useHostState(host, (s) => s.agents[agentId])!
  const run = useHostState(host, (s) => s.runs[agent.runId])
  const agents = useHostState(host, (s) => s.agents)
  const runs = useHostState(host, (s) => s.runs)
  const waiting = useHostState(host, (s) => s.waiting)
  const pullRequests = useHostState(host, (s) => s.pullRequests)
  const canTerminal = useHostState(host, (s) => !!s.daemon?.capabilities.includes('terminals'))
  const lead = isLead(agent)
  const cost = usePolled(() => getCost(host.client, { runId: agent.runId }, true), `${host.info.id}/${agent.runId}`, 30_000)
  const here = useMemo(() => joinWaiting(waiting, runs, agents).filter((w) => w.agent.runId === agent.runId), [waiting, runs, agents, agent.runId])
  const prs = useMemo(() => prRows(pullRequests, agents, lead ? (a) => a.runId === agent.runId : (a) => a.id === agent.id), [pullRequests, agents, lead, agent.runId, agent.id])
  const table = cost.value ? costRows(cost.value) : undefined
  const mine = lead ? undefined : costOf(cost.value, agent)
  return (
    <>
      <Section title={lead ? 'Run' : 'Where'}>
        <dl className="grid grid-cols-[4.5rem_1fr] gap-x-2 gap-y-1 text-xs">
          {lead && run && (
            <>
              <dt className="text-muted-foreground">Run</dt>
              <dd className="truncate" title={run.title}>{run.title || run.id}</dd>
            </>
          )}
          <dt className="text-muted-foreground">Project</dt>
          <dd className="truncate" title={agent.repo}>{repoName(agent.repo)}</dd>
          <dt className="text-muted-foreground">Host</dt>
          <dd className="truncate">{host.info.name}</dd>
          {agent.branch && (
            <>
              <dt className="text-muted-foreground">Branch</dt>
              <dd className="truncate font-mono" title={agent.branch}>{agent.branch}</dd>
            </>
          )}
          <dt className="text-muted-foreground">Role</dt>
          <dd>{ROLE_LABEL[agent.role]}</dd>
          <dt className="text-muted-foreground">Model</dt>
          <dd className="truncate">{[agent.kind, agent.model, agent.effort].filter(Boolean).join(' · ')}</dd>
        </dl>
        <div className="mt-2 flex gap-1.5">
          {canTerminal && agent.terminalId && (
            <Button size="sm" variant="outline" onClick={() => app.selectAgent(host.info.id, agent.id, 'terminal')}>
              <Terminal /> Terminal
            </Button>
          )}
          {agent.worktree && (
            <Button size="sm" variant="outline" title={agent.worktree} onClick={() => void navigator.clipboard?.writeText(agent.worktree)}>
              <Copy /> Copy path
            </Button>
          )}
        </div>
      </Section>
      <Section title="Cost">
        {cost.error && !cost.value && <p className="text-xs text-destructive">Couldn't read the cost: {cost.error}</p>}
        {cost.loading && !cost.value && <p className="text-xs text-muted-foreground">Loading…</p>}
        {table && (
          <>
            <p className="flex items-baseline justify-between text-xs">
              <span className="text-muted-foreground">{lead ? 'Run total' : 'This agent'}</span>
              <span className="text-sm font-semibold tabular-nums" data-testid="cost-total">{dollars(lead ? table.total : (mine ?? 0))}</span>
            </p>
            {!lead && <p className="text-[11px] text-muted-foreground">of {dollars(table.total)} for the run</p>}
            {lead && (
              <ul className="mt-1.5 space-y-0.5 text-xs" aria-label="Cost per agent">
                {table.rows.map((r) => (
                  <li key={r.name} className="flex justify-between gap-2">
                    <span className="truncate">{r.name}</span>
                    <span className="tabular-nums text-muted-foreground">{dollars(r.usd)}</span>
                  </li>
                ))}
                {table.rows.length === 0 && <li className="text-muted-foreground">Nothing spent yet.</li>}
              </ul>
            )}
          </>
        )}
      </Section>
      {lead && (
        <Section title="Waiting on you" count={here.length}>
          {here.length === 0 ? (
            <p className="text-xs text-muted-foreground">Nothing in this run.</p>
          ) : (
            <ul className="space-y-0.5">
              {here.map((w) => (
                <li key={`${w.kind}:${w.message?.id ?? w.agent.id}`}>
                  <button
                    type="button"
                    onClick={() => app.selectAgent(host.info.id, w.agent.id, w.kind === 'input' ? 'terminal' : 'messages')}
                    className="w-full truncate rounded px-1 py-0.5 text-left text-xs hover:bg-accent"
                  >
                    <span className="font-medium">{w.agent.name}</span> {w.kind === 'plan' ? 'sent a plan' : w.kind === 'question' ? 'asked a question' : 'is stopped at a prompt'}
                  </button>
                </li>
              ))}
            </ul>
          )}
        </Section>
      )}
      <Section title="Pull requests" count={prs.length}>
        {prs.length === 0 ? <p className="text-xs text-muted-foreground">None yet.</p> : (
          <ul className="space-y-1">
            {prs.map((p) => (
              <PrItem key={p.url || `${p.agentId}#${p.number}`} pr={p} showAgent={lead} host={host} />
            ))}
          </ul>
        )}
      </Section>
    </>
  )
}

/** A comment on a PR goes to the agent that wrote it, as a task: it works in the same conversation, on the same branch. */
function CommentBox({ host, pr, onDone }: { host: HostRuntime; pr: PrRow; onDone: () => void }) {
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  async function send() {
    const body = text.trim()
    if (!body || busy) return
    setBusy(true)
    setError('')
    try {
      await sendMessage(host.client, pr.agentId, MessageType.TASK, `Comment on ${pr.url || `PR #${pr.number}`}:\n\n${body}`)
      onDone()
    } catch (e) {
      setError(e instanceof Error ? e.message.replace(/^\[[a-z_]+\]\s*/, '') : String(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <form
      className="mt-1 flex flex-col gap-1 px-1"
      onSubmit={(e) => {
        e.preventDefault()
        void send()
      }}
    >
      <Textarea aria-label={`Comment on #${pr.number} for ${pr.agentName}`} rows={3} autoFocus value={text} onChange={(e) => setText(e.target.value)} placeholder={`Tell ${pr.agentName} what to change`} />
      {error && <p role="alert" className="text-[11px] text-destructive">{error}</p>}
      <div className="flex justify-end gap-1">
        <Button size="sm" variant="ghost" onClick={onDone}>Cancel</Button>
        <Button size="sm" type="submit" disabled={busy || !text.trim()}>Send to {pr.agentName}</Button>
      </div>
    </form>
  )
}

const CHECKS_TONE: Record<string, string> = { pass: 'border-state-done text-state-done', failing: 'border-destructive text-destructive', pending: 'border-state-needs text-state-needs' }

function PrItem({ pr, showAgent, host }: { pr: PrRow; showAgent: boolean; host: HostRuntime }) {
  const [commenting, setCommenting] = useState(false)
  return (
    <li className="text-xs">
      <button
        type="button"
        disabled={!pr.url}
        title={pr.url}
        onClick={() => window.desktop?.openExternal(pr.url)}
        className="flex w-full items-center gap-1.5 rounded px-1 py-0.5 text-left hover:bg-accent disabled:opacity-60"
      >
        <span className="font-medium">#{pr.number}</span>
        <Badge className={cn(pr.state === 'open' && !pr.draft && 'border-primary text-primary')}>{pr.draft && pr.state === 'open' ? 'draft' : pr.state}</Badge>
        {pr.checks && <Badge className={CHECKS_TONE[pr.checks]}>{pr.checks === 'failing' ? 'checks failing' : `checks ${pr.checks}`}</Badge>}
        {pr.stale && <span className="text-muted-foreground" title="GitHub couldn't be read the last time: this is what was seen before">stale</span>}
        <ExternalLink className="ml-auto size-3 shrink-0 text-muted-foreground" />
      </button>
      <div className="flex items-center gap-1 px-1">
        {showAgent && <p className="min-w-0 flex-1 truncate text-[11px] text-muted-foreground">{pr.agentName}</p>}
        {pr.state === 'open' && (
          <Button size="sm" variant="ghost" className="ml-auto h-5 gap-1 px-1 text-[11px]" aria-expanded={commenting} aria-label={`Comment on #${pr.number}`} onClick={() => setCommenting(!commenting)}>
            <MessageSquarePlus className="size-3" /> Comment
          </Button>
        )}
      </div>
      {commenting && <CommentBox host={host} pr={pr} onDone={() => setCommenting(false)} />}
    </li>
  )
}

function Quota({ host }: { host: HostRuntime }) {
  const usage = usePolled(() => getUsage(host.client), host.info.id, 60_000)
  const q = useMemo(() => (usage.value ? quotaRows(usage.value) : undefined), [usage.value])
  return (
    <Section title="Quota">
      {usage.loading && !q && !usage.error && <p className="text-xs text-muted-foreground">Loading…</p>}
      {usage.error && !q && <p className="text-xs text-muted-foreground">No reading: {usage.error}</p>}
      {q && q.rows.length === 0 && <p className="text-xs text-muted-foreground">The host has no quota reading.</p>}
      {q && q.rows.length > 0 && (
        <ul className="space-y-2" aria-label="Quota">
          {q.rows.map((r) => (
            <li key={r.label} className="text-xs">
              <div className="flex justify-between gap-2">
                <span className="truncate">{r.label}</span>
                <span className={cn('tabular-nums', r.atReserve ? 'font-semibold text-state-needs' : 'text-muted-foreground')}>{Math.round(r.used)}%</span>
              </div>
              <div className="relative mt-0.5 h-1.5 overflow-hidden rounded-full bg-muted" role="progressbar" aria-label={r.label} aria-valuenow={Math.round(r.used)} aria-valuemin={0} aria-valuemax={100}>
                <div className={cn('h-full rounded-full', r.atReserve ? 'bg-state-needs' : 'bg-primary')} style={{ width: `${Math.min(100, Math.max(0, r.used))}%` }} />
                {q.reserve < 100 && <div className="absolute inset-y-0 w-px bg-foreground/60" style={{ left: `${q.reserve}%` }} title={`Reserve: nothing new starts at ${q.reserve}%`} />}
              </div>
              {r.resetsAt && <p className="text-[11px] text-muted-foreground">{resetsIn(r.resetsAt)}</p>}
            </li>
          ))}
        </ul>
      )}
    </Section>
  )
}
