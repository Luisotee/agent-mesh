import { Maximize2, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { useApp, useAppState } from '@/context'
import { cn } from '@/lib/utils'
import type { HostRuntime } from '@/model'
import { tabKey } from '@/workspace'
import { AgentPanel } from './agent-panel'

/**
 * The main pane split: the open agent and the panes the user pinned beside it, next to each other or stacked. Each
 * pane has its own view of its agent; a pane's header makes it the main one or closes it (its tab stays open).
 */
export function Panes({ host, agentId }: { host: HostRuntime; agentId?: string }) {
  const app = useApp()
  const hosts = useAppState((s) => s.hosts)
  const side = useAppState((s) => s.side)
  const dir = useAppState((s) => s.splitDir)
  return (
    <div role="group" aria-label="Panes" className={cn('flex min-h-0 flex-1 gap-px bg-border', dir === 'row' ? 'flex-row' : 'flex-col')}>
      <section aria-label="Main pane" className="flex min-h-0 min-w-0 flex-1 basis-0 flex-col bg-background">
        {agentId ? (
          <AgentPanel key={agentId} host={host} agentId={agentId} />
        ) : (
          <div className="flex flex-1 items-center justify-center p-8 text-sm text-muted-foreground">Pick an agent to show here.</div>
        )}
      </section>
      {side.map((p) => {
        const h = hosts.find((x) => x.info.id === p.hostId)
        return (
          <section key={tabKey(p)} aria-label="Split pane" className="flex min-h-0 min-w-0 flex-1 basis-0 flex-col bg-background">
            <header className="flex items-center justify-end gap-1 border-b px-2 py-0.5">
              <Button size="icon" variant="ghost" className="size-6" aria-label="Make this the main pane" title="Make this the main pane" onClick={() => app.focusPane(p)}>
                <Maximize2 className="size-3.5" />
              </Button>
              <Button size="icon" variant="ghost" className="size-6" aria-label="Close this pane" title="Close this pane (its tab stays open)" onClick={() => app.unsplit(p)}>
                <X className="size-3.5" />
              </Button>
            </header>
            {h ? <AgentPanel key={p.agentId} host={h} agentId={p.agentId} view={{ tab: p.tab, setTab: (t) => app.setPaneTab(p, t) }} /> : <p className="p-6 text-sm text-muted-foreground">That host is gone.</p>}
          </section>
        )
      })}
    </div>
  )
}
