import { Code, ConnectError } from '@connectrpc/connect'
import { CircleAlert, ClipboardList, MessageCircleQuestion, ShieldQuestion } from 'lucide-react'
import { useMemo, useState } from 'react'
import { decidePermission } from '@/actions'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { useApp, useHostState } from '@/context'
import { joinWaiting, waitingCount, type Waiting } from '@/derive'
import type { PendingPermission } from '@/gen/agentmesh/v1/mesh_pb'
import type { HostRuntime } from '@/model'

const LABEL: Record<Waiting['kind'], string> = {
  plan: 'sent a plan',
  question: 'asked a question',
  input: 'needs you in its terminal',
}

const ICON = {
  plan: <ClipboardList className="size-4 text-primary" />,
  question: <MessageCircleQuestion className="size-4 text-state-needs" />,
  input: <CircleAlert className="size-4 text-state-needs" />,
}

/**
 * What waits on the user, across the host's open runs: the plans and questions to answer, the prompts to deal with.
 * An agent stopped at a permission prompt the host holds for its parent has Allow and Deny here (the parent may answer
 * first); any other prompt is for its terminal.
 */
export function WaitingList({ host }: { host: HostRuntime }) {
  const app = useApp()
  const rows = useHostState(host, (s) => s.waiting)
  const runs = useHostState(host, (s) => s.runs)
  const agents = useHostState(host, (s) => s.agents)
  const permissions = useHostState(host, (s) => s.permissions)
  // A held prompt replaces the "needs you in its terminal" row of its agent: the same wait, with something to press.
  const held = useMemo(() => new Set(permissions.map((p) => p.agentId)), [permissions])
  const items = useMemo(() => joinWaiting(rows, runs, agents).filter((w) => !(w.kind === 'input' && held.has(w.agent.id))), [rows, runs, agents, held])
  const prompts = useMemo(() => permissions.filter((p) => agents[p.agentId]), [permissions, agents])
  const count = waitingCount(rows, permissions, runs, agents)
  if (count === 0) return null
  return (
    <section aria-label="Waiting on you" className="border-b px-2 pb-2">
      <h2 className="flex items-center gap-2 px-1 py-2 text-xs font-semibold tracking-wide text-muted-foreground uppercase">
        Waiting on you
        <span className="rounded-full bg-state-needs px-1.5 text-[10px] leading-4 font-bold text-primary-foreground">{count}</span>
      </h2>
      <ul>
        {prompts.map((p) => (
          <li key={`permission:${p.id}`}>
            <PermissionRow host={host} permission={p} />
          </li>
        ))}
        {items.map((w) => (
          <li key={`${w.kind}:${w.message?.id ?? w.agent.id}`}>
            <button
              type="button"
              onClick={() => app.selectAgent(host.info.id, w.agent.id, w.kind === 'input' ? 'terminal' : 'messages')}
              className="flex w-full items-start gap-2 rounded-md px-2 py-1.5 text-left hover:bg-accent"
            >
              <span className="mt-0.5">{ICON[w.kind]}</span>
              <span className="min-w-0 flex-1">
                <span className="block truncate">
                  <span className="font-medium">{w.agent.name}</span> {LABEL[w.kind]}
                </span>
                {w.run && <span className="block truncate text-[11px] text-muted-foreground">{w.run.title || w.run.id}</span>}
              </span>
            </button>
          </li>
        ))}
      </ul>
    </section>
  )
}

/** What a failed answer says: a prompt someone else answered first is not an error. */
export function permissionError(e: unknown): string {
  const err = ConnectError.from(e)
  if (err.code === Code.NotFound) return 'Already answered (by its lead, at its terminal, or it ran out).'
  return err.rawMessage || err.message
}

/** One held permission prompt: the tool and what it is about to touch, with Allow, and Deny with the reason the agent reads. */
function PermissionRow({ host, permission: p }: { host: HostRuntime; permission: PendingPermission }) {
  const app = useApp()
  const parent = useHostState(host, (s) => (p.parentId ? s.agents[p.parentId]?.name : undefined)) ?? p.parentId
  const [denying, setDenying] = useState(false)
  const [why, setWhy] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [gone, setGone] = useState(false)
  async function decide(decision: 'allow' | 'deny') {
    if (busy) return
    setBusy(true)
    setError('')
    try {
      await decidePermission(host.client, p.id, decision, why.trim())
      host.store.setState((s) => ({ permissions: s.permissions.filter((x) => x.id !== p.id) }))
    } catch (e) {
      setError(permissionError(e))
      // The prompt is gone (answered elsewhere): the host tells so with an event and the list is read again; until then the buttons do nothing.
      setGone(ConnectError.from(e).code === Code.NotFound)
    } finally {
      setBusy(false)
    }
  }
  return (
    <div aria-label={`${p.agentName} asks to use ${p.tool}`} className="flex flex-col gap-1.5 rounded-md px-2 py-1.5">
      <button type="button" onClick={() => app.selectAgent(host.info.id, p.agentId, 'terminal')} className="flex items-start gap-2 text-left hover:underline">
        <ShieldQuestion className="mt-0.5 size-4 shrink-0 text-state-needs" />
        <span className="min-w-0 flex-1">
          <span className="block truncate"><span className="font-medium">{p.agentName}</span> asks to use <code className="rounded bg-muted px-1">{p.tool}</code></span>
          {p.summary && <code className="block truncate text-[11px] text-muted-foreground" title={p.summary}>{p.summary}</code>}
          {parent && <span className="block truncate text-[11px] text-muted-foreground">held for {parent}</span>}
        </span>
      </button>
      {gone ? null : denying ? (
        <form
          className="flex items-center gap-1.5"
          onSubmit={(e) => {
            e.preventDefault()
            if (why.trim()) void decide('deny')
          }}
        >
          <Input aria-label={`Why ${p.agentName} may not`} autoFocus value={why} onChange={(e) => setWhy(e.target.value)} placeholder="Why, and what to do instead" className="h-7 flex-1 text-xs" />
          <Button size="sm" type="submit" variant="destructive" disabled={busy || !why.trim()}>Deny</Button>
          <Button size="sm" type="button" variant="ghost" onClick={() => setDenying(false)}>Cancel</Button>
        </form>
      ) : (
        <div className="flex gap-1.5 pl-6">
          <Button size="sm" disabled={busy} onClick={() => void decide('allow')}>Allow</Button>
          <Button size="sm" variant="outline" disabled={busy} onClick={() => setDenying(true)}>Deny…</Button>
        </div>
      )}
      {error && <p role="alert" className="pl-6 text-[11px] text-destructive">{error}</p>}
    </div>
  )
}
