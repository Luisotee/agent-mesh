import { MessageSquare, SquareTerminal } from 'lucide-react'
import { Tip } from '@/components/ui/tooltip'
import { useHostState } from '@/context'
import { cn } from '@/lib/utils'
import type { AgentTab, HostRuntime } from '@/model'
import { displayChord } from '@/keys'

/** The view an agent shows: its terminal when it has one and the view is `terminal`, else the chat. */
export function viewShown(tab: AgentTab, hasTerminal: boolean): AgentTab {
  return tab === 'terminal' && hasTerminal ? 'terminal' : 'chat'
}

/** Whether an agent's own CLI can be shown (the host has terminals and the agent has one). */
export function useHasTerminal(host: HostRuntime, agentId: string): boolean {
  const can = useHostState(host, (s) => !!s.daemon?.capabilities.includes('terminals'))
  const has = useHostState(host, (s) => !!s.agents[agentId]?.terminalId)
  return can && has
}

/**
 * The agent's two views as a switch, "Chat | CLI": its conversation, or its own CLI in a terminal (kept for the prompts and
 * dialogs only the CLI shows). It is not the project's shell, which is a tab of its own. Nothing shows for an agent with no terminal.
 */
export function ViewSwitch({ host, agentId, tab, setTab, chord }: { host: HostRuntime; agentId: string; tab: AgentTab; setTab: (t: AgentTab) => void; chord?: string }) {
  const has = useHasTerminal(host, agentId)
  if (!has) return null
  const shown = viewShown(tab, has)
  const hint = chord ? ` (${displayChord(chord)})` : ''
  const item = (t: AgentTab, label: string, tip: string, icon: React.ReactNode) => (
    <Tip label={`${tip}${shown === t ? '' : hint}`}>
      <button
        type="button"
        aria-pressed={shown === t}
        aria-label={label}
        onClick={() => setTab(t)}
        className={cn('flex h-6 items-center gap-1 rounded px-1.5 text-xs outline-none focus-visible:ring-2 focus-visible:ring-ring', shown === t ? 'bg-background text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground')}
      >
        {icon} {label}
      </button>
    </Tip>
  )
  return (
    <div role="group" aria-label="View" className="flex shrink-0 items-center gap-0.5 rounded-md bg-muted p-0.5">
      {item('chat', 'Chat', 'The conversation', <MessageSquare className="size-3.5" />)}
      {item('terminal', 'CLI', "The agent's own CLI, with the prompts only it shows", <SquareTerminal className="size-3.5" />)}
    </div>
  )
}
