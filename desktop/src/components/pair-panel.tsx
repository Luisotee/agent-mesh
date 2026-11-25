import { Check, Copy, Smartphone } from 'lucide-react'
import { useEffect, useMemo, useRef, useState } from 'react'
import { createPairing, listDevices, revokeDevice } from '@/actions'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input, Select } from '@/components/ui/input'
import { Field } from '@/components/ui/label'
import { useHostState } from '@/context'
import { ActorClass } from '@/gen/agentmesh/v1/actor_pb'
import type { CreatePairingResponse } from '@/gen/agentmesh/v1/mesh_pb'
import type { Device } from '@/gen/agentmesh/v1/types_pb'
import type { HostRuntime } from '@/model'
import { setDevices } from '@/store'
import { QrCode } from './qr-code'

const NONE: string[] = []

const LIFETIMES = [
  { label: '10 minutes', seconds: 600 },
  { label: '1 hour', seconds: 3600 },
  { label: '24 hours', seconds: 86400 },
]

/** The id of the host's own device (docs/relay.md): the only one that may pair, list or revoke devices. */
export const LOCAL_DEVICE = 'device-local'

/** Whether this app is the host's own device, so the host will let it manage paired devices. */
export function canPair(host: HostRuntime['store']): boolean {
  const s = host.getState()
  return s.conn.kind === 'ready' && s.conn.hello.actor?.id === LOCAL_DEVICE
}

const say = (e: unknown) => (e instanceof Error ? e.message.replace(/^\[[a-z_]+\]\s*/, '') : String(e))
const when = (d: Device) => (d.createdAt ? new Date(Number(d.createdAt.seconds) * 1000).toLocaleString() : '')

/**
 * Pair a device: a one-time link and its QR code for another app (an Android phone, another desktop) to add this
 * host, and the list of devices already paired with a way to revoke each. Only the host's own device may do it,
 * so the screen is offered only there; the host refuses anyone else.
 */
export function PairPanel({ host }: { host: HostRuntime }) {
  const devices = useHostState(host, (s) => s.devices)
  const caps = useHostState(host, (s) => s.daemon?.capabilities ?? NONE)
  const reachable = caps.includes('tls') || caps.includes('relay')
  const [name, setName] = useState('')
  const [readOnly, setReadOnly] = useState(false)
  const [lifetime, setLifetime] = useState(600)
  const [directUrl, setDirectUrl] = useState('')
  const [made, setMade] = useState<{ res: CreatePairingResponse; known: Set<string> }>()
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [loaded, setLoaded] = useState(false)
  const [copied, setCopied] = useState(false)

  useEffect(() => {
    let live = true
    listDevices(host.client).then(
      (r) => {
        if (live) host.store.setState(setDevices(r.devices))
        if (live) setLoaded(true)
      },
      (e: unknown) => {
        if (live) setError(say(e))
      },
    )
    return () => {
      live = false
    }
  }, [host])

  // A device that appears after the link was made is the one that used it (DevicePaired reaches this device only).
  const paired = made ? devices.find((d) => !made.known.has(d.id)) : undefined

  async function create(e: React.FormEvent) {
    e.preventDefault()
    // The devices already paired are what tells a new one from them: wait for the list.
    if (busy || !loaded) return
    setBusy(true)
    setError('')
    try {
      const res = await createPairing(host.client, { name: name.trim(), readOnly, ttlSeconds: lifetime, directUrl: directUrl.trim() })
      setMade({ res, known: new Set(host.store.getState().devices.map((d) => d.id)) })
      setCopied(false)
    } catch (err) {
      setError(say(err))
    } finally {
      setBusy(false)
    }
  }

  async function copy(text: string) {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(true)
    } catch {
      setError("couldn't copy: select the link and copy it")
    }
  }

  return (
    <section aria-label="Pair a device" className="flex min-h-0 flex-1 flex-col gap-6 overflow-y-auto p-6">
      <header className="flex flex-col gap-1">
        <h1 className="text-base font-semibold">Pair a device with {host.info.name}</h1>
        <p className="text-xs text-muted-foreground">
          Make a one-time link for another app (a phone, another desktop). It works once and expires; whoever has it can pair until then, so treat it like a password.
        </p>
      </header>

      {!made && (
        <form onSubmit={create} className="flex max-w-md flex-col gap-3">
          <Field label="Name of the device" hint="Optional; the device may give its own name.">
            <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="Pixel 9" />
          </Field>
          <Field label="Link lasts">
            <Select value={lifetime} onChange={(e) => setLifetime(Number(e.target.value))}>
              {LIFETIMES.map((l) => (
                <option key={l.seconds} value={l.seconds}>{l.label}</option>
              ))}
            </Select>
          </Field>
          <Field
            label="Address the device reaches this host at"
            hint={
              caps.includes('tls')
                ? 'https://host:port of this host\'s --listen address, if another device reaches it directly (the host\'s own listen address when empty). A relay needs no address.'
                : 'This host does not listen for other machines (no --listen). With a relay set up the device needs no address.'
            }
          >
            <Input value={directUrl} onChange={(e) => setDirectUrl(e.target.value)} placeholder="https://192.168.1.5:9443" spellCheck={false} />
          </Field>
          <label className="flex items-center gap-2 text-xs">
            <input type="checkbox" checked={readOnly} onChange={(e) => setReadOnly(e.target.checked)} /> Read-only: the device can look but not act
          </label>
          {!reachable && (
            <p role="note" className="text-xs text-state-needs">
              This host has neither a direct TLS listener nor a relay, so a device on another machine can't reach it. Start meshd with <code className="font-mono">--listen</code> or the relay flags first.
            </p>
          )}
          {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
          <Button type="submit" disabled={busy || !loaded}>Create pairing link</Button>
        </form>
      )}

      {made && (
        <div className="flex flex-col items-start gap-4" data-testid="pairing">
          {paired ? (
            <p role="status" className="flex items-center gap-2 text-sm font-medium text-state-done">
              <Check className="size-4" /> Paired: {paired.name}
            </p>
          ) : (
            <>
              <QrCode text={made.res.uri} label="Pairing QR code" />
              <div className="flex w-full max-w-xl flex-col gap-1.5">
                <label htmlFor="pairing-link" className="text-xs font-medium text-muted-foreground">Pairing link</label>
                <div className="flex gap-2">
                  <Input id="pairing-link" readOnly value={made.res.uri} className="font-mono text-xs" onFocus={(e) => e.currentTarget.select()} />
                  <Button variant="outline" onClick={() => void copy(made.res.uri)}>
                    {copied ? <Check /> : <Copy />} {copied ? 'Copied' : 'Copy'}
                  </Button>
                </div>
                <Expiry at={made.res.expiresAt} />
              </div>
            </>
          )}
          {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
          <Button variant="secondary" onClick={() => setMade(undefined)}>{paired ? 'Pair another device' : 'Make another link'}</Button>
        </div>
      )}

      <div className="flex max-w-2xl flex-col gap-2">
        <h2 className="text-sm font-semibold">Paired devices</h2>
        {!loaded && !error && <p className="text-xs text-muted-foreground">Loading…</p>}
        {loaded && devices.length === 0 && <p className="text-xs text-muted-foreground">No device is paired yet.</p>}
        {devices.length > 0 && (
          <ul aria-label="Paired devices" className="divide-y rounded-lg border bg-card">
            {devices.map((d) => (
              <DeviceRow key={d.id} host={host} device={d} />
            ))}
          </ul>
        )}
      </div>
    </section>
  )
}

/** When the link stops working, from the host's clock. */
function Expiry({ at }: { at?: { seconds: bigint } }) {
  const text = useMemo(() => (at ? new Date(Number(at.seconds) * 1000).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : ''), [at])
  return <p className="text-xs text-muted-foreground">Works once, until {text}.</p>
}

function DeviceRow({ host, device }: { host: HostRuntime; device: Device }) {
  const [confirm, setConfirm] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined)
  useEffect(() => () => clearTimeout(timer.current), [])
  const isLocal = device.id === LOCAL_DEVICE
  async function revoke() {
    if (busy) return
    setBusy(true)
    setError('')
    try {
      await revokeDevice(host.client, device.id)
      // The DeviceRevoked event takes the row away; so does this, in case it is slow.
      host.store.setState((s) => ({ devices: s.devices.filter((x) => x.id !== device.id) }))
    } catch (e) {
      setError(say(e))
      setConfirm(false)
    } finally {
      setBusy(false)
    }
  }
  return (
    <li className="flex items-center gap-3 px-3 py-2.5 text-[13px]">
      <Smartphone className="size-4 shrink-0 text-muted-foreground" aria-hidden />
      <div className="min-w-0 flex-1">
        <p className="truncate font-medium">
          {device.name || device.id} {device.class === ActorClass.READ_ONLY_DEVICE && <Badge>read-only</Badge>}
        </p>
        <p className="truncate text-xs text-muted-foreground">Paired {when(device)}</p>
        {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
      </div>
      {!isLocal &&
        (confirm ? (
          <Button size="sm" variant="destructive" disabled={busy} onClick={() => void revoke()} aria-label={`Revoke ${device.name || device.id}`}>
            Revoke {device.name || 'device'}?
          </Button>
        ) : (
          <Button size="sm" variant="outline" onClick={() => setConfirm(true)} aria-label={`Revoke access of ${device.name || device.id}`}>
            Revoke
          </Button>
        ))}
    </li>
  )
}
