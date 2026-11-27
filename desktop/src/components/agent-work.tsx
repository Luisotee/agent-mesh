import { Archive, ArchiveRestore, FileCode2 } from 'lucide-react'
import { useEffect, useState } from 'react'
import { archiveAgent, getAgentChanges, getAgentProgress, openInEditor, resumeAgent } from '@/actions'
import { ActorClass } from '@/gen/agentmesh/v1/actor_pb'
import { archiveAdvice, canArchive, changeSummary, contextMeter, contextTone, ignoredLines, markPct, planSummary, sizeText, STATUS_MARK } from '@/agent-work'
import { Button } from '@/components/ui/button'
import { SkeletonLines } from '@/components/ui/skeleton'
import { Tip } from '@/components/ui/tooltip'
import { Dialog, DialogContent } from '@/components/ui/dialog'
import { useHostState } from '@/context'
import type { AgentWait } from '@/gen/agentmesh/v1/mesh_pb'
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
      <h3 className="mb-1.5 flex items-center gap-1.5 text-xs font-semibold text-muted-foreground">
        {title}
        {count !== undefined && <span className="font-normal">{count}</span>}
      </h3>
      {children}
    </section>
  )
}

/**
 * What an idle agent waits on (its background job, a wait it declared, an answer, a message), read from the host while it is
 * idle and shown: again when its state changes. `undefined` until read, and for an agent that is not idle.
 */
export function useAgentWait(host: HostRuntime, agent: Agent | undefined, active: boolean): AgentWait | undefined {
  const idle = agent?.state === AgentState.IDLE
  const id = agent?.id ?? ''
  // What was read in an earlier idle stretch is another read: the key moves on when the agent leaves idle.
  const [left, setLeft] = useState(0)
  useEffect(() => { if (!idle) setLeft((n) => n + 1) }, [idle])
  const got = usePolled(async () => (await getAgentProgress(host.client, id)).wait, `${host.info.id}/${id}/wait/${left}`, 15_000, active && idle && !agent?.archived, String(agent?.state))
  return idle ? got.value : undefined
}

/** The agent's plan (its todo list), from its CLI's record: for a CLI the host can't read, nothing. Its context is the composer's ring. */
export function Progress({ host, agent }: { host: HostRuntime; agent: Agent }) {
  const got = usePolled(async () => getAgentProgress(host.client, agent.id), `${host.info.id}/${agent.id}`, 20_000, !agent.archived, String(agent.state))
  const p = got.value
  if (!p?.supported) return null
  const plan = planSummary(p.plan)
  if (plan.total === 0) return null
  return (
    <Section title="Progress" count={`${plan.done} of ${plan.total}`}>
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
    </Section>
  )
}

/**
 * How much of its context window the agent's conversation fills, as a short bar by the composer ("84k / 200k"; its CLI's
 * record: for a CLI the host can't read, nothing). A tick sits where the host acts on the conversation (`GetAgentProgress.marks`:
 * a lead's reset, a compaction), the bar turns amber from the first one and red from 90%; the tooltip says what each does.
 */
export function ContextMeter({ host, agent, active }: { host: HostRuntime; agent: Agent; active: boolean }) {
  const got = usePolled(async () => getAgentProgress(host.client, agent.id), `${host.info.id}/${agent.id}/ring`, 30_000, active && !agent.archived, String(agent.state))
  const p = got.value
  const ctx = p?.supported ? contextMeter(p.contextTokens, p.contextWindow) : undefined
  if (!p || !ctx) return null
  const tokens = Number(p.contextTokens)
  const window = Number(p.contextWindow)
  const marks = p.marks.map((m) => ({ ...m, tokens: Number(m.tokens) })).filter((m) => markPct(m.tokens, window) !== undefined)
  const tone = contextTone(tokens, window, marks.map((m) => m.tokens))
  return (
    <Tip
      side="top"
      label={
        <div className="flex max-w-64 flex-col gap-0.5">
          <span>Context: {ctx.text}</span>
          {marks.map((m) => <span key={m.setting} className="text-muted-foreground">{m.label} at {sizeText(m.tokens)}</span>)}
          <span className="text-muted-foreground">The CLI also compacts on its own when it nears full.</span>
        </div>
      }
    >
      <span role="meter" aria-label="Context used" aria-valuemin={0} aria-valuemax={100} aria-valuenow={ctx.pct} aria-valuetext={ctx.text} data-testid="context-text" className="flex shrink-0 items-center gap-1.5 px-1 text-xs text-muted-foreground">
        <span aria-hidden className="relative h-1.5 w-20 rounded-full bg-muted">
          <span data-tone={tone} className={cn('absolute inset-y-0 left-0 rounded-full', tone === 'over' ? 'bg-destructive' : tone === 'warn' ? 'bg-state-needs' : 'bg-identity')} style={{ width: `${ctx.pct}%` }} />
          {marks.map((m) => <span key={m.setting} data-testid="context-mark" style={{ left: `${markPct(m.tokens, window)}%` }} className="absolute -top-0.5 -bottom-0.5 w-px bg-foreground/60" />)}
        </span>
        <span className="tabular-nums">{sizeText(tokens)} / {sizeText(window)}</span>
      </span>
    </Tip>
  )
}

/** The files the agent changed since its branch started, and what is uncommitted or on no remote: read from its worktree (`GetAgentChanges`). */
export function Changes({ host, agent }: { host: HostRuntime; agent: Agent }) {
  const got = usePolled(async () => getAgentChanges(host.client, agent.id), `${host.info.id}/${agent.id}/${agent.archived}`, 20_000, !!agent.worktree, String(agent.state))
  const c = got.value
  if (!agent.worktree) return null
  const shown = c?.files.slice(0, 8) ?? []
  return (
    <Section title="Files changed" count={c?.available ? String(c.files.length) + (c.truncated ? '+' : '') : undefined}>
      {got.error && !c && <p className="text-xs text-destructive">Couldn't read the changes: {got.error}</p>}
      {got.loading && !c && <SkeletonLines label="Reading the worktree" lines={2} />}
      {c && (
        <>
          <p className="text-xs text-muted-foreground" data-testid="change-summary">{changeSummary(c)}</p>
          {c.available && c.reason && <p className="text-xs text-muted-foreground">{c.reason}</p>}
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
  const readOnly = useHostState(host, (s) => s.daemon?.actor?.class === ActorClass.READ_ONLY_DEVICE)
  const [error, setError] = useState('')
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)
  if (!agent.worktree || readOnly) return null
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
        {local && !agent.archived && !agent.peer && (
          <Tip label="Open its worktree in the host's editor (the host's editor.command setting)">
            <Button size="sm" variant="outline" disabled={busy} onClick={() => void run(() => openInEditor(host.client, agent.id), 'Opened in the editor.')}>
              <FileCode2 /> Open in editor
            </Button>
          </Tip>
        )}
        {agent.archived ? (
          <Button size="sm" variant="outline" disabled={busy} onClick={() => void run(() => resumeAgent(host.client, agent.id), 'Restoring: the worktree is made again from its branch.')}>
            <ArchiveRestore /> Restore
          </Button>
        ) : (
          canArchive(agent) && <ArchiveButton host={host} agent={agent} disabled={busy} />
        )}
      </div>
      {note && <p role="status" className="text-xs text-muted-foreground">{note}</p>}
      {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
    </div>
  )
}

const LIVE: readonly AgentState[] = [AgentState.WORKING, AgentState.NEEDS_INPUT, AgentState.STARTING]

/** Archive with the warning the host's own check gives: uncommitted work blocks it, commits on no remote ask to be confirmed. */
function ArchiveButton({ host, agent, disabled }: { host: HostRuntime; agent: Agent; disabled: boolean }) {
  const [open, setOpen] = useState(false)
  return (
    <>
      <Button size="sm" variant="outline" disabled={disabled} onClick={() => setOpen(true)}>
        <Archive /> Archive
      </Button>
      <ArchiveDialog host={host} agent={agent} open={open} onOpenChange={setOpen} />
    </>
  )
}

/** Archive's question: what the worktree holds that would be lost, and a tick to go on when something would be. */
export function ArchiveDialog({ host, agent, open, onOpenChange: setOpen }: { host: HostRuntime; agent: Agent; open: boolean; onOpenChange: (open: boolean) => void }) {
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [sure, setSure] = useState(false)
  const read = usePolled(async () => getAgentChanges(host.client, agent.id), `archive/${host.info.id}/${agent.id}`, 5_000, open)
  // The ignored files are a walk of the worktree: asked once when the dialog opens, not on its 5 s poll (a very long interval keeps the hook's shape).
  const ignoredRead = usePolled(async () => getAgentChanges(host.client, agent.id, true), `archive-ignored/${host.info.id}/${agent.id}`, 3_600_000, open)
  const advice = archiveAdvice(read.value)
  const ignored = ignoredRead.value ? ignoredLines(ignoredRead.value) : { lines: [], more: 0 }
  /** Closing forgets what was ticked and said: the next opening starts as a new question. */
  function close(next: boolean) {
    setOpen(next)
    if (!next) {
      setSure(false)
      setError('')
    }
  }
  async function confirm() {
    if (busy) return
    setBusy(true)
    setError('')
    try {
      await archiveAgent(host.client, agent.id, !!advice.warn)
      close(false)
    } catch (e) {
      setError(say(e))
    } finally {
      setBusy(false)
    }
  }
  return (
      <Dialog open={open} onOpenChange={close}>
        <DialogContent title={`Archive ${agent.name}?`} description="Stops it and removes its worktree from the disk; its branch is kept, and a message to it (or Restore) makes the worktree again.">
          <div className="flex flex-col gap-3 text-sm">
            {LIVE.includes(agent.state) && <p className="text-xs text-state-needs">It is {agent.state === AgentState.WORKING ? 'working' : 'busy'} now: archiving stops it.</p>}
            {read.value && <p className="text-xs text-muted-foreground">{changeSummary(read.value)}</p>}
            {ignored.lines.length > 0 && (
              <div className="text-xs">
                <p className="text-muted-foreground">Ignored files that go with it, and that git has no copy of:</p>
                <ul aria-label="Ignored files that will be deleted" className="mt-1 max-h-32 overflow-y-auto font-mono">
                  {ignored.lines.map((e) => (
                    <li key={e.path} className="flex justify-between gap-3"><span className="truncate">{e.path}</span><span className="shrink-0 text-muted-foreground">{e.size}</span></li>
                  ))}
                  {ignored.more > 0 && <li className="text-muted-foreground">and {ignored.more} more</li>}
                </ul>
              </div>
            )}
            {read.error && !read.value && <p role="alert" className="text-xs text-destructive">Couldn't read the worktree: {read.error}</p>}
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
              <Button variant="ghost" onClick={() => close(false)}>Cancel</Button>
              <Button variant="destructive" disabled={busy || !read.value || !!advice.blocked || (!!advice.warn && !sure)} onClick={() => void confirm()}>Archive</Button>
            </div>
          </div>
        </DialogContent>
      </Dialog>
  )
}
