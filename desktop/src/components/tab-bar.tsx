import { X } from 'lucide-react'
import { useApp, useAppState } from '@/context'
import { cn } from '@/lib/utils'
import { tabKey } from '@/workspace'
import { useHostViews } from '@/hooks'
import { StateIcon } from './state-icon'

/** The agents open as tabs, across hosts: a click shows one, the × closes it. */
export function TabBar() {
  const app = useApp()
  const tabs = useAppState((s) => s.tabs)
  const hosts = useAppState((s) => s.hosts)
  const hostId = useAppState((s) => s.hostId)
  const agentId = useAppState((s) => s.agentId)
  const views = useHostViews(hosts)
  if (tabs.length === 0) return null
  const many = hosts.length > 1
  return (
    <div role="tablist" aria-label="Open agents" className="flex shrink-0 gap-px overflow-x-auto border-b bg-surface px-1 pt-1">
      {tabs.map((t) => {
        const view = views.find((v) => v.hostId === t.hostId)
        const agent = view?.agents[t.agentId]
        const active = t.hostId === hostId && t.agentId === agentId
        const label = agent?.name ?? t.agentId
        return (
          <div
            key={tabKey(t)}
            className={cn('group flex max-w-52 min-w-0 items-center rounded-t-md border border-b-0 text-[13px]', active ? 'bg-background text-foreground' : 'border-transparent text-muted-foreground hover:bg-accent')}
          >
            <button
              type="button"
              role="tab"
              aria-selected={active}
              title={many && view ? `${label} on ${view.hostName}` : label}
              onClick={() => app.selectAgent(t.hostId, t.agentId)}
              className="flex min-w-0 items-center gap-1.5 py-1.5 pr-1 pl-2.5 outline-none focus-visible:ring-2 focus-visible:ring-ring"
            >
              {agent ? <StateIcon agent={agent} className="size-3.5" /> : null}
              <span className="truncate">{label}</span>
              {many && view && <span className="shrink-0 text-[10px] text-muted-foreground">{view.hostName}</span>}
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
        )
      })}
    </div>
  )
}
