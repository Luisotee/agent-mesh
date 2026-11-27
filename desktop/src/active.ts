import { AgentRole, AgentState, type Agent } from './gen/agentmesh/v1/types_pb'

/**
 * Whether an agent is live, for the panel's tree and the dock: most agents of a run sit idle or are over, and listing
 * them all buries the few that do something. Live is working, starting, needing input, limited, in error or unknown; an
 * idle agent only when something waits on it or it waits on something (an answer to its question or plan), and a run's
 * lead and the open agent always. A finished or stopped agent is resting.
 */
export function isActive(a: Agent, o: { waiting: boolean; open: boolean }): boolean {
  if (o.open || a.role === AgentRole.ROOT) return true
  switch (a.state) {
    case AgentState.IDLE:
      return o.waiting
    case AgentState.EXITED:
    case AgentState.RETIRED:
      return false
    default:
      return true
  }
}
