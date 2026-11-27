import type { Shell } from './gen/agentmesh/v1/mesh_pb'
import { projectKey } from './sidebar'

/**
 * A project shell's tab is an ordinary tab (`workspace.ts`) whose "agent id" is `shell:<terminal id>`: the tabs, panes, drag,
 * layout and Ctrl+Tab all key on that string, so a shell needs no second kind of tab. The shells themselves are the host's
 * (`ListShells`), kept in the app state by host id.
 */
const PREFIX = 'shell:'

export const shellTabId = (shellId: string) => PREFIX + shellId
export const isShellTab = (agentId: string | undefined): agentId is string => !!agentId && agentId.startsWith(PREFIX)
export const shellIdOf = (agentId: string) => agentId.slice(PREFIX.length)

/** The project a shell tab is in, as the sidebar files projects (`sidebar.ts` `projectKey`): its host and folder. */
export const shellProject = (hostId: string, s: Shell) => projectKey(hostId, s.repo)

/** The shells of a host that no tab shows: what the + menu offers to open again. */
export function closedShells(shells: Shell[] | undefined, tabs: ReadonlyArray<{ hostId: string; agentId: string }>, hostId: string, repo?: string): Shell[] {
  return (shells ?? []).filter((s) => !s.exited && (!repo || s.repo === repo) && !tabs.some((t) => t.hostId === hostId && t.agentId === shellTabId(s.id)))
}
