import { Columns2, Rows2, X } from 'lucide-react'
import { useApp, useAppState } from '@/context'
import { cn } from '@/lib/utils'
import { MAX_SIDE_PANES, tabKey } from '@/workspace'
import { useHostViews } from '@/hooks'
import { StateIcon } from './state-icon'

/** The agents open as tabs, across hosts: a click shows one, the × closes it. */
export function TabBar() {
  const app = useApp()
  const tabs = useAppState((s) => s.tabs)
  const hosts = useAppState((s) => s.hosts)
  const hostId = useAppState((s) => s.hostId)
  const agentId = useAppState((s) => s.agentId)
  const side = useAppState((s) => s.side)
  const dir = useAppState((s) => s.splitDir)
  const views = useHostViews(hosts)
  if (tabs.length === 0) return null
  const many = hosts.length > 1
  return (
    <div className="flex shrink-0 items-end border-b bg-surface px-1 pt-1">
    <div role="tablist" aria-label="Open agents" className="flex min-w-0 flex-1 gap-px overflow-x-auto">
      {tabs.map((t) => {
        const view = views.find((v) => v.hostId === t.hostId)
        const agent = view?.agents[t.agentId]
        const active = t.hostId === hostId && t.agentId === agentId
        const beside = side.some((p) => p.hostId === t.hostId && p.agentId === t.agentId)
        const label = agent?.name ?? t.agentId
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
            className={cn('group flex max-w-52 min-w-0 items-center rounded-t-md border border-b-0 text-[13px]', active ? 'bg-background text-foreground' : 'border-transparent text-muted-foreground hover:bg-accent')}
          >
            <button
              type="button"
              role="tab"
              aria-selected={active}
              data-beside={beside ? 'true' : undefined}
              title={many && view ? `${label} on ${view.hostName}` : label}
              onClick={() => app.selectAgent(t.hostId, t.agentId)}
              className="flex min-w-0 items-center gap-1.5 py-1.5 pr-1 pl-2.5 outline-none focus-visible:ring-2 focus-visible:ring-ring"
            >
              {agent ? <StateIcon agent={agent} className="size-3.5" /> : null}
              <span className="truncate">{label}</span>
              {many && view && <span className="shrink-0 text-[10px] text-muted-foreground">{view.hostName}</span>}
            </button>
            {!active && !beside && side.length < MAX_SIDE_PANES && (
              <button
                type="button"
                aria-label={`Show ${label} beside`}
                title="Show beside the open agent"
                onClick={() => app.pinSplit(t.hostId, t.agentId)}
                className="hidden size-5 shrink-0 place-items-center rounded hover:bg-muted group-hover:grid group-focus-within:grid"
              >
                <Columns2 className="size-3" />
              </button>
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
    {side.length > 0 && (
      <button
        type="button"
        aria-label={dir === 'row' ? 'Stack the panes' : 'Put the panes side by side'}
        title={dir === 'row' ? 'Stack the panes' : 'Put the panes side by side'}
        onClick={() => app.setSplitDir(dir === 'row' ? 'column' : 'row')}
        className="mb-1 ml-1 grid size-6 shrink-0 place-items-center rounded hover:bg-muted"
      >
        {dir === 'row' ? <Rows2 className="size-3.5" /> : <Columns2 className="size-3.5" />}
      </button>
    )}
    </div>
  )
}
