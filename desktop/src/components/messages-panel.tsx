import { CheckCircle2, ChevronDown, ChevronUp, ClipboardList, FileText, MessageCircleQuestion, XCircle } from 'lucide-react'
import { MarkdownField } from '@/components/ui/markdown-field'
import { useState } from 'react'
import { answer } from '@/actions'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { MESSAGE_LABEL } from '@/derive'
import { MessageType, type Message } from '@/gen/agentmesh/v1/types_pb'
import type { HostRuntime } from '@/model'
import { Markdown } from './markdown'

const LONG = 700

function fromName(m: Message): string {
  return m.from?.name || m.from?.id || 'someone'
}

function stamp(m: Message): string {
  return m.createdAt ? new Date(Number(m.createdAt.seconds) * 1000).toLocaleString() : ''
}

const ICON: Partial<Record<MessageType, React.ReactNode>> = {
  [MessageType.PLAN]: <ClipboardList className="size-4 text-primary" />,
  [MessageType.QUESTION]: <MessageCircleQuestion className="size-4 text-state-needs" />,
  [MessageType.DONE]: <CheckCircle2 className="size-4 text-state-done" />,
  [MessageType.FAILED]: <XCircle className="size-4 text-state-failed" />,
}

/** A mesh message to or from an agent, in its chat: a plan or question waiting on the user is answerable in place. */
export function MessageCard({ host, message: m, open }: { host: HostRuntime; message: Message; open: boolean }) {
  const long = m.body.length > LONG
  const [expanded, setExpanded] = useState(open)
  const collapsed = long && !expanded && !open
  return (
    <article aria-label={`${MESSAGE_LABEL[m.type]} from ${fromName(m)}`} className={`flex flex-col gap-2 rounded-lg border bg-card p-3 ${open ? 'border-state-needs' : ''}`}>
      <header className="flex items-center gap-2 text-xs">
        {ICON[m.type] ?? <FileText className="size-4 text-muted-foreground" />}
        <Badge>{MESSAGE_LABEL[m.type]}</Badge>
        <span className="font-medium">{fromName(m)}</span>
        <span className="text-muted-foreground">to {m.toName || m.toId}</span>
        <span className="flex-1" />
        {open && <Badge className="border-state-needs text-state-needs">waiting for you</Badge>}
        <time className="text-muted-foreground">{stamp(m)}</time>
      </header>
      <div className={collapsed ? 'max-h-40 overflow-hidden [mask-image:linear-gradient(black_60%,transparent)]' : ''}>
        <Markdown>{m.body}</Markdown>
      </div>
      {long && !open && (
        <Button size="sm" variant="ghost" className="self-start" onClick={() => setExpanded((e) => !e)}>
          {expanded ? <ChevronUp /> : <ChevronDown />} {expanded ? 'Show less' : 'Show all'}
        </Button>
      )}
      {open && <AnswerForm host={host} message={m} />}
    </article>
  )
}

/** The user's reply: a plan is approved or sent back with changes, a question is answered; both go to the author as an `answer`. */
export function AnswerForm({ host, message: m }: { host: HostRuntime; message: Message }) {
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const author = m.from?.id ?? ''
  const isPlan = m.type === MessageType.PLAN
  async function send(body: string) {
    if (busy) return
    setBusy(true)
    setError('')
    try {
      await answer(host.client, author, body)
      setText('')
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <form
      aria-label={isPlan ? 'Answer the plan' : 'Answer the question'}
      className="flex flex-col gap-2 border-t pt-3"
      onSubmit={(e) => {
        e.preventDefault()
        if (text.trim()) void send(text.trim())
      }}
    >
      <MarkdownField
        aria-label={isPlan ? 'Changes to ask for' : 'Your answer'}
        className="min-h-16"
        value={text}
        onValueChange={setText}
        placeholder={isPlan ? 'What should change? (to approve, no text needed)' : 'Answer for the agent'}
      />
      <div className="flex items-center gap-2">
        {isPlan && (
          // Typed changes are not an approval: send them with Request changes, or clear the box to approve.
          <Button disabled={busy || !!text.trim()} title={text.trim() ? 'Clear the text to approve, or use Request changes' : undefined} onClick={() => void send('approved')}>
            Approve
          </Button>
        )}
        <Button type="submit" variant={isPlan ? 'secondary' : 'default'} disabled={busy || !text.trim()}>
          {isPlan ? 'Request changes' : 'Send answer'}
        </Button>
        {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
      </div>
    </form>
  )
}
