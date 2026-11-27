import { SquareTerminal } from 'lucide-react'
import { useEffect, useState } from 'react'
import { Button } from '@/components/ui/button'
import { Chip } from '@/components/ui/chip'
import { Dialog, DialogContent } from '@/components/ui/dialog'
import { MoreButton, type MenuEntry } from '@/components/ui/menu'
import { useApp, useAppState } from '@/context'
import { repoName } from '@/derive'
import type { HostRuntime } from '@/model'
import { shellIdOf, shellTabId } from '@/shells'
import { TerminalView } from './terminal-view'

/** A project shell: a plain login shell in the project's folder on the host, in the pane an agent's chat would be in. */
export function ShellPanel({ host, agentId }: { host: HostRuntime; agentId: string }) {
  const app = useApp()
  const id = shellIdOf(agentId)
  const shells = useAppState((s) => s.shells[host.info.id])
  const failure = useAppState((s) => s.shellsError[host.info.id])
  const shell = shells?.find((x) => x.id === id)
  if (!shell) {
    // A restored tab shows before the host's shells are read: the shell is not missing, only not here yet.
    return (
      <div className="flex flex-1 flex-col items-center justify-center gap-2 p-8 text-sm text-muted-foreground">
        {shells ? 'That shell has ended.' : failure ? <p role="alert">{`The host's shells could not be read: ${failure}`}</p> : 'Connecting to the host…'}
        {!shells && failure && <Button size="sm" variant="outline" onClick={() => void app.loadShells(host.info.id)}>Try again</Button>}
      </div>
    )
  }
  const entries: MenuEntry[] = [{ label: 'End the shell…', onSelect: () => app.closeTab({ hostId: host.info.id, agentId: shellTabId(id) }), destructive: true }]
  return (
    <article aria-label={`Terminal in ${repoName(shell.repo)}`} className="flex min-h-0 flex-1 flex-col gap-2 p-3 pt-2">
      <header className="flex min-w-0 items-center gap-2">
        <SquareTerminal className="size-4 shrink-0 text-muted-foreground" />
        <h1 className="truncate text-[14px] font-semibold">Terminal</h1>
        <Chip>{repoName(shell.repo)}</Chip>
        <p className="min-w-0 flex-1 truncate text-xs text-muted-foreground" title={shell.repo}>{shell.repo}</p>
        <MoreButton label="Menu of this terminal" entries={entries} />
      </header>
      <TerminalView host={host} shellId={id} active />
    </article>
  )
}

/** Asked when a shell tab is closed: end the shell, or close the tab and leave it running (the + menu opens it again). */
export function ShellCloseDialog() {
  const app = useApp()
  const closing = useAppState((s) => s.closingShell)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const shells = useAppState((s) => (closing ? s.shells[closing.hostId] : undefined))
  // A shell that ended on its own is not asked about: its tab just closes. The host's list says which (read fresh as the question opens).
  useEffect(() => {
    if (closing) void app.loadShells(closing.hostId)
  }, [closing, app])
  useEffect(() => {
    if (closing && shells && !shells.some((x) => shellTabId(x.id) === closing.agentId)) app.closeShellTab(closing, true)
  }, [closing, shells, app])
  if (!closing) return null
  const ref = closing
  const end = async () => {
    setBusy(true)
    setError('')
    try {
      await app.endShell(ref.hostId, shellIdOf(ref.agentId))
      app.closeShellTab(ref, false)
    } catch (e) {
      setError(e instanceof Error ? e.message.replace(/^\[[a-z_]+\]\s*/, '') : String(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog open onOpenChange={(o) => { if (!o) { setError(''); app.closeShellTab(ref, false) } }}>
      <DialogContent title="Close this terminal?" description="The shell can end with its tab, or keep running on the host: the + menu opens it again, and it survives restarting the app.">
        {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={() => app.closeShellTab(ref, false)}>Cancel</Button>
          <Button variant="outline" onClick={() => app.closeShellTab(ref, true)}>Keep running</Button>
          <Button variant="destructive" disabled={busy} onClick={() => void end()}>End shell</Button>
        </div>
      </DialogContent>
    </Dialog>
  )
}
