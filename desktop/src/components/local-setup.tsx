import { Check, Copy, RefreshCw } from 'lucide-react'
import { useCallback, useEffect, useRef, useState } from 'react'
import type { LocalCandidate, LocalFix } from '../../shared/api'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { useApp, useAppState } from '@/context'
import { cn } from '@/lib/utils'

const STATE_LABEL: Record<LocalCandidate['state'], string> = {
  ready: 'Ready',
  'not-installed': 'Not installed',
  'not-running': 'Not running',
  'no-listen': 'Not reachable from Windows',
  unreachable: "Can't be reached",
}

const FIX_LABEL: Record<LocalFix, string> = {
  start: 'Start the daemon',
  'enable-listen': 'Let Windows reach it',
}

/** What a fix will do, said before the click. */
const FIX_HINT: Record<LocalFix, string> = {
  start: 'Runs systemctl --user enable --now agent-mesh.',
  'enable-listen': 'Adds a systemd drop-in that makes the daemon listen on 127.0.0.1:9443, and restarts it (running agents keep their terminals).',
}

const clean = (e: unknown) => (e instanceof Error ? e.message.replace(/^Error invoking remote method '[^']*': (Error: )?/, '') : String(e))

/**
 * Looks for a daemon on this machine (a socket here, or a WSL distro on Windows) and connects to the one it finds, with
 * nothing pasted. Where it finds a problem it says which and offers the fix the app can run; what the user has to run
 * themselves is shown to copy. `auto` (the first-run screen): on the app's first run a single ready daemon is connected without a click, and what is connected opens the setup chat; without it (the add dialog) the host is only added.
 * `onReady`: for a host that is already saved (its banner's "Check this machine"): a ready daemon is retried, not added again.
 */
export function LocalSetup({ auto = false, onConnected, onReady }: { auto?: boolean; onConnected?: () => void; onReady?: () => void }) {
  const app = useApp()
  const firstRun = useAppState((s) => s.firstRun)
  const [found, setFound] = useState<LocalCandidate[]>()
  const [busy, setBusy] = useState('')
  const [error, setError] = useState('')
  const [copied, setCopied] = useState('')
  const connected = useRef(false)
  // A callback a parent makes anew on every render must not make the scan start again.
  const done = useRef(onConnected)
  done.current = onConnected

  const connect = useCallback(
    async (id: string) => {
      if (connected.current) return
      connected.current = true
      setBusy(id)
      setError('')
      try {
        await app.connectLocal(id, auto)
        done.current?.()
      } catch (e) {
        connected.current = false
        setError(clean(e))
        setBusy('')
      }
    },
    [app, auto],
  )

  const scan = useCallback(
    async () => {
      setBusy('scan')
      setError('')
      try {
        const got = await app.api.local.detect()
        setFound(got)
        const ready = got.filter((c) => c.state === 'ready')
        // The first run connects by itself, once; after a host was removed the user decides.
        const first = app.store.getState().firstRun
        if (first) app.store.setState({ firstRun: false })
        if (auto && first && ready.length === 1) await connect(ready[0]!.id)
        else setBusy('')
      } catch (e) {
        setError(clean(e))
        setBusy('')
      }
    },
    [app, auto, connect],
  )

  useEffect(() => {
    void scan()
  }, [scan])

  async function fix(c: LocalCandidate, f: LocalFix) {
    setBusy(`${c.id}:${f}`)
    setError('')
    try {
      setFound(await app.api.local.fix(c.id, f))
    } catch (e) {
      setError(clean(e))
    } finally {
      setBusy('')
    }
  }

  async function copy(text: string) {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(text)
    } catch {
      setError("couldn't copy: select the text and copy it")
    }
  }

  const scanning = busy === 'scan' || (!found && !error)
  return (
    <section aria-label="Daemons on this computer" className="flex flex-col gap-3">
      {scanning && <p role="status" className="text-xs text-muted-foreground">{auto && firstRun ? 'Looking for agent-mesh on this computer…' : 'Looking…'}</p>}
      {found?.map((c) => (
        <div key={c.id} data-testid={`local-${c.id}`} className={cn('flex flex-col gap-2 rounded-md border p-3', c.state === 'ready' && 'border-state-done')}>
          <div className="flex items-center gap-2">
            <h2 className="min-w-0 flex-1 truncate text-sm font-medium">{c.label}</h2>
            <Badge>{STATE_LABEL[c.state]}</Badge>
          </div>
          <p className="text-xs text-muted-foreground">{c.detail}</p>
          {c.commands && (
            <ul className="flex flex-col gap-1">
              {c.commands.map((cmd) => (
                <li key={cmd} className="flex items-center gap-2">
                  <code className="min-w-0 flex-1 truncate rounded bg-muted px-2 py-1 font-mono text-xs">{cmd}</code>
                  <Button size="icon" variant="ghost" aria-label={`Copy ${cmd}`} onClick={() => void copy(cmd)}>
                    {copied === cmd ? <Check /> : <Copy />}
                  </Button>
                </li>
              ))}
            </ul>
          )}
          <div className="flex flex-wrap items-center gap-2">
            {c.state === 'ready' && !onReady && (
              <Button size="sm" disabled={!!busy} onClick={() => void connect(c.id)}>
                {busy === c.id ? 'Connecting…' : 'Connect'}
              </Button>
            )}
            {c.state === 'ready' && onReady && (
              <Button size="sm" disabled={!!busy} onClick={onReady}>
                Retry the connection
              </Button>
            )}
            {c.fixes.map((f) => (
              <span key={f} className="flex flex-col gap-0.5">
                <Button size="sm" variant="secondary" disabled={!!busy} onClick={() => void fix(c, f)} title={FIX_HINT[f]}>
                  {busy === `${c.id}:${f}` ? 'Working…' : FIX_LABEL[f]}
                </Button>
                <span className="max-w-64 text-xs text-muted-foreground">{FIX_HINT[f]}</span>
              </span>
            ))}
          </div>
        </div>
      ))}
      {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
      {found && (
        <div>
          <Button size="sm" variant="ghost" disabled={!!busy} onClick={() => void scan()}>
            <RefreshCw /> Look again
          </Button>
        </div>
      )}
    </section>
  )
}
