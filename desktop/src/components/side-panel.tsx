import { CircleCheck, CircleX, Copy, ExternalLink, Hand, LoaderCircle, MessageSquarePlus, PanelRightClose, Terminal, ChevronRight } from 'lucide-react'
import { MarkdownField } from '@/components/ui/markdown-field'
import { useMemo, useState } from 'react'
import { getAgent, getCost, holdPullRequest, sendMessage } from '@/actions'
import { RUN_LEVEL_LABEL, runLevelOf } from '@/autonomy'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Chip } from '@/components/ui/chip'
import { Fold } from '@/components/ui/collapsible'
import { SkeletonLines } from '@/components/ui/skeleton'
import { Tip } from '@/components/ui/tooltip'
import { ModelControl } from './model-control'
import { Changes, Progress, WorktreeActions } from './agent-work'
import { StateIcon } from './state-icon'
import { MessageType, type Agent } from '@/gen/agentmesh/v1/types_pb'
import { useApp, useAppState, useHostState } from '@/context'
import { isActive } from '@/active'
import { joinWaiting, repoName, ROLE_LABEL } from '@/derive'
import { usePolled } from '@/polled'
import { cn } from '@/lib/utils'
import type { HostRuntime } from '@/model'
import { leadRows, type WorkerRow } from '@/sidebar'
import { costOf, costRows, dollars, historyRows, isLead, prRows, type PrRow } from '@/side-panel'

/**
 * Beside the open agent: its whole tree first (the lead, its workers and their reviewers and askers; a click opens
 * one), then the pull requests of the run (for a lead) or of the agent, what it is doing, the cost folded to one
 * line, its model history and its details. What the host derives is read from the host (`GetCost`, `ListPullRequests`).
 * Autonomy is in the project's settings, the quota in the sidebar's footer.
 */
export function SidePanel({ host, agentId }: { host: HostRuntime; agentId?: string }) {
  const app = useApp()
  const agent = useHostState(host, (s) => (agentId ? s.agents[agentId] : undefined))
  const run = useHostState(host, (s) => (agent ? s.runs[agent.runId] : undefined))
  return (
    <aside aria-label="Side panel" className="flex w-72 shrink-0 flex-col overflow-y-auto border-l bg-surface">
      <header className="flex items-center gap-2 border-b px-3 py-2">
        <h2 className="min-w-0 flex-1 truncate text-[13px] font-semibold">{agent ? run?.title || agent.name : 'Details'}</h2>
        <Tip label="Hide the side panel">
          <Button size="icon" variant="ghost" className="size-6" aria-label="Hide the side panel" onClick={() => app.setPanel(false)}>
            <PanelRightClose className="size-3.5" />
          </Button>
        </Tip>
      </header>
      {agent ? <ForAgent host={host} agentId={agent.id} /> : <p className="p-3 text-xs text-muted-foreground">Open an agent to see its tree, pull requests and cost here.</p>}
    </aside>
  )
}

function Section({ title, children, count }: { title: string; children: React.ReactNode; count?: number }) {
  return (
    <section aria-label={title} className="border-b px-3 py-2.5">
      <h3 className="mb-1.5 flex items-center gap-1.5 text-xs font-semibold text-muted-foreground">
        {title}
        {count !== undefined && <span className="font-normal tabular-nums">{count}</span>}
      </h3>
      {children}
    </section>
  )
}

function ForAgent({ host, agentId }: { host: HostRuntime; agentId: string }) {
  const agent = useHostState(host, (s) => s.agents[agentId])!
  const agents = useHostState(host, (s) => s.agents)
  const pullRequests = useHostState(host, (s) => s.pullRequests)
  const lead = isLead(agent)
  const cost = usePolled(() => getCost(host.client, { runId: agent.runId }, true), `${host.info.id}/${agent.runId}`, 30_000)
  const prs = useMemo(() => prRows(pullRequests, agents, lead ? (a) => a.runId === agent.runId : (a) => a.id === agent.id), [pullRequests, agents, lead, agent.runId, agent.id])
  const table = cost.value ? costRows(cost.value) : undefined
  const mine = lead ? undefined : costOf(cost.value, agent)
  const total = table ? dollars(lead ? table.total : (mine ?? 0)) : undefined
  return (
    <>
      <AgentTree host={host} agent={agent} />
      <PullRequests prs={prs} lead={lead} host={host} />
      <Progress host={host} agent={agent} />
      <Changes host={host} agent={agent} />
      <div className="border-b px-3 pb-2.5 empty:hidden"><WorktreeActions key={agent.id} host={host} agent={agent} /></div>
      <Fold title={lead ? 'Run total' : 'Cost'} summary={total ?? (cost.error ? 'unread' : undefined)}>
        {cost.error && !cost.value && <p className="text-xs text-destructive">Couldn't read the cost: {cost.error}</p>}
        {cost.loading && !cost.value && <SkeletonLines label="Reading the cost" lines={2} />}
        {table && (
          <>
            <p className="flex items-baseline justify-between text-xs">
              <span className="text-muted-foreground">{lead ? 'Run total' : 'This agent'}</span>
              <span className="text-sm font-semibold tabular-nums" data-testid="cost-total">{total}</span>
            </p>
            {!lead && <p className="text-xs text-muted-foreground">of {dollars(table.total)} for the run</p>}
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
      </Fold>
      <ModelHistory host={host} agent={agent} />
      <Details host={host} agent={agent} />
    </>
  )
}

/** The open agent's tree: its run's lead, the workers, and the reviewers and askers of each, with their state. */
function AgentTree({ host, agent }: { host: HostRuntime; agent: Agent }) {
  const app = useApp()
  const runs = useHostState(host, (s) => s.runs)
  const agents = useHostState(host, (s) => s.agents)
  const waiting = useHostState(host, (s) => s.waiting)
  const waitingAll = useHostState(host, (s) => s.waitingAll)
  const [showResting, setShowResting] = useState(false)
  const row = useMemo(() => {
    const run = runs[agent.runId]
    if (!run) return undefined
    const ids = new Set(joinWaiting(waiting, runs, agents).map((w) => w.agent.id))
    const mine = Object.fromEntries(Object.entries(agents).filter(([, a]) => a.runId === run.id))
    return leadRows({ hostId: host.info.id, hostName: host.info.name, here: true, up: true, loading: false, runs: { [run.id]: run }, agents: mine, waiting: ids })[0]
  }, [runs, agents, waiting, agent.runId, host.info.id, host.info.name])
  const waits = useMemo(() => new Set([...waitingAll, ...waiting].map((w) => w.agentId)), [waitingAll, waiting])
  if (!row) return null
  // Live agents only; the idle and finished ones rest behind a count. A parent of a live agent stays, so the tree keeps its shape.
  const live = (a: Agent) => isActive(a, { waiting: waits.has(a.id), open: a.id === agent.id })
  const workerLive = (w: WorkerRow): boolean => live(w.agent) || w.helpers.some(live) || w.workers.some(workerLive)
  const shown = (w: WorkerRow) => showResting || workerLive(w)
  const item = (a: Agent, depth: number) => (
    <li key={a.id}>
      <button
        type="button"
        aria-current={a.id === agent.id ? 'true' : undefined}
        onClick={() => app.selectAgent(host.info.id, a.id)}
        style={{ paddingLeft: `${0.25 + depth * 0.875}rem` }}
        className={cn('flex w-full items-center gap-1.5 rounded py-1 pr-1 text-left text-xs outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring', a.id === agent.id && 'bg-accent font-medium', a.needsInput > 0 && 'text-glow')}
      >
        <StateIcon agent={a} className="size-3.5" />
        <span className="min-w-0 flex-1 truncate">{a.name}</span>
        <Chip>{ROLE_LABEL[a.role]}</Chip>
      </button>
    </li>
  )
  const helpers = (hs: Agent[]) => (showResting ? hs : hs.filter(live))
  const rendered: Array<[Agent, number]> = []
  const walk = (w: WorkerRow, depth: number) => {
    rendered.push([w.agent, depth])
    for (const h of helpers(w.helpers)) rendered.push([h, depth + 1])
    for (const c of w.workers.filter(shown)) walk(c, depth + 1)
  }
  if (row.lead) rendered.push([row.lead, 0])
  for (const h of helpers(row.helpers)) rendered.push([h, 1])
  for (const w of row.workers.filter(shown)) walk(w, 1)
  const total = (w: WorkerRow): number => 1 + w.helpers.length + w.workers.reduce((n, c) => n + total(c), 0)
  const hidden = (row.lead ? 1 : 0) + row.helpers.length + row.workers.reduce((n, w) => n + total(w), 0) - rendered.length
  return (
    <Section title="Agent tree">
      <ul aria-label="Agent tree">{rendered.map(([a, depth]) => item(a, depth))}</ul>
      {(hidden > 0 || showResting) && (
        <button type="button" aria-pressed={showResting} onClick={() => setShowResting(!showResting)} className="mt-1 flex items-center gap-1 rounded px-1 py-0.5 text-xs text-muted-foreground outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring">
          <ChevronRight className={cn('size-3 transition-transform', showResting && 'rotate-90')} />
          {showResting ? 'Hide resting' : `${hidden} resting`}
        </button>
      )}
    </Section>
  )
}

/** Where the agent is and what it is, folded: project, host, branch, worktree, session and ids, its model, its terminal. */
function Details({ host, agent }: { host: HostRuntime; agent: Agent }) {
  const app = useApp()
  const run = useHostState(host, (s) => s.runs[agent.runId])
  const canTerminal = useHostState(host, (s) => !!s.daemon?.capabilities.includes('terminals'))
  const own = runLevelOf(run?.autonomy)
  const reveal = useAppState((s) => s.modelShown)
  const rows: Array<[string, string, boolean?]> = [
    ['Project', repoName(agent.repo)],
    ['Host', host.info.name],
    ['Role', ROLE_LABEL[agent.role]],
    ['Branch', agent.branch, true],
    ['Worktree', agent.worktree, true],
    ['Session', agent.sessionId, true],
    ['Agent id', agent.id, true],
    ['Run id', agent.runId, true],
  ]
  return (
    <Fold title="Details" reveal={reveal} onRevealed={() => app.modelRevealed()}>
      <dl className="grid grid-cols-[4.5rem_1fr] gap-x-2 gap-y-1 text-xs">
        {rows.filter(([, v]) => v).map(([k, v, mono]) => (
          <div key={k} className="contents">
            <dt className="text-muted-foreground">{k}</dt>
            <dd className={cn('truncate', mono && 'font-mono')} title={v}>{v}</dd>
          </div>
        ))}
        {own && (
          <>
            <dt className="text-muted-foreground">Autonomy</dt>
            <dd>{RUN_LEVEL_LABEL[own]} <span className="text-muted-foreground">(this run's own)</span></dd>
          </>
        )}
        {run?.autonomy && run.autonomy.askBefore.length > 0 && (
          <>
            <dt className="text-muted-foreground">Asks before</dt>
            <dd>{run.autonomy.askBefore.join('; ')}</dd>
          </>
        )}
      </dl>
      <ModelControl host={host} agent={agent} className="mt-1.5" />
      <div className="mt-2 flex gap-1.5">
        {canTerminal && agent.terminalId && (
          <Button size="sm" variant="outline" onClick={() => app.selectAgent(host.info.id, agent.id, 'terminal')}>
            <Terminal /> Terminal
          </Button>
        )}
        {agent.worktree && (
          <Button size="sm" variant="outline" onClick={() => void navigator.clipboard?.writeText(agent.worktree)}>
            <Copy /> Copy path
          </Button>
        )}
      </div>
    </Fold>
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
      <MarkdownField aria-label={`Comment on #${pr.number} for ${pr.agentName}`} className="min-h-16" autoFocus value={text} onValueChange={setText} placeholder={`Tell ${pr.agentName} what to change`} />
      {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
      <div className="flex justify-end gap-1">
        <Button size="sm" variant="ghost" onClick={onDone}>Cancel</Button>
        <Button size="sm" type="submit" disabled={busy || !text.trim()}>Send to {pr.agentName}</Button>
      </div>
    </form>
  )
}

/** A PR's checks as one icon (✓ ✗ ◐) with the word in its hint, so a row says it without a badge of words. */
const CHECKS: Record<string, { icon: typeof CircleCheck; tone: string; label: string }> = {
  pass: { icon: CircleCheck, tone: 'text-state-done', label: 'Checks pass' },
  failing: { icon: CircleX, tone: 'text-destructive', label: 'Checks failing' },
  pending: { icon: LoaderCircle, tone: 'text-state-needs', label: 'Checks running' },
}

/**
 * The open pull requests of the agent (of the whole run for a lead) first, as they are the ones to act on; the merged and
 * closed ones are folded under a count, since a stack of finished PRs would bury them.
 */
function PullRequests({ prs, lead, host }: { prs: PrRow[]; lead: boolean; host: HostRuntime }) {
  const [showDone, setShowDone] = useState(false)
  const open = prs.filter((p) => p.state === 'open')
  const done = prs.filter((p) => p.state !== 'open')
  const item = (p: PrRow) => <PrItem key={p.url || `${p.agentId}#${p.number}`} pr={p} showAgent={lead} host={host} />
  return (
    <Section title="Pull requests" count={prs.length}>
      {prs.length === 0 && <p className="text-xs text-muted-foreground">None yet.</p>}
      {open.length === 0 && done.length > 0 && <p className="text-xs text-muted-foreground">None open.</p>}
      {open.length > 0 && <ul aria-label="Open pull requests" className="space-y-1">{open.map(item)}</ul>}
      {done.length > 0 && (
        <>
          <button type="button" aria-expanded={showDone} onClick={() => setShowDone(!showDone)} className="mt-1.5 flex w-full items-center gap-1 rounded text-left text-xs text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring">
            <ChevronRight className={cn('size-3 transition-transform motion-reduce:transition-none', showDone && 'rotate-90')} />
            Merged and closed <span className="tabular-nums">{done.length}</span>
          </button>
          {showDone && <ul aria-label="Merged and closed pull requests" className="mt-1 space-y-1">{done.map(item)}</ul>}
        </>
      )}
    </Section>
  )
}

function PrItem({ pr, showAgent, host }: { pr: PrRow; showAgent: boolean; host: HostRuntime }) {
  const [commenting, setCommenting] = useState(false)
  const [holdError, setHoldError] = useState('')
  async function toggleHold() {
    setHoldError('')
    try {
      await holdPullRequest(host.client, pr.url, pr.held)
    } catch (e) {
      setHoldError(e instanceof Error ? e.message.replace(/^\[[a-z_]+\]\s*/, '') : String(e))
    }
  }
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
        {pr.checks && CHECKS[pr.checks] && (
          <Tip label={CHECKS[pr.checks]!.label}>
            <span role="img" aria-label={CHECKS[pr.checks]!.label} className={CHECKS[pr.checks]!.tone}>
              {(() => { const I = CHECKS[pr.checks]!.icon; return <I className={cn('size-3.5', pr.checks === 'pending' && 'animate-spin motion-reduce:animate-none')} /> })()}
            </span>
          </Tip>
        )}
        {pr.held && <Badge className="border-state-needs text-state-needs">held</Badge>}
        {pr.stale && <Tip label="GitHub couldn't be read the last time: this is what was seen before"><span className="text-muted-foreground">stale</span></Tip>}
        <ExternalLink className="ml-auto size-3 shrink-0 text-muted-foreground" />
      </button>
      <div className="flex items-center gap-1 px-1">
        {showAgent && <p className="min-w-0 flex-1 truncate text-xs text-muted-foreground">{pr.agentName}</p>}
        {pr.state === 'open' && (
<Tip label={pr.held ? 'Let a root merge it again' : 'No root merges it until you release it'}>
                    <Button size="sm" variant="ghost" className="h-5 gap-1 px-1 text-xs" aria-pressed={pr.held} aria-label={`${pr.held ? 'Release' : 'Hold'} the merge of #${pr.number}`} onClick={() => void toggleHold()}>
            <Hand className="size-3" /> {pr.held ? 'Release' : 'Hold merge'}
          </Button>
          </Tip>
        )}
        {pr.state === 'open' && (
          <Button size="sm" variant="ghost" className="ml-auto h-5 gap-1 px-1 text-xs" aria-expanded={commenting} aria-label={`Comment on #${pr.number}`} onClick={() => setCommenting(!commenting)}>
            <MessageSquarePlus className="size-3" /> Comment
          </Button>
        )}
      </div>
      {holdError && <p role="alert" className="px-1 text-xs text-destructive">{holdError}</p>}
      {commenting && <CommentBox host={host} pr={pr} onDone={() => setCommenting(false)} />}
    </li>
  )
}

/** The models the agent ran on, oldest first: read from the host (`GetAgent`) when the agent is shown and again when its model or effort changes. */
function ModelHistory({ host, agent }: { host: HostRuntime; agent: Agent }) {
  const got = usePolled(async () => getAgent(host.client, agent.id), `${host.info.id}/${agent.id}/${agent.model}/${agent.effort}`, 120_000)
  const rows = historyRows(got.value?.modelHistory ?? [])
  // Older hosts keep none; one row is no history (the model is shown above).
  if (rows.length < 2) return null
  return (
    <Fold title="Model history" summary={String(rows.length)}>
      <ol aria-label="Model history" className="space-y-0.5 text-xs">
        {rows.map((r, i) => (
          <li key={i} className="flex justify-between gap-2">
            <span className="truncate">{r.text}</span>
            <span className="shrink-0 text-muted-foreground" title={r.at ? new Date(r.at).toLocaleString() : undefined}>{r.why}</span>
          </li>
        ))}
      </ol>
    </Fold>
  )
}
