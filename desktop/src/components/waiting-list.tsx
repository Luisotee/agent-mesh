import { CircleAlert, ClipboardList, MessageCircleQuestion } from 'lucide-react'
import { useMemo } from 'react'
import { useApp, useHostState } from '@/context'
import { waitingOnYou, type Waiting } from '@/derive'
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

/** What waits on the user, across the host's open runs: the plans and questions to answer, the prompts to deal with. */
export function WaitingList({ host }: { host: HostRuntime }) {
  const app = useApp()
  const runs = useHostState(host, (s) => s.runs)
  const agents = useHostState(host, (s) => s.agents)
  const messages = useHostState(host, (s) => s.messages)
  const items = useMemo(() => waitingOnYou(runs, agents, messages), [runs, agents, messages])
  if (items.length === 0) return null
  return (
    <section aria-label="Waiting on you" className="border-b px-2 pb-2">
      <h2 className="flex items-center gap-2 px-1 py-2 text-xs font-semibold tracking-wide text-muted-foreground uppercase">
        Waiting on you
        <span className="rounded-full bg-state-needs px-1.5 text-[10px] leading-4 font-bold text-primary-foreground">{items.length}</span>
      </h2>
      <ul>
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
