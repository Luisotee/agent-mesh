import { useState } from 'react'
import { Button } from './button'
import { Dialog, DialogContent } from './dialog'

/** A yes-or-no question before something that can't be taken back (Stop, Retire): the action's failure shows in it. */
export function ConfirmDialog({ open, onOpenChange, title, description, action, run }: { open: boolean; onOpenChange: (open: boolean) => void; title: string; description: string; action: string; run: () => Promise<unknown> }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  async function go() {
    if (busy) return
    setBusy(true)
    setError('')
    try {
      await run()
      onOpenChange(false)
    } catch (e) {
      setError(e instanceof Error ? e.message.replace(/^\[[a-z_]+\]\s*/, '') : String(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog open={open} onOpenChange={(o) => { onOpenChange(o); if (!o) setError('') }}>
      <DialogContent title={title} description={description}>
        {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={() => onOpenChange(false)}>Cancel</Button>
          <Button variant="destructive" disabled={busy} onClick={() => void go()}>{action}</Button>
        </div>
      </DialogContent>
    </Dialog>
  )
}
