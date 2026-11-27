import { create } from '@bufbuild/protobuf'
import type { Event } from './gen/agentmesh/v1/types_pb'
import { type Shell, ShellSchema } from './gen/agentmesh/v1/mesh_pb'
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

/**
 * A host's shells after one of its events (`ShellOpened`, `ShellClosed`), so a second device's tab bar follows without listing
 * again. A list not read yet (undefined) stays so: the first `ListShells` is the whole of it. An opened shell is added once
 * (a replay of the event changes nothing); a closed one is marked exited, not removed, so its open tab keeps its project and
 * the + menu stops offering it (`closedShells`); the next listing drops it, and with it the tab, as it always did.
 */
export function applyShellEvent(shells: Shell[] | undefined, p: Event['payload']): Shell[] | undefined {
  if (!shells) return shells
  if (p.case === 'shellOpened') {
    const o = p.value
    return shells.some((x) => x.id === o.id) ? shells : [...shells, create(ShellSchema, { id: o.id, repo: o.repo, startedAt: o.startedAt })]
  }
  if (p.case === 'shellClosed') {
    const c = p.value
    return shells.some((x) => x.id === c.id && !x.exited) ? shells.map((x) => (x.id === c.id ? create(ShellSchema, { ...x, exited: true, exitCode: c.exitCode }) : x)) : shells
  }
  return shells
}
