import { ChevronRight, PanelLeftClose, PanelLeftOpen, Plus, Search, Server } from 'lucide-react'
import { AnimatePresence, m } from 'motion/react'
import { useMemo, useState, type KeyboardEvent } from 'react'
import { renameRun, retireAgent, stopAgent } from '@/actions'
import { Button } from '@/components/ui/button'
import { Chip, Chips } from '@/components/ui/chip'
import { ConfirmDialog } from '@/components/ui/confirm'
import { Input } from '@/components/ui/input'
import { MoreButton, RowContextMenu, type MenuEntry } from '@/components/ui/menu'
import { Skeleton } from '@/components/ui/skeleton'
import { Tip } from '@/components/ui/tooltip'
import { useApp, useAppState, useHostState } from '@/context'
import { githubNote } from '@/buckets'
import { repoName, ROLE_LABEL, statusSentence } from '@/derive'
import { AgentState, type Agent, type Run } from '@/gen/agentmesh/v1/types_pb'
import { useSidebarHosts } from '@/hooks'
import { hueStyle } from '@/hue'
import { displayChord } from '@/keys'
import { cn } from '@/lib/utils'
import type { HostRuntime } from '@/model'
import { appKeys } from '@/prefs'
import { buildProjects, helpersLabel, anyWorking, type LeadRow, type ProjectRow, type WorkerRow } from '@/sidebar'
import { vendorOfKind } from '@/vendor'
import { ArchiveDialog } from './agent-work'
import { SidebarFooter } from './sidebar-footer'
import { StateIcon } from './state-icon'
import { VendorIcon } from './vendor-icon'
import { WaitingList } from './waiting-list'

const NO_HIDDEN: Record<string, string[]> = {}

/**
 * The one sidebar: the projects of every connected host in one list, each with its runs as their leads and the
 * workers under a lead (folded until opened). Reviewers and askers are a chip on the agent they work for. Right-click,
 * the hover ⋯ and Shift+F10 open a row's menu. The footer holds the quota, settings and what adds things.
 */
export function ProjectSidebar() {
  const app = useApp()
  const hosts = useAppState((s) => s.hosts)
  const pins = useAppState((s) => s.pins)
  const hidden = useAppState((s) => s.hidden) ?? NO_HIDDEN
  const collapsed = useAppState((s) => s.sidebarCollapsed)
  const side = useSidebarHosts(hosts)
  const projects = useMemo(() => buildProjects(side, pins), [side, pins])
  // A project taken off the sidebar comes back while something in it waits on the user.
  const shown = projects.filter((p) => p.needs > 0 || !(hidden[p.hostId] ?? []).includes(p.repo))
  const hiddenRows = projects.filter((p) => !shown.includes(p))
  const many = hosts.length > 1
  const keys = appKeys(hosts[0]?.store.getState().settings ?? {})
  const loading = side.some((h) => h.loading)
  if (collapsed) {
    return (
      <aside aria-label="Sidebar" className="flex w-14 shrink-0 flex-col items-center gap-2 border-r bg-surface py-3">
        <Tip side="right" label={`Show the sidebar (${displayChord(keys.sidebar)})`}>
          <Button size="icon" variant="ghost" aria-label="Show the sidebar" onClick={() => app.setSidebarCollapsed(false)}><PanelLeftOpen /></Button>
        </Tip>
        <nav aria-label="Projects" className="flex min-h-0 flex-1 flex-col items-center gap-1.5 overflow-y-auto">
          {shown.map((p) => (
            <Tip key={p.key} side="right" label={many ? `${p.name} on ${p.hostName}` : p.name}>
              <button type="button" aria-label={p.name} onClick={() => app.setSidebarCollapsed(false)} className={cn('relative rounded-md outline-none focus-visible:ring-2 focus-visible:ring-ring', !p.up && 'opacity-50')}>
                <ProjectTile project={p} />
                {p.needs > 0 && <span className="absolute -top-1 -right-1 grid min-w-4 place-items-center rounded-full bg-glow px-1 text-xs leading-4 font-bold text-primary-foreground tabular-nums">{p.needs}</span>}
              </button>
            </Tip>
          ))}
        </nav>
      </aside>
    )
  }
  return (
    <aside aria-label="Sidebar" className="flex w-72 shrink-0 flex-col border-r bg-surface">
      <div className="flex items-center gap-1.5 px-2 pt-2.5 pb-2">
        <button
          type="button"
          data-sidebar-search
          onClick={() => app.openPalette(true)}
          className="flex h-8 min-w-0 flex-1 items-center gap-2 rounded-md border bg-background px-2 text-[13px] text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
        >
          <Search className="size-4" />
          <span className="flex-1 truncate text-left">Search</span>
          {keys.search && <kbd className="font-sans text-xs">{displayChord(keys.search)}</kbd>}
        </button>
        <Tip label={`Fold the sidebar (${displayChord(keys.sidebar)})`}>
          <Button size="icon" variant="ghost" aria-label="Fold the sidebar" onClick={() => app.setSidebarCollapsed(true)}><PanelLeftClose /></Button>
        </Tip>
      </div>
      <nav aria-label="Projects" className="flex min-h-0 flex-1 flex-col">
        <div className="min-h-0 flex-1 overflow-y-auto pb-3">
          {hosts.map((h) => <HostWaiting key={h.info.id} host={h} many={many} />)}
          {hosts.map((h) => <GithubNote key={h.info.id} host={h} many={many} />)}
          <h2 className="px-3 pt-2.5 pb-1 text-xs font-semibold text-muted-foreground">Projects</h2>
          {loading && projects.length === 0 && <SidebarSkeleton />}
          {!loading && projects.length === 0 && (
            <div className="flex flex-col items-center gap-2 px-4 py-6 text-center text-xs text-muted-foreground">
              <p>No projects yet. Start a lead agent in one to give it work.</p>
              <Button size="sm" variant="secondary" onClick={() => app.openWorkspace(true)}><Plus /> New lead</Button>
            </div>
          )}
          <ul className="px-1.5">
            {shown.map((p) => <ProjectItem key={p.key} project={p} many={many} />)}
          </ul>
          {hiddenRows.length > 0 && <HiddenProjects rows={hiddenRows} />}
        </div>
      </nav>
      <SidebarFooter />
    </aside>
  )
}

const sidebarRows = () => [...document.querySelectorAll<HTMLElement>('nav[aria-label="Projects"] [data-sidebar-row]')]

/** Opens the agent row after (1) or before (-1) the open one in the sidebar, as it is shown (the `sidebar_next`/`sidebar_prev` keys). */
export function stepSidebar(by: 1 | -1): void {
  const rows = sidebarRows().filter((r) => r.dataset.sidebarRow === 'agent')
  if (rows.length === 0) return
  const at = rows.findIndex((r) => r.getAttribute('aria-current') === 'true')
  const next = rows[at < 0 ? (by > 0 ? 0 : rows.length - 1) : (at + by + rows.length) % rows.length]!
  next.click()
  next.focus()
}

/** Focuses the sidebar's search field: the `/` key, outside a text field. */
export function focusSearch(): boolean {
  const el = document.querySelector<HTMLElement>('[data-sidebar-search]')
  el?.click()
  return !!el
}

function SidebarSkeleton() {
  return (
    <div role="status" aria-label="Loading the projects" className="flex flex-col gap-2 px-3 py-2"><span className="sr-only">Loading the projects</span>
      {[0, 1, 2, 3].map((i) => (
        <div key={i} className="flex items-center gap-2 py-1">
          <Skeleton className="size-6 rounded-md" />
          <Skeleton className="w-2/3" />
        </div>
      ))}
    </div>
  )
}

function HostWaiting({ host, many }: { host: HostRuntime; many: boolean }) {
  return <WaitingList host={host} label={many ? host.info.name : undefined} />
}

function GithubNote({ host, many }: { host: HostRuntime; many: boolean }) {
  const problem = useHostState(host, (s) => s.githubProblem)
  const note = githubNote(problem)
  if (!note) return null
  return <p role="note" className="mx-3 mt-2 rounded-md border border-dashed px-2 py-1.5 text-xs text-muted-foreground">{many ? `${host.info.name}: ` : ''}{note}</p>
}

/** A project's mark: its initial on its hue. */
function ProjectTile({ project }: { project: ProjectRow }) {
  return (
    <span style={hueStyle(project.repo)} aria-hidden className="grid size-6 shrink-0 place-items-center rounded-md bg-identity/20 text-xs font-bold text-identity">
      {(project.name[0] ?? '?').toUpperCase()}
    </span>
  )
}

const hostOf = (app: ReturnType<typeof useApp>, hostId: string) => app.store.getState().hosts.find((h) => h.info.id === hostId)
const copy = (text: string) => void navigator.clipboard?.writeText(text)

/** Opens a row's menu from the keyboard (Shift+F10, the menu key): the row's ⋯ button opens it. */
function menuKey(e: KeyboardEvent<HTMLElement>) {
  if ((e.key === 'F10' && e.shiftKey) || e.key === 'ContextMenu') {
    e.preventDefault()
    e.stopPropagation()
    e.currentTarget.closest('[data-row]')?.querySelector<HTMLElement>('[data-row-more]')?.click()
  }
}

function ProjectItem({ project: p, many }: { project: ProjectRow; many: boolean }) {
  const app = useApp()
  const [open, setOpen] = useState(true)
  const entries: MenuEntry[] = [
    { label: 'New lead here', onSelect: () => app.openWorkspace(true, undefined, { hostId: p.hostId, repo: p.repo }), disabled: !p.repo || !p.up },
    { label: 'Project settings…', onSelect: () => app.openProjectSettings({ hostId: p.hostId, repo: p.repo }), disabled: !p.repo },
    'separator',
    { label: p.pinned ? 'Unpin' : 'Pin', onSelect: () => app.togglePin(p.hostId, p.repo), disabled: !p.repo },
    { label: 'Copy path', onSelect: () => copy(p.repo), disabled: !p.repo },
    { label: 'Remove from the sidebar', onSelect: () => app.toggleHidden(p.hostId, p.repo), disabled: !p.repo },
  ]
  const label = many ? `${p.name} on ${p.hostName}` : p.name
  const empty = p.runs.length === 0 && p.earlier.length === 0 && p.also.length === 0
  return (
    <li style={hueStyle(p.repo)} aria-label={label} className={cn('mb-0.5', !p.up && 'opacity-60')}>
      <RowContextMenu entries={entries}>
        <div data-row className="group flex items-center gap-1 rounded-md pr-1 hover:bg-accent">
          <button
            type="button"
            data-sidebar-row="project"
            aria-expanded={open}
            onClick={() => setOpen(!open)}
            onKeyDown={menuKey}
            title={p.repo || undefined}
            className="flex min-w-0 flex-1 items-center gap-2 rounded-md py-1.5 pl-1 text-left text-[13px] outline-none focus-visible:ring-2 focus-visible:ring-identity"
          >
            <ChevronRight className={cn('size-3.5 shrink-0 text-muted-foreground transition-transform motion-reduce:transition-none', open && 'rotate-90')} />
            <ProjectTile project={p} />
            <span className="min-w-0 flex-1 truncate font-semibold">{p.name}</span>
          </button>
          {many && !p.here && (
            <Tip label={p.up ? p.hostName : (p.down ?? p.hostName)}>
              <span tabIndex={0} aria-label={`On ${p.hostName}`} className="grid size-5 place-items-center text-muted-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"><Server className="size-3.5" /></span>
            </Tip>
          )}
          {!p.up && !(many && !p.here) && <Tip label={p.down ?? ''}><span tabIndex={0} className="text-xs text-muted-foreground">offline</span></Tip>}
          {p.needs > 0 && <span aria-label={`${p.needs} waiting on you`} className="grid min-w-5 place-items-center rounded-full bg-glow px-1.5 text-xs leading-5 font-bold text-primary-foreground tabular-nums">{p.needs}</span>}
          <span data-row-more className="contents"><MoreButton label={`Menu of ${label}`} entries={entries} /></span>
        </div>
      </RowContextMenu>
      {open && (
        <div className="ml-3 border-l border-identity/30 pl-1">
          {empty && (
            <div className="flex items-center gap-2 px-2 py-1.5 text-xs text-muted-foreground">
              <span className="flex-1">No runs here yet.</span>
              {p.repo && p.up && <Button size="sm" variant="ghost" className="h-6 px-1.5 text-xs" onClick={() => app.openWorkspace(true, undefined, { hostId: p.hostId, repo: p.repo })}><Plus className="size-3.5" /> New lead</Button>}
            </div>
          )}
          <ul>
            <AnimatePresence initial={false}>
              {p.runs.map((r) => <LeadItem key={r.run.id} row={r} />)}
            </AnimatePresence>
          </ul>
          {p.also.length > 0 && (
            <Folded title="Also working here" count={p.also.length}>
              {p.also.map((r) => <LeadItem key={r.run.id} row={r} />)}
            </Folded>
          )}
          {p.earlier.length > 0 && (
            <Folded title="Earlier" count={p.earlier.length}>
              {p.earlier.map((r) => <LeadItem key={r.run.id} row={r} />)}
            </Folded>
          )}
        </div>
      )}
    </li>
  )
}

function Folded({ title, count, children }: { title: string; count: number; children: React.ReactNode }) {
  const [open, setOpen] = useState(false)
  return (
    <div>
      <button type="button" aria-expanded={open} onClick={() => setOpen(!open)} className="flex w-full items-center gap-1 rounded-md px-2 py-1 text-left text-xs text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring">
        <ChevronRight className={cn('size-3 transition-transform motion-reduce:transition-none', open && 'rotate-90')} />
        {title} <span className="tabular-nums">{count}</span>
      </button>
      {open && <ul>{children}</ul>}
    </div>
  )
}

function HiddenProjects({ rows }: { rows: ProjectRow[] }) {
  const app = useApp()
  const [open, setOpen] = useState(false)
  return (
    <div className="px-1.5 pt-1">
      <button type="button" aria-expanded={open} onClick={() => setOpen(!open)} className="flex w-full items-center gap-1 rounded-md px-2 py-1 text-left text-xs text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring">
        <ChevronRight className={cn('size-3 transition-transform motion-reduce:transition-none', open && 'rotate-90')} />
        Removed from the sidebar <span className="tabular-nums">{rows.length}</span>
      </button>
      {open && (
        <ul>
          {rows.map((p) => (
            <li key={p.key} className="flex items-center gap-2 px-2 py-1 text-xs">
              <ProjectTile project={p} />
              <span className="min-w-0 flex-1 truncate">{p.name}</span>
              <Button size="sm" variant="ghost" className="h-6 px-1.5 text-xs" onClick={() => app.toggleHidden(p.hostId, p.repo)}>Show</Button>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

/** The dialogs a row's menu can open, one at a time. */
type RowDialog = { kind: 'stop' | 'retire' | 'archive'; agent: Agent } | undefined

function RowDialogs({ host, dialog, onClose }: { host?: HostRuntime; dialog: RowDialog; onClose: () => void }) {
  if (!host || !dialog) return null
  const a = dialog.agent
  if (dialog.kind === 'archive') return <ArchiveDialog host={host} agent={a} open onOpenChange={(o) => !o && onClose()} />
  return dialog.kind === 'stop' ? (
    <ConfirmDialog open onOpenChange={(o) => !o && onClose()} title={`Stop ${a.name}?`} description="Its process ends; its conversation is kept." action="Stop" run={() => stopAgent(host.client, a.id)} />
  ) : (
    <ConfirmDialog open onOpenChange={(o) => !o && onClose()} title={`Retire ${a.name}?`} description="Its process stops and its conversation is kept; a message brings it back." action="Retire" run={() => retireAgent(host.client, a.id)} />
  )
}

const live = (a: Agent) => a.state !== AgentState.EXITED && a.state !== AgentState.RETIRED

/** What every agent's menu offers (a lead's adds the run's entries). */
function agentEntries(app: ReturnType<typeof useApp>, hostId: string, a: Agent, open: (d: RowDialog) => void): MenuEntry[] {
  const h = hostOf(app, hostId)
  const terminals = !!h?.store.getState().daemon?.capabilities.includes('terminals') && !!a.terminalId
  return [
    { label: 'Open chat', onSelect: () => app.selectAgent(hostId, a.id, 'chat') },
    { label: 'Open its terminal', onSelect: () => app.selectAgent(hostId, a.id, 'terminal'), disabled: !terminals },
    { label: 'Model…', onSelect: () => { app.selectAgent(hostId, a.id); app.setPanel(true) } },
    { label: 'Send a message…', onSelect: () => app.selectAgent(hostId, a.id, 'overview') },
    'separator',
    { label: 'Copy worktree path', onSelect: () => copy(a.worktree), disabled: !a.worktree },
    'separator',
    { label: 'Stop', onSelect: () => open({ kind: 'stop', agent: a }), disabled: !live(a), destructive: true },
    { label: 'Retire', onSelect: () => open({ kind: 'retire', agent: a }), disabled: !live(a), destructive: true },
  ]
}

/** A run, as its lead: open it, or open the chevron to see its workers. */
function LeadItem({ row }: { row: LeadRow }) {
  const app = useApp()
  const host = useAppState((s) => s.hosts.find((h) => h.info.id === row.hostId))
  const selectedHere = useAppState((s) => s.hostId === row.hostId && !!s.agentId && s.agentId !== row.lead?.id && [...row.helpers, ...flatAgents(row.workers)].some((a) => a.id === s.agentId))
  const renaming = useAppState((s) => s.renaming?.hostId === row.hostId && s.renaming.runId === row.run.id)
  const [open, setOpen] = useState(false)
  const [dialog, setDialog] = useState<RowDialog>()
  // The open agent's lead stays open, so the open agent's row is in sight.
  const expanded = open || selectedHere
  const lead = row.lead
  const title = row.run.title || lead?.name || 'Untitled run'
  const entries: MenuEntry[] = [
    ...(lead ? [{ label: 'Open', onSelect: () => app.selectAgent(row.hostId, lead.id) }] : []),
    { label: 'Rename', onSelect: () => app.renameRun({ hostId: row.hostId, runId: row.run.id }), hint: 'F2' },
    { label: 'Add an agent', onSelect: () => app.openWorkspace(true, row.run.id, { hostId: row.hostId, repo: lead?.repo ?? '' }) },
    { label: 'Copy run id', onSelect: () => copy(row.run.id) },
    ...(lead
      ? (['separator', ...agentEntries(app, row.hostId, lead, setDialog).slice(1), { label: 'Archive…', onSelect: () => setDialog({ kind: 'archive', agent: lead }), destructive: true }] as MenuEntry[])
      : []),
  ]
  const kids = row.workers.length > 0
  return (
    <m.li initial={{ opacity: 0 }} animate={{ opacity: 1 }} exit={{ opacity: 0 }} transition={{ duration: 0.14 }} aria-label={title}>
      <RowContextMenu entries={entries}>
        <div data-row className="group flex items-start gap-0.5 pr-1">
          {kids ? (
            <button type="button" aria-label={expanded ? `Fold ${title}` : `Show the workers of ${title}`} aria-expanded={expanded} onClick={() => setOpen(!expanded)} className="mt-1.5 grid size-5 shrink-0 place-items-center rounded text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring">
              <ChevronRight className={cn('size-3.5 transition-transform motion-reduce:transition-none', expanded && 'rotate-90')} />
            </button>
          ) : (
            <span className="size-5 shrink-0" />
          )}
          {renaming ? (
            <RunTitleEditor host={host} run={row.run} onDone={() => app.renameRun(undefined)} />
          ) : lead ? (
            <AgentButton hostId={row.hostId} agent={lead} title={title} helpers={row.helpers} onKeyDown={(e) => { menuKey(e); if (e.key === 'F2') { e.preventDefault(); app.renameRun({ hostId: row.hostId, runId: row.run.id }) } }} />
          ) : (
            <span className="min-w-0 flex-1 truncate px-2 py-1.5 text-[13px] text-muted-foreground">{title}: no agents yet</span>
          )}
          <span data-row-more className="mt-1.5 flex"><MoreButton label={`Menu of ${title}`} entries={entries} /></span>
        </div>
      </RowContextMenu>
      {kids && expanded && (
        <ul className="ml-5 border-l border-identity/50 pl-1">
          {row.workers.map((w) => <WorkerItem key={w.agent.id} hostId={row.hostId} row={w} />)}
        </ul>
      )}
      <RowDialogs host={host} dialog={dialog} onClose={() => setDialog(undefined)} />
    </m.li>
  )
}

const flatAgents = (rows: WorkerRow[]): Agent[] => rows.flatMap((w) => [w.agent, ...w.helpers, ...flatAgents(w.workers)])

function WorkerItem({ hostId, row }: { hostId: string; row: WorkerRow }) {
  const app = useApp()
  const host = useAppState((s) => s.hosts.find((h) => h.info.id === hostId))
  const [dialog, setDialog] = useState<RowDialog>()
  const a = row.agent
  const entries: MenuEntry[] = [...agentEntries(app, hostId, a, setDialog), { label: 'Archive…', onSelect: () => setDialog({ kind: 'archive', agent: a }), destructive: true }]
  return (
    <li style={row.elsewhere ? hueStyle(a.repo) : undefined}>
      <RowContextMenu entries={entries}>
        <div data-row className="group flex items-start gap-0.5 pr-1">
          <AgentButton hostId={hostId} agent={a} helpers={row.helpers} tag={row.elsewhere ? repoName(a.repo) : undefined} onKeyDown={menuKey} />
          <span data-row-more className="mt-1.5 flex"><MoreButton label={`Menu of ${a.name}`} entries={entries} /></span>
        </div>
      </RowContextMenu>
      {row.workers.length > 0 && (
        <ul className="ml-4 border-l border-identity/50 pl-1">
          {row.workers.map((w) => <WorkerItem key={w.agent.id} hostId={hostId} row={w} />)}
        </ul>
      )}
      <RowDialogs host={host} dialog={dialog} onClose={() => setDialog(undefined)} />
    </li>
  )
}

/** An agent's row: its state, its name (a lead's is its run's title), what it is doing, and chips for its helpers, repo and model. */
function AgentButton({ hostId, agent: a, title, helpers, tag, onKeyDown }: { hostId: string; agent: Agent; title?: string; helpers: Agent[]; tag?: string; onKeyDown?: (e: KeyboardEvent<HTMLButtonElement>) => void }) {
  const app = useApp()
  const selected = useAppState((s) => s.hostId === hostId && s.agentId === a.id)
  const help = helpersLabel(helpers)
  return (
    <button
      type="button"
      data-sidebar-row="agent"
      aria-current={selected ? 'true' : undefined}
      aria-label={title ? `${title}, ${ROLE_LABEL[a.role]} ${a.name}` : a.name}
      onClick={() => app.selectAgent(hostId, a.id)}
      onKeyDown={onKeyDown}
      className={cn(
        'flex min-w-0 flex-1 items-start gap-2 rounded-md px-1.5 py-1.5 text-left text-[13px] outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-identity',
        selected && 'bg-accent shadow-[inset_2px_0_0_var(--hue,var(--primary))]',
        a.needsInput > 0 && 'bg-glow/10 ring-1 ring-glow/40',
      )}
    >
      <span className="mt-0.5"><StateIcon agent={a} /></span>
      <span className="min-w-0 flex-1">
        <span className="block truncate font-medium">{title ?? a.name}</span>
        <span className="block truncate text-xs text-muted-foreground">{statusSentence(a)}</span>
        <Chips className="mt-0.5">
          <VendorIcon kind={a.kind} model={a.model} className="size-3" />
          {tag && <Chip className="border-identity/50 text-identity">{tag}</Chip>}
          {help && <Chip className={cn(anyWorking(helpers) && 'border-state-working/60')}>{help}</Chip>}
          {[vendorOfKind(a.kind) === 'other' || !a.model ? a.kind : '', a.model].filter(Boolean).map((x, i) => <Chip key={i}>{x}</Chip>)}
          {a.archived && <Chip>archived</Chip>}
        </Chips>
      </span>
    </button>
  )
}

/** A run's title, being edited: Enter saves, Escape keeps it, leaving the field saves. */
function RunTitleEditor({ host, run, onDone }: { host?: HostRuntime; run: Run; onDone: () => void }) {
  const [text, setText] = useState(run.title)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  async function save() {
    const next = text.trim()
    if (busy) return // Enter and the blur that follows it are one save
    if (!host || !next || next === run.title) return onDone()
    setBusy(true)
    try {
      await renameRun(host.client, run.id, next)
      onDone()
    } catch (e) {
      setError(e instanceof Error ? e.message.replace(/^\[[a-z_]+\]\s*/, '') : String(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <form className="flex min-w-0 flex-1 flex-col gap-1 py-1" onSubmit={(e) => { e.preventDefault(); void save() }}>
      <Input aria-label="Run title" autoFocus value={text} onChange={(e) => setText(e.target.value)} onKeyDown={(e) => { if (e.key === 'Escape') { e.stopPropagation(); onDone() } }} onBlur={() => void save()} className="h-7 text-[13px]" />
      {error && <span role="alert" className="text-xs text-destructive">{error}</span>}
    </form>
  )
}
