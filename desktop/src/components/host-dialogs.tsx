import { Plus } from 'lucide-react'
import { useState } from 'react'
import type { OpenedLink } from '../../shared/api'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogTrigger } from '@/components/ui/dialog'
import { Field } from '@/components/ui/label'
import { Input, Select, Textarea } from '@/components/ui/input'
import { useApp } from '@/context'

/** The form for a host: the state dir of a daemon on this machine, or a pairing link from another host's "Pair a device" screen. */
export function AddHostForm({ onDone, opened }: { onDone?: () => void; opened?: OpenedLink }) {
  const app = useApp()
  // A link the operating system opened the app with fills the form in; adding the host stays the user's click.
  const [kind, setKind] = useState<'state-dir' | 'pair'>(opened ? 'pair' : 'state-dir')
  const [name, setName] = useState(opened?.hostName ?? '')
  const [path, setPath] = useState('')
  const [link, setLink] = useState(opened?.link ?? '')
  const [deviceName, setDeviceName] = useState('Desktop')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      await app.addHost(kind === 'state-dir' ? { name, kind, path: path.trim() } : { name, kind, link: link.trim(), deviceName })
      onDone?.()
    } catch (err) {
      setError(err instanceof Error ? err.message.replace(/^Error invoking remote method '[^']*': (Error: )?/, '') : String(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <form onSubmit={submit} className="flex flex-col gap-3">
      {opened && (
        <p role="note" className="rounded-md border border-state-needs p-2 text-xs">
          The app was opened by a pairing link{opened.hostName ? ` for ${opened.hostName}` : ''}. It comes from outside the app (a web page, a message, a QR scanner): add the host only if you made the link or trust who did.
        </p>
      )}
      <Field label="Name">
        <Input autoFocus required={kind === 'state-dir'} value={name} onChange={(e) => setName(e.target.value)} placeholder={kind === 'pair' ? "the host's own name" : 'home'} />
      </Field>
      <Field label="How to reach it">
        <Select value={kind} onChange={(e) => setKind(e.target.value as typeof kind)}>
          <option value="state-dir">A daemon on this machine (its state dir)</option>
          <option value="pair">Another machine, by a pairing link</option>
        </Select>
      </Field>
      {kind === 'state-dir' ? (
        <Field label="State directory" hint="The dir meshd was started with (--state). The app reads its socket and device token.">
          <Input required value={path} onChange={(e) => setPath(e.target.value)} placeholder="/home/you/.local/state/meshd" />
        </Field>
      ) : (
        <>
          <Field label="Pairing link" hint="From the other host's Pair a device screen (or mesh pair). It works once, for a few minutes.">
            <Textarea required value={link} onChange={(e) => setLink(e.target.value)} placeholder="agentmesh://pair?v=1&h=…" spellCheck={false} />
          </Field>
          <Field label="This device's name" hint="What the host calls this app in its list of paired devices.">
            <Input required value={deviceName} onChange={(e) => setDeviceName(e.target.value)} />
          </Field>
        </>
      )}
      {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
      <Button type="submit" disabled={busy}>Add host</Button>
    </form>
  )
}

/** The add-a-host dialog; a link opened from outside (`opened`) opens it filled in, and `onClose` tells the app it was dealt with. */
export function AddHostDialog({ opened, onClose }: { opened?: OpenedLink; onClose?: () => void }) {
  const [clicked, setClicked] = useState(false)
  const open = clicked || !!opened
  const setOpen = (o: boolean) => {
    setClicked(o)
    if (!o) onClose?.()
  }
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="icon" variant="ghost" aria-label="Add a host" />}>
        <Plus />
      </DialogTrigger>
      <DialogContent title="Add a host" description="A host is a machine running meshd, the agent-mesh daemon.">
        <AddHostForm key={opened?.link} opened={opened} onDone={() => setOpen(false)} />
      </DialogContent>
    </Dialog>
  )
}
