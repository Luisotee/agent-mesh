import { useState } from 'react'
import { Columns2, PanelRight, Plus, Rows2, SquareTerminal, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { displayChord } from '@/keys'
import { useApp, useAppState } from '@/context'
import { cn } from '@/lib/utils'
import { hueStyle } from '@/hue'
import { MAX_SIDE_PANES, projectTabs, tabKey, type TabRef } from '@/workspace'
import { agentProject } from '@/sidebar'
import { repoName } from '@/derive'
import { AgentRole, type Agent } from '@/gen/agentmesh/v1/types_pb'
import type { HostRuntime } from '@/model'
import { useHostViews, useKeys } from '@/hooks'
import { StateIcon } from './state-icon'
import { Tip } from '@/components/ui/tooltip'
import { ActionMenu, type MenuEntry } from '@/components/ui/menu'
import { closedShells, isShellTab, shellIdOf, shellProject, shellTabId } from '@/shells'

/** The repo the run of `a` is led from (its root agent's), else its own. */
function leadRepo(host: HostRuntime | undefined, a: Agent): string {
  const agents = host?.store.getState().agents ?? {}
  return Object.values(agents).find((x) => x.runId === a.runId && x.role === AgentRole.ROOT)?.repo || a.repo
}

/**
 * The open project's agents as tabs (one tab bar per project: opening an agent of another project shows that
 * project's tabs, each project keeping its own): a click shows one, the × or a middle click closes it, the wheel scrolls the
 * strip, a drag onto another tab moves it there (onto an edge of the main pane, a split), and a double click on the
 * strip's empty space or the + starts a new lead in the project. The side panel's toggle sits at its end.
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
  const views = useHostViews(hosts)
  const keys = useKeys(hosts)
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
  // The shells of this project that no tab shows (kept running when their tab closed): the + menu opens them again.
  const hiddenShells = project ? closedShells(allShells[project.hostId], all, project.hostId, project.repo) : []
  const [shellError, setShellError] = useState('')
  const newTerminal = () => {
    if (!project) return
    setShellError('')
    app.newShell(project.hostId, project.repo).catch((e) => setShellError(e instanceof Error ? e.message.replace(/^\[[a-z_]+\]\s*/, '') : String(e)))
  }
  const plusEntries: MenuEntry[] = [
    { label: 'New lead', onSelect: () => newLead() },
    { label: 'New terminal', onSelect: newTerminal, disabled: !project },
    ...(hiddenShells.length > 0 ? (['separator', ...hiddenShells.map((s, i) => ({ label: hiddenShells.length > 1 ? `Open terminal ${i + 1}` : 'Open the running terminal', onSelect: () => app.selectAgent(project!.hostId, shellTabId(s.id)) }))] as MenuEntry[]) : []),
  ]
  const newLead = () => app.store.getState().hosts.length > 0 && app.openWorkspace(true, undefined, project)
  return (
    <div className="flex min-h-9 shrink-0 items-end border-b bg-surface px-1 pt-1">
    <div
      role="tablist"
      aria-label="Open agents"
      className="flex min-w-0 flex-1 gap-px self-stretch overflow-x-auto [scrollbar-width:none]"
      onWheel={(e) => {
        if (e.deltaY && !e.deltaX) e.currentTarget.scrollLeft += e.deltaY
      }}
      onDoubleClick={(e) => {
        if (e.target === e.currentTarget) newLead()
      }}
    >
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
          <div
            key={tabKey(t)}
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
              'group relative flex max-w-52 min-w-0 shrink-0 items-center self-end rounded-t-md border border-b-0 text-[13px]',
              active ? 'bg-background text-foreground after:absolute after:inset-x-0 after:bottom-0 after:h-0.5 after:bg-identity' : 'border-transparent text-muted-foreground hover:bg-accent',
            )}
          >
            <button
              type="button"
              role="tab"
              aria-selected={active}
              data-beside={beside ? 'true' : undefined}
              title={many && view ? `${label} on ${view.hostName}` : label}
              onClick={() => app.selectAgent(t.hostId, t.agentId)}
              className="flex min-w-0 items-center gap-1.5 py-1.5 pr-1 pl-2.5 outline-none focus-visible:ring-2 focus-visible:ring-identity"
            >
              {agent ? <StateIcon agent={agent} className="size-3.5" /> : shellTab ? <SquareTerminal className="size-3.5" /> : null}
              <span className="truncate">{label}</span>
              {many && view && <span className="shrink-0 text-xs text-muted-foreground">{view.hostName}</span>}
            </button>
            {!active && !beside && side.length < MAX_SIDE_PANES && (
              <Tip label="Show beside the open agent">
                <button
                  type="button"
                  aria-label={`Show ${label} beside`}
                  onClick={() => app.pinSplit(t.hostId, t.agentId)}
                  className="hidden size-5 shrink-0 place-items-center rounded hover:bg-muted group-hover:grid group-focus-within:grid"
                >
                  <Columns2 className="size-3" />
                </button>
              </Tip>
            )}
            <button
              type="button"
              aria-label={`Close ${label}`}
              onClick={() => app.closeTab(t)}
              className="mr-1 grid size-5 shrink-0 place-items-center rounded hover:bg-muted"
            >
              <X className="size-3" />
            </button>
          </div>
        )
      })}
    </div>
    {shellError && <span role="alert" className="self-center px-2 text-xs text-destructive">{shellError}</span>}
    {tabs.length === 0 && <span className="self-center px-2 text-xs text-muted-foreground">No agent open</span>}
    <Tip label={project ? `New in ${repoName(project.repo)}` : 'New lead or terminal'}>
      <span className="mb-1 ml-1">
        <ActionMenu
          entries={plusEntries}
          align="end"
          trigger={
            <Button size="icon" variant="ghost" className="size-7" aria-label={project ? `New in ${repoName(project.repo)}` : 'New in this project'} onClick={() => project && void app.loadShells(project.hostId)}>
              <Plus />
            </Button>
          }
        />
      </span>
    </Tip>
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
    </div>
  )
}
