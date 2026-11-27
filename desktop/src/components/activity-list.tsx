import { Bell } from 'lucide-react'
import { useEffect, useState } from 'react'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent } from '@/components/ui/dialog'
import { Tip } from '@/components/ui/tooltip'
import { useApp, useHostState } from '@/context'
import { loadActivitySeen, saveActivitySeen } from '@/layout'
import type { HostRuntime } from '@/model'
import { type ActivityItem } from '@/store'

const KIND: Record<string, string> = {
  input: 'Needs you',
  plan: 'Plan',
  question: 'Question',
  report: 'Report',
  failed: 'Failed',
  exited: 'Exited',
  limited: 'Usage limit',
  error: 'Error',
  finished: 'Finished',
  ready: 'Ready to merge',
}

const clock = (at: number) => (at ? new Date(at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : '')

/**
 * The bell and its Activity list: what the host decided to tell the user (`UserNotice` events), the ones that were for
 * them and the ones that only informed (a worker's plan to a lead that decides it). Unread is counted on this device
 * (`layout.ts`); a click on an entry opens that agent.
 */
export function ActivityBell({ host }: { host: HostRuntime }) {
  const app = useApp()
  const items = useHostState(host, (s) => s.activity)
  const hostId = host.info.id
  const [seen, setSeen] = useState(() => loadActivitySeen(hostId))
  const [open, setOpen] = useState(false)
  useEffect(() => setSeen(loadActivitySeen(hostId)), [hostId])
  const newest = items.length > 0 ? items[items.length - 1]!.seq : 0n
  const unread = items.filter((i) => i.seq > seen).length
  // Open, everything shown counts as read.
  useEffect(() => {
    if (open && newest > seen) {
      setSeen(newest)
      saveActivitySeen(hostId, newest)
    }
  }, [open, newest, seen, hostId])
  const shown: ActivityItem[] = [...items].reverse()
  return (
    <>
      <Tip label={unread ? `Activity (${unread} new)` : 'Activity'}>
        <Button size="icon" variant={open ? 'secondary' : 'ghost'} aria-label="Activity" className="relative" onClick={() => setOpen(true)}>
          <Bell />
          {unread > 0 && <span className="absolute -top-0.5 -right-0.5 rounded-full bg-state-needs px-1 text-[11px] leading-4 font-bold text-primary-foreground">{unread > 99 ? '99+' : unread}</span>}
        </Button>
      </Tip>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent title="Activity" description={'What happened in the open runs since this app connected. Entries marked "for you" are what notifies; the rest is a lead handling its workers.'}>
          {shown.length === 0 ? (
            <p className="py-4 text-center text-sm text-muted-foreground">Nothing yet.</p>
          ) : (
            <ul aria-label="Activity" className="max-h-[60vh] overflow-y-auto">
              {shown.map((i) => (
                <li key={String(i.seq)}>
                  <button
                    type="button"
                    className="flex w-full items-start gap-2 rounded-md px-2 py-1.5 text-left hover:bg-accent"
                    onClick={() => {
                      setOpen(false)
                      if (i.notice.agentId) app.selectAgent(hostId, i.notice.agentId, i.notice.tab === 'terminal' ? 'terminal' : 'chat')
                    }}
                  >
                    <span className="min-w-0 flex-1">
                      <span className="block truncate text-sm font-medium">{i.notice.title}</span>
                      {i.notice.body && <span className="block truncate text-xs text-muted-foreground">{i.notice.body}</span>}
                    </span>
                    <span className="shrink-0 text-xs text-muted-foreground">
                      {KIND[i.notice.kind] ?? i.notice.kind}
                      {i.notice.forUser && ' · for you'} · {clock(i.at)}
                    </span>
                  </button>
                </li>
              ))}
            </ul>
          )}
        </DialogContent>
      </Dialog>
    </>
  )
}
