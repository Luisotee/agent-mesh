import { Archive, ArchiveRestore, FileCode2 } from 'lucide-react'
import { useState } from 'react'
import { archiveAgent, getAgentChanges, getAgentProgress, openInEditor, resumeAgent } from '@/actions'
import { archiveAdvice, changeSummary, contextMeter, planSummary, STATUS_MARK } from '@/agent-work'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent } from '@/components/ui/dialog'
import { useHostState } from '@/context'
import type { Agent } from '@/gen/agentmesh/v1/types_pb'
import { AgentState } from '@/gen/agentmesh/v1/types_pb'
import type { HostRuntime } from '@/model'
import { usePolled } from '@/polled'
import { cn } from '@/lib/utils'
import { canPair } from './pair-panel'

const say = (e: unknown) => (e instanceof Error ? e.message.replace(/^\[[a-z_]+\]\s*/, '') : String(e))

/** A titled block of the side panel (the same look as its other sections). */
function Section({ title, count, children }: { title: string; count?: string; children: React.ReactNode }) {
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

/** The agent's plan (its todo list) and its context meter, from its CLI's record: for a CLI the host can't read, nothing. */
export function Progress({ host, agent }: { host: HostRuntime; agent: Agent }) {
  const got = usePolled(async () => getAgentProgress(host.client, agent.id), `${host.info.id}/${agent.id}/${agent.state}`, 20_000, !agent.archived)
  const p = got.value
  if (!p?.supported) return null
  const plan = planSummary(p.plan)
  const ctx = contextMeter(p.contextTokens, p.contextWindow)
  if (plan.total === 0 && !ctx) return null
  return (
    <Section title="Progress" count={plan.total > 0 ? `${plan.done} of ${plan.total}` : undefined}>
      {plan.total > 0 && (
        <>
          <progress aria-label="Plan progress" max={plan.total} value={plan.done} className="h-1.5 w-full overflow-hidden rounded bg-muted accent-primary" />
          {plan.current && <p className="mt-1 truncate text-xs" title={plan.current}>Now: {plan.current}</p>}
          <ol aria-label="Plan" className="mt-1.5 space-y-0.5 text-xs">
            {p.plan.map((it, i) => (
              <li key={i} className={cn('flex gap-1.5', it.status === 'completed' && 'text-muted-foreground line-through')}>
                <span aria-hidden className="shrink-0">{it.status === 'completed' ? '✓' : it.status === 'in_progress' ? '›' : '·'}</span>
                <span className="min-w-0 break-words">{it.content}</span>
              </li>
            ))}
          </ol>
        </>
      )}
      {ctx && (
        <div className={cn(plan.total > 0 && 'mt-2')}>
          <p className="flex justify-between text-[11px] text-muted-foreground"><span>Context</span><span data-testid="context-text">{ctx.text}</span></p>
          <meter aria-label="Context used" min={0} max={100} low={70} high={90} optimum={0} value={ctx.pct} className="mt-0.5 h-1.5 w-full" />
        </div>
      )}
    </Section>
  )
}

/** The files the agent changed since its branch started, and what is uncommitted or on no remote: read from its worktree (`GetAgentChanges`). */
export function Changes({ host, agent }: { host: HostRuntime; agent: Agent }) {
  const got = usePolled(async () => getAgentChanges(host.client, agent.id), `${host.info.id}/${agent.id}/${agent.state}/${agent.archived}`, 20_000, !!agent.worktree)
  const c = got.value
  if (!agent.worktree) return null
  const shown = c?.files.slice(0, 8) ?? []
  return (
    <Section title="Files changed" count={c?.available ? String(c.files.length) + (c.truncated ? '+' : '') : undefined}>
      {got.error && !c && <p className="text-xs text-destructive">Couldn't read the changes: {got.error}</p>}
      {got.loading && !c && <p className="text-xs text-muted-foreground">Loading…</p>}
      {c && (
        <>
          <p className="text-xs text-muted-foreground" data-testid="change-summary">{changeSummary(c)}</p>
          {c.available && c.reason && <p className="text-[11px] text-muted-foreground">{c.reason}</p>}
          {shown.length > 0 && (
            <ul aria-label="Changed files" className="mt-1.5 space-y-0.5 text-xs">
              {shown.map((f) => (
                <li key={f.path} className="flex items-baseline gap-1.5">
                  <span aria-label={f.status} title={f.status} className={cn('w-3 shrink-0 text-center font-mono', f.uncommitted && 'text-state-needs')}>{STATUS_MARK[f.status] ?? '·'}</span>
                  <span className="min-w-0 flex-1 truncate font-mono" title={f.path}>{f.path}</span>
                  {(f.additions > 0 || f.deletions > 0) && <span className="shrink-0 tabular-nums text-muted-foreground">+{f.additions} −{f.deletions}</span>}
                </li>
              ))}
              {c.files.length > shown.length && <li className="text-muted-foreground">and {c.files.length - shown.length}{c.truncated ? '+' : ''} more</li>}
            </ul>
          )}
        </>
      )}
    </Section>
  )
}

/**
 * Open in editor (the host's own device only: it runs the host's `editor.command` on the host's machine), Archive (retire,
 * remove the worktree, keep the branch) or, for an archived agent, Restore (the worktree is made again from the branch).
 */
export function WorktreeActions({ host, agent }: { host: HostRuntime; agent: Agent }) {
  const local = useHostState(host, (s) => s.conn.kind === 'ready' && canPair(host.store))
  const [error, setError] = useState('')
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)
  if (!agent.worktree) return null
  async function run(what: () => Promise<unknown>, done = '') {
    if (busy) return
    setBusy(true)
    setError('')
    setNote('')
    try {
      await what()
      setNote(done)
    } catch (e) {
      setError(say(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="mt-2 flex flex-col gap-1.5">
      {agent.archived && (
        <p role="status" className="text-xs text-muted-foreground">Archived: its worktree was removed and its branch <code className="font-mono">{agent.branch}</code> kept.</p>
      )}
      <div className="flex flex-wrap gap-1.5">
        {local && !agent.archived && (
          <Button size="sm" variant="outline" disabled={busy} title="Open its worktree in the host's editor (the host's editor.command setting)" onClick={() => void run(() => openInEditor(host.client, agent.id), 'Opened in the editor.')}>
            <FileCode2 /> Open in editor
          </Button>
        )}
        {agent.archived ? (
          <Button size="sm" variant="outline" disabled={busy} onClick={() => void run(() => resumeAgent(host.client, agent.id), 'Restoring: the worktree is made again from its branch.')}>
            <ArchiveRestore /> Restore
          </Button>
        ) : (
          <ArchiveButton host={host} agent={agent} disabled={busy} />
        )}
      </div>
      {note && <p role="status" className="text-[11px] text-muted-foreground">{note}</p>}
      {error && <p role="alert" className="text-[11px] text-destructive">{error}</p>}
    </div>
  )
}

const LIVE: readonly AgentState[] = [AgentState.WORKING, AgentState.NEEDS_INPUT, AgentState.STARTING]

/** Archive with the warning the host's own check gives: uncommitted work blocks it, commits on no remote ask to be confirmed. */
function ArchiveButton({ host, agent, disabled }: { host: HostRuntime; agent: Agent; disabled: boolean }) {
  const [open, setOpen] = useState(false)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [sure, setSure] = useState(false)
  const read = usePolled(async () => getAgentChanges(host.client, agent.id), `archive/${host.info.id}/${agent.id}`, 5_000, open)
  const advice = archiveAdvice(read.value)
  async function confirm() {
    if (busy) return
    setBusy(true)
    setError('')
    try {
      await archiveAgent(host.client, agent.id, !!advice.warn)
      setOpen(false)
      setSure(false)
    } catch (e) {
      setError(say(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <>
      <Button size="sm" variant="outline" disabled={disabled} onClick={() => setOpen(true)}>
        <Archive /> Archive
      </Button>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent title={`Archive ${agent.name}?`} description="Stops it and removes its worktree from the disk; its branch is kept, and a message to it (or Restore) makes the worktree again.">
          <div className="flex flex-col gap-3 text-sm">
            {LIVE.includes(agent.state) && <p className="text-xs text-state-needs">It is {agent.state === AgentState.WORKING ? 'working' : 'busy'} now: archiving stops it.</p>}
            {read.value && <p className="text-xs text-muted-foreground">{changeSummary(read.value)}</p>}
            {advice.blocked && <p role="alert" className="text-xs text-destructive">{advice.blocked}</p>}
            {advice.warn && (
              <label className="flex items-start gap-2 text-xs">
                <input type="checkbox" checked={sure} onChange={(e) => setSure(e.target.checked)} className="mt-0.5" />
                <span>{advice.warn} Archive anyway.</span>
              </label>
            )}
            <p className="text-xs text-muted-foreground">Ignored files in the worktree (such as <code className="font-mono">node_modules</code>) go with it; the ones listed in the repo's <code className="font-mono">.worktreeinclude</code> are copied back on restore.</p>
            {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
            <div className="flex justify-end gap-2">
              <Button variant="ghost" onClick={() => setOpen(false)}>Cancel</Button>
              <Button variant="destructive" disabled={busy || !!advice.blocked || (!!advice.warn && !sure)} onClick={() => void confirm()}>Archive</Button>
            </div>
          </div>
        </DialogContent>
      </Dialog>
    </>
  )
}
