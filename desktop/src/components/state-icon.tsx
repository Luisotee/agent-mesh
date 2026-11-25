import { CircleAlert, CircleCheck, CircleDashed, CircleHelp, Gauge, LoaderCircle, Moon, OctagonAlert, OctagonX, Pause, Square } from 'lucide-react'
import { AgentState, type Agent } from '@/gen/agentmesh/v1/types_pb'
import { cn } from '@/lib/utils'

/**
 * An agent's state as an icon whose shape differs by state, so it reads without colour: a dashed circle starting,
 * a pause idle, a spinner working (still when the user prefers reduced motion), a warning needing you, a check
 * done, a square stopped by you, a crossed octagon failed, a question mark unknown, a moon retired (stopped on
 * purpose, a message wakes it), a gauge at its usage limit, an octagon with a mark its last turn ended in an error.
 */
export function StateIcon({ agent, className }: { agent: Agent; className?: string }) {
  const c = cn('size-4 shrink-0', className)
  switch (agent.state) {
    case AgentState.STARTING:
      return <CircleDashed aria-label="starting" className={cn(c, 'text-state-idle')} />
    case AgentState.IDLE:
      return <Pause aria-label="idle" className={cn(c, 'text-state-idle')} />
    case AgentState.WORKING:
      return <LoaderCircle aria-label="working" className={cn(c, 'animate-spin text-state-working motion-reduce:animate-none')} />
    case AgentState.NEEDS_INPUT:
      return <CircleAlert aria-label="needs you" className={cn(c, 'text-state-needs')} />
    case AgentState.EXITED:
      // A process ended by a signal has exit code -1 (Go's ExitCode), and the reason is gone once the host reloads it.
      if (agent.stateDetail === 'killed' || agent.exitCode < 0) return <Square aria-label="stopped" className={cn(c, 'text-state-idle')} />
      return agent.exitCode === 0 && agent.stateDetail !== 'lost' ? (
        <CircleCheck aria-label="done" className={cn(c, 'text-state-done')} />
      ) : (
        <OctagonX aria-label="failed" className={cn(c, 'text-state-failed')} />
      )
    case AgentState.RETIRED:
      return <Moon aria-label="retired" className={cn(c, 'text-state-idle')} />
    case AgentState.LIMITED:
      return <Gauge aria-label="at its limit" className={cn(c, 'text-state-needs')} />
    case AgentState.ERROR:
      return <OctagonAlert aria-label="error" className={cn(c, 'text-state-failed')} />
    default:
      return <CircleHelp aria-label="unknown" className={cn(c, 'text-state-unknown')} />
  }
}
