import { Eye, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { useApp, useAppState } from '@/context'
import type { ViewRequestRow } from '@/model'

/** What a request asks to show, in words: "Fix's chat", "the settings". */
export function describeView(r: ViewRequestRow, agentName: (hostId: string, id: string) => string | undefined, runTitle: (hostId: string, id: string) => string | undefined): string {
  const v = r.request
  switch (v.view) {
    case 'agent':
      return `${agentName(r.hostId, v.target) ?? 'an agent'}'s chat`
    case 'terminal':
      return `${agentName(r.hostId, v.target) ?? 'an agent'}'s terminal`
    case 'run':
      return `the run ${runTitle(r.hostId, v.target) ?? ''}`.trim()
    default:
      return v.target ? `the settings (${v.target})` : 'the settings'
  }
}

/**
 * What an agent or the CLI asked the app to show (`OpenView`): a toast naming who asks and what, with Open and Dismiss. The
 * app never switches the user's view by itself for an agent unless they turned `app.open_views` on (and then not while they type).
 */
export function ViewToasts() {
  const app = useApp()
  const rows = useAppState((s) => s.viewRequests)
  const hosts = useAppState((s) => s.hosts)
  if (rows.length === 0) return null
  const lookup = (hostId: string) => hosts.find((h) => h.info.id === hostId)?.store.getState()
  return (
    <div role="region" aria-label="Requests to show a view" className="pointer-events-none fixed right-4 bottom-4 z-50 flex flex-col gap-2">
      {rows.map((r) => (
        <div key={r.id} role="status" className="pointer-events-auto flex w-80 items-start gap-2 rounded-lg border bg-popover p-3 text-sm shadow-lg">
          <Eye className="mt-0.5 size-4 shrink-0 text-muted-foreground" aria-hidden />
          <p className="min-w-0 flex-1 text-[13px]">
            <span className="font-medium">{r.request.from || 'An agent'}</span> wants to show you {describeView(r, (h, id) => lookup(h)?.agents[id]?.name, (h, id) => lookup(h)?.runs[id]?.title)}.
          </p>
          <div className="flex shrink-0 gap-1">
            <Button size="sm" onClick={() => app.openViewRequest(r.id)}>Open</Button>
            <Button size="icon" variant="ghost" className="size-7" aria-label="Dismiss" onClick={() => app.dismissViewRequest(r.id)}><X className="size-3.5" /></Button>
          </div>
        </div>
      ))}
    </div>
  )
}
