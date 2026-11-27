import { useState } from 'react'
import { rewindAgent } from '@/actions'
import { splitImages } from '@/attachments'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent } from '@/components/ui/dialog'
import { useApp, useAppState, useHostState } from '@/context'
import { MESSAGE_LABEL } from '@/derive'
import type { Message } from '@/gen/agentmesh/v1/types_pb'
import type { HostRuntime } from '@/model'
import { ago } from '@/dock'
import { useNow } from '@/hooks'
import type { Block } from '@/transcript'

const say = (e: unknown) => (e instanceof Error ? e.message.replace(/^\[[a-z_]+\]\s*/, '') : String(e))
const ms = (t: Message['createdAt']) => (t ? Number(t.seconds) * 1000 + Math.floor(t.nanos / 1e6) : 0)

type Prompt = Extract<Block, { kind: 'user' }>

/**
 * Rewind (Esc Esc, or ⋯ › Rewind…): the agent's prompts in the chat, newest first; the one picked is confirmed with
 * what a rewind leaves alone (files, commits, and the mesh messages it sent since, listed), then `RewindAgent` takes
 * the conversation back to before it and its text comes back into the composer, to edit and send again.
 */
export function RewindDialog({ host, agentId, blocks, messages }: { host: HostRuntime; agentId: string; blocks: Block[]; messages: Message[] }) {
  const app = useApp()
  const of = useAppState((s) => s.rewindFor)
  const open = of?.hostId === host.info.id && of.agentId === agentId
  const name = useHostState(host, (s) => s.agents[agentId]?.name ?? agentId)
  const [picked, setPicked] = useState<Prompt>()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const now = useNow(30_000)
  const close = () => {
    app.openRewind(undefined)
    setPicked(undefined)
    setError('')
  }
  const prompts = blocks.filter((b): b is Prompt => b.kind === 'user').reverse()
  // With no time on the prompt (an entry without one) nothing can be said to be since it.
  const since = picked && picked.at > 0 ? messages.filter((m) => m.from?.id === agentId && ms(m.createdAt) >= picked.at) : []
  async function rewind(p: Prompt) {
    if (busy) return
    setBusy(true)
    setError('')
    try {
      const res = await rewindAgent(host.client, agentId, p.entry.id)
      const back = splitImages(res.text)
      app.store.setState({ composerFill: { agentId, text: back.text, images: back.paths } })
      close()
    } catch (e) {
      setError(say(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog open={open} onOpenChange={(o) => !o && close()}>
      {open && (
        <DialogContent title={`Rewind ${name}`} description={picked ? undefined : 'Take the conversation back to before one of its prompts. The prompt comes back into the box to edit and send again.'}>
          {!picked ? (
            prompts.length === 0 ? (
              <p className="text-sm text-muted-foreground">No prompts in the conversation shown. Load earlier entries in the chat to go further back.</p>
            ) : (
              <ul aria-label="Prompts" className="flex max-h-[50vh] flex-col gap-1 overflow-y-auto">
                {prompts.map((p) => (
                  <li key={p.id}>
                    <button type="button" onClick={() => setPicked(p)} className="flex w-full items-baseline gap-2 rounded-md px-2 py-1.5 text-left text-sm outline-none hover:bg-muted focus-visible:ring-2 focus-visible:ring-ring">
                      <span className="min-w-0 flex-1 truncate">{splitImages(p.entry.text).text.split('\n')[0] || 'An image'}</span>
                      {p.at > 0 && <time className="shrink-0 text-xs text-muted-foreground">{ago(p.at, now)}</time>}
                    </button>
                  </li>
                ))}
              </ul>
            )
          ) : (
            <div className="flex flex-col gap-3 text-sm">
              <blockquote className="max-h-32 overflow-y-auto border-l-2 pl-3 whitespace-pre-wrap text-muted-foreground">{picked.entry.text}</blockquote>
              <p>
                Its CLI restarts on the conversation up to before this prompt, which comes back into the box. Files and commits are not rolled back.
              </p>
              {since.length > 0 && (
                <div role="note" className="flex flex-col gap-1 rounded-md border border-state-needs/40 p-2 text-xs">
                  <p>It sent these since, and they stay sent:</p>
                  <ul className="list-disc pl-4">
                    {since.map((m) => (
                      <li key={m.id}>
                        {MESSAGE_LABEL[m.type]} to {m.toName || m.toId}: {m.body.split('\n')[0]}
                      </li>
                    ))}
                  </ul>
                </div>
              )}
            </div>
          )}
          {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
          <div className="flex justify-end gap-2">
            {picked && (
              <Button type="button" variant="ghost" onClick={() => setPicked(undefined)}>
                Back
              </Button>
            )}
            <Button type="button" variant="ghost" onClick={close}>
              Cancel
            </Button>
            {picked && (
              <Button type="button" variant="destructive" disabled={busy} onClick={() => void rewind(picked)}>
                Rewind
              </Button>
            )}
          </div>
        </DialogContent>
      )}
    </Dialog>
  )
}
