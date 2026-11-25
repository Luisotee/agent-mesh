import { useEffect, useState } from 'react'
import { Button } from '@/components/ui/button'
import { useHostState } from '@/context'
import type { HostRuntime } from '@/model'
import { ConnectionLogDialog } from './connection-log'

/** Shown above the content whenever a host isn't connected: what is wrong, when it retries, and the log. */
export function ConnectionBanner({ host }: { host: HostRuntime }) {
  const conn = useHostState(host, (s) => s.conn)
  const [now, setNow] = useState(Date.now())
  useEffect(() => {
    if (conn.kind !== 'reconnecting') return
    const id = setInterval(() => setNow(Date.now()), 500)
    return () => clearInterval(id)
  }, [conn.kind])
  if (conn.kind === 'ready') return null
  let text: string
  switch (conn.kind) {
    case 'connecting':
      text = `Connecting to ${host.info.name}…`
      break
    case 'reconnecting': {
      const s = Math.max(0, Math.ceil((conn.retryAt - now) / 1000))
      text = `${host.info.name} is unreachable (${conn.error}). Trying again in ${s} s.`
      break
    }
    case 'stopped':
      text = `${conn.message} (${{ auth: "the host didn't accept the app's token", incompatible: 'the host and the app speak different protocols', config: 'the host entry is wrong' }[conn.reason]})`
  }
  const stopped = conn.kind === 'stopped'
  return (
    <div role="status" className={`flex items-center gap-3 border-b px-4 py-2 text-xs ${stopped ? 'bg-destructive/15' : 'bg-state-needs/10'}`}>
      <span className="min-w-0 flex-1 truncate">{text}</span>
      <Button size="sm" variant="outline" onClick={() => host.connection.retryNow()}>
        Retry now
      </Button>
      <ConnectionLogDialog host={host} />
    </div>
  )
}
