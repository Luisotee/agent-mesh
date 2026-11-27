import { AgentWaitKind, type AgentWait } from './gen/agentmesh/v1/mesh_pb'
import { AgentRole, AgentState, type Agent } from './gen/agentmesh/v1/types_pb'

/** An idle agent on its own background job or a wait it declared is not at rest: it will go on without anyone's message. */
export const holdsOnItsOwn = (w?: AgentWait): boolean => w?.kind === AgentWaitKind.BACKGROUND || w?.kind === AgentWaitKind.DECLARED

/**
 * Whether an agent is live, for the panel's tree and the dock: most agents of a run sit idle or are over, and listing
 * them all buries the few that do something. Live is working, starting, needing input, limited, in error or unknown; an
 * idle agent only when something waits on it or it waits on something (an answer to its question or plan), and a run's
 * lead, the open agent and a worker that just started (`keep`) always. An idle agent also counts while it waits on its own
 * background job or a wait it declared (`wait`, the host's `ListAgentWaits`). A finished or stopped agent is resting.
 */
export function isActive(a: Agent, o: { waiting: boolean; keep: boolean; wait?: AgentWait }): boolean {
  if (o.keep || a.role === AgentRole.ROOT) return true
  switch (a.state) {
    case AgentState.IDLE:
      return o.waiting || holdsOnItsOwn(o.wait)
    case AgentState.EXITED:
    case AgentState.RETIRED:
      return false
    default:
      return true
  }
}
