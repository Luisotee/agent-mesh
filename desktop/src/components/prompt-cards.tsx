import { Code, ConnectError } from '@connectrpc/connect'
import { ClipboardList, MessageCircleQuestion, ShieldQuestion } from 'lucide-react'
import { useId, useMemo, useState } from 'react'
import { decidePermission } from '@/actions'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox, Radio, RadioGroup } from '@/components/ui/choice'
import { Input, Select } from '@/components/ui/input'
import type { PromptQuestion } from '@/gen/agentmesh/v1/chat_pb'
import { PermissionKind, type PendingPermission } from '@/gen/agentmesh/v1/mesh_pb'
import type { HostRuntime } from '@/model'
import { answersFromResult, answersOf, hasPreviews, isComplete, pickOption, previewOf, writeOther, type Picks } from '@/prompts'
import type { ToolCall } from '@/transcript'
import { cn } from '@/lib/utils'
import { Markdown } from './markdown'

/** What a failed answer says: a prompt someone else answered first (at the terminal, from another device) is not an error. */
function say(e: unknown): { text: string; gone: boolean } {
  const err = ConnectError.from(e)
  if (err.code === Code.NotFound) return { text: 'Already answered (at its terminal, from another device, or it ran out).', gone: true }
  return { text: err.rawMessage || err.message, gone: false }
}

/** What every card does: send the answer, drop the prompt from the list on success, say what went wrong otherwise. */
function useDecide(host: HostRuntime, p: PendingPermission) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [gone, setGone] = useState(false)
  async function decide(decision: 'allow' | 'deny', why = '', extra: Parameters<typeof decidePermission>[4] = {}) {
    if (busy) return
    setBusy(true)
    setError('')
    try {
      await decidePermission(host.client, p.id, decision, why.trim(), extra)
      host.store.setState((s) => ({ permissions: s.permissions.filter((x) => x.id !== p.id) }))
    } catch (e) {
      const m = say(e)
      setError(m.text)
      setGone(m.gone)
    } finally {
      setBusy(false)
    }
  }
  return { busy, error, gone, decide }
}

/** The shell of a card: who is asking, what, and that it waits for the user. */
function Card({ p, icon, title, children, className }: { p: PendingPermission; icon: React.ReactNode; title: string; children: React.ReactNode; className?: string }) {
  return (
    <article aria-label={`${title} from ${p.agentName}`} data-kind="prompt" className={cn('flex flex-col gap-3 rounded-lg border border-state-needs bg-card p-3', className)}>
      <header className="flex items-center gap-2 text-xs">
        {icon}
        <Badge>{title}</Badge>
        <span className="font-medium">{p.agentName}</span>
        <span className="flex-1" />
        <Badge className="border-state-needs text-state-needs">waiting for you</Badge>
      </header>
      {children}
    </article>
  )
}

/** The prompts an agent waits on that only the user answers, as cards in its chat. */
export function PromptCards({ host, prompts, readOnly }: { host: HostRuntime; prompts: PendingPermission[]; readOnly: boolean }) {
  return (
    <ol aria-label="Waiting for you" className="flex flex-col gap-3">
      {prompts.map((p) => (
        <li key={p.id}>
          {p.kind === PermissionKind.QUESTION ? <QuestionCard host={host} prompt={p} readOnly={readOnly} /> : p.kind === PermissionKind.PLAN ? <PlanCard host={host} prompt={p} readOnly={readOnly} /> : <ToolCard host={host} prompt={p} readOnly={readOnly} />}
        </li>
      ))}
    </ol>
  )
}

const READ_ONLY = 'This device is read-only: answer it from another device, or at the agent’s terminal.'

/** An AskUserQuestion: every question with its options (radio or checkbox), an "Other" text, previews beside the options. */
function QuestionCard({ host, prompt: p, readOnly }: { host: HostRuntime; prompt: PendingPermission; readOnly: boolean }) {
  const [picks, setPicks] = useState<Picks>({})
  const { busy, error, gone, decide } = useDecide(host, p)
  const complete = isComplete(p.questions, picks)
  return (
    <Card p={p} icon={<MessageCircleQuestion className="size-4 text-state-needs" />} title={p.questions.length === 1 ? 'Question' : `${p.questions.length} questions`}>
      <form
        className="flex flex-col gap-4"
        onSubmit={(e) => {
          e.preventDefault()
          if (complete && !readOnly) void decide('allow', '', { answers: answersOf(p.questions, picks) })
        }}
      >
        {p.questions.map((q) => (
          <Question key={q.question} q={q} picks={picks} onPicks={setPicks} disabled={readOnly || busy || gone} />
        ))}
        <div className="flex items-center gap-2">
          <Button type="submit" size="sm" disabled={!complete || readOnly || busy || gone}>Send answers</Button>
          <Button type="button" size="sm" variant="ghost" disabled={readOnly || busy || gone} onClick={() => void decide('deny', 'The user dismissed the question without answering. Carry on without it, or ask in your reply instead.')}>
            Dismiss
          </Button>
          {readOnly && <span className="text-xs text-muted-foreground">{READ_ONLY}</span>}
        </div>
      </form>
      {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
    </Card>
  )
}

/** One question: its header and text, the options, and the "Other" row every question has. With previews the options sit beside the preview of the one looked at. */
function Question({ q, picks, onPicks, disabled }: { q: PromptQuestion; picks: Picks; onPicks: (f: (cur: Picks) => Picks) => void; disabled: boolean }) {
  const id = useId()
  const [peek, setPeek] = useState<string>()
  const pick = picks[q.question]
  const chosen = pick?.labels ?? []
  const otherOn = (pick?.other ?? '').trim() !== ''
  const previews = hasPreviews(q)
  const preview = previewOf(q, picks, peek)
  const options = (
    <div className="flex min-w-0 flex-1 flex-col gap-1">
      {q.multiSelect ? (
        q.options.map((o, i) => (
          <label key={o.label} onMouseEnter={() => setPeek(o.label)} onMouseLeave={() => setPeek(undefined)} className="flex cursor-pointer items-start gap-2 rounded-md px-1.5 py-1 hover:bg-muted">
            <Checkbox className="mt-0.5" aria-labelledby={`${id}-o${i}`} disabled={disabled} checked={chosen.includes(o.label)} onCheckedChange={() => onPicks((cur) => pickOption(cur, q, o.label))} />
            <span id={`${id}-o${i}`} className="text-sm">
              <span className="font-medium">{o.label}</span>
              {o.description && <span className="block text-xs text-muted-foreground">{o.description}</span>}
            </span>
          </label>
        ))
      ) : (
        <RadioGroup<string> aria-labelledby={`${id}-q`} value={chosen[0] ?? ''} onValueChange={(v) => onPicks((cur) => pickOption(cur, q, v))} disabled={disabled}>
          {q.options.map((o, i) => (
            <label key={o.label} onMouseEnter={() => setPeek(o.label)} onMouseLeave={() => setPeek(undefined)} className="flex cursor-pointer items-start gap-2 rounded-md px-1.5 py-1 hover:bg-muted">
              <Radio className="mt-0.5" value={o.label} aria-labelledby={`${id}-o${i}`} />
              <span id={`${id}-o${i}`} className="text-sm">
                <span className="font-medium">{o.label}</span>
                {o.description && <span className="block text-xs text-muted-foreground">{o.description}</span>}
              </span>
            </label>
          ))}
        </RadioGroup>
      )}
      <div className="flex items-center gap-2 px-1.5">
        <span className={cn('grid size-4 shrink-0 place-items-center rounded-full border', q.multiSelect && 'rounded-[4px]', otherOn ? 'border-primary bg-primary' : 'border-input')} aria-hidden />
        <Input aria-label={`Other answer to ${q.question}`} disabled={disabled} value={pick?.other ?? ''} onChange={(e) => onPicks((cur) => writeOther(cur, q, e.target.value))} placeholder="Other: write your own" className="h-8 flex-1" />
      </div>
    </div>
  )
  return (
    <fieldset className="flex min-w-0 flex-col gap-1.5">
      <legend id={`${id}-q`} className="mb-1 text-sm font-medium">
        {q.header && <Badge className="mr-2 align-middle">{q.header}</Badge>}
        {q.question}
      </legend>
      {previews ? (
        <div className="flex flex-col gap-3 md:flex-row">
          {options}
          <div aria-label="Preview" className="min-h-24 min-w-0 flex-1 overflow-auto rounded-md border bg-surface p-2.5 text-sm">
            {preview ? <Markdown>{preview}</Markdown> : <span className="text-xs text-muted-foreground">No preview for this option.</span>}
          </div>
        </div>
      ) : (
        options
      )}
    </fieldset>
  )
}

/** An ExitPlanMode: the plan, approved (and the mode it goes on in) or sent back with what to change. */
function PlanCard({ host, prompt: p, readOnly }: { host: HostRuntime; prompt: PendingPermission; readOnly: boolean }) {
  const [mode, setMode] = useState('')
  const [changing, setChanging] = useState(false)
  const [why, setWhy] = useState('')
  const { busy, error, gone, decide } = useDecide(host, p)
  const off = readOnly || busy || gone
  return (
    <Card p={p} icon={<ClipboardList className="size-4 text-primary" />} title="Plan">
      <div className="max-h-96 overflow-auto rounded-md border bg-surface p-3">
        <Markdown>{p.plan}</Markdown>
      </div>
      {changing ? (
        <form
          className="flex items-center gap-1.5"
          onSubmit={(e) => {
            e.preventDefault()
            if (why.trim()) void decide('deny', why)
          }}
        >
          <Input aria-label="What to change" autoFocus value={why} onChange={(e) => setWhy(e.target.value)} placeholder="What to change in the plan" className="flex-1" />
          <Button size="sm" type="submit" disabled={off || !why.trim()}>Send back</Button>
          <Button size="sm" type="button" variant="ghost" onClick={() => setChanging(false)}>Cancel</Button>
        </form>
      ) : (
        <div className="flex flex-wrap items-center gap-2">
          <Button size="sm" disabled={off} onClick={() => void decide('allow', '', { mode })}>Approve</Button>
          <Select aria-label="After approving" value={mode} disabled={off} onChange={(e) => setMode(e.target.value)} className="h-8 w-auto">
            <option value="">then keep the current mode</option>
            <option value="default">then ask before each edit</option>
            <option value="acceptEdits">then accept edits</option>
          </Select>
          <Button size="sm" variant="outline" disabled={off} onClick={() => setChanging(true)}>Ask for changes…</Button>
          {readOnly && <span className="text-xs text-muted-foreground">{READ_ONLY}</span>}
        </div>
      )}
      {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
    </Card>
  )
}

/** A tool the agent wants to run that only the user decides (a root's own, a push): Allow, or Deny with the reason it reads. */
function ToolCard({ host, prompt: p, readOnly }: { host: HostRuntime; prompt: PendingPermission; readOnly: boolean }) {
  const [denying, setDenying] = useState(false)
  const [why, setWhy] = useState('')
  const { busy, error, gone, decide } = useDecide(host, p)
  const off = readOnly || busy || gone
  return (
    <Card p={p} icon={<ShieldQuestion className="size-4 text-state-needs" />} title="Permission">
      <p className="text-sm">
        wants to use <code className="rounded bg-muted px-1">{p.tool}</code>
      </p>
      {p.summary && <code className="block max-h-32 overflow-auto rounded-md border bg-surface px-2.5 py-1.5 font-mono text-xs break-all whitespace-pre-wrap">{p.summary}</code>}
      {denying ? (
        <form
          className="flex items-center gap-1.5"
          onSubmit={(e) => {
            e.preventDefault()
            if (why.trim()) void decide('deny', why)
          }}
        >
          <Input aria-label={`Why ${p.agentName} may not`} autoFocus value={why} onChange={(e) => setWhy(e.target.value)} placeholder="Why, and what to do instead" className="flex-1" />
          <Button size="sm" type="submit" variant="destructive" disabled={off || !why.trim()}>Deny</Button>
          <Button size="sm" type="button" variant="ghost" onClick={() => setDenying(false)}>Cancel</Button>
        </form>
      ) : (
        <div className="flex items-center gap-2">
          <Button size="sm" disabled={off} onClick={() => void decide('allow')}>Allow</Button>
          <Button size="sm" variant="outline" disabled={off} onClick={() => setDenying(true)}>Deny…</Button>
          {readOnly && <span className="text-xs text-muted-foreground">{READ_ONLY}</span>}
        </div>
      )}
      {error && <p role="alert" className="text-xs text-destructive">{error}</p>}
    </Card>
  )
}

/** A question the agent asked, in the conversation: what it asked and what was answered (at the terminal or here). */
export function AskedRow({ call, waiting }: { call: ToolCall; waiting: boolean }) {
  const answers = useMemo(() => (call.result ? answersFromResult(call.result.text, call.questions) : new Map<string, string>()), [call])
  return (
    <div data-kind="asked" className="flex max-w-[72ch] flex-col gap-1.5 rounded-md border bg-card px-3 py-2 text-sm">
      <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
        <MessageCircleQuestion className="size-3.5" />
        {call.questions.length === 1 ? 'Asked a question' : `Asked ${call.questions.length} questions`}
        {!call.result && (waiting ? ' · waiting for you' : ' · no answer yet')}
      </p>
      <ul className="flex flex-col gap-1">
        {call.questions.map((q) => (
          <li key={q.question}>
            <span className="font-medium">{q.question}</span>
            {answers.has(q.question) && <span className="block text-muted-foreground">→ {answers.get(q.question)}</span>}
          </li>
        ))}
      </ul>
      {call.result && answers.size === 0 && <p className="text-xs text-muted-foreground">{call.result.text}</p>}
    </div>
  )
}
