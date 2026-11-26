import { Code, ConnectError } from '@connectrpc/connect'
import { ExternalLink, Network } from 'lucide-react'
import { useEffect, useState } from 'react'
import { tailnetStatus } from '@/actions'
import { useHostState } from '@/context'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import type { TailnetStatusResponse } from '@/gen/agentmesh/v1/mesh_pb'
import type { HostRuntime } from '@/model'
import { safeLoginUrl, tailnetAddresses, tailnetRunning, tailnetStateLabel } from '@/tailnet'
import { QrCode } from './qr-code'

const say = (e: unknown) => (e instanceof Error ? e.message.replace(/^\[[a-z_]+\]\s*/, '') : String(e))

/** How often the node is asked again: fast while it is coming up or waiting for a login, slowly once it runs. */
const FAST_MS = 5_000
const SLOW_MS = 30_000

/**
 * The host's built-in Tailscale node (`meshd --tailscale`, docs/relay.md): its state, the login URL while it needs
 * one (a link and a QR code for a phone), its addresses, and a way to put its address into a pairing link. Only the
 * host's own device may ask `TailnetStatus` (the login URL attaches the node to whichever tailnet opens it), so this
 * is shown with the pairing screen and nowhere else.
 */
export function TailnetPanel({ host, onUse }: { host: HostRuntime; onUse?: (address: string) => void }) {
  const [status, setStatus] = useState<TailnetStatusResponse>()
  const [error, setError] = useState('')
  // A host older than this app doesn't have the action: say nothing instead of an error on every visit.
  const [unsupported, setUnsupported] = useState(false)
  // What is shown belongs to one host on one connection: a different host, or the same one reconnected (it may have
  // been restarted with or without --tailscale, or upgraded), starts over.
  const connected = useHostState(host, (s) => s.conn.kind === 'ready')
  useEffect(() => {
    setStatus(undefined)
    setError('')
    setUnsupported(false)
  }, [host, connected])

  const running = status ? tailnetRunning(status) : false
  useEffect(() => {
    if (unsupported || !connected) return
    let live = true
    let timer: ReturnType<typeof setTimeout> | undefined
    // The next ask waits for the last answer: fast while the node comes up or waits for a login, slowly once it runs,
    // and slowly while it is off (meshd may be restarted with --tailscale while this is open).
    const load = () =>
      tailnetStatus(host.client).then(
        (r) => {
          if (!live) return
          setStatus(r)
          setError('')
          timer = setTimeout(load, tailnetRunning(r) || !r.enabled ? SLOW_MS : FAST_MS)
        },
        (e: unknown) => {
          if (!live) return
          if (e instanceof ConnectError && e.code === Code.Unimplemented) {
            setUnsupported(true)
            return
          }
          setError(say(e))
          timer = setTimeout(load, FAST_MS)
        },
      )
    void load()
    return () => {
      live = false
      clearTimeout(timer)
    }
  }, [host, unsupported, connected])

  if (unsupported) return null
  if (!status) {
    return error ? (
      <p role="alert" className="max-w-2xl text-xs text-destructive">Tailscale: {error}</p>
    ) : null
  }
  const login = safeLoginUrl(status.authUrl)
  const addresses = tailnetAddresses(status)
  return (
    <section aria-label="Tailscale" className="flex max-w-2xl flex-col gap-3 rounded-lg border bg-card p-4 text-[13px]">
      <header className="flex items-center gap-2">
        <Network className="size-4 text-muted-foreground" aria-hidden />
        <h2 className="text-sm font-semibold">Tailscale</h2>
        {status.enabled && <Badge data-testid="tailnet-state">{tailnetStateLabel(status.state)}</Badge>}
      </header>

      {!status.enabled ? (
        <p className="text-xs text-muted-foreground">
          This host's built-in Tailscale node is off. Start meshd with <code className="font-mono">--tailscale</code> to reach it from your other devices over a tailnet (docs/relay.md).
        </p>
      ) : (
        <>
          {status.problems.length > 0 && (
            <div role="alert" aria-label="Tailscale problems" className="text-xs text-state-needs">
              <ul className="list-disc pl-5">
                {status.problems.map((p) => (
                  <li key={p}>{p}</li>
                ))}
              </ul>
            </div>
          )}
          {login && (
            <div className="flex flex-col items-start gap-2">
              <p className="text-xs text-muted-foreground">
                The node needs a login. Open the link, or scan the code, with an account on the tailnet that should get this host: whoever opens it attaches the node to their tailnet.
              </p>
              <Button variant="outline" onClick={() => window.desktop.openExternal(login)}>
                <ExternalLink /> Log in to Tailscale
              </Button>
              <QrCode text={login} label="Tailscale login QR code" className="size-40" />
            </div>
          )}
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-xs">
            {status.hostname && (
              <>
                <dt className="text-muted-foreground">Node</dt>
                <dd className="font-mono">{status.hostname}</dd>
              </>
            )}
            {status.dnsName && (
              <>
                <dt className="text-muted-foreground">MagicDNS</dt>
                <dd className="font-mono">{status.dnsName.replace(/\.$/, '')}</dd>
              </>
            )}
            {status.addresses.length > 0 && (
              <>
                <dt className="text-muted-foreground">Addresses</dt>
                <dd className="font-mono">{status.addresses.join(', ')}</dd>
              </>
            )}
            {status.controlUrl && (
              <>
                <dt className="text-muted-foreground">Control server</dt>
                <dd className="font-mono">{status.controlUrl}</dd>
              </>
            )}
          </dl>
          {running && onUse && addresses.length > 0 && (
            <div className="flex flex-col items-start gap-2">
              <p className="text-xs text-muted-foreground">
                A device on the same tailnet (the Tailscale app on it) reaches this host at these addresses, pinned to its certificate as on any direct route.
              </p>
              <div className="flex flex-wrap gap-2">
                {addresses.map((a) => (
                  <Button key={a} size="sm" variant="secondary" onClick={() => onUse(a)} aria-label={`Use ${a} in the pairing link`}>
                    Use {a}
                  </Button>
                ))}
              </div>
            </div>
          )}
        </>
      )}
      {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
    </section>
  )
}
