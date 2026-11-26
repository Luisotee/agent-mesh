import { MessageCircle, QrCode } from 'lucide-react'
import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { useApp, useAppState, useHostState } from '@/context'
import type { HostRuntime } from '@/model'
import { canPair } from './pair-panel'
import { NewRunDialog } from './spawn-dialogs'

/** What the main pane shows with nothing open: the next steps for this host, not a bare instruction. */
export function EmptyPane({ host }: { host: HostRuntime }) {
  const app = useApp()
  const empty = useHostState(host, (s) => Object.keys(s.agents).length === 0)
  const mayPair = useHostState(host, () => canPair(host.store))
  const setupError = useAppState((s) => s.setupError)
  const [busy, setBusy] = useState(false)
  async function setup() {
    setBusy(true)
    await app.openSetup(host.info.id)
    setBusy(false)
  }
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-4 p-8 text-center">
      <p className="max-w-md text-sm text-muted-foreground">
        {empty
          ? `Nothing is running on ${host.info.name} yet. The setup assistant helps connect your repos, other machines and your phone; or start a run yourself.`
          : 'Pick an agent in a run to see it here.'}
      </p>
      {setupError && <p role="alert" className="max-w-md text-xs text-destructive">{setupError}</p>}
      <div className="flex flex-wrap justify-center gap-2">
        <Button disabled={busy} onClick={() => void setup()}>
          <MessageCircle /> {busy ? 'Starting…' : 'Set up with the assistant'}
        </Button>
        <NewRunDialog host={host} />
        {mayPair && (
          <Button variant="outline" onClick={() => app.openPairing(true)}>
            <QrCode /> Pair a device
          </Button>
        )}
      </div>
    </div>
  )
}
