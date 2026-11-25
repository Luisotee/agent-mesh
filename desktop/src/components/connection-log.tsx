import { ScrollText } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogTrigger } from '@/components/ui/dialog'
import { useHostState } from '@/context'
import type { HostRuntime } from '@/model'

/** The connection log of a host: what the connection did, newest last, for troubleshooting. */
export function ConnectionLogDialog({ host }: { host: HostRuntime }) {
  const log = useHostState(host, (s) => s.log)
  const conn = useHostState(host, (s) => s.conn)
  return (
    <Dialog>
      <DialogTrigger render={<Button size="sm" variant="ghost" aria-label="Connection log" />}>
        <ScrollText /> Log
      </DialogTrigger>
      <DialogContent title={`Connection to ${host.info.name}`} description={`${host.info.kind === 'tcp' ? 'TCP' : 'State dir'} ${host.info.target}, now ${conn.kind}`}>
        <ol className="max-h-80 overflow-y-auto rounded-md border bg-surface p-2 font-mono text-xs">
          {log.length === 0 && <li className="text-muted-foreground">Nothing yet.</li>}
          {log.map((l, i) => (
            <li key={i} className="flex gap-2 py-0.5">
              <time className="shrink-0 text-muted-foreground">{new Date(l.at).toLocaleTimeString()}</time>
              <span>{l.text}</span>
            </li>
          ))}
        </ol>
      </DialogContent>
    </Dialog>
  )
}
