import { Pencil, Plus, UserPlus } from 'lucide-react'
import { useMemo, useState } from 'react'
import { Badge } from '@/components/ui/badge'
import { Chip, Chips } from '@/components/ui/chip'
import { renameRun } from '@/actions'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'
import { useApp, useAppState, useHostState } from '@/context'
import { BUCKET_LABEL, bucketOf, githubNote, groupByProject, groupByState, waitingIds, type Bucket, type BucketContext, type Group } from '@/buckets'
import { buildTrees, joinWaiting, repoName, ROLE_LABEL, statusSentence, type TreeNode } from '@/derive'
import { RunStatus, type Agent, type Run } from '@/gen/agentmesh/v1/types_pb'
import { cn } from '@/lib/utils'
import { hueStyle } from '@/hue'
import { vendorOfKind } from '@/vendor'
import type { HostRuntime, SidebarGroup } from '@/model'
import { StateIcon } from './state-icon'
import { VendorIcon } from './vendor-icon'
import { WaitingList } from './waiting-list'
import { NewRunDialog, SpawnDialog } from './spawn-dialogs'
import { WorkspaceButton } from './workspace-dialog'

/** The host's runs, each as a tree from its lead: workers under it, their read-only agents under them. */
export function RunSidebar({ host }: { host: HostRuntime }) {
  const runs = useHostState(host, (s) => s.runs)
  const agents = useHostState(host, (s) => s.agents)
  const waiting = useHostState(host, (s) => s.waiting)
  const pullRequests = useHostState(host, (s) => s.pullRequests)
  const group = useAppState((s) => s.sidebarGroup)
  const githubProblem = useHostState(host, (s) => s.githubProblem)
  // Before the first snapshot of a connection that is coming up; a host that is down shows its banner and the empty text.
  const loading = useHostState(host, (s) => !s.synced && (s.conn.kind === 'connecting' || s.conn.kind === 'ready'))
  const trees = useMemo(() => buildTrees(runs, agents), [runs, agents])
  const ctx = useMemo<BucketContext>(() => ({ waiting: waitingIds(joinWaiting(waiting, runs, agents)), pullRequests }), [waiting, runs, agents, pullRequests])
  const groups = useMemo(() => (group === 'project' ? groupByProject(runs, agents, ctx, repoName) : group === 'state' ? groupByState(runs, agents, ctx) : []), [group, runs, agents, ctx])
  return (
    <nav aria-label="Runs" className="flex min-h-0 flex-1 flex-col">
      <WaitingList host={host} />
      <div className="flex items-center justify-between px-3 py-2">
        <h2 className="text-[13px] font-semibold">Runs</h2>
        <div className="flex gap-1.5">
          <WorkspaceButton host={host} />
          <NewRunDialog host={host} />
        </div>
      </div>
      <GroupSwitch />
      {githubNote(githubProblem) && <p role="note" className="mx-3 mb-2 rounded-md border border-dashed px-2 py-1.5 text-xs text-muted-foreground">{githubNote(githubProblem)}</p>}
      <div className="min-h-0 flex-1 overflow-y-auto px-2 pb-3">
        {loading && trees.length === 0 && (
          <div role="status" aria-label="Loading the runs" className="flex flex-col gap-2 px-2 py-2">
            {[0, 1, 2, 3].map((i) => (
              <div key={i} className="flex items-start gap-2 py-1">
                <Skeleton className="size-4 rounded-full" />
                <div className="flex flex-1 flex-col gap-1.5"><Skeleton className="w-2/3" /><Skeleton className="h-2.5 w-1/2" /></div>
              </div>
            ))}
          </div>
        )}
        {!loading && trees.length === 0 && (
          <p className="px-2 py-6 text-center text-xs text-muted-foreground">No runs yet. Start one to give a lead agent its work.</p>
        )}
        {group !== 'run' && groups.map((g) => <GroupSection key={g.key} g={g} host={host} ctx={ctx} by={group} runs={runs} />)}
        {group !== 'run' && trees.length > 0 && groups.length === 0 && <p className="px-2 py-6 text-center text-xs text-muted-foreground">No agents in the open runs.</p>}
        {group === 'run' && trees.map((t) => (
          <section key={t.run.id} className="mb-3" aria-label={t.run.title || t.run.id}>
            <header className="flex items-center gap-2 px-2 py-1">
              <RunTitle host={host} run={t.run} />
              {t.run.status === RunStatus.CLOSED && <Badge>closed</Badge>}
              {t.needsInput > 0 && <Badge className="border-state-needs text-state-needs">{t.needsInput} need you</Badge>}
              <SpawnDialog host={host} runId={t.run.id} trigger={<Button size="icon" variant="ghost" className="size-6" aria-label={`Add an agent to ${t.run.title || t.run.id}`}><UserPlus className="size-3.5" /></Button>} />
            </header>
            <p className="px-2 pb-1 font-mono text-xs text-muted-foreground">{t.run.id}</p>
            {t.roots.length === 0 && <p className="px-2 py-1 text-xs text-muted-foreground">No agents. <Plus className="inline size-3" /> adds one.</p>}
            <ul>
              {t.roots.map((n) => (
                <TreeRow key={n.agent.id} node={n} host={host} depth={0} />
              ))}
            </ul>
          </section>
        ))}
      </div>
    </nav>
  )
}

/** A run's title; the pencil (or a double click) makes it editable, Enter saves, Escape keeps it. */
export function RunTitle({ host, run }: { host: HostRuntime; run: Run }) {
  const [editing, setEditing] = useState(false)
  const [text, setText] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const title = run.title || 'Untitled run'
  async function save() {
    const next = text.trim()
    if (busy) return // Enter and the blur that follows it are one save
    if (!next || next === run.title) return setEditing(false)
    setBusy(true)
    try {
      await renameRun(host.client, run.id, next)
      setEditing(false)
      setError('')
    } catch (e) {
      setError(e instanceof Error ? e.message.replace(/^\[[a-z_]+\]\s*/, '') : String(e))
    } finally {
      setBusy(false)
    }
  }
  const open = () => {
    setText(run.title)
    setError('')
    setEditing(true)
  }
  if (!editing) {
    return (
      <>
        <h3 className="min-w-0 flex-1 truncate text-[13px] font-semibold" title="Double click to rename" onDoubleClick={open}>{title}</h3>
        <Button size="icon" variant="ghost" className="size-6" aria-label={`Rename ${title}`} onClick={open}><Pencil className="size-3.5" /></Button>
      </>
    )
  }
  return (
    <form className="flex min-w-0 flex-1 flex-col gap-1" onSubmit={(e) => { e.preventDefault(); void save() }}>
      <Input aria-label="Run title" autoFocus value={text} onChange={(e) => setText(e.target.value)} onKeyDown={(e) => { if (e.key === 'Escape') setEditing(false) }} onBlur={() => void save()} className="h-7 text-[13px]" />
      {error && <span role="alert" className="text-xs text-destructive">{error}</span>}
    </form>
  )
}

const GROUPS: { id: SidebarGroup; label: string }[] = [
  { id: 'run', label: 'Runs' },
  { id: 'project', label: 'Projects' },
  { id: 'state', label: 'Status' },
]

/** Runs as trees, the agents by repo, or the agents by status bucket. */
function GroupSwitch() {
  const app = useApp()
  const group = useAppState((s) => s.sidebarGroup)
  return (
    <div role="group" aria-label="Group the agents by" className="mx-3 mb-2 flex rounded-md border p-0.5">
      {GROUPS.map((g) => (
        <button
          key={g.id}
          type="button"
          aria-pressed={group === g.id}
          onClick={() => app.setSidebarGroup(g.id)}
          className={cn('h-6 flex-1 rounded-sm text-xs font-medium text-muted-foreground hover:text-foreground', group === g.id && 'bg-accent text-foreground')}
        >
          {g.label}
        </button>
      ))}
    </div>
  )
}

const BUCKET_TONE: Record<Bucket, string> = {
  needs: 'border-state-needs text-state-needs',
  failed: 'border-destructive text-destructive',
  review: 'border-primary text-primary',
  working: '',
  idle: '',
  done: '',
}

/** One project or one bucket: its agents, flat, each with the run it belongs to (and, by project, its bucket). */
function GroupSection({ g, host, ctx, by, runs }: { g: Group; host: HostRuntime; ctx: BucketContext; by: SidebarGroup; runs: Record<string, { title: string; id: string }> }) {
  // Done is history: folded away until asked for, whichever way the agents are grouped.
  const [showDone, setShowDone] = useState(false)
  const foldable = by === 'state' && g.key === 'done'
  const shown = foldable && !showDone ? [] : g.agents
  const badges = (['needs', 'failed', 'review'] as const).filter((b) => g.counts[b] > 0)
  return (
    <section className="mb-3" aria-label={g.label}>
      <header className="flex items-center gap-2 px-2 py-1">
        <h3 className="min-w-0 flex-1 truncate text-[13px] font-semibold" title={g.title}>{g.label}</h3>
        {by === 'project' && badges.map((b) => <Badge key={b} className={BUCKET_TONE[b]}>{g.counts[b]} {BUCKET_LABEL[b].toLowerCase()}</Badge>)}
        {foldable ? (
          <button type="button" aria-expanded={showDone} onClick={() => setShowDone(!showDone)} className="text-xs text-muted-foreground hover:text-foreground">
            {showDone ? 'Hide' : 'Show'} {g.agents.length}
          </button>
        ) : (
          <span className="text-xs text-muted-foreground">{g.agents.length}</span>
        )}
      </header>
      <ul>
        {shown.map((a) => {
          const run = runs[a.runId]
          const bucket = by === 'project' ? BUCKET_LABEL[bucketOf(a, ctx)] : repoName(a.repo)
          return <AgentRow key={a.id} agent={a} host={host} meta={[bucket, run ? run.title || run.id : '']} />
        })}
      </ul>
    </section>
  )
}

/** A lead and the agents under it; the thread down to its workers is in the project's hue. */
function TreeRow({ node, host, depth }: { node: TreeNode; host: HostRuntime; depth: number }) {
  return (
    <li style={depth === 0 ? hueStyle(node.agent.repo) : undefined}>
      <AgentButton agent={node.agent} host={host} />
      {node.children.length > 0 && (
        <ul className={cn('ml-4 border-l border-identity/50 pl-1', depth > 2 && 'ml-2')}>
          {node.children.map((c) => (
            <TreeRow key={c.agent.id} node={c} host={host} depth={depth + 1} />
          ))}
        </ul>
      )}
    </li>
  )
}

function AgentRow({ agent, host, meta }: { agent: Agent; host: HostRuntime; meta: string[] }) {
  return (
    <li style={hueStyle(agent.repo)}>
      <AgentButton agent={agent} host={host} meta={meta} />
    </li>
  )
}

/** An agent's row: its state icon, name and role, a plain sentence for what it is doing, and where it runs. */
function AgentButton({ agent: a, host, meta }: { agent: Agent; host: HostRuntime; meta?: string[] }) {
  const app = useApp()
  const selected = useAppState((s) => s.hostId === host.info.id && s.agentId === a.id)
  return (
      <button
        type="button"
        aria-current={selected ? 'true' : undefined}
        onClick={() => app.selectAgent(host.info.id, a.id)}
        className={cn(
          'flex w-full items-start gap-2 rounded-md px-2 py-1.5 text-left text-[13px] outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-identity',
          selected && 'bg-accent shadow-[inset_2px_0_0_var(--hue,var(--primary))]',
          a.needsInput > 0 && 'bg-glow/10 ring-1 ring-glow/40',
        )}
      >
        <span className="mt-0.5"><StateIcon agent={a} /></span>
        <span className="min-w-0 flex-1">
          <span className="flex items-center gap-1.5">
            <span className="truncate font-medium">{a.name}</span>
            <Badge>{ROLE_LABEL[a.role]}</Badge>
          </span>
          <span className="block truncate text-xs text-muted-foreground">{statusSentence(a)}</span>
          <Chips className="mt-0.5">
            <VendorIcon kind={a.kind} model={a.model} className="size-3" />
            {/* The vendor's mark names a known CLI; an unknown one, or one with no model, is named in a chip. */}
            {(meta ?? [repoName(a.repo), vendorOfKind(a.kind) === 'other' || !a.model ? a.kind : '', a.model, a.effort]).filter(Boolean).map((m, i) => <Chip key={i}>{m}</Chip>)}
          </Chips>
        </span>
      </button>
  )
}
