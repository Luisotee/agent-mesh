import { AnimatePresence, m, useReducedMotion } from 'motion/react'
import { useEffect, useRef, useState } from 'react'
import { Columns2, MoreHorizontal, PanelRight, Plus, Rows2, SquareTerminal, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { displayChord } from '@/keys'
import { useApp, useAppState, useHostState } from '@/context'
import { cn } from '@/lib/utils'
import { hueStyle } from '@/hue'
import { MAX_SIDE_PANES, projectTabs, tabKey, type TabRef } from '@/workspace'
import { agentProject } from '@/sidebar'
import { repoName } from '@/derive'
import { AgentRole, AgentState, type Agent } from '@/gen/agentmesh/v1/types_pb'
import type { HostRuntime } from '@/model'
import type { Shell } from '@/gen/agentmesh/v1/mesh_pb'
import { useHostViews, useKeys } from '@/hooks'
import { StateIcon } from './state-icon'
import { VendorIcon } from './vendor-icon'
import { Tip } from '@/components/ui/tooltip'
import { ActionMenu, RowContextMenu, type MenuEntry } from '@/components/ui/menu'
import { ConfirmDialog } from '@/components/ui/confirm'
import { stopAgent } from '@/actions'
import { cliKinds, cliName, hostClis } from '@/cli'
import { rewindUnsupported } from '@/attachments'
import { ActorClass } from '@/gen/agentmesh/v1/actor_pb'
import { ViewSwitch } from './view-switch'
import { closedShells, isShellTab, shellIdOf, shellProject, shellTabId } from '@/shells'

/** The repo the run of `a` is led from (its root agent's), else its own. */
function leadRepo(host: HostRuntime | undefined, a: Agent): string {
  const agents = host?.store.getState().agents ?? {}
  return Object.values(agents).find((x) => x.runId === a.runId && x.role === AgentRole.ROOT)?.repo || a.repo
}

const live = (a: Agent) => a.state !== AgentState.EXITED && a.state !== AgentState.RETIRED
const reason = (e: unknown) => (e instanceof Error ? e.message.replace(/^\[[a-z_]+\]\s*/, '') : String(e))

/**
 * The open project's agents as tabs (one tab bar per project: opening an agent of another project shows that
 * project's tabs, each project keeping its own). Every tab has the same width (it only narrows when many are open),
 * so nothing moves under the pointer. A click shows one, the × or a middle click closes it, right-click (or Shift+F10)
 * opens its menu, the wheel scrolls the strip, a drag onto another tab moves it there (onto an edge of the main pane, a
 * split). The + follows the last tab, as in a browser: a plain agent, a lead or a terminal in the project. At the bar's
 * end are the open agent's view switch (Chat | CLI), its menu, the split direction and the side panel's toggle.
 */
export function TabBar() {
  const app = useApp()
  const all = useAppState((s) => s.tabs)
  const hosts = useAppState((s) => s.hosts)
  const hostId = useAppState((s) => s.hostId)
  const agentId = useAppState((s) => s.agentId)
  const side = useAppState((s) => s.side)
  const dir = useAppState((s) => s.splitDir)
  const panelOpen = useAppState((s) => s.panelOpen)
  const dragging = useAppState((s) => s.dragTab)
  const closed = useAppState((s) => s.closedTabs.length)
  const views = useHostViews(hosts)
  const keys = useKeys(hosts)
  // Width and opacity are not covered by `MotionConfig`'s reduced motion (only transforms are): they stop here.
  const still = !!useReducedMotion()
  const many = hosts.length > 1
  // This project's tabs: those of the open agent's project (with none open, all of them).
  const open = hostId && agentId ? { hostId, agentId } : undefined
  const allShells = useAppState((s) => s.shells)
  const shellOf = (t: TabRef) => (isShellTab(t.agentId) ? allShells[t.hostId]?.find((x) => x.id === shellIdOf(t.agentId)) : undefined)
  const projectOf = (t: TabRef) => {
    if (!isShellTab(t.agentId)) return agentProject(t.hostId, views.find((v) => v.hostId === t.hostId)?.agents, t.agentId)
    const sh = shellOf(t)
    return sh ? shellProject(t.hostId, sh) : undefined
  }
  const tabs = projectTabs(all, projectOf, open)
  const openAgent = open && views.find((v) => v.hostId === open.hostId)?.agents[open.agentId]
  const runs = useAppState((s) => s.hosts.find((h) => h.info.id === hostId))
  // A new lead goes in the open project (its host and the repo its run is led from).
  const openShellRow = open && shellOf(open)
  const project = open && openAgent ? { hostId: open.hostId, repo: leadRepo(runs, openAgent) } : open && openShellRow ? { hostId: open.hostId, repo: openShellRow.repo } : undefined
  // The agent to stop with the host of its tab, which is not always the open one's.
  const [stopping, setStopping] = useState<{ hostId: string; agent: Agent }>()
  const newLead = () => app.store.getState().hosts.length > 0 && app.openWorkspace(true, undefined, project)
  // A tab whose agent finished a turn while another tab was shown carries a quiet dot until it is opened.
  const [finished, setFinished] = useState<ReadonlySet<string>>(new Set())
  // Whether a tab's agent has been at work since it was last idle: a quiet long turn reads as unknown, which is still work.
  const worked = useRef(new Set<string>())
  useEffect(() => {
    const keys = new Set(all.map(tabKey))
    // A tab that was closed starts over when it is opened again.
    for (const k of [...worked.current]) if (!keys.has(k)) worked.current.delete(k)
    const next = new Set([...finished].filter((k) => keys.has(k)))
    for (const t of all) {
      const a = isShellTab(t.agentId) ? undefined : views.find((v) => v.hostId === t.hostId)?.agents[t.agentId]
      if (!a) continue
      const key = tabKey(t)
      // On screen: the main pane or a split beside it.
      const shown = (t.hostId === hostId && t.agentId === agentId) || side.some((p) => p.hostId === t.hostId && p.agentId === t.agentId)
      if (a.state === AgentState.WORKING || a.state === AgentState.UNKNOWN) worked.current.add(key)
      else if (a.state === AgentState.IDLE && worked.current.delete(key) && !shown) next.add(key)
      if (shown) next.delete(key)
    }
    if (next.size !== finished.size || [...next].some((k) => !finished.has(k))) setFinished(next)
  }, [all, views, hostId, agentId, side, finished])

  /** A tab's menu: the same list for the right-click and the ⋯ of the open one. */
  const entriesOf = (t: TabRef, agent: Agent | undefined, label: string): MenuEntry[] => {
    const at = tabs.findIndex((x) => tabKey(x) === tabKey(t))
    // A shell asks before it closes (end it or keep it), so the bulk closes leave shells to their own ×.
    const closeAll = (ts: TabRef[]) => ts.filter((x) => !isShellTab(x.agentId)).forEach((x) => app.closeTab(x))
    const active = t.hostId === hostId && t.agentId === agentId
    const beside = side.some((p) => p.hostId === t.hostId && p.agentId === t.agentId)
    const host = hosts.find((h) => h.info.id === t.hostId)?.store.getState()
    const readOnly = host?.daemon?.actor?.class === ActorClass.READ_ONLY_DEVICE
    return [
      ...(agent ? ([{ label: 'Send a message…', onSelect: () => app.openMessage(t) }] as MenuEntry[]) : []),
      { label: 'Show beside', onSelect: () => app.pinSplit(t.hostId, t.agentId), disabled: active || beside || side.length >= MAX_SIDE_PANES },
      'separator',
      { label: 'Close', onSelect: () => app.closeTab(t) },
      { label: 'Close others', onSelect: () => closeAll(tabs.filter((x) => tabKey(x) !== tabKey(t))), disabled: tabs.length < 2 },
      { label: 'Close to the right', onSelect: () => closeAll(tabs.slice(at + 1)), disabled: at < 0 || at === tabs.length - 1 },
      { label: 'Reopen closed tab', onSelect: () => app.reopenTab(), disabled: closed === 0 },
      { label: 'Copy name', onSelect: () => void navigator.clipboard?.writeText(label) },
      ...(agent
        ? ([
            {
              label: 'Rewind…',
              // Back to before one of its prompts: Claude Code only, the conversation only.
              onSelect: () => {
                app.selectAgent(t.hostId, t.agentId, 'chat')
                app.openRewind(t)
              },
              disabled: !!rewindUnsupported(agent.kind) || !host?.synced || readOnly,
              hint: 'Esc Esc',
            },
          ] as MenuEntry[])
        : []),
      ...(agent ? (['separator', { label: 'Shut down…', onSelect: () => setStopping({ hostId: t.hostId, agent }), disabled: !live(agent), destructive: true }] as MenuEntry[]) : []),
    ]
  }

  return (
    <div className="flex min-h-9 shrink-0 items-end border-b bg-surface px-1 pt-1">
      <div
        role="tablist"
        aria-label="Open agents"
        className="flex min-w-0 gap-px self-stretch overflow-x-auto [scrollbar-width:none]"
        onWheel={(e) => {
          if (e.deltaY && !e.deltaX) e.currentTarget.scrollLeft += e.deltaY
        }}
      >
        <AnimatePresence initial={false}>
        {tabs.map((t) => {
          const view = views.find((v) => v.hostId === t.hostId)
          const shellTab = isShellTab(t.agentId)
          const shells = allShells[t.hostId]
          const shell = shellTab ? shells?.find((x) => x.id === shellIdOf(t.agentId)) : undefined
          const agent = shellTab ? undefined : view?.agents[t.agentId]
          const active = t.hostId === hostId && t.agentId === agentId
          const beside = side.some((p) => p.hostId === t.hostId && p.agentId === t.agentId)
          // A shell's tab: "Terminal", numbered when the project has several.
          const nth = shell && shells ? shells.filter((x) => x.repo === shell.repo).findIndex((x) => x.id === shell.id) + 1 : 0
          const label = shellTab ? (nth > 1 ? `Terminal ${nth}` : 'Terminal') : (agent?.name ?? t.agentId)
          return (
            <m.div
              key={tabKey(t)}
              initial={{ opacity: 0, y: 6 }}
              animate={{ opacity: 1, y: 0 }}
              exit={{ opacity: 0, width: 0, minWidth: 0, overflow: 'hidden', pointerEvents: 'none' }}
              transition={{ duration: still ? 0 : 0.14 }}
              className="flex w-48 min-w-28 shrink self-end"
            >
            <RowContextMenu entries={entriesOf(t, agent, label)} className="flex w-full min-w-0 self-end">
              <div
                draggable
                // Dropped on an edge of the main pane (`DropEdges`), the tab opens as a split there.
                onDragStart={(e) => {
                  e.dataTransfer.setData('text/plain', tabKey(t))
                  e.dataTransfer.effectAllowed = 'move'
                  app.setDragTab(t)
                }}
                onDragEnd={() => app.setDragTab(undefined)}
                onDragOver={(e) => {
                  if (dragging && tabKey(dragging) !== tabKey(t)) e.preventDefault()
                }}
                onDrop={(e) => {
                  if (!dragging || tabKey(dragging) === tabKey(t)) return
                  e.preventDefault()
                  app.moveTab(dragging, t)
                }}
                // A middle click closes the tab, as in a browser.
                onAuxClick={(e) => {
                  if (e.button === 1) {
                    e.preventDefault()
                    app.closeTab(t)
                  }
                }}
                onMouseDown={(e) => {
                  if (e.button === 1) e.preventDefault()
                }}
                style={agent ? hueStyle(agent.repo) : shell ? hueStyle(shell.repo) : undefined}
                // The open tab is underlined in its project's hue.
                className={cn(
                  'group relative flex w-full min-w-0 items-center rounded-t-md border border-b-0 text-[13px]',
                  active ? 'bg-linear-to-b from-identity/10 to-background text-foreground after:absolute after:inset-x-0 after:bottom-0 after:h-0.5 after:bg-identity' : 'border-transparent text-muted-foreground hover:bg-accent',
                )}
              >
                <button
                  type="button"
                  role="tab"
                  aria-selected={active}
                  data-beside={beside ? 'true' : undefined}
                  title={many && view ? `${label} on ${view.hostName}` : label}
                  onClick={() => app.selectAgent(t.hostId, t.agentId)}
                  className="flex min-w-0 flex-1 items-center gap-1.5 py-1.5 pr-1 pl-2.5 outline-none focus-visible:ring-2 focus-visible:ring-identity"
                >
                  {agent ? <StateIcon agent={agent} className="size-3.5 shrink-0" /> : shellTab ? <SquareTerminal className="size-3.5 shrink-0" /> : null}
                  {agent && <VendorIcon decorative kind={agent.kind} model={agent.model} className="size-3 shrink-0 opacity-80" />}
                  <span className="truncate">{label}</span>
                  {finished.has(tabKey(t)) && <span role="img" aria-label="Finished a turn while you were away" title="Finished a turn while you were away" className="size-1.5 shrink-0 rounded-full bg-primary" />}
                  {many && view && <span className="shrink-0 text-xs text-muted-foreground">{view.hostName}</span>}
                </button>
                <button
                  type="button"
                  aria-label={`Close ${label}`}
                  onClick={() => app.closeTab(t)}
                  className="mr-1 grid size-5 shrink-0 place-items-center rounded hover:bg-muted"
                >
                  <X className="size-3" />
                </button>
              </div>
            </RowContextMenu>
            </m.div>
          )
        })}
        </AnimatePresence>
      </div>
      <PlusMenu project={project} host={runs} newLead={newLead} />
      {/* The empty space: with no tab it says so. (A new lead starts from the +, as one thing does one way.) */}
      <div className="flex min-w-4 flex-1 items-center self-stretch px-2">
        {tabs.length === 0 && <span className="text-xs text-muted-foreground">No agent open</span>}
      </div>
      {open && runs && openAgent && <OpenAgentControls host={runs} agent={openAgent} entries={entriesOf(open, openAgent, openAgent.name)} chord={keys.chat_toggle} />}
      {side.length > 0 && (
        <Tip label={dir === 'row' ? 'Stack the panes' : 'Put the panes side by side'}>
          <button
            type="button"
            aria-label={dir === 'row' ? 'Stack the panes' : 'Put the panes side by side'}
            onClick={() => app.setSplitDir(dir === 'row' ? 'column' : 'row')}
            className="mb-1 ml-1 grid size-6 shrink-0 place-items-center rounded hover:bg-muted"
          >
            {dir === 'row' ? <Rows2 className="size-3.5" /> : <Columns2 className="size-3.5" />}
          </button>
        </Tip>
      )}
      <Tip label={`${panelOpen ? 'Hide' : 'Show'} the side panel (${displayChord(keys.panel)})`}>
        <Button size="icon" variant={panelOpen ? 'secondary' : 'ghost'} className="mb-1 ml-1 size-7" aria-label="Side panel" aria-pressed={panelOpen} onClick={() => app.setPanel(!panelOpen)}>
          <PanelRight />
        </Button>
      </Tip>
      <ConfirmDialog
        open={!!stopping}
        onOpenChange={(o) => !o && setStopping(undefined)}
        title={`Shut down ${stopping?.agent.name ?? ''}?`}
        description={stopping?.agent.plain ? 'Its CLI exits. Its conversation is kept.' : 'Its CLI exits. A task or an answer to it later brings it back in the same conversation.'}
        action={`Shut down ${stopping?.agent.name ?? ''}`}
        run={async () => {
          const h = hosts.find((x) => x.info.id === stopping?.hostId)
          if (!h || !stopping) throw new Error('That host is gone')
          await stopAgent(h.client, stopping.agent.id)
        }}
      />
    </div>
  )
}

/** The open agent's controls at the bar's end: its view switch and its menu (its model is the composer's picker). */
function OpenAgentControls({ host, agent, entries, chord }: { host: HostRuntime; agent: Agent; entries: MenuEntry[]; chord?: string }) {
  const app = useApp()
  const tab = useAppState((s) => s.tab)
  return (
    <div role="toolbar" aria-label="Open agent" className="mb-1 ml-1 flex min-w-0 shrink-0 items-center gap-1">
      <ViewSwitch host={host} agentId={agent.id} tab={tab} setTab={(t) => app.setTab(t)} chord={chord} />
      <ActionMenu
        align="end"
        entries={entries}
        trigger={
          <button type="button" aria-label={`Menu of ${agent.name}`} className="grid size-6 shrink-0 place-items-center rounded text-muted-foreground outline-none hover:bg-muted hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring data-popup-open:bg-muted">
            <MoreHorizontal className="size-4" />
          </button>
        }
      />
    </div>
  )
}

/**
 * The + after the last tab. A plain agent (a CLI like in a terminal: no task, no mesh), a lead (the New workspace dialog),
 * a terminal, and the project's terminals no tab shows. An error is a toast.
 */
function PlusMenu({ project, host, newLead }: { project?: { hostId: string; repo: string }; host?: HostRuntime; newLead: () => void }) {
  const app = useApp()
  const allShells = useAppState((s) => s.shells)
  const all = useAppState((s) => s.tabs)
  const label = project ? `New in ${repoName(project.repo)}` : 'New lead or terminal'
  const trigger = (
    <Button size="icon" variant="ghost" className="mb-1 ml-1 size-7 shrink-0" aria-label={project ? label : 'New in this project'} onClick={() => project && void app.loadShells(project.hostId)}>
      <Plus />
    </Button>
  )
  if (!project || !host) {
    return (
      <Tip label={label}>
        <ActionMenu entries={[{ label: 'New lead', onSelect: newLead }]} trigger={trigger} />
      </Tip>
    )
  }
  return <ProjectPlus project={project} host={host} shells={allShells[project.hostId]} tabs={all} newLead={newLead} label={label} trigger={trigger} />
}

function ProjectPlus({ project, host, shells, tabs, newLead, label, trigger }: { project: { hostId: string; repo: string }; host: HostRuntime; shells: Shell[] | undefined; tabs: TabRef[]; newLead: () => void; label: string; trigger: React.ReactElement<Record<string, unknown>> }) {
  const app = useApp()
  const settings = useHostState(host, (s) => s.settings)
  const installed = useHostState(host, (s) => hostClis(s.daemon))
  const hidden = closedShells(shells, tabs, project.hostId, project.repo)
  const fail = (e: unknown) => app.notify(reason(e), 'error')
  const entries: MenuEntry[] = [
    ...cliKinds(settings, installed).map((kind) => ({ label: `New ${cliName(kind)} agent`, onSelect: () => void app.newAgent(project.hostId, project.repo, kind).catch(fail) })),
    'separator',
    { label: 'New lead…', onSelect: newLead },
    { label: 'New terminal', onSelect: () => void app.newShell(project.hostId, project.repo).catch(fail) },
    ...(hidden.length > 0 ? (['separator', ...hidden.map((s, i) => ({ label: hidden.length > 1 ? `Open terminal ${i + 1}` : 'Open the running terminal', onSelect: () => app.selectAgent(project.hostId, shellTabId(s.id)) }))] as MenuEntry[]) : []),
  ]
  return (
    <Tip label={`${label}: an agent like claude in a terminal (no task, no mesh), a lead, or a terminal`}>
      <span className="flex">
        <ActionMenu entries={entries} align="start" trigger={trigger} />
      </span>
    </Tip>
  )
}
