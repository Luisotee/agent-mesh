import { Plus } from 'lucide-react'
import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogTrigger } from '@/components/ui/dialog'
import { Field } from '@/components/ui/label'
import { Input, Select } from '@/components/ui/input'
import { useApp } from '@/context'

/** The form for a host: a state dir of a daemon on this machine, or a loopback/VPN address with its token. */
export function AddHostForm({ onDone }: { onDone?: () => void }) {
  const app = useApp()
  const [kind, setKind] = useState<'state-dir' | 'tcp'>('state-dir')
  const [name, setName] = useState('')
  const [path, setPath] = useState('')
  const [address, setAddress] = useState('')
  const [token, setToken] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      await app.addHost(kind === 'state-dir' ? { name, kind, path: path.trim() } : { name, kind, address: address.trim(), token })
      onDone?.()
    } catch (err) {
      setError(err instanceof Error ? err.message.replace(/^Error invoking remote method '[^']*': (Error: )?/, '') : String(err))
    } finally {
      setBusy(false)
    }
  }
  return (
    <form onSubmit={submit} className="flex flex-col gap-3">
      <Field label="Name">
        <Input autoFocus required value={name} onChange={(e) => setName(e.target.value)} placeholder="home" />
      </Field>
      <Field label="How to reach it">
        <Select value={kind} onChange={(e) => setKind(e.target.value as typeof kind)}>
          <option value="state-dir">A daemon on this machine (its state dir)</option>
          <option value="tcp">An address (loopback, VPN) with its token</option>
        </Select>
      </Field>
      {kind === 'state-dir' ? (
        <Field label="State directory" hint="The dir meshd was started with (--state). The app reads its socket and device token.">
          <Input required value={path} onChange={(e) => setPath(e.target.value)} placeholder="/home/you/.local/state/meshd" />
        </Field>
      ) : (
        <>
          <Field label="Address" hint="host:port of meshd's --tcp listener.">
            <Input required value={address} onChange={(e) => setAddress(e.target.value)} placeholder="127.0.0.1:7777" />
          </Field>
          <Field label="Device token" hint="Stored on this machine in the app's data folder, readable only by you.">
            <Input required type="password" value={token} onChange={(e) => setToken(e.target.value)} />
          </Field>
        </>
      )}
      {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
      <Button type="submit" disabled={busy}>Add host</Button>
    </form>
  )
}

export function AddHostDialog() {
  const [open, setOpen] = useState(false)
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="icon" variant="ghost" aria-label="Add a host" />}>
        <Plus />
      </DialogTrigger>
      <DialogContent title="Add a host" description="A host is a machine running meshd, the agent-mesh daemon.">
        <AddHostForm onDone={() => setOpen(false)} />
      </DialogContent>
    </Dialog>
  )
}
